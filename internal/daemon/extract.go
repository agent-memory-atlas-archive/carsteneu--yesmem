package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// cleanupFinishedShortSessionLearnings supersedes learnings of sessions that are
// too short to extract and have gone quiet. Narrative rows of those sessions go
// with them, so the narrative marker is cleared here as well: the daemon is the
// only place that cleanup runs, and a marker left behind keeps the session
// unnarrated once it grows. It reports the outcome and logs a cleanup error:
// swallowing it would hide a broken cleanup for the whole run.
func cleanupFinishedShortSessionLearnings(store *storage.Store) int {
	cleanedIDs, err := store.SupersedeShortSessionLearnings()
	if err != nil {
		log.Printf("warn: clean up finished short sessions: %v", err)
		return 0
	}
	if _, err := store.ClearSupersededNarrativeMarkers(); err != nil {
		log.Printf("warn: clear narrative markers of finished short sessions: %v", err)
	}
	if len(cleanedIDs) > 0 {
		log.Printf("  Cleaned %d learnings from finished short sessions (<%d messages, silent >%s)",
			len(cleanedIDs), storage.ShortSessionMaxMessages, storage.ShortSessionQuietPeriod)
	}
	return len(cleanedIDs)
}

// markSessionsExtracted marks daemon-internal extraction sessions (cluster
// labeling etc.) and then the remaining short sessions as extracted, so neither
// shows up as pending. The targeted pass must run FIRST: once every short session
// is marked, it finds nothing. Both marks log their error: a silently failed mark
// re-surfaces the same sessions as pending on every extraction run.
func markSessionsExtracted(store *storage.Store) {
	if marked, err := store.MarkExtractionSessionsExtracted(); err != nil {
		log.Printf("warn: mark extraction-internal sessions as extracted: %v", err)
	} else if marked > 0 {
		log.Printf("  Marked %d extraction-internal sessions as extracted", marked)
	}
	if marked, err := store.MarkShortSessionsExtracted(storage.ShortSessionMaxMessages - 1); err != nil {
		log.Printf("warn: mark short sessions as extracted: %v", err)
	} else if marked > 0 {
		log.Printf("  Marked %d short sessions (≤%d messages) as extracted",
			marked, storage.ShortSessionMaxMessages-1)
	}
}

