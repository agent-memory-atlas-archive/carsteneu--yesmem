package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/indexer"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// runArchiveSessions backfills archive documents for indexed sessions that have
// none: the ones indexed before the scanner archived at all, and the ones whose
// archive write failed and whose watermark has moved past them since. Only
// sessions without a JSONL file are covered; a failed ArchiveFile copy of a
// file-backed session belongs to the indexer.
func runArchiveSessions() {
	dataDir := yesmemDataDir()

	fs := flag.NewFlagSet("archive-sessions", flag.ExitOnError)
	project := fs.String("project", "", "only sessions of this project")
	fs.StringVar(project, "p", "", "only sessions of this project (short)")
	dryRun := fs.Bool("dry-run", false, "list the sessions without a document, write nothing")
	fs.BoolVar(dryRun, "n", false, "dry-run (short)")
	limit := fs.Int("limit", 0, "stop after this many documents (0: no limit)")
	fs.Parse(os.Args[2:])

	// storage.Open and not OpenReadOnly: this store must resolve messages.db, and
	// the read-only opener maps messages to yesmem.db, which would find no messages
	// for a split installation.
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %s", textutil.ErrorLine(err))
	}
	defer store.Close()

	opts := indexer.ArchiveBackfillOptions{
		Project:  *project,
		DryRun:   *dryRun,
		Limit:    *limit,
		Progress: os.Stdout,
	}
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))
	if cfg != nil {
		opts.ExcludeProjects = cfg.ExcludeProjects
	}
	// The backfill has to write where the daemon writes: with a configured
	// paths.archive, a hardcoded dataDir/archive checks and fills a second tree.
	archDir := cfg.ArchiveDir(dataDir)
	if !*dryRun {
		// A dry run writes nothing, so it must not create the base directory either.
		if err := archive.EnsureBaseDir(archDir); err != nil {
			log.Fatalf("archive sessions: %v", err)
		}
	}

	res, err := indexer.ArchiveMissingSessions(store, archive.New(archDir), opts)
	if err != nil {
		log.Fatalf("archive sessions: %s", textutil.ErrorLine(err))
	}

	action := "archived"
	if *dryRun {
		action = "would archive"
	}
	fmt.Printf("archive: %q\n", res.ArchiveDir)
	fmt.Printf("sessions without JSONL file: %d, already archived: %d, excluded project: %d, %s: %d\n",
		res.Considered, res.Present, res.Excluded, action, len(res.Written))
	if len(res.Incomplete) > 0 {
		fmt.Printf("left unwritten, no readable messages or the stored count does not match them: %d\n", len(res.Incomplete))
		for _, id := range res.Incomplete {
			fmt.Printf("  %q\n", id)
		}
	}
}
