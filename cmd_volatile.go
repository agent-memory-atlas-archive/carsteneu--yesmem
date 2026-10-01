package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/daemon"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// runVolatileSweep classifies active learnings that look like volatile state
// snapshots with the LLM sentinel and (with --apply) applies the verdicts:
// volatile → superseded as noise, bounded → 30-day TTL + staleness_type.
func runVolatileSweep() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	fs := flag.NewFlagSet("volatile-sweep", flag.ExitOnError)
	apply := fs.Bool("apply", false, "apply verdicts to the database (default: dry-run)")
	limit := fs.Int("limit", 0, "limit number of classified candidates (0 = all)")
	fs.Parse(os.Args[2:])

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	var candidates []models.Learning
	rows, err := store.DB().Query(`SELECT id, category, content FROM learnings WHERE superseded_by IS NULL`)
	if err != nil {
		log.Fatalf("query actives: %v", err)
	}
	for rows.Next() {
		var l models.Learning
		if err := rows.Scan(&l.ID, &l.Category, &l.Content); err != nil {
			log.Fatalf("scan: %v", err)
		}
		if extraction.LooksLikeStateSnapshot(l.Content) {
			candidates = append(candidates, l)
		}
	}
	rows.Close()
	fmt.Fprintf(os.Stderr, "Kandidaten (Recruiter): %d\n", len(candidates))
	if *limit > 0 && len(candidates) > *limit {
		candidates = candidates[:*limit]
	}
	if len(candidates) == 0 {
		fmt.Println("Nothing to do.")
		return
	}
	fmt.Fprintln(os.Stderr, "\n-- dry-run: no changes made. Use --apply to write verdicts.")

	apiKey := cfg.ResolvedAPIKey()
	if apiKey == "" {
		apiKey = daemon.ReadClaudeCodeAPIKey()
	}
	if apiKey == "" {
		log.Fatal("No API key — set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure in config.yaml")
	}
	client, err := extraction.NewLLMClient(cfg.LLM.Provider, apiKey, cfg.QualityModelID(), cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
	if err != nil {
		log.Fatalf("LLM client: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Sentinel-Client: %s (provider %s, model %s, quality %s)\n",
		client.Name(), cfg.LLM.Provider, client.Model(), cfg.QualityModelID())

	counts := map[string]int{}
	superseded := 0
	bounded := 0
	const batchSize = 15
	for start := 0; start < len(candidates); start += batchSize {
		end := start + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[start:end]
		verdicts, err := extraction.ClassifyVolatileBatch(client, batch)
		if err != nil {
			log.Printf("batch %d-%d: %v", start, end, err)
			continue
		}
		for _, v := range verdicts {
			counts[v.Verdict]++
			if !*apply {
				if v.Verdict == "volatile" {
					fmt.Printf("VOLATILE #%d (%.2f): %s\n", v.ID, v.Confidence, v.Reason)
				}
				continue
			}
			switch v.Verdict {
			case "volatile":
				if _, err := store.DB().Exec(`UPDATE learnings SET superseded_by = -1,
					supersede_reason = 'volatile snapshot (sentinel #90115)', valid_until = datetime('now')
					WHERE id = ? AND superseded_by IS NULL`, v.ID); err != nil {
					log.Printf("supersede %d: %v", v.ID, err)
				} else {
					superseded++
				}
			case "bounded":
				if _, err := store.DB().Exec(`UPDATE learnings SET valid_until = datetime('now', '+30 days'),
					staleness_type = 'volatile' WHERE id = ? AND superseded_by IS NULL`, v.ID); err != nil {
					log.Printf("bound %d: %v", v.ID, err)
				} else {
					bounded++
				}
			}
		}
		done := start + batchSize
		if done > len(candidates) {
			done = len(candidates)
		}
		fmt.Fprintf(os.Stderr, "  ... %d/%d classified\n", done, len(candidates))
	}

	fmt.Printf("\nVerdicts: volatile=%d bounded=%d durable=%d (apply=%v)\n",
		counts["volatile"], counts["bounded"], counts["durable"], *apply)
	if *apply {
		fmt.Printf("Applied: %d superseded, %d TTL-bounded\n", superseded, bounded)
	}
}