// runInitialExtraction processes sessions in 5 phases to avoid API quota competition.
// Phase 1 (Indexing) already happened before this function is called.
func runInitialExtraction(ext extraction.SessionExtractor, evoExt *extraction.Extractor, store *storage.Store, cfg *config.Config, client extraction.LLMClient, qualityClient extraction.LLMClient, handler *Handler) {
	// Wait for VectorStore to finish loading (async init). Needed for Phase 3.5 + 4.5.
	for i := 0; i < 12; i++ {
		if handler.VectorStore() != nil {
			break
		}
		if i == 0 {
			log.Println("  Waiting for VectorStore to load...")
		}
		time.Sleep(5 * time.Second)
	}

	sessions, err := store.ListAllSessions("", 0)
	if err != nil {
		log.Printf("warn: list sessions for extraction: %v", err)
		return
	}

	// Cleanup: supersede learnings from trivially short sessions that have gone
	// quiet. User-stated learnings and sessions still in flight stay untouched.
	cleanupFinishedShortSessionLearnings(store)

	// Mark daemon-internal extraction sessions (cluster labeling etc.) as extracted
	// so they don't clutter the pending count or trigger self-referential re-extraction.
	markSessionsExtracted(store)

	// Check which sessions already have been extracted (persistent marker)
	var toExtract []models.Session
	for _, s := range sessions {
		if s.ExtractedAt.IsZero() && s.MessageCount >= storage.ShortSessionMaxMessages {
			toExtract = append(toExtract, s)
		}
	}

	// Apply max_age_days filter
	toExtract = FilterByMaxAge(toExtract, cfg.Extraction.MaxAgeDays)

	// Sort newest first — onboarding extracts recent sessions first so the user
	// gets a useful briefing within minutes. Older sessions backfill on subsequent runs.
	sort.Slice(toExtract, func(i, j int) bool {
		return toExtract[i].StartedAt.After(toExtract[j].StartedAt)
	})

	// Apply max_per_run cap (takes newest first thanks to sort above)
	if cfg.Extraction.MaxPerRun > 0 && len(toExtract) > cfg.Extraction.MaxPerRun {
		remaining := len(toExtract) - cfg.Extraction.MaxPerRun
		log.Printf("  Capping extraction: %d sessions pending, processing %d newest first (%d deferred)", len(toExtract), cfg.Extraction.MaxPerRun, remaining)
		toExtract = toExtract[:cfg.Extraction.MaxPerRun]
	}

	// Log cost estimate before starting
	est := EstimateExtractionCost(store, toExtract, cfg.Extraction.Model)
	LogExtractionEstimate(est, client)

	pendingNarratives, narrativeErr := pendingNarrativeSessions(store)
	if narrativeErr != nil {
		log.Printf("warn: pending narratives: %v", narrativeErr)
	}
	pendingProfiles, profileErr := countPendingProfiles(store)
	if profileErr != nil {
		log.Printf("warn: count pending profiles: %v", profileErr)
	}
	existingTraits, _ := store.GetActivePersonaTraits("default", 0.0)
	personaNeedsWork := len(existingTraits) == 0 || len(toExtract) > 0

	if len(toExtract) == 0 && len(pendingNarratives) == 0 && pendingProfiles == 0 && !personaNeedsWork {
		// A failed pending query yields empty sets too, so the early exit must name
		// the failure: "nothing pending" and "could not ask" are different states.
		log.Printf("━━━ Initial background scan skipped: no extraction/profile/narrative/persona work pending (narratives: %s, profiles: %s) ━━━",
			pendingSkipReason(narrativeErr), pendingSkipReason(profileErr))
		return
	}
	evolutionAfterID, _ := store.GetMaxLearningID()

	// ━━━ Phase 2: Extraction (LLM — learnings only, NO evolution) ━━━
	if len(toExtract) > 0 {
		if !extraction.HasBudget(client) {
			log.Println("━━━ Phase 2: Extraction skipped (budget exhausted) ━━━")
		} else {
			log.Printf("━━━ Phase 2: Extraction (%d sessions) ━━━", len(toExtract))
			RunExtractionPhase(ext, store, toExtract, cfg, client)
		}
	} else {
		log.Println("━━━ Phase 2: No sessions need extraction ━━━")
	}

	// ━━━ Phase 2.5: Embed learnings (synchronous, before Evolution) ━━━
	// SSE embeddings are fast (pure lookup+pool, no neural inference), so we run
	// synchronously to ensure vectors are available for embedding-based dedup in Phase 3.
	// Without this, the first Evolution run after initial extraction has no vectors.
	if handler.VectorStore() != nil && len(toExtract) > 0 {
		pending, _ := store.GetPendingLearningsForEmbedding(0)
		if len(pending) > 0 {
			log.Printf("━━━ Phase 2.5: Embed %d learnings (sync, SSE) ━━━", len(pending))
			bin, _ := os.Executable()
			cmd := exec.Command(bin, "embed-learnings")
			homeDir, _ := os.UserHomeDir()
			logPath := filepath.Join(homeDir, ".claude", "yesmem", "logs", "embed.log")
			os.MkdirAll(filepath.Dir(logPath), 0755)
			if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
			if err := cmd.Run(); err != nil {
				log.Printf("  Embed sync: failed: %v", err)
			} else {
				log.Printf("  Embed sync: done (%d items)", len(pending))
			}
		}
	}

	// ━━━ Phase 3: Evolution (1 bulk LLM call per category) ━━━
	if cfg.Evolution.AutoResolve && len(toExtract) > 0 {
		if !extraction.HasBudget(client) {
			log.Println("━━━ Phase 3: Evolution skipped (budget exhausted) ━━━")
		} else {
			evolutionScope, err := buildEvolutionScope(store, toExtract)
			if err != nil {
				log.Printf("━━━ Phase 3: Evolution skipped (scope build failed: %v) ━━━", err)
			} else if len(evolutionScope) == 0 {
				log.Println("━━━ Phase 3: Evolution skipped (no new learning categories) ━━━")
			} else {
				log.Printf("━━━ Phase 3: Knowledge Evolution (scoped to %d project/category groups) ━━━", countEvolutionScope(evolutionScope))
				checked, superseded := evoExt.RunEvolutionForScopeSince(store, evolutionScope, nil, evolutionAfterID)
				log.Printf("  Evolution complete: %d checked, %d superseded", checked, superseded)
			}
		}
	} else if cfg.Evolution.AutoResolve {
		log.Println("━━━ Phase 3: Evolution skipped (no new extraction delta) ━━━")
	}

	// ━━━ Phase 3.5: Auto-embed remaining learnings via external process ━━━
	// Only needed when Phase 2.5 didn't run (no new extraction) but pending embeddings
	// exist from prior runs. Runs async (fire-and-forget) since Evolution already passed.
	if handler.VectorStore() != nil && len(toExtract) == 0 {
		pending, _ := store.GetPendingLearningsForEmbedding(0)
		if len(pending) > 0 {
			bin, _ := os.Executable()
			cmd := exec.Command("nice", "-n", "19", bin, "embed-learnings")
			homeDir, _ := os.UserHomeDir()
			logPath := filepath.Join(homeDir, ".claude", "yesmem", "logs", "embed.log")
			os.MkdirAll(filepath.Dir(logPath), 0755)
			if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
			if err := cmd.Start(); err != nil {
				log.Printf("  Auto-embed: failed to spawn embed-learnings: %v", err)
			} else {
				log.Printf("  Auto-embed: spawned external process (PID %d) for %d pending items", cmd.Process.Pid, len(pending))
				handler.SetEmbedProcess(cmd.Process)
				go cmd.Wait() // reap zombie
			}
		} else {
			log.Printf("  Auto-embed: no pending items")
		}
	}

	// ━━━ Phase 3.75: Code Descriptions (LLM — 1 project per cycle, most active first) ━━━
	log.Printf("━━━ Phase 3.75: Code Descriptions ━━━")
	GenerateCodeDescriptions(store, cfg, qualityClient)

	// ━━━ Phase 4: Narratives (LLM — session handovers, uses quality model) ━━━
	if len(pendingNarratives) > 0 {
		if !extraction.HasBudget(qualityClient) {
			log.Printf("━━━ Phase 4: Session Narratives skipped (quality budget exhausted, %d pending) ━━━", len(pendingNarratives))
		} else {
			log.Printf("━━━ Phase 4: Session Narratives (%d pending) ━━━", len(pendingNarratives))
			GenerateMissingNarratives(store, sessions, cfg, qualityClient, pendingNarratives)
		}
	} else {
		log.Printf("━━━ Phase 4: Session Narratives skipped (%s) ━━━", pendingSkipReason(narrativeErr))
	}

	// ━━━ Phase 4.5: Learning Clustering (Metamemory) ━━━
	log.Printf("━━━ Phase 4.5: Learning Clustering ━━━")
	ClusterLearnings(store, qualityClient, handler.VectorStore())

	// ━━━ Phase 4.6: Recurrence Detection ━━━
	alertCount := DetectRecurrence(store, client)
	if alertCount > 0 {
		log.Printf("⚠ Phase 4.6: %d recurrence alerts generated", alertCount)
	}

	// ━━━ Phase 5: Profiles ━━━
	if pendingProfiles > 0 {
		if !extraction.HasBudget(qualityClient) {
			log.Printf("━━━ Phase 5: Profiles skipped (quality budget exhausted, %d pending) ━━━", pendingProfiles)
		} else {
			log.Printf("━━━ Phase 5: Profiles (%d pending) ━━━", pendingProfiles)
			GenerateProfiles(store, cfg, qualityClient)
		}
	} else {
		log.Printf("━━━ Phase 5: Profiles skipped (%s) ━━━", pendingSkipReason(profileErr))
	}

	// ━━━ Phase 6: Persona Signals ━━━
	log.Printf("━━━ Phase 6: Persona Signals ━━━")

	// Bootstrap: if no persona traits exist yet, seed from existing learnings
	// and run signal extraction over ALL sessions (not just last 10)
	if len(existingTraits) == 0 {
		log.Println("  Initial persona bootstrap...")
		bootstrapPersonaFromLearnings(store)
		if extraction.HasBudget(client) {
			extractPersonaSignalsWithLimit(store, sessions, cfg, client, 0)
		} else {
			log.Println("  Persona extraction skipped (budget exhausted)")
		}
	} else if len(toExtract) > 0 {
		if extraction.HasBudget(client) {
			extractPersonaSignals(store, sessions, cfg, client)
		} else {
			log.Println("  Persona extraction skipped (budget exhausted)")
		}
	} else {
		log.Println("  Persona skipped (no new extraction delta)")
		return
	}

	// Persona directive synthesis uses quality model (Opus) for better relationship anchors
	// But first: deduplicate traits that are semantically identical
	if handler.EmbedProvider() != nil {
		dedupPersonaTraits(store, handler.EmbedProvider())
	}

	DecayContextTraits(store)

	synthesizePersonaDirective(store, qualityClient)
	synthesizeUserProfile(store, qualityClient)

	log.Printf("━━━ All phases complete ━━━")
}

