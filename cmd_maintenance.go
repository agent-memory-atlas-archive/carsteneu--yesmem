package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/daemon"
	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/sanitize"
	"github.com/carsteneu/yesmem/internal/storage"
)

// redacted wraps a provider client in the secret redactor when
// secrets_sanitization.enabled is on. Package main builds its own clients — the
// factory in internal/daemon/llm_client_factory.go covers the daemon paths only —
// so without this the maintenance commands would send raw session text to the
// provider. runQuickstart's pair is wrapped inside daemon.RunQuickstart.
func redacted(cfg *config.Config, c extraction.LLMClient) extraction.LLMClient {
	if cfg == nil || c == nil || !cfg.SecretsSanitization.Enabled {
		return c
	}
	return extraction.NewSanitizingClient(c, sanitize.NewSecretRedactor(cfg.SecretsSanitization.AllowedExceptions))
}

func runReextract() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	// Parse flags
	fs := flag.NewFlagSet("reextract", flag.ExitOnError)
	project := fs.String("project", "", "filter by project")
	fs.StringVar(project, "p", "", "filter by project (short)")
	last := fs.Int("last", 0, "limit to last N sessions")
	fs.IntVar(last, "l", 0, "limit to last N sessions (short)")
	dryRun := fs.Bool("dry-run", false, "list sessions without re-extracting")
	fs.BoolVar(dryRun, "n", false, "dry-run (short)")
	fs.Parse(os.Args[2:])

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// List sessions matching filters
	sessions, err := store.ListAllSessions(*project, *last)
	if err != nil {
		log.Fatalf("list sessions: %v", err)
	}

	// Filter to sessions with enough messages
	var toProcess []models.Session
	for _, s := range sessions {
		if s.MessageCount > 5 {
			toProcess = append(toProcess, s)
		}
	}

	if len(toProcess) == 0 {
		fmt.Fprintln(os.Stderr, "No sessions to re-extract.")
		return
	}

	fmt.Fprintf(os.Stderr, "Sessions to re-extract: %d\n\n", len(toProcess))
	for _, s := range toProcess {
		fmt.Fprintf(os.Stderr, "  %s  %-25s  %3d msgs  %s\n",
			truncID(s.ID), s.ProjectShort, s.MessageCount, s.StartedAt.Format("2006-01-02"))
	}

	if *dryRun {
		fmt.Fprintf(os.Stderr, "\n--dry-run: no changes made.\n")
		return
	}

	// Need LLM client for extraction
	apiKey := cfg.ResolvedAPIKey()
	if apiKey == "" {
		apiKey = daemon.ReadClaudeCodeAPIKey()
	}
	if apiKey == "" {
		log.Fatal("No API key — set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure in config.yaml")
	}

	client, err := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.ModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
	if err != nil {
		log.Fatalf("LLM client: %v", err)
	}
	client = redacted(cfg, client)

	// Respect extraction mode from config (two-pass vs single-pass)
	var ext extraction.SessionExtractor
	if cfg.Extraction.Mode == "two-pass" {
		extractClient, qErr := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.QualityModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
		if qErr == nil && extractClient != nil {
			ext = extraction.NewTwoPassExtractor(client, redacted(cfg, extractClient), store)
			fmt.Fprintf(os.Stderr, "Mode: two-pass (summarize=%s, extract=%s)\n", client.Model(), extractClient.Model())
		} else {
			ext = extraction.NewExtractor(client, store)
			fmt.Fprintf(os.Stderr, "Mode: single-pass fallback (quality client unavailable)\n")
		}
	} else {
		ext = extraction.NewExtractor(client, store)
		fmt.Fprintf(os.Stderr, "Mode: single-pass (%s)\n", client.Model())
	}
	ext.SetChunkSize(cfg.Extraction.ChunkSize)

	workers := 3
	if client.Name() == "cli" {
		workers = 1
	}
	total := len(toProcess)
	startTime := time.Now()
	fmt.Fprintf(os.Stderr, "\nRe-extracting with %d workers, model: %s\n\n", workers, client.Model())

	var (
		mu      sync.Mutex
		done    int
		errors  int
		deleted int64
	)

	work := make(chan models.Session, total)
	for _, s := range toProcess {
		work <- s
	}
	close(work)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				// Delete existing auto-extracted learnings
				supersededIDs, err := store.SupersedeSessionLearnings(s.ID)
				if err != nil {
					log.Printf("  warn: delete learnings for %s: %v", truncID(s.ID), err)
				}
				// Reset extracted_at so daemon won't skip this session
				store.ResetSessionExtracted(s.ID)
				mu.Lock()
				deleted += int64(len(supersededIDs))
				mu.Unlock()

				// Re-run extraction
				msgs, err := store.GetMessagesBySession(s.ID)
				if err != nil || len(msgs) == 0 {
					mu.Lock()
					errors++
					done++
					mu.Unlock()
					continue
				}

				if err := ext.ExtractAndStore(s.ID, s.ProjectShort, msgs, false); err != nil {
					mu.Lock()
					errors++
					done++
					d := done
					mu.Unlock()
					log.Printf("  [%d/%d] WARN %s: %v", d, total, truncID(s.ID), err)

					if strings.Contains(err.Error(), "rate_limit") {
						time.Sleep(30 * time.Second)
					}
				} else {
					store.MarkSessionExtracted(s.ID)
					mu.Lock()
					done++
					d := done
					elapsed := time.Since(startTime)
					eta := time.Duration(0)
					if d > 0 {
						eta = time.Duration(float64(elapsed) / float64(d) * float64(total-d))
					}
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  [%d/%d] OK %s (%s, %d msgs) — elapsed %s, ETA %s\n",
						d, total, truncID(s.ID), s.ProjectShort, s.MessageCount,
						elapsed.Round(time.Second), eta.Round(time.Second))
				}
			}
		}()
	}

	wg.Wait()
	fmt.Fprintf(os.Stderr, "\nExtraction done: %d sessions, %d errors, %d old learnings superseded\n", done-errors, errors, deleted)

	// Migrate hit_counts from superseded learnings to new ones
	migrated, err := store.MigrateHitCounts()
	if err != nil {
		log.Printf("warn: migrate hit counts: %v", err)
	} else if migrated > 0 {
		fmt.Fprintf(os.Stderr, "Migrated hit_counts to %d new learnings\n", migrated)
	}
}

