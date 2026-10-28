package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// storeDirListing lists the data directory, ignoring the -wal/-shm companions.
// SQLite creates those two sidecars for any read of a WAL-mode database; they are
// not a data write. The database files themselves must not change.
func storeDirListing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		names = append(names, name)
	}
	return fmt.Sprint(names)
}

// Read-only consumers (the spend CLI) must not mutate the live database: a plain
// Open() writes the schema, runs migrations and creates the runtime/messages
// siblings next to yesmem.db.
func TestOpenReadOnlyDoesNotCreateOrWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yesmem.db")
	day := time.Now().Format("2006-01-02")

	w, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if err := w.TrackSpend(day, "extract", 1.5); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	before := storeDirListing(t, dir)

	ro, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	history, err := ro.GetSpendHistory(7)
	if err != nil {
		t.Fatalf("read on read-only store: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 spend row, got %d", len(history))
	}

	if _, err := ro.InsertLearning(&models.Learning{
		Category:  "gotcha",
		Content:   "must not be written",
		CreatedAt: time.Now(),
	}); err == nil {
		t.Error("write on a read-only store succeeded")
	}

	if after := storeDirListing(t, dir); after != before {
		t.Errorf("read-only open changed the data dir: %s -> %s", before, after)
	}
}

// Opening a missing database read-only must fail instead of creating one.
func TestOpenReadOnlyMissingDB(t *testing.T) {
	dir := t.TempDir()

	if _, err := OpenReadOnly(filepath.Join(dir, "yesmem.db")); err == nil {
		t.Fatal("expected an error for a missing database")
	}
	if after := storeDirListing(t, dir); after != "[]" {
		t.Errorf("read-only open created files: %s", after)
	}
}