// pendingNarrativeSessions names the Phase-4 workload: sessions that still owe a
// narrative and that the generator can summarize. The phase gate and the
// generator read the same set, so the count can never promise work the generator
// then declines (a session below the text-message threshold stays pending forever
// and is reloaded every cycle).
func pendingNarrativeSessions(store *storage.Store) (map[string]struct{}, error) {
	return store.PendingNarrativeSessionIDs(minNarrativeMessages)
}

func countPendingProfiles(store *storage.Store) (int, error) {
	var count int
	// Top-level sessions only: the stored session count comes from
	// CountProjectSessions, which filters the same way. Counting subagent rows
	// here made the profile look stale forever (YM-49).
	err := store.DB().QueryRow(`SELECT COUNT(*)
		FROM (
			SELECT project_short, COUNT(*) AS session_count
			FROM sessions
			WHERE parent_session_id IS NULL
			GROUP BY project_short
			HAVING COUNT(*) >= 3
		) p
		LEFT JOIN project_profiles pp ON pp.project = p.project_short
		WHERE pp.project IS NULL
		OR (pp.model_used != 'manual' AND pp.session_count < p.session_count)`).Scan(&count)
	return count, err
}

// pendingSkipReason renders the reason a phase was skipped. "up to date" is a
// statement about the data, so it must not stand in for a query that never
// answered (YM-367): a broken gate query used to read exactly like a finished
// cycle, on every start.
func pendingSkipReason(err error) string {
	if err != nil {
		return "pending query failed: " + err.Error()
	}
	return "up to date"
}