func runQuickstart() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	// Parse flags
	lastN := 5
	args := os.Args[2:]
	for i, arg := range args {
		switch arg {
		case "--last", "-l":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &lastN)
			}
		}
	}

	// Resolve API key
	apiKey := cfg.ResolvedAPIKey()
	if apiKey == "" {
		apiKey = daemon.ReadClaudeCodeAPIKey()
	}
	if apiKey == "" {
		log.Fatal("No API key — set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure in config.yaml")
	}

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Extraction client (Sonnet)
	client, err := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.ModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
	if err != nil {
		log.Fatalf("LLM client: %v", err)
	}

	// Quality client (Opus) — for narratives, persona synthesis
	var qualityClient extraction.LLMClient
	if cfg.NarrativeModelID() != cfg.ModelID() {
		qc, qErr := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.NarrativeModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
		if qErr != nil {
			log.Printf("Quality model unavailable, using extraction model: %v", qErr)
			qualityClient = client
		} else {
			qualityClient = qc
		}
	} else {
		qualityClient = client
	}

	fmt.Fprintf(os.Stderr, "Quickstart: analyzing last %d sessions (extract=%s, quality=%s)\n\n",
		lastN, client.Model(), qualityClient.Model())

	if err := daemon.RunQuickstart(store, cfg, client, qualityClient, lastN); err != nil {
		log.Fatalf("quickstart: %v", err)
	}
}

func runResolveStale() {
	dataDir := yesmemDataDir()

	dryRun := false
	for _, arg := range os.Args[2:] {
		if arg == "--dry-run" || arg == "-n" {
			dryRun = true
		}
	}

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Reactivate items from previous batch-cleanup so they can be re-evaluated.
	batchCount, err := store.CountBatchCleanup()
	if err != nil {
		log.Fatalf("count batch-cleanup: %v", err)
	}
	if batchCount > 0 {
		if dryRun {
			fmt.Fprintf(os.Stderr, "Would reactivate %d items from previous batch-cleanup (--dry-run).\n", batchCount)
		} else {
			reactivated, err := store.ReactivateBatchCleanup()
			if err != nil {
				log.Fatalf("reactivate batch-cleanup: %v", err)
			}
			fmt.Fprintf(os.Stderr, "Reactivated %d items from previous batch-cleanup for re-evaluation.\n", reactivated)
		}
	}

	unfinished, err := store.GetActiveLearnings("unfinished", "", "", "", 0)
	if err != nil {
		log.Fatalf("get unfinished: %v", err)
	}

	if len(unfinished) == 0 {
		fmt.Println("No active unfinished items found.")
		return
	}

	fmt.Fprintf(os.Stderr, "Found %d active unfinished items:\n\n", len(unfinished))
	for _, u := range unfinished {
		age := time.Since(u.CreatedAt).Hours() / 24
		fmt.Fprintf(os.Stderr, "  #%d [%s] (%.0fd old) %s\n", u.ID, u.Project, age, u.Content)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "\n--dry-run: no changes made. Use resolve(id) or resolve_by_text() to resolve items.\n")
		return
	}

	// Without --dry-run: mark items older than 30 days as stale
	var staleIDs []int64
	cutoff := time.Now().AddDate(0, 0, -30)
	for _, u := range unfinished {
		if u.CreatedAt.Before(cutoff) {
			staleIDs = append(staleIDs, u.ID)
		}
	}

	if len(staleIDs) == 0 {
		fmt.Fprintf(os.Stderr, "\nNo items older than 30 days. Nothing to mark stale.\n")
		return
	}

	if err := store.SetTaskTypeBatch(staleIDs, storage.TaskTypeStale); err != nil {
		log.Fatalf("set task_type stale: %v", err)
	}

	fmt.Fprintf(os.Stderr, "\nMarked %d items as stale (>30 days old).\n", len(staleIDs))

	// Regenerate MEMORY.md. The Claude projects directory comes from the
	// configuration, the same path the running daemon uses.
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))
	daemon.GenerateAllMemoryMDs(store, cfg.ClaudeProjectsDir())
	fmt.Fprintf(os.Stderr, "MEMORY.md updated.\n")
}

