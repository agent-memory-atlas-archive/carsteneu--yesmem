package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/carsteneu/yesmem/internal/clustering"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/storage"
)

func runTraitCleanup() {
	dryRun := false
	threshold := 0.85
	for i, arg := range os.Args[2:] {
		switch arg {
		case "--dry-run", "-n":
			dryRun = true
		case "--threshold", "-t":
			if i+1 < len(os.Args[2:]) {
				fmt.Sscanf(os.Args[2:][i+1], "%f", &threshold)
			}
		}
	}

	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	provider, err := embedding.NewProviderFromConfig(cfg.Embedding)
	if err != nil {
		log.Fatalf("embedding provider: %v", err)
	}
	defer provider.Close()

	if !provider.Enabled() {
		log.Fatal("Embedding provider disabled — set embedding.provider to 'static' in config.yaml")
	}

	traits, err := store.GetActivePersonaTraits("default", 0.0)
	if err != nil {
		log.Fatalf("get traits: %v", err)
	}

	if len(traits) == 0 {
		fmt.Fprintln(os.Stderr, "No active traits found.")
		return
	}

	fmt.Fprintf(os.Stderr, "Embedding %d traits (threshold: %.2f)...\n", len(traits), threshold)

	// Build text representations for embedding
	texts := make([]string, len(traits))
	for i, t := range traits {
		texts[i] = fmt.Sprintf("%s %s %s", t.Dimension, t.TraitKey, t.TraitValue)
	}

	// Embed in one batch
	vectors, err := provider.Embed(context.Background(), texts)
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	// Find clusters via pairwise cosine similarity
	type pair struct {
		i, j int
		sim  float64
	}
	var similar []pair
	for i := 0; i < len(vectors); i++ {
		for j := i + 1; j < len(vectors); j++ {
			// Only compare within same dimension
			if traits[i].Dimension != traits[j].Dimension {
				continue
			}
			sim := clustering.CosineSimilarity(vectors[i], vectors[j])
			if sim >= threshold {
				similar = append(similar, pair{i, j, sim})
			}
		}
	}

	if len(similar) == 0 {
		fmt.Fprintln(os.Stderr, "No similar trait pairs found. Data is clean.")
		return
	}

	// Sort by similarity descending
	sort.Slice(similar, func(a, b int) bool { return similar[a].sim > similar[b].sim })

	fmt.Fprintf(os.Stderr, "\nFound %d similar pairs:\n\n", len(similar))

	// Track which traits are resolved, so a pair is not re-processed. In dry-run
	// mode a drop is resolved unless the storage guard would refuse it; in the
	// live mode the affected row count decides. A protected trait therefore
	// stays unresolved in both modes and its remaining pairs are still compared.
	resolved := make(map[int]bool)
	candidates := 0
	supersededCount := 0

	for _, p := range similar {
		if resolved[p.i] || resolved[p.j] {
			continue // Already handled
		}

		ti, tj := traits[p.i], traits[p.j]
		keep, drop := ti, tj
		dropIdx := p.i
		if tj.Confidence > ti.Confidence || (tj.Confidence == ti.Confidence && tj.EvidenceCount > ti.EvidenceCount) {
			keep, drop = tj, ti
			dropIdx = p.j
		}

		fmt.Fprintf(os.Stderr, "  [%.2f] KEEP: %q.%q=%q (conf:%.2f, ev:%d)\n",
			p.sim, keep.Dimension, keep.TraitKey, keep.TraitValue, keep.Confidence, keep.EvidenceCount)
		fmt.Fprintf(os.Stderr, "         DROP: %q.%q=%q (conf:%.2f, ev:%d)\n",
			drop.Dimension, drop.TraitKey, drop.TraitValue, drop.Confidence, drop.EvidenceCount)
		// A user_override drop is refused by the storage guard, so it does not
		// count as a supersede candidate in either mode.
		if drop.Source != "user_override" {
			candidates++
		}

		if dryRun {
			if drop.Source != "user_override" {
				resolved[dropIdx] = true
			}
			continue
		}

		affected, err := store.SupersedePersonaTrait("default", drop.Dimension, drop.TraitKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "         supersede failed: %v\n", err)
			continue
		}
		if affected == 0 {
			fmt.Fprintf(os.Stderr, "         PROTECTED (user_override) — kept active\n")
			continue
		}
		resolved[dropIdx] = true
		supersededCount += int(affected)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "\n%d traits to supersede.\n", candidates)
		fmt.Fprintln(os.Stderr, "--dry-run: no changes made.")
		return
	}
	fmt.Fprintf(os.Stderr, "\nDone: %d traits superseded. %d remaining.\n", supersededCount, len(traits)-supersededCount)
}