func hasDocSyncWork(store *storage.Store) bool {
	sources, err := store.ListDocSources("")
	if err != nil || len(sources) == 0 {
		return false
	}
	for _, src := range sources {
		// Mirrors runDocSync: rules and skill sources are not chunk sources, so
		// their file mtime never means "there is work to do".
		if src.IsRules || src.IsSkill {
			continue
		}
		if src.Path == "" {
			// Nothing to walk, so this source is normally not work. It can still
			// hold documents: a registration that lost its path keeps its chunks
			// until a sync sweeps them, and runDocSync is the only place that
			// does. Without this line a path-less source that is the only one is
			// never swept, because the tick never calls runDocSync.
			files, err := store.GetDocChunkFiles(src.ID)
			if err != nil {
				// A failed read is not the observation "this source is clean";
				// take the conservative direction like the stat branch below,
				// which treats any error other than "not found" as work.
				return true
			}
			if len(files) > 0 {
				return true
			}
			continue
		}
		// The daemon's working directory is not the caller's: a relative path
		// would stat or walk whatever tree sits at that name next to the
		// daemon, while runDocSync skips the source as unresolvable. That
		// combination schedules a full sync on every tick forever.
		if !filepath.IsAbs(src.Path) {
			// Without this line the operator loses the only hint that the
			// source is unusable: runDocSync is the place that logs it, and the
			// gate never lets the source reach runDocSync.
			log.Printf("[doc-sync] %q: skipped relative path %q (re-register it with an absolute path)", src.Name, src.Path)
			continue
		}
		info, err := os.Stat(src.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return true
		}
		mod := info.ModTime()
		// One clock reading per source: a document touched while this loop is
		// still iterating must not look like a future mtime to a later source.
		now := time.Now()
		if info.IsDir() {
			// A directory's mtime only moves when an entry is added or removed
			// directly inside it, so an edit in a subdirectory is invisible to
			// a stat. Ask the walk for the newest document instead. A removal
			// is NOT covered while any document remains: the survivor is older
			// than last_sync and the directory's own moved mtime is never
			// consulted. The fallback below only keeps a tree that lost its
			// last document from going quiet without a trace.
			if newest, ok := ingest.NewestDocMtime(src.Path, now); ok {
				mod = newest
			}
		}
		if mod.After(now) {
			// A mtime ahead of the wall clock cannot be ordered against
			// last_sync, which is CURRENT_TIMESTAMP: it stays "after the last
			// sync" on every tick and would run the full sync forever. It
			// becomes a change signal once the clock passes it.
			log.Printf("[doc-sync] %q: skipped a modification time ahead of the clock (%s)", src.Name, mod.Format(time.RFC3339))
			continue
		}
		if mod.After(src.LastSync) {
			return true
		}
	}
	return false
}

func isLikelyGitRepo(path string) bool {
	for dir := path; dir != "" && dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info != nil {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}
	return false
}