// runRestoreShortSessionLearnings reports the learnings that a cleanup discarded
// although the user stated them, and reactivates them on request. Two cleanup
// classes are covered: the short-session rule and the junk cleanup. Listing runs
// on a read-only connection; --apply and --ids are the only writers, and they
// write only rows of the damaged classes.
func runRestoreShortSessionLearnings() {
	dataDir := yesmemDataDir()

	fs := flag.NewFlagSet("restore-short-session-learnings", flag.ExitOnError)
	apply := fs.Bool("apply", false, "reactivate every listed learning")
	idsFlag := fs.String("ids", "", "comma-separated learning ids to reactivate (implies --apply)")
	limit := fs.Int("limit", 0, "list at most N learnings per cleanup class (0: all)")
	fs.Parse(os.Args[2:])

	var ids []int64
	if *idsFlag != "" {
		var err error
		ids, err = parseLearningIDs(*idsFlag)
		if err != nil {
			log.Fatalf("--ids: %v", err)
		}
		*apply = true
	}

	dbPath := filepath.Join(dataDir, "yesmem.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "No yesmem database at %s. Nothing to inspect.\n", dbPath)
		return
	}
	var store *storage.Store
	var err error
	if *apply {
		store, err = storage.Open(dbPath)
	} else {
		store, err = storage.OpenReadOnly(dbPath)
	}
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// printClass lists one cleanup class, so the operator can tell the reasons
	// apart before deciding what to restore.
	printClass := func(label string, rows []models.Learning) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(os.Stderr, "%d learnings were discarded by the %s cleanup although the user stated them:\n\n", len(rows), label)
		for _, l := range rows {
			session := l.SessionID
			if session == "" {
				session = "no session"
			}
			fmt.Fprintf(os.Stderr, "  #%d %s %s (%s) %s\n",
				l.ID, clipText(l.Source, 14), l.CreatedAt.Format("2006-01-02"),
				clipText(session, 40), clipText(l.Content, 100))
		}
		fmt.Fprintln(os.Stderr)
	}

	if !*apply {
		foundShort, err := store.FindWronglySupersededShortSessionLearnings(*limit)
		if err != nil {
			log.Fatalf("find wrongly superseded: %v", err)
		}
		foundJunk, err := store.FindWronglySupersededJunkLearnings(*limit)
		if err != nil {
			log.Fatalf("find wrongly superseded junk learnings: %v", err)
		}
		if len(foundShort)+len(foundJunk) == 0 {
			fmt.Fprintln(os.Stderr, "No learnings were wrongly superseded by a cleanup.")
			return
		}
		printClass("short-session", foundShort)
		printClass("junk", foundJunk)
		fmt.Fprintln(os.Stderr, "Nothing restored. Run again with --apply or --ids 1,2,3 once the list is reviewed.")
		return
	}

	if len(ids) == 0 {
		foundShort, err := store.FindWronglySupersededShortSessionLearnings(*limit)
		if err != nil {
			log.Fatalf("find wrongly superseded: %v", err)
		}
		foundJunk, err := store.FindWronglySupersededJunkLearnings(*limit)
		if err != nil {
			log.Fatalf("find wrongly superseded junk learnings: %v", err)
		}
		for _, l := range foundShort {
			ids = append(ids, l.ID)
		}
		for _, l := range foundJunk {
			ids = append(ids, l.ID)
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "Nothing to restore.")
		return
	}

	// A repeated id would otherwise read as an unrestored remainder.
	seenID := make(map[int64]bool, len(ids))
	unique := ids[:0]
	for _, id := range ids {
		if !seenID[id] {
			seenID[id] = true
			unique = append(unique, id)
		}
	}
	ids = unique

	restoredShort, err := store.RestoreWronglySupersededShortSessionLearnings(ids)
	if err != nil {
		log.Fatalf("restore: %v", err)
	}
	restoredJunk, err := store.RestoreWronglySupersededJunkLearnings(ids)
	if err != nil {
		log.Fatalf("restore junk victims: %v", err)
	}
	restored := restoredShort + restoredJunk
	fmt.Fprintf(os.Stderr, "Restored %d of %d learnings.\n", restored, len(ids))
	if restored < int64(len(ids)) {
		fmt.Fprintln(os.Stderr, "The remainder does not match the damaged classes (a cleanup reason plus a user-stated source) and stays superseded.")
	}
	fmt.Fprintln(os.Stderr, "Restored learnings are searchable immediately; with embedding.search.method: ivf the daemon rebuilds its vector index only on restart.")
}

