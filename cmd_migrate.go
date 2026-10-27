package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/storage"

	_ "modernc.org/sqlite"
)

func runMigrateProject() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: yesmem migrate-project <from> <to> [--no-backup]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Renames all project references in the database.")
		fmt.Fprintln(os.Stderr, "Accepts short names (memory) or full paths (/home/user/memory).")
		fmt.Fprintln(os.Stderr, "Creates a backup before migrating (unless --no-backup).")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Example: yesmem migrate-project memory yesmem")
		fmt.Fprintln(os.Stderr, "Example: yesmem migrate-project /home/user/old-project /home/user/new-project")
		os.Exit(1)
	}

	fromArg := os.Args[2]
	toArg := os.Args[3]

	noBackup := false
	dryRun := false
	for _, arg := range os.Args[4:] {
		if arg == "--no-backup" {
			noBackup = true
		}
		if arg == "--dry-run" {
			dryRun = true
		}
	}

	if fromArg == toArg {
		fmt.Fprintln(os.Stderr, "Error: <from> and <to> must be different")
		os.Exit(1)
	}

	dataDir := yesmemDataDir()
	dbPath := filepath.Join(dataDir, "yesmem.db")

	// Resolve full paths and short names from input.
	// If input contains '/', treat as full path → derive short name.
	// If input is a short name → look up full path from sessions table.
	fromPath, fromShort := resolveProjectNames(dbPath, fromArg)
	toPath, toShort := resolveProjectNames(dbPath, toArg)

	// If target has no full path yet (new project not in sessions),
	// require explicit --to-path flag or warn.
	if toPath == "" && fromPath != "" {
		// Check if there's a --to-path flag
		for i, arg := range os.Args[4:] {
			if arg == "--to-path" && i+1 < len(os.Args[4:])-1 {
				toPath = os.Args[4+i+1]
			}
		}
		if toPath == "" {
			fmt.Fprintf(os.Stderr, "Warning: target project '%s' not found in sessions.\n", toArg)
			fmt.Fprintf(os.Stderr, "  Sessions will be updated to path '%s' (= source path).\n", fromPath)
			fmt.Fprintf(os.Stderr, "  Use --to-path /actual/path to override.\n\n")
			toPath = fromPath
		}
	}
	if toShort == "" {
		toShort = toArg
	}
	if fromShort == "" {
		fromShort = fromArg
	}

	if fromPath == "" {
		fmt.Fprintf(os.Stderr, "Error: no sessions found for project '%s'\n", fromArg)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Migrating project:\n")
	fmt.Fprintf(os.Stderr, "  Path:  %s → %s\n", fromPath, toPath)
	fmt.Fprintf(os.Stderr, "  Short: %s → %s\n\n", fromShort, toShort)

	// Open store
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Dry-run mode
	if dryRun {
		res, err := store.MigrateProjectDryRun(fromPath, toPath, fromShort, toShort)
		if err != nil {
			log.Fatalf("dry-run: %v", err)
		}
		printMigrateResult("Would migrate (dry-run):", res, fromShort, toShort)
		return
	}

	// Backup unless --no-backup
	if !noBackup {
		backupDir := filepath.Join(dataDir, "backups")
		if err := os.MkdirAll(backupDir, 0755); err != nil {
			log.Fatalf("create backup dir: %v", err)
		}
		now := time.Now().Format("20060102-150405")
		dest := filepath.Join(backupDir, fmt.Sprintf("yesmem-%s.db", now))

		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			log.Fatalf("open db for backup: %v", err)
		}
		if _, err := db.Exec("VACUUM INTO ?", dest); err != nil {
			db.Close()
			log.Fatalf("backup: %v", err)
		}
		db.Close()

		rel, _ := filepath.Rel(filepath.Dir(backupDir), dest)
		fmt.Fprintf(os.Stderr, "Backup created: %s\n\n", rel)
	}

	// Open store and run migration
	res, err := store.MigrateProject(fromPath, toPath, fromShort, toShort)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}

	printMigrateResult("Migrated:", res, fromShort, toShort)

	// The database is renamed, the archive is not: session documents archived
	// under the old encoded project name would be written a second time under the
	// new one by the next archiving pass (YM-320). Both key spaces are moved —
	// ProjectDirName of the full path, and of the short name that legacy rows
	// carry as a project.
	// The archive lives where the daemon writes it, not necessarily beside the
	// database: a configured paths.archive points elsewhere, and a hardcoded
	// dataDir/archive would make this whole step a silent no-op (YM-320, review).
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))
	arch := archive.New(cfg.ArchiveDir(dataDir))
	// Named once, unconditionally: the base comes from the configuration, and an
	// operator who pointed paths.archive at the wrong directory sees it here
	// instead of after folders were renamed there.
	fmt.Fprintf(os.Stderr, "Archive base: %s\n", arch.BaseDir())
	for _, pair := range [][2]string{
		{archive.ProjectDirName(fromPath), archive.ProjectDirName(toPath)},
		{archive.ProjectDirName(fromShort), archive.ProjectDirName(toShort)},
	} {
		if pair[0] == pair[1] {
			continue
		}
		moved, kept, err := arch.MoveProjectDir(pair[0], pair[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: archive folder %s -> %s not moved: %v\n", pair[0], pair[1], err)
			fmt.Fprintf(os.Stderr, "  The database rename is already committed. Move %s to %s by hand before the next archiving run, or those session documents are written twice.\n",
				filepath.Join(arch.BaseDir(), pair[0]), filepath.Join(arch.BaseDir(), pair[1]))
			continue
		}
		if moved > 0 || kept > 0 {
			fmt.Fprintf(os.Stderr, "Archive: %d document(s) moved, %d kept (%s -> %s)\n", moved, kept, pair[0], pair[1])
		}
	}
}