func buildEvolutionScope(store *storage.Store, sessions []models.Session) (map[string]map[string]struct{}, error) {
	if len(sessions) == 0 {
		return nil, nil
	}

	sessionIDs := make([]string, 0, len(sessions))
	for _, s := range sessions {
		sessionIDs = append(sessionIDs, s.ID)
	}

	learnings, err := store.GetActiveLearningsBySessionIDs(sessionIDs)
	if err != nil {
		return nil, err
	}

	scope := make(map[string]map[string]struct{})
	for _, learning := range learnings {
		if learning.Category == "narrative" || learning.Category == "unfinished" {
			continue
		}
		if _, ok := scope[learning.Project]; !ok {
			scope[learning.Project] = make(map[string]struct{})
		}
		scope[learning.Project][learning.Category] = struct{}{}
	}
	return scope, nil
}

func countEvolutionScope(scope map[string]map[string]struct{}) int {
	total := 0
	for _, categories := range scope {
		total += len(categories)
	}
	return total
}

// The retry budget of a session is storage.ExtractionAttemptLimit: the counter
// lives on the session row, the batch path reads it as a derived skip. The limit
// is never written into skip_extraction, because that column carries the marker
// the skip_indexing tool sets on the user's behalf — one column with two meanings
// would make a re-extraction either override the user's instruction or leave a
// session excluded with no way back. ResetSessionExtracted and a session that
// grows both clear the counter, so the budget is consecutive failures, not a
// lifetime sentence.

// recordExtractionFailure counts a failed attempt for the session and returns the
// new count. A failed write is logged and ignored: the extraction already failed,
// and losing the count only costs one more retry.
func recordExtractionFailure(store *storage.Store, sessionID string) int {
	attempts, err := store.RecordExtractionAttempt(sessionID)
	if err != nil {
		log.Printf("warn: count failed extraction for %s: %v", truncID(sessionID), err)
		return 0
	}
	return attempts
}

// runExtractionPhase processes sessions with parallel workers (no evolution).
func RunExtractionPhase(ext extraction.SessionExtractor, store *storage.Store, sessions []models.Session, cfg *config.Config, client extraction.LLMClient) {
	workers := 3
	if client != nil && client.Name() == "cli" {
		workers = 1 // CLI: sequential, Pro/Max rate limits
	}
	startTime := time.Now()
	total := len(sessions)

	var (
		mu       sync.Mutex
		done     int
		errCount int
	)

	work := make(chan models.Session, total)
	for _, s := range sessions {
		work <- s
	}
	close(work)

	var wg sync.WaitGroup
	var apiDown int32 // atomic: set to 1 when API is consistently failing
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			consecutiveErrors := 0
			for s := range work {
				if atomic.LoadInt32(&apiDown) == 1 {
					mu.Lock()
					done++
					mu.Unlock()
					continue // drain queue without API calls
				}
				// Skip sessions marked for no extraction, and sessions that
				// exhausted their extraction attempts.
				if store.IsExtractionSkipped(s.ID) || store.ExtractionAttemptsExhausted(s.ID) {
					mu.Lock()
					done++
					mu.Unlock()
					continue
				}
				if err := extractSession(ext, store, s.ID, false); err != nil {
					mu.Lock()
					errCount++
					done++
					d := done
					mu.Unlock()
					log.Printf("  [%d/%d] ⚠ %s: %v", d, total, truncID(s.ID), err)
					if errors.Is(err, extraction.ErrPartial) {
						log.Printf("  %s: partial extraction, the session stays pending", truncID(s.ID))
					}
					if attempts := recordExtractionFailure(store, s.ID); attempts == storage.ExtractionAttemptLimit {
						log.Printf("  %s: gave up after %d failed extraction attempts, it is not retried until the session is extracted again", truncID(s.ID), attempts)
					}

					// An answer that arrives proves the API is up, even when it
					// does not parse. Only a pass in which every chunk's call
					// failed is evidence of an outage, so only that trips the
					// breaker (YM-317, YM-362).
					if extraction.IsOutage(err) {
						consecutiveErrors++
					} else {
						consecutiveErrors = 0
					}

					if consecutiveErrors >= 5 {
						log.Printf("  Extraction worker: %d consecutive errors — marking API as down", consecutiveErrors)
						atomic.StoreInt32(&apiDown, 1)
						continue
					}
					if strings.Contains(err.Error(), "rate_limit") {
						time.Sleep(30 * time.Second)
					} else {
						time.Sleep(2 * time.Second)
					}
				} else {
					consecutiveErrors = 0
					store.MarkSessionExtracted(s.ID)
					// Certify the message range this pass saw, so a session that a
					// fork had already marked cannot be re-extracted every cycle.
					// Full passes use fork_index 0; fork rows start at 1.
					if s.MessageCount > 0 {
						if err := store.InsertForkCoverage(s.ID, 0, s.MessageCount-1, 0); err != nil {
							log.Printf("warn: coverage for %s: %v", truncID(s.ID), err)
						}
					}
					mu.Lock()
					done++
					d := done
					mu.Unlock()
					log.Printf("  [%d/%d] ✓ %s (%s)", d, total, truncID(s.ID), s.ProjectShort)
					// Yield between sessions — lets BM25 readers through
					runtime.Gosched()
				}
			}
		}()
	}

	wg.Wait()
	log.Printf("  Extraction: %d processed, %d errors, %v (%d workers)", done, errCount, time.Since(startTime).Round(time.Second), workers)
}

