package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos56eExportRestoresTheValidUntilTTL is the round-2 review finding on
// YM-348: the export format left valid_until out because it was "the supersession
// tombstone, always written together with superseded_by". Since the store open no
// longer wipes it, an active row can carry a TTL (cmd_volatile bounds a volatile
// learning that way), and an export/import cycle would have turned that bounded
// learning into a permanent one.
func TestLos56eExportRestoresTheValidUntilTTL(t *testing.T) {
	store := mustOpen(t)
	now := time.Now().UTC().Truncate(time.Second)
	validUntil := now.Add(30 * 24 * time.Hour)

	id, err := store.InsertLearning(&models.Learning{
		Category:   "gotcha",
		Content:    "bounded volatile learning",
		Project:    "los56e",
		Source:     "user_stated",
		CreatedAt:  now,
		ValidUntil: &validUntil,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The stored value must be there before the export is judged.
	var stored string
	if err := store.readerDB().QueryRow(`SELECT COALESCE(valid_until, '') FROM learnings WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read valid_until: %v", err)
	}
	if stored == "" {
		t.Fatal("INSERT dropped the TTL: an inserted ValidUntil must reach the column")
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if _, err := store.ExportLearnings(outPath); err != nil {
		t.Fatalf("export: %v", err)
	}

	fresh := mustOpen(t)
	if _, _, err := fresh.ImportLearnings(outPath); err != nil {
		t.Fatalf("import: %v", err)
	}
	ids, err := fresh.GetRecentLearningIDs(1, "")
	if err != nil || len(ids) != 1 {
		t.Fatalf("recent ids: %v (err %v)", ids, err)
	}
	got, err := fresh.GetLearning(ids[0])
	if err != nil {
		t.Fatalf("get learning: %v", err)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(validUntil) {
		t.Fatalf("valid_until = %v, want %v: a restored TTL must not become permanent", got.ValidUntil, validUntil)
	}
}