// runRestoreRuleBasedLearnings reports the learnings that the rule-based dedup
// displaced although the user owns them, and reactivates them on request. The
// dedup leaves two shapes of damage behind, and each one has its own listing and
// its own apply: a row that lost against a displacer, and a row the
// substance-free retire discarded (sentinel 0, no successor at all, YM-467). Dry
// run by default: the listing runs on a read-only connection, and --apply /
// --apply-no-displacer (or their id lists) are the only writers. No scheduler, no
// background run: this is a decision someone reviews, not a job that fires.
//
// Restoring the no-successor class can bring back rows whose content is
// junk-shaped ("[retired #123]"), because the class is defined by the sentinel
// and the source, not by the content, and no cleanup removes a user-owned row
// any more. That is the operator's decision, which is why every row is listed
// with its source and its reason before anything is written.
func runRestoreRuleBasedLearnings() {
	dataDir := yesmemDataDir()

	fs := flag.NewFlagSet("restore-rulebased-learnings", flag.ExitOnError)
	apply := fs.Bool("apply", false, "reactivate every listed learning of the displacer class")
	idsFlag := fs.String("ids", "", "comma-separated learning ids of the displacer class to reactivate (implies --apply)")
	applyNoDisplacer := fs.Bool("apply-no-displacer", false, "reactivate every listed learning of the no-successor class")
	noDisplacerIDsFlag := fs.String("no-displacer-ids", "", "comma-separated learning ids of the no-successor class to reactivate (implies --apply-no-displacer)")
	limit := fs.Int("limit", 0, "list at most N candidates per class; the whole damaged class is scanned either way")
	fs.Parse(os.Args[2:])

	var ids, noDisplacerIDs []int64
	if *idsFlag != "" {
		var err error
		ids, err = parseLearningIDs(*idsFlag)
		if err != nil {
			log.Fatalf("--ids: %v", err)
		}
		*apply = true
	}
	if *noDisplacerIDsFlag != "" {
		var err error
		noDisplacerIDs, err = parseLearningIDs(*noDisplacerIDsFlag)
		if err != nil {
			log.Fatalf("--no-displacer-ids: %v", err)
		}
		*applyNoDisplacer = true
	}

	dbPath := filepath.Join(dataDir, "yesmem.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "No yesmem database at %s. Nothing to inspect.\n", dbPath)
		return
	}
	var store *storage.Store
	var err error
	if *apply || *applyNoDisplacer {
		store, err = storage.Open(dbPath)
	} else {
		store, err = storage.OpenReadOnly(dbPath)
	}
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	scan, err := store.FindRuleBasedSupersededUserLearnings(*limit)
	if err != nil {
		log.Fatalf("find rule-based superseded: %v", err)
	}

	// noSuccessor switches the headline and the per-row line: the no-successor
	// class has no displacer to name, so printing one would be a lie.
	printVictims := func(victims []storage.RuleBasedSupersedeVictim, noSuccessor bool) {
		if len(victims) == 0 {
			return
		}
		if noSuccessor {
			fmt.Fprintf(os.Stderr, "%d user-owned learnings were retired as substance-free with no successor at all:\n\n", len(victims))
		} else {
			fmt.Fprintf(os.Stderr, "%d learnings were displaced by the rule-based dedup although the user owns them:\n\n", len(victims))
		}
		for _, v := range victims {
			if v.DisplacerID <= 0 {
				fmt.Fprintf(os.Stderr, "  #%d %s %s no displacer %s | %s\n",
					v.ID, clipText(v.Source, 14), v.CreatedAt.Format("2006-01-02"),
					clipText(v.Reason, 46), clipText(v.Content, 60))
				continue
			}
			fmt.Fprintf(os.Stderr, "  #%d %s %s displacer=#%d (%s) %s\n",
				v.ID, clipText(v.Source, 14), v.CreatedAt.Format("2006-01-02"),
				v.DisplacerID, clipText(v.DisplacerSource, 14), clipText(v.Reason, 46))
		}
		fmt.Fprintln(os.Stderr)
	}

	// Every excluded class is named with its count. The dry run is the only
	// surface the operator decides on, so a report that lists the candidates and
	// drops the rest silently would read as the whole damage.
	reportSkipped := func(sc *storage.RuleBasedSupersedeScan) {
		if sc.Skipped() == 0 {
			return
		}
		fmt.Fprintf(os.Stderr, "%d user-owned learnings with a rule-based reason stay superseded:\n", sc.Skipped())
		if sc.SkippedHighTrustDisplacer > 0 {
			fmt.Fprintf(os.Stderr, "  %d: their displacer is user-owned too, which makes it a legitimate supersede chain\n", sc.SkippedHighTrustDisplacer)
		}
		if sc.SkippedInactiveDisplacer > 0 {
			fmt.Fprintf(os.Stderr, "  %d: their displacer was itself replaced, has expired, or no longer exists\n", sc.SkippedInactiveDisplacer)
		}
		if sc.SkippedSentinel > 0 {
			fmt.Fprintf(os.Stderr, "  %d: retired with a negative sentinel instead of a successor (a cleanup, a resolve, or a deleted successor), so the reason still says why\n", sc.SkippedSentinel)
		}
		if sc.SkippedEmptyContent > 0 {
			fmt.Fprintf(os.Stderr, "  %d: their content is empty\n", sc.SkippedEmptyContent)
		}
		if sc.UnlistedByLimit > 0 {
			fmt.Fprintf(os.Stderr, "  %d: beyond the --limit\n", sc.UnlistedByLimit)
		}
		fmt.Fprintln(os.Stderr, "This command clears none of them; each class needs its own decision.")
	}

	if !*apply && !*applyNoDisplacer {
		if len(scan.Victims) == 0 && len(scan.NoDisplacer) == 0 && scan.Skipped() == 0 {
			fmt.Fprintln(os.Stderr, "No user-owned learning carries a rule-based reason. Nothing to repair.")
			return
		}
		printVictims(scan.Victims, false)
		printVictims(scan.NoDisplacer, true)
		if len(scan.Victims) > 0 || len(scan.NoDisplacer) > 0 {
			fmt.Fprintln(os.Stderr, "Nothing restored. Run again with --apply (displacer class) or --apply-no-displacer (no-successor class) once the list is reviewed.")
		}
		reportSkipped(scan)
		return
	}

	// applyClass writes one class: its own id list, its own re-check in the
	// storage layer, its own listing as the record of what the write clears. The
	// listing is printed before the write and is built from the ids that are
	// really written: an id the operator added by hand is not part of the
	// candidate list and must not appear as if it were.
	applyClass := func(label string, classIDs []int64, candidates []storage.RuleBasedSupersedeVictim,
		noSuccessor bool, restore func([]int64) (int64, error)) int64 {
		if len(classIDs) == 0 {
			for _, v := range candidates {
				classIDs = append(classIDs, v.ID)
			}
		}
		if len(classIDs) == 0 {
			fmt.Fprintf(os.Stderr, "Nothing to restore in the %s class.\n", label)
			return 0
		}

		// A repeated id would otherwise read as an unrestored remainder.
		seenID := make(map[int64]bool, len(classIDs))
		unique := classIDs[:0]
		for _, id := range classIDs {
			if !seenID[id] {
				seenID[id] = true
				unique = append(unique, id)
			}
		}
		classIDs = unique

		known := make(map[int64]storage.RuleBasedSupersedeVictim, len(candidates))
		for _, v := range candidates {
			known[v.ID] = v
		}
		chosen := make([]storage.RuleBasedSupersedeVictim, 0, len(classIDs))
		for _, id := range classIDs {
			if v, ok := known[id]; ok {
				chosen = append(chosen, v)
			}
		}
		printVictims(chosen, noSuccessor)
		if outside := len(classIDs) - len(chosen); outside > 0 {
			fmt.Fprintf(os.Stderr, "%d of the %d given ids are not candidates of the %s class; the update leaves them superseded.\n\n", outside, len(classIDs), label)
		}

		restored, err := restore(classIDs)
		if err != nil {
			log.Fatalf("restore the %s class: %v", label, err)
		}
		fmt.Fprintf(os.Stderr, "Restored %d of %d learnings of the %s class.\n", restored, len(classIDs), label)
		displacerNote := ", an active displacer"
		if noSuccessor {
			displacerNote = ""
		}
		if restored < int64(len(classIDs)) {
			fmt.Fprintf(os.Stderr, "The remainder no longer matches the %s class (a rule-based reason, a user-owned source, non-empty content%s) and stays superseded.\n",
				label, displacerNote)
		}
		return restored
	}

	var restored int64
	if *apply {
		restored += applyClass("displacer", ids, scan.Victims, false, store.RestoreRuleBasedSupersededUserLearnings)
	}
	if *applyNoDisplacer {
		restored += applyClass("no-successor", noDisplacerIDs, scan.NoDisplacer, true, store.RestoreRuleBasedSupersededUserLearningsNoDisplacer)
	}

	// A --limit or an interrupted run leaves candidates behind, and the class the
	// run did not ask for stays as it is either way. Both are reported from a
	// fresh scan: the pre-write one would name rows this run just restored.
	after, err := store.FindRuleBasedSupersededUserLearnings(0)
	if err != nil {
		log.Fatalf("find rule-based superseded after the restore: %v", err)
	}
	if n := len(after.Victims); n == 1 {
		fmt.Fprintln(os.Stderr, "1 displacer candidate is still listed; run the dry run again to see it, or --apply to restore it.")
	} else if n > 1 {
		fmt.Fprintf(os.Stderr, "%d displacer candidates are still listed; run the dry run again to see them, or --apply to restore them.\n", n)
	}
	if n := len(after.NoDisplacer); n == 1 {
		fmt.Fprintln(os.Stderr, "1 no-successor candidate is still listed; run the dry run again to see it, or --apply-no-displacer to restore it.")
	} else if n > 1 {
		fmt.Fprintf(os.Stderr, "%d no-successor candidates are still listed; run the dry run again to see them, or --apply-no-displacer to restore them.\n", n)
	}
	reportSkipped(after)
	if restored > 0 {
		fmt.Fprintln(os.Stderr, "Restored learnings are searchable in the BM25 and vector lanes immediately; the anticipated-query lane sees them again only after a re-embed. With embedding.search.method: ivf the daemon rebuilds its vector index only on restart.")
	}
}

func parseLearningIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("not a learning id: %q", part)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no ids given")
	}
	return ids, nil
}

func runResolveCheck() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: yesmem resolve-check <commit-message>")
		os.Exit(1)
	}
	commitMsg := os.Args[2]

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// The same message the hook and the plugin send: the decision layer knows how
	// to read a subject, and a local prefix rewrite would make the CLI disagree
	// with the other two entry points about the same commit.
	// Same scope as the hook: a git hook runs with cwd = repository, and an empty
	// project would be an unscoped lookup.
	project, _ := os.Getwd()
	decision, err := store.ResolveCommit(commitMsg, project)
	if err != nil {
		log.Printf("resolve-check: %v", err)
		return
	}
	if decision.Target == nil {
		// Nothing is resolved when the subject references nothing or references too
		// much. The reason goes to stderr so a no-op stays explainable.
		if decision.Reason != "" {
			fmt.Fprintf(os.Stderr, "resolve-check: no resolution: %s\n", decision.Reason)
		}
		return
	}
	best := decision.Target

	fmt.Fprintf(os.Stderr, "resolve-check: resolved #%d (%s) via commit: %s\n", best.ID, best.Content, commitMsg)

	// Update MEMORY.md, in the Claude projects directory the configuration names.
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))
	daemon.GenerateAllMemoryMDs(store, cfg.ClaudeProjectsDir())
}

