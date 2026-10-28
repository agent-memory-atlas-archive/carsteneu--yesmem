package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-252 (PRX-P29): handleSetPlan persists the resolved project, but plans
// written before that fix (and every plan whose row still carries an empty
// project) kept it empty forever: update_plan never resolved or persisted one,
// so a reload rebuilt the docs hint globals-only.
func TestHandleUpdatePlan_BackfillsProject_Los11b(t *testing.T) {
	h, store := mustHandler(t)
	defer cleanupPlans()

	const project = "/home/me/backfill-proj"
	store.UpsertDocSource(&storage.DocSource{
		Name: "backfill-ref", Project: project, DocType: "reference", ExampleQuery: "how to",
	})

	now := time.Now()
	if err := store.UpsertPlan(&storage.PlanRow{
		ThreadID:  "thread-backfill",
		Content:   "⬜ Step 1",
		Status:    "active",
		Scope:     "session",
		Project:   "",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	LoadPlansFromDB(store)

	// The MCP path sends no project, only _cwd (internal/mcp/server.go).
	resp := h.handleUpdatePlan(map[string]any{
		"thread_id": "thread-backfill",
		"completed": []any{"Step 1"},
		"_cwd":      project,
	})
	if resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	planStore.RLock()
	gotProject := planStore.plans["thread-backfill"].Project
	gotHint := planStore.plans["thread-backfill"].DocsHint
	planStore.RUnlock()

	if gotProject != project {
		t.Fatalf("in-memory project = %q, want the _cwd-resolved project", gotProject)
	}
	if !strings.Contains(gotHint, "backfill-ref") {
		t.Errorf("docs hint was not rebuilt for the backfilled project: %q", gotHint)
	}

	rows, err := store.GetActivePlans()
	if err != nil {
		t.Fatalf("get active plans: %v", err)
	}
	var persisted string
	for _, r := range rows {
		if r.ThreadID == "thread-backfill" {
			persisted = r.Project
		}
	}
	if persisted != project {
		t.Fatalf("persisted project = %q, want the backfilled project", persisted)
	}
}

// Without a project param and without _cwd there is nothing to resolve: the
// update must leave the row alone instead of inventing a project.
func TestHandleUpdatePlan_NoProjectSourceLeavesPlanAlone_Los11b(t *testing.T) {
	h, store := mustHandler(t)
	defer cleanupPlans()

	now := time.Now()
	if err := store.UpsertPlan(&storage.PlanRow{
		ThreadID:  "thread-nosource",
		Content:   "⬜ Step 1",
		Status:    "active",
		Scope:     "session",
		Project:   "",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	LoadPlansFromDB(store)

	if resp := h.handleUpdatePlan(map[string]any{
		"thread_id": "thread-nosource",
		"add":       []any{"⬜ Step 2"},
	}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	planStore.RLock()
	got := planStore.plans["thread-nosource"].Project
	planStore.RUnlock()
	if got != "" {
		t.Fatalf("project = %q, want it untouched without a source", got)
	}

	rows, err := store.GetActivePlans()
	if err != nil {
		t.Fatalf("get active plans: %v", err)
	}
	for _, r := range rows {
		if r.ThreadID == "thread-nosource" && r.Project != "" {
			t.Fatalf("persisted project = %q, want it untouched", r.Project)
		}
	}
}

// A plan that already carries a project must keep it: update_plan runs on every
// checkpoint and must not rewrite a correct value from a missing _cwd.
func TestHandleUpdatePlan_KeepsExistingProject_Los11b(t *testing.T) {
	h, store := mustHandler(t)
	defer cleanupPlans()

	const project = "/home/me/existing-proj"
	if resp := h.handleSetPlan(map[string]any{
		"plan":      "⬜ Step 1",
		"thread_id": "thread-existing",
		"project":   project,
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	if resp := h.handleUpdatePlan(map[string]any{
		"thread_id": "thread-existing",
		"add":       []any{"⬜ Step 2"},
	}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	planStore.RLock()
	got := planStore.plans["thread-existing"].Project
	planStore.RUnlock()
	if got != project {
		t.Fatalf("project = %q, want the unchanged %q", got, project)
	}

	rows, err := store.GetActivePlans()
	if err != nil {
		t.Fatalf("get active plans: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ThreadID != "thread-existing" {
			continue
		}
		found = true
		if r.Project != project {
			t.Fatalf("persisted project = %q, want %q", r.Project, project)
		}
	}
	if !found {
		t.Fatal("the plan row disappeared from the store")
	}
}