func printMigrateResult(header string, res *storage.MigrateResult, fromShort, toShort string) {
	fmt.Fprintf(os.Stderr, "%s\n", header)
	fmt.Fprintf(os.Stderr, "  sessions:          %4d  (by path)\n", res.Sessions)
	fmt.Fprintf(os.Stderr, "  session_tracking:  %4d  (by path)\n", res.Tracking)
	fmt.Fprintf(os.Stderr, "  learnings:         %4d  (by short)\n", res.Learnings)
	fmt.Fprintf(os.Stderr, "  project_profiles:  %4d  (by short)\n", res.Profiles)
	fmt.Fprintf(os.Stderr, "  file_coverage:     %4d  (by short)\n", res.Coverage)
	fmt.Fprintf(os.Stderr, "  claudemd_state:    %4d  (by short)\n", res.ClaudeMD)
	fmt.Fprintf(os.Stderr, "  refined_briefings: %4d  (by short)\n", res.Briefings)
	fmt.Fprintf(os.Stderr, "  learning_clusters: %4d  (by short)\n", res.Clusters)
	fmt.Fprintf(os.Stderr, "  doc_sources:       %4d  (by short)\n", res.DocSources)
	fmt.Fprintf(os.Stderr, "  knowledge_gaps:    %4d  (by short)\n", res.Gaps)
	fmt.Fprintf(os.Stderr, "  pinned_learnings:  %4d  (by short)\n", res.Pins)
	fmt.Fprintf(os.Stderr, "  contradictions:    %4d  (by short)\n", res.Contradictions)
	fmt.Fprintf(os.Stderr, "  query_log:         %4d  (by short)\n", res.QueryLog)
	fmt.Fprintf(os.Stderr, "  query_clusters:    %4d  (by short)\n", res.QueryClusters)
	fmt.Fprintf(os.Stderr, "  agent_broadcasts:  %4d  (by short)\n", res.Broadcasts)
	fmt.Fprintf(os.Stderr, "  ─────────────────────────────────\n")
	fmt.Fprintf(os.Stderr, "  Total:             %4d\n", res.Total())
	fmt.Fprintf(os.Stderr, "\nProject '%s' → '%s' complete\n", fromShort, toShort)
}

// resolveProjectNames takes a user input (short name or full path) and returns
// (fullPath, shortName). Looks up sessions table to find the mapping.
func resolveProjectNames(dbPath, input string) (fullPath, shortName string) {
	if strings.Contains(input, "/") {
		// Input is a full path — derive short name from last segment
		fullPath = input
		shortName = filepath.Base(input)
		return
	}

	// Input is a short name — look up full path from sessions
	shortName = input
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return
	}
	defer db.Close()

	row := db.QueryRow(`SELECT DISTINCT project FROM sessions WHERE project_short = ? LIMIT 1`, input)
	if err := row.Scan(&fullPath); err != nil {
		// Not found — that's OK for the target project (it may not exist yet)
		return "", input
	}
	return
}