func stripCommitPrefix(msg string) string {
	prefixes := []string{
		"NEW:", "FIX:", "UPD:", "FEAT:", "REFACTOR:", "CHORE:", "DOC:", "TEST:",
		"feat:", "fix:", "chore:", "docs:", "refactor:", "test:", "style:", "perf:",
		"feat(", "fix(", "chore(", "docs(",
	}
	for _, p := range prefixes {
		if len(msg) > len(p) && msg[:len(p)] == p {
			msg = msg[len(p):]
			// Also strip closing paren for scoped commits like "feat(api):"
			if idx := 0; idx < len(msg) && msg[0] == ')' {
				msg = msg[1:]
			}
			if len(msg) > 0 && msg[0] == ':' {
				msg = msg[1:]
			}
			break
		}
	}
	return strings.TrimSpace(msg)
}

func runBackfillFlavor() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	fs := flag.NewFlagSet("backfill-flavor", flag.ExitOnError)
	last := fs.Int("last", 0, "limit to last N sessions")
	fs.IntVar(last, "l", 0, "limit (short)")
	dryRun := fs.Bool("dry-run", false, "list sessions without backfilling")
	fs.BoolVar(dryRun, "n", false, "dry-run (short)")
	force := fs.Bool("force", false, "re-generate all flavors (not just missing)")
	fs.Parse(os.Args[2:])

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	var sessionIDs []string
	if *force {
		sessionIDs, err = store.AllSessionIDs(*last)
	} else {
		sessionIDs, err = store.SessionsWithoutFlavor(*last)
	}
	if err != nil {
		log.Fatalf("query: %v", err)
	}

	if len(sessionIDs) == 0 {
		fmt.Fprintln(os.Stderr, "No sessions to process.")
		return
	}

	mode := "missing"
	if *force {
		mode = "ALL (force)"
	}
	fmt.Fprintf(os.Stderr, "Sessions to process (%s): %d\n", mode, len(sessionIDs))
	if *dryRun {
		for _, id := range sessionIDs {
			fmt.Fprintf(os.Stderr, "  %s\n", truncID(id))
		}
		fmt.Fprintln(os.Stderr, "\n--dry-run: no changes made.")
		return
	}

	apiKey := cfg.ResolvedAPIKey()
	if apiKey == "" {
		apiKey = daemon.ReadClaudeCodeAPIKey()
	}
	if apiKey == "" {
		log.Fatal("No API key — set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure in config.yaml")
	}

	client, err := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.ModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
	if err != nil {
		log.Fatalf("LLM client: %v", err)
	}
	client = redacted(cfg, client)

	const flavorPrompt = `Du bist ein Session-Analyst. Du bekommst eine Transkription einer Programmier-Session.

1. Fasse den CHARAKTER der Session zusammen (max 400 Zeichen).
   Beschreibe die DYNAMIK, STIMMUNG und WENDEPUNKTE, nicht nur den Inhalt.
   Was war der Bogen? Was war der praegende Moment?

2. Bewerte die EMOTIONALE INTENSITAET der Session (0.0 - 1.0).
   0.0 = ruhige Routine, Config-Arbeit
   0.3 = normale Entwicklung, alles laeuft
   0.5 = interessante Herausforderung, mehrere Ansaetze
   0.7 = Frustration oder Durchbruch, deutliche Stimmungswechsel
   0.9 = intensive Session mit starken Wendepunkten
   1.0 = Ausnahme-Session (Architektur-Entscheidung, grundlegender Pivot)

WICHTIG: Schreibe KEINEN Code. Fuehre KEINE Befehle aus. Vervollstaendige NICHTS aus der Transkription.

Beispiele guter Flavors:
- "3h Sandbox-Kampf → User schaltet sie aus → dann laeuft alles. Wendepunkt: direkter DB-Weg statt Workarounds."
- "Sauberer Sprint, alles klappt auf Anhieb. TDD-Zyklus durchgezogen, 12 Tests gruen."
- "Frust: Race Condition, 5 Ansaetze, pragmatischer Workaround. Am Ende hat atomare DB-Op alles geloest."
- "Tiefes Design-Gespraech ueber Erinnerungsqualitaet. User fragt 'was brauchst DU?' — das oeffnet alles."`

	flavorSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"flavor":              map[string]any{"type": "string"},
			"emotional_intensity": map[string]any{"type": "number"},
		},
		"required":             []string{"flavor", "emotional_intensity"},
		"additionalProperties": false,
	}

	done := 0
	for _, sid := range sessionIDs {
		msgs, err := store.GetMessagesBySession(sid)
		if err != nil || len(msgs) < 3 {
			continue
		}

		// Build compact summary: budget-based (5k tokens ≈ 20k chars)
		const charBudget = 20000
		var content strings.Builder
		for _, m := range msgs {
			role := m.Role
			text := m.Content
			remaining := charBudget - content.Len()
			if remaining <= 0 {
				break
			}
			if len(text) > remaining {
				text = text[:remaining] + "..."
			}
			fmt.Fprintf(&content, "[%s] %s\n", role, text)
		}

		response, err := client.CompleteJSON(flavorPrompt, content.String(), flavorSchema)
		if err != nil {
			log.Printf("warn: flavor for %s: %v", truncID(sid), err)
			if strings.Contains(err.Error(), "rate_limit") {
				time.Sleep(60 * time.Second)
			}
			continue
		}

		// Parse JSON response
		var result struct {
			Flavor             string  `json:"flavor"`
			EmotionalIntensity float64 `json:"emotional_intensity"`
		}
		if jsonErr := json.Unmarshal([]byte(response), &result); jsonErr != nil {
			log.Printf("warn: parse flavor for %s: %v", truncID(sid), jsonErr)
			continue
		}

		flavor := strings.TrimSpace(result.Flavor)
		flavor = strings.Trim(flavor, "\"'")
		runes := []rune(flavor)
		if len(runes) > 400 {
			flavor = string(runes[:400])
		}

		intensity := result.EmotionalIntensity
		if intensity < 0 {
			intensity = 0
		}
		if intensity > 1 {
			intensity = 1
		}

		n, err := store.UpdateSessionFlavorAndIntensity(sid, flavor, intensity)
		if err != nil {
			log.Printf("warn: update %s: %v", truncID(sid), err)
			continue
		}

		done++
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s → %q (intensity: %.1f, %d learnings)\n", done, len(sessionIDs), truncID(sid), flavor, intensity, n)

		// Rate limit pause
		time.Sleep(1 * time.Second)
	}

	fmt.Fprintf(os.Stderr, "\nDone: %d/%d sessions backfilled.\n", done, len(sessionIDs))
}