// extractSession runs extraction for a single session by ID.
// autoResolve controls whether evolution checks run inline (true for file-watcher, false for bulk).
func extractSession(ext extraction.SessionExtractor, store *storage.Store, sessionID string, autoResolve bool) error {
	msgs, err := store.GetMessagesBySession(sessionID)
	if err != nil || len(msgs) == 0 {
		return err
	}

	// Look up project for this session
	session, _ := store.GetSession(sessionID)
	project := ""
	if session != nil {
		project = session.ProjectShort
	}

	return ext.ExtractAndStore(sessionID, project, msgs, autoResolve)
}

// generateProfiles creates/updates project profiles for projects with enough sessions.
func GenerateProfiles(store *storage.Store, cfg *config.Config, client extraction.LLMClient) {
	projects, err := store.ListProjects()
	if err != nil {
		return
	}

	if client == nil {
		return
	}

	profilesDone := 0
	for _, p := range projects {
		if p.SessionCount < 3 { // Need at least 3 sessions
			continue
		}

		// Check if profile already exists and is recent
		existing, err := store.GetProjectProfile(p.ProjectShort)
		if err == nil {
			// Never overwrite manually set profiles
			if existing.ModelUsed == "manual" {
				log.Printf("  profile %s: skipped (manual)", p.ProjectShort)
				continue
			}
			if existing.SessionCount >= p.SessionCount {
				log.Printf("  profile %s: skipped (up to date, %d sessions)", p.ProjectShort, existing.SessionCount)
				continue
			}
		}

		fmt.Fprintf(os.Stderr, "\r  ⟳ Generating profile: %-30s (%d sessions)   ", p.ProjectShort, p.SessionCount)
		if err := extraction.GenerateProjectProfile(client, store, p.ProjectShort); err != nil {
			log.Printf("\n  ⚠ profile %s: %v", p.ProjectShort, err)
		} else {
			profilesDone++
		}

		time.Sleep(1 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "\r%s\r", "                                                                                  ")
	log.Printf("  Profiles generated: %d", profilesDone)
}

// narrativeBudgetBackoff tracks when to retry narrative generation after budget exhaustion.
var narrativeBudgetBackoff time.Time

// generateMissingNarratives creates narrative summaries for the sessions the
// pending set names. The set already carries the gates (text messages,
// quarantine), so the loop can rely on it and never loads a session the
// generator would have to reject.
func GenerateMissingNarratives(store *storage.Store, sessions []models.Session, cfg *config.Config, client extraction.LLMClient, pending map[string]struct{}) {
	if client == nil {
		log.Println("  Skipped: no LLM client")
		return
	}

	if time.Now().Before(narrativeBudgetBackoff) {
		log.Printf("  Narratives deferred: budget backoff until %s", narrativeBudgetBackoff.Format("15:04"))
		return
	}

	// Filter to sessions needing narratives (persistent marker, min messages)
	var toProcess []models.Session
	for _, s := range sessions {
		if _, ok := pending[s.ID]; ok {
			toProcess = append(toProcess, s)
		}
	}

	if len(toProcess) == 0 {
		log.Println("  All narratable sessions have narratives")
		return
	}

	workers := 3
	if client.Name() == "cli" {
		workers = 1 // CLI: sequential, Pro/Max rate limits
	}
	total := len(toProcess)
	log.Printf("  %d sessions need narratives (%d workers)", total, workers)

	var (
		mu        sync.Mutex
		generated int
		written   = make(map[string][]int64)
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
				msgs, err := store.GetMessagesBySession(s.ID)
				if err != nil || len(msgs) < minNarrativeMessages {
					continue
				}

				summaries := SummarizeMessages(msgs)
				if len(summaries) < minNarrativeMessages {
					continue
				}

				project := s.ProjectShort
				userMsg := BuildNarrativeUserMessageWithTime(summaries, project, sessionTimeRange(s))

				response, err := client.Complete(NarrativePrompt(), userMsg)
				if err != nil {
					if strings.Contains(err.Error(), "budget exceeded") {
						mu.Lock()
						narrativeBudgetBackoff = time.Now().Add(1 * time.Hour)
						mu.Unlock()
						log.Printf("  Narratives: budget exceeded, backing off until %s", narrativeBudgetBackoff.Format("15:04"))
						return
					}
					if strings.Contains(err.Error(), "rate_limit") {
						time.Sleep(30 * time.Second)
					}
					continue
				}

				narrative := CleanNarrativeResponse(response)
				if narrative == "" {
					continue
				}

				narrativeID, err := store.InsertLearning(&models.Learning{
					Category:   "narrative",
					Content:    narrative,
					Project:    project,
					SessionID:  s.ID,
					CreatedAt:  s.StartedAt,
					ModelUsed:  client.Model(),
					Confidence: 1.0,
					Source:     "llm_extracted",
				})
				if err != nil {
					log.Printf("  narrative %s: insert failed: %v", truncID(s.ID), err)
					continue
				}
				if project != "" {
					mu.Lock()
					written[project] = append(written[project], narrativeID)
					mu.Unlock()
				}
				store.MarkSessionNarrative(s.ID)

				// Session flavor from the narrative — every extracted session
				// (CC and opencode) carries one, independent of the fork
				// pipeline. Marks count = real learnings of this session
				// (narrative/pulse excluded; 0 keeps the flavor gated).
				if flavor := deriveSessionFlavor(narrative); flavor != "" {
					marks, err := store.CountLearningsForSession(s.ID)
					if err != nil {
						log.Printf("  Flavor: count learnings for %s failed: %v", s.ID, err)
					} else if _, err := store.UpdateSessionFlavorOnlyEmpty(s.ID, flavor, marks); err != nil {
						log.Printf("  Flavor: stamp for %s failed: %v", s.ID, err)
					}
				}

				mu.Lock()
				generated++
				g := generated
				mu.Unlock()
				if g%10 == 0 {
					log.Printf("  Narratives: %d/%d generated", g, total)
				}
			}
		}()
	}

	wg.Wait()
	pruneNarratives(store, written)
	log.Printf("  Narratives complete: %d generated", generated)
}

