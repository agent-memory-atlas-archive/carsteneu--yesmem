package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func runBackup() {
	dataDir := yesmemDataDir()
	now := time.Now().Format("20060102-150405")

	// Default backup dir
	backupDir := filepath.Join(dataDir, "backups")

	// Optional override: yesmem backup [path]
	if len(os.Args) > 2 {
		backupDir = os.Args[2]
	}

	if err := os.MkdirAll(backupDir, 0700); err != nil {
		log.Fatalf("create backup dir: %v", err)
	}
	// MkdirAll leaves the mode of a directory it did not create, so an existing
	// backup directory keeps whatever mode an earlier version gave it (0755). The
	// snapshots are full transcripts, and VACUUM INTO writes them readable for the
	// whole duration of a multi-gigabyte run, so the directory has to close the gap.
	if err := os.Chmod(backupDir, 0700); err != nil {
		log.Fatalf("harden backup dir: %v", err)
	}

	// The databases storage.Open keeps in the data directory. messages.db carries
	// the message bodies since the migrate-messages split, caps.db the cap_store
	// tables of every saved cap; a backup without either restores the knowledge
	// base but loses part of the installation.
	for _, db := range []struct {
		name   string
		suffix string
	}{
		{"yesmem.db", ""},
		{"runtime.db", "-runtime"},
		{"messages.db", "-messages"},
		{"caps.db", "-caps"},
	} {
		src := filepath.Join(dataDir, db.name)
		info, statErr := os.Stat(src)
		if os.IsNotExist(statErr) {
			// A fresh installation creates its side databases on first use; the
			// main database is the installation itself.
			if db.name == "yesmem.db" {
				log.Fatalf("backup %s: %v", db.name, statErr)
			}
			fmt.Printf("! %s not present, skipped\n", db.name)
			continue
		}
		if statErr != nil {
			log.Fatalf("backup %s: %v", db.name, statErr)
		}
		if info.Size() == 0 {
			// An empty file is a broken database, not a missing one. Skipping it
			// would report a successful backup that cannot be restored.
			log.Fatalf("backup %s: file %s is empty", db.name, src)
		}
		dest := filepath.Join(backupDir, fmt.Sprintf("yesmem-%s%s.db", now, db.suffix))
		if err := vacuumInto(src, dest); err != nil {
			log.Fatalf("backup %s: %v", db.name, err)
		}
		// VACUUM INTO writes the copy under the process umask, and a backup holds
		// full transcripts. The data directory protects the live files; the backup
		// directory is a command argument and may be anywhere.
		if err := os.Chmod(dest, 0o600); err != nil {
			log.Fatalf("harden %s: %v", dest, err)
		}
		printResult(db.name, src, dest, backupDir)
	}
}

func vacuumInto(srcPath, destPath string) error {
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer db.Close()

	// The source can carry a live writer; without a timeout VACUUM INTO fails with
	// SQLITE_BUSY on the first competing write.
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		return fmt.Errorf("busy_timeout: %w", err)
	}

	_, err = db.Exec("VACUUM INTO ?", destPath)
	if err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	return nil
}

func printResult(name, srcPath, destPath, backupDir string) {
	srcInfo, _ := os.Stat(srcPath)
	destInfo, _ := os.Stat(destPath)

	srcSize := humanSize(srcInfo.Size())
	destSize := humanSize(destInfo.Size())

	// Show relative path from backupDir's parent for cleaner output
	rel, err := filepath.Rel(filepath.Dir(backupDir), destPath)
	if err != nil {
		rel = destPath
	}

	fmt.Printf("✓ Backed up %s (%s) → %s (%s)\n", name, srcSize, rel, destSize)
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%d KB", b/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
