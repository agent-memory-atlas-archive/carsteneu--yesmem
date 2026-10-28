package hooks

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-260 (Los 11d): the PostToolUse commit hook resolved the first hit of an OR
// search. These tests pin the live case that closed learning #66092 and the
// positive case that must keep working.

const los11dHookProject = "/home/tester/memory/yesmem"

func insertHookTask(t *testing.T, store *storage.Store, content string) int64 {
	t.Helper()
	id, err := store.InsertLearning(&models.Learning{
		Category: "unfinished", Content: content, Project: los11dHookProject,
		CanonicalProject: "yesmem", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
	})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return id
}

func TestResolveMatchingTask_Los11d_LiveCaseStaysOpen(t *testing.T) {
	store := mustOpenHooksDB(t)
	wrongID := insertHookTask(t, store, "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation")

	msg := resolveMatchingTask(store, "Los 11: Proxy Rules-Cache und Injektionszähler", los11dHookProject)
	if strings.Contains(msg, "auto-resolved") {
		t.Fatalf("the live case must not resolve, got %q", msg)
	}

	item, err := store.GetLearning(wrongID)
	if err != nil || item == nil {
		t.Fatalf("get learning: %v", err)
	}
	if item.SupersededBy != nil {
		t.Fatalf("the live case must stay open, #%d was resolved", wrongID)
	}
}

func TestResolveMatchingTask_Los11d_ResolvesTheNamedTask(t *testing.T) {
	store := mustOpenHooksDB(t)
	insertHookTask(t, store, "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation")
	targetID := insertHookTask(t, store, "Los 11: Proxy Rules-Cache und Injektionszähler reparieren (YM-260)")

	msg := resolveMatchingTask(store, "fix(hooks): graded resolver (YM-260)", los11dHookProject)
	if !strings.Contains(msg, "auto-resolved") {
		t.Fatalf("expected the named item to resolve, got %q", msg)
	}

	item, err := store.GetLearning(targetID)
	if err != nil || item == nil {
		t.Fatalf("get learning: %v", err)
	}
	if item.SupersededBy == nil {
		t.Fatalf("expected #%d to be resolved", targetID)
	}
}