// pruneNarratives applies the per-project retention once per run and spares every
// row this run wrote. Pruning per insert instead would let a concurrent worker's
// prune drop a story another worker just paid for: a narrative carries its
// session's start time, so one written for an older session sorts to the bottom
// of its project, and the session keeps the marker that calls it narrated.
//
// The rows are grouped by repository root because that is the scope the prune
// uses. One call per worktree path would spare only that worktree's rows and
// delete the stories the other workers wrote seconds ago.
func pruneNarratives(store *storage.Store, written map[string][]int64) {
	byRoot := make(map[string][]int64, len(written))
	for project, ids := range written {
		if project == "" {
			continue
		}
		scope := models.RepositoryRoot(project)
		if scope == "" {
			scope = project
		}
		byRoot[scope] = append(byRoot[scope], ids...)
	}
	for scope, ids := range byRoot {
		n, err := store.DeleteOldNarratives(scope, narrativeKeep, ids...)
		if err != nil {
			log.Printf("  narrative cleanup for %s: %v", scope, err)
			continue
		}
		if n > 0 {
			log.Printf("  Narrative cleanup: deleted %d old narratives for %s", n, scope)
		}
	}
}

// forkCoverageSlack is how many trailing messages a session must accumulate
// beyond its last certified range before the batch pass reopens it. Without it a
// session that is still growing would be fully re-extracted on every cycle.
const forkCoverageSlack = 20

// batchExtractionNeeded reports whether the batch pass owes this session an
// extraction pass.
func batchExtractionNeeded(store *storage.Store, s models.Session) bool {
	if s.MessageCount < storage.ShortSessionMaxMessages {
		return false
	}
	if s.ExtractedAt.IsZero() {
		return true
	}
	// A fork covers only the messages that existed when it ran, and the fork
	// handler marks the whole session as extracted. The tail that arrived after
	// the last fork was never extracted (YM-51).
	//
	// A session that is still growing would otherwise be re-extracted in full on
	// every cycle, so only a tail of at least forkCoverageSlack messages counts.
	// A session that has been silent long enough to be finished gets its tail in
	// full, however short it is.
	slack := forkCoverageSlack
	// Only a session with an end timestamp and enough silence counts as finished.
	// Without one there is no evidence the session stopped growing.
	if storage.SessionFinished(s.EndedAt, time.Now()) {
		slack = 1
	}
	return store.ForkCoverageIncomplete(s.ID, s.MessageCount, slack)
}

