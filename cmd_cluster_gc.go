package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/storage"
)

// runClusterGC reports cluster rows no live project maps to and deletes them with
// --apply. The default is a listing, and it opens the database read-only: a look at
// the state must not run schema migrations on a live installation.
//
// Two things keep the sweep from deleting data that still means something (YM-423):
// a key whose rows name a live learning is kept and reported, and --migrate re-points
// the legacy keys whose learnings unanimously name one repository path before the
// sweep runs. Keys without that proof stay where they are and are listed with the
// reason.
//
// The stale change-detection hashes live in runtime.db, which a read-only handle
// cannot reach (it maps the proxy state onto yesmem.db), so they are collected with
// --apply only. Reporting an empty list from the wrong file would be a false "all
// clean".
func runClusterGC(args []string) {
	apply := hasFlag(args, "--apply")
	migrate := hasFlag(args, "--migrate")

	path := filepath.Join(yesmemDataDir(), "yesmem.db")
	if !apply {
		// Nothing to report on a store that was never created, and the read-only
		// open cannot create it.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Println("clusters-gc: 0 keys, dry-run (no database yet)")
			return
		}
	}

	var store *storage.Store
	var err error
	if apply {
		store, err = storage.Open(path)
	} else {
		store, err = storage.OpenReadOnly(path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "clusters-gc: open store: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	moves := 0
	if migrate {
		moved, skipped, migErr := store.MigrateLegacyClusterKeys(apply)
		moves = len(moved)
		// Printed before the error is handled: an --apply run that fails halfway has
		// already moved the listed keys, and the operator has to be able to read that.
		for _, m := range moved {
			fmt.Printf("  %s cluster key: %q -> %q (%d rows)\n", migrationVerb(apply), m.From, m.To, m.Rows)
		}
		for _, s := range skipped {
			fmt.Printf("  keep legacy cluster key: %q (%s)\n", s.Key, s.Reason)
		}
		if migErr != nil {
			fmt.Fprintf(os.Stderr, "clusters-gc: migrate: %v\n", migErr)
			os.Exit(1)
		}
	}

	// Supersede edges whose target row no longer exists. The delete paths clear
	// them since YM-435, so what is left comes from an older binary or a manual
	// delete. The listing is free; --apply rewrites the edges.
	//
	// Runs before the cluster sweep on purpose: a store that cannot be cluster
	// pruned (a fresh database has no cluster table yet) must still report its
	// dangling edges.
	edges, cleared, err := store.RepairDanglingSupersedeEdges(apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clusters-gc: %v\n", err)
		os.Exit(1)
	}
	for _, e := range edges {
		fmt.Printf("  dangling %s edge: learning #%d -> #%d (%s)\n", e.Column, e.ID, e.Target, edgeVerb(apply))
	}

	res, err := store.PruneOrphanLearningClusters(apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clusters-gc: %v\n", err)
		os.Exit(1)
	}
	for _, key := range res.Kept {
		fmt.Printf("  keep unreachable cluster key: %q (its rows still name live learnings)\n", key)
	}
	for _, key := range res.Orphans {
		fmt.Printf("  unreachable cluster key: %q\n", key)
	}

	if !apply {
		if migrate {
			fmt.Printf("clusters-gc: %d keys, %d kept (%d rows), %d dangling supersede edge(s), %d migration(s) pending, dry-run (re-run with --apply to write; stale cluster hashes need --apply too)\n",
				len(res.Orphans), len(res.Kept), res.KeptRows, len(edges), moves)
		} else {
			fmt.Printf("clusters-gc: %d keys, %d kept (%d rows), %d dangling supersede edge(s), dry-run (--migrate lists the stale keys, --apply writes; stale cluster hashes need --apply too)\n",
				len(res.Orphans), len(res.Kept), res.KeptRows, len(edges))
		}
		return
	}

	deadHashes, hashesDeleted, err := store.PruneOrphanClusterHashes(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clusters-gc: %v\n", err)
		os.Exit(1)
	}
	for _, key := range deadHashes {
		fmt.Printf("  unreachable cluster hash: %q\n", key)
	}
	// cleared counts the rows the UPDATEs touched, not the rows the report above
	// listed: the two are only equal without concurrent writers.
	fmt.Printf("clusters-gc: %d keys, %d rows deleted, %d stale hashes deleted, %d keys kept, %d migrated, %d dangling supersede edge(s) cleared\n",
		len(res.Orphans), res.Deleted, hashesDeleted, len(res.Kept), moves, cleared)
}

func edgeVerb(apply bool) string {
	if apply {
		return "cleared"
	}
	return "would clear"
}

func migrationVerb(apply bool) string {
	if apply {
		return "migrated"
	}
	return "would migrate"
}