func runEmbedLearnings() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	force := false
	includeAll := false
	batchSize := 512
	throttle := 0 * time.Millisecond // static embeddings are instant — no throttle needed
	args := os.Args[2:]
	for i, arg := range args {
		switch arg {
		case "--force", "-f":
			force = true
		case "--all", "-a":
			includeAll = true
		case "--batch-size", "-b":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &batchSize)
			}
		case "--no-throttle":
			throttle = 0
		case "--throttle":
			if i+1 < len(args) {
				if d, err := time.ParseDuration(args[i+1]); err == nil {
					throttle = d
				}
			}
		}
	}

	// Open DB
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Create embedding provider from config
	provider, err := embedding.NewProviderFromConfig(cfg.Embedding)
	if err != nil {
		log.Fatalf("create embedding provider: %v", err)
	}
	defer provider.Close()

	if !provider.Enabled() {
		fmt.Fprintf(os.Stderr, "Embedding provider is disabled (provider: %q). Set embedding.provider to 'static' in config.yaml.\n", cfg.Embedding.Provider)
		os.Exit(1)
	}

	// No VectorStore needed — vectors are saved to SQLite via SaveEmbeddingVectors().
	// VectorStore reads directly from learnings.embedding_vector (brute-force cosine).

	// Get learnings to embed
	var learnings []models.Learning
	if includeAll {
		learnings, err = store.GetAllLearningsForEmbedding()
	} else {
		learnings, err = store.GetPendingLearningsForEmbedding(0)
	}
	if err != nil {
		log.Fatalf("get learnings: %v", err)
	}

	label := "active"
	if includeAll {
		label = "all (incl. superseded)"
	}
	fmt.Fprintf(os.Stderr, "Found %d %s learnings. Embedding with batch size %d...\n", len(learnings), label, batchSize)

	// Convert to IndexItems — use V2 enriched embedding text when available
	items := make([]embedding.IndexItem, len(learnings))
	for i, l := range learnings {
		embText := l.EmbeddingText
		if embText == "" {
			embText = l.BuildEmbeddingText()
		}
		items[i] = embedding.IndexItem{
			ID:      fmt.Sprintf("%d", l.ID),
			Content: embText,
			Metadata: map[string]string{
				"category": l.Category,
				"project":  l.Project,
				"domain":   l.Domain,
			},
		}
	}

	stats, err := embedding.MigrateEmbeddings(
		context.Background(), provider, nil, items, batchSize, force, throttle,
		func(ids []string, vectors [][]float32) {
			results := make([]storage.EmbeddingResult, 0, len(ids))
			for i, id := range ids {
				var n int64
				if _, err := fmt.Sscanf(id, "%d", &n); err == nil {
					results = append(results, storage.EmbeddingResult{ID: n, Vector: vectors[i]})
				}
			}
			if len(results) > 0 {
				if err := store.SaveEmbeddingVectors(results); err != nil {
					log.Printf("warn: save vectors to SQLite: %v", err)
				}
			}
		},
	)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Done. Total: %d, Embedded: %d, Skipped: %d, Errors: %d\n",
		stats.Total, stats.Embedded, stats.Skipped, stats.Errors)

	// Notify running daemon to reload vector store
	if stats.Embedded > 0 {
		conn, err := net.DialTimeout("unix", filepath.Join(dataDir, "daemon.sock"), 500*time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Note: daemon not reachable for reload (%v)\n", err)
			return
		}
		defer conn.Close()
		msg := `{"jsonrpc":"2.0","method":"reload_vectors","id":1}` + "\n"
		conn.Write([]byte(msg))
		buf := make([]byte, 1024)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _ := conn.Read(buf)
		fmt.Fprintf(os.Stderr, "Daemon reload: %s\n", string(buf[:n]))
	}
}

