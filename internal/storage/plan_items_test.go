package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// Plan items are stored as a JSON string beside the free text; an absent
// structure is the empty string, so a plan written before the column existed
// reads back exactly as it was written.
func TestPlanRowItemsRoundtrip(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().Truncate(time.Second)

	items := `[{"content":"read the doc","status":"completed","depth":0},{"content":"wire the ledger","reason":"doc risk 2","depth":1}]`
	row := &PlanRow{
		ThreadID:  "t-items",
		Content:   "free text twin",
		Items:     items,
		Status:    "active",
		Scope:     "session",
		Project:   "proj",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.UpsertPlan(row); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.GetPlan("t-items")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Items != items {
		t.Errorf("items not roundtripped: %q", got.Items)
	}
	if got.Content != "free text twin" {
		t.Errorf("content must stay untouched, got %q", got.Content)
	}

	active, err := s.GetActivePlans()
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	var found *PlanRow
	for _, p := range active {
		if p.ThreadID == "t-items" {
			found = p
		}
	}
	if found == nil {
		t.Fatal("plan missing from GetActivePlans")
	}
	if found.Items != items {
		t.Errorf("items not roundtripped through GetActivePlans: %q", found.Items)
	}
}

// A row written without items must read back with an empty Items string: that
// empty value is what tells every renderer to fall back to the free text.
func TestPlanRowWithoutItemsReadsEmpty(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().Truncate(time.Second)
	if err := s.UpsertPlan(&PlanRow{ThreadID: "t-legacy", Content: "legacy text", Status: "active", Scope: "session", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetPlan("t-legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Items != "" {
		t.Errorf("expected empty items, got %q", got.Items)
	}
	if got.Content != "legacy text" {
		t.Errorf("expected legacy content, got %q", got.Content)
	}
}

// An existing installation carries the plans table without the items column.
// Opening the database must add it, keep the old rows readable, and let the
// next write store items.
func TestPlanItemsColumnAddedToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if err := s1.UpsertPlan(&PlanRow{ThreadID: "t-upgrade", Content: "before", Status: "active", Scope: "session", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Simulate the pre-items schema.
	if _, err := s1.db.Exec(`ALTER TABLE plans DROP COLUMN items`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetPlan("t-upgrade")
	if err != nil {
		t.Fatalf("get after upgrade: %v", err)
	}
	if got.Content != "before" || got.Items != "" {
		t.Errorf("legacy row changed by the migration: content=%q items=%q", got.Content, got.Items)
	}

	items := `[{"content":"after the migration"}]`
	got.Items = items
	got.UpdatedAt = time.Now().Truncate(time.Second)
	if err := s2.UpsertPlan(got); err != nil {
		t.Fatalf("upsert after upgrade: %v", err)
	}
	again, err := s2.GetPlan("t-upgrade")
	if err != nil {
		t.Fatalf("get after write: %v", err)
	}
	if again.Items != items {
		t.Errorf("items not persisted after migration: %q", again.Items)
	}
}
