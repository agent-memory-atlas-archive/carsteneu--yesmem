package main

import (
	"io"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/daemon"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// seedCapLearning stores a cap row the way an older version left it on disk:
// save_cap refuses a retired name (that is the rule this test guards), so the row
// is written directly.
func seedCapLearning(t *testing.T, store *storage.Store, name string) {
	t.Helper()
	ctx, err := daemon.CapMeta{
		Name: name, Description: "seeded", Version: 2,
		Scripts: []daemon.ScriptMeta{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "echo hi", Schema: `{"type":"object"}`,
		}},
	}.ToJSON()
	if err != nil {
		t.Fatalf("cap meta: %v", err)
	}
	content := name + " — seeded"
	l := &models.Learning{
		Content:     content,
		Category:    "cap",
		Source:      "user_stated",
		Confidence:  1.0,
		Context:     ctx,
		Domain:      "code",
		TriggerRule: "cap:" + name,
		CreatedAt:   time.Now().UTC(),
		ContentHash: textutil.ContentHash(content),
	}
	if _, err := store.InsertLearning(l); err != nil {
		t.Fatalf("insert cap %s: %v", name, err)
	}
}

func activeCapNames(t *testing.T, store *storage.Store) map[string]bool {
	t.Helper()
	caps, err := store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("list caps: %v", err)
	}
	names := map[string]bool{}
	for _, cap := range caps {
		meta, err := daemon.ParseCapMeta(cap.Context)
		if err != nil {
			continue
		}
		names[meta.Name] = true
	}
	return names
}

// YM-181 [LIVE-8] und YM-197 [DEV-DT33] (Los 56i): das Cap-Learning von
// wiki_export (#65743) ist auf der Live-Installation aktiv, obwohl das Cap aus
// dem Bundle entfernt wurde und die Pipeline in "yesmem wiki-render" liegt. Der
// Migrationslauf muss es ausmustern, sonst bleibt es im Katalog und als dritter
// Wiki-Schreiber erreichbar.
func TestLos56i_MigrationRetiresTheObsoleteCap(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	seedCapLearning(t, store, "wiki_export")
	seedCapLearning(t, store, "telegram")

	if _, err := retireObsoleteCaps(store, io.Discard); err != nil {
		t.Fatalf("retire: %v", err)
	}

	names := activeCapNames(t, store)
	if names["wiki_export"] {
		t.Error("wiki_export is still active after the migration")
	}
	if !names["telegram"] {
		t.Error("telegram was retired although the bundle still ships it")
	}

	// Idempotent: a second run finds nothing to retire.
	n, err := retireObsoleteCaps(store, io.Discard)
	if err != nil {
		t.Fatalf("second retire: %v", err)
	}
	if n != 0 {
		t.Errorf("second run retired %d caps, want 0", n)
	}
}