// extractionNeeded checks if the session has already been extracted.
func extractionNeeded(store *storage.Store, sessionID string) bool {
	sess, err := store.GetSession(sessionID)
	if err != nil {
		return true // can't check → assume needed
	}
	return sess.ExtractedAt.IsZero()
}

// runBatchExtraction runs Phase 2 (extraction) + 2.5 (embed) + 3 (evolution) for all pending sessions.
// Called by the batch extraction cycle goroutine.
func runBatchExtraction(ext extraction.SessionExtractor, evoExt *extraction.Extractor, store *storage.Store, cfg *config.Config, client extraction.LLMClient, qualityClient extraction.LLMClient, handler *Handler) {
	sessions, err := store.ListAllSessions("", 0)
	if err != nil {
		log.Printf("warn: batch extraction list sessions: %v", err)
		return
	}

	// Cleanup: mark daemon-internal extraction sessions first (targeted),
	// then mark remaining short sessions (generic catch-all).
	markSessionsExtracted(store)

	// Build pending list — skip sessions younger than min_session_age_hours (default 24h)
	// because the forked agent proxy handles real-time extraction for active sessions.
	minAgeH := cfg.Extraction.MinSessionAgeH
	if minAgeH <= 0 {
		minAgeH = 24
	}
	cutoff := time.Now().Add(-time.Duration(minAgeH) * time.Hour)
	var toExtract []models.Session
	for _, s := range sessions {
		if batchExtractionNeeded(store, s) && s.StartedAt.Before(cutoff) {
			toExtract = append(toExtract, s)
		}
	}
	toExtract = FilterByMaxAge(toExtract, cfg.Extraction.MaxAgeDays)

	// Sort newest first
	sort.Slice(toExtract, func(i, j int) bool {
		return toExtract[i].StartedAt.After(toExtract[j].StartedAt)
	})

	if cfg.Extraction.MaxPerRun > 0 && len(toExtract) > cfg.Extraction.MaxPerRun {
		toExtract = toExtract[:cfg.Extraction.MaxPerRun]
	}

	if len(toExtract) == 0 {
		return
	}
	evolutionAfterID, _ := store.GetMaxLearningID()

	// Phase 2: Extraction
	if !extraction.HasBudget(client) {
		log.Println("  Batch extraction skipped (budget exhausted)")
		return
	}
	log.Printf("  Phase 2: Extracting %d sessions", len(toExtract))
	RunExtractionPhase(ext, store, toExtract, cfg, client)

	// Phase 2.5: Embed (sync, needed for evolution)
	if handler.VectorStore() != nil {
		pending, _ := store.GetPendingLearningsForEmbedding(0)
		if len(pending) > 0 {
			log.Printf("  Phase 2.5: Embed %d learnings", len(pending))
			bin, _ := os.Executable()
			cmd := exec.Command(bin, "embed-learnings")
			homeDir, _ := os.UserHomeDir()
			logPath := filepath.Join(homeDir, ".claude", "yesmem", "logs", "embed.log")
			os.MkdirAll(filepath.Dir(logPath), 0755)
			if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
			if err := cmd.Run(); err != nil {
				log.Printf("  Embed: failed: %v", err)
			}
		}
	}

	// Phase 3: Evolution (scoped to extracted sessions)
	if cfg.Evolution.AutoResolve && extraction.HasBudget(client) {
		evolutionScope, err := buildEvolutionScope(store, toExtract)
		if err == nil && len(evolutionScope) > 0 {
			log.Printf("  Phase 3: Evolution (%d project/category groups)", countEvolutionScope(evolutionScope))
			checked, superseded := evoExt.RunEvolutionForScopeSince(store, evolutionScope, nil, evolutionAfterID)
			log.Printf("  Evolution: %d checked, %d superseded", checked, superseded)
		}
	}

	log.Printf("━━━ Batch extraction complete ━━━")
}

// placeholder to satisfy import
var _ models.Session

// truncID safely truncates an ID to 8 chars for logging.
func truncID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