func truncID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// runPendingConfirmation lists the learnings parked in the retired
// "pending_confirmation" status so the historical conflicts can be sifted by
// hand (Decision D6). Nothing is rewritten automatically.
func runPendingConfirmation() {
	dataDir := yesmemDataDir()

	fs := flag.NewFlagSet("pending-confirmation", flag.ExitOnError)
	project := fs.String("project", "", "filter by project")
	fs.StringVar(project, "p", "", "filter by project (short)")
	limit := fs.Int("limit", storage.PendingConfirmationListLimit, "max rows")
	fs.IntVar(limit, "l", storage.PendingConfirmationListLimit, "max rows (short)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(os.Args[2:])

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	learnings, total, err := store.GetPendingConfirmationLearnings(*project, *limit)
	if err != nil {
		log.Fatalf("query pending confirmation: %v", err)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"total":     total,
			"shown":     len(learnings),
			"learnings": learnings,
		}); err != nil {
			log.Fatalf("encode: %v", err)
		}
		return
	}

	if total == 0 {
		fmt.Fprintln(os.Stderr, "No active learnings in the retired pending_confirmation status.")
		return
	}
	fmt.Fprintf(os.Stderr, "%d of %d active learning(s) carry the retired pending_confirmation status.\n\n", len(learnings), total)
	for _, l := range learnings {
		proj := l.Project
		if proj == "" {
			proj = "(global)"
		}
		fmt.Printf("#%-6d %-28s %-14s %-14s %s\n", l.ID, clipText(proj, 28), clipText(l.Category, 14), clipText(l.Source, 14), clipText(l.Content, 120))
	}
	if len(learnings) < total {
		fmt.Fprintf(os.Stderr, "\n... %d more not shown — raise --limit to see them.\n", total-len(learnings))
	}
	fmt.Fprintln(os.Stderr, "\nThe status is no longer written (Decision D6). These rows carry no recorded counterpart, so decide per row: supersede the outdated one, or link the pair explicitly (relate_learnings).")
}

// clipText trims stored content for terminal display and replaces control
// characters, so escape sequences from ingested text cannot reach the terminal.
func clipText(s string, max int) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(s) {
		if count >= max {
			b.WriteRune('…')
			break
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			r = ' '
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}
