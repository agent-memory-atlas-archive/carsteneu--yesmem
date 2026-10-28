package daemon

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-260 (Los 11d): the v2 plugin sends the raw commit message. Before this
// ticket it called resolve_by_text, which after YM-251 refuses every realistic
// commit; before that it closed the first OR hit. The RPC now closes only the
// task the commit subject names.

func insertDaemonTask(t *testing.T, s *storage.Store, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "unfinished", Content: content, Project: "/home/tester/memory/yesmem",
		CanonicalProject: "yesmem", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
	})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return id
}

func resolveByCommitResult(t *testing.T, h *Handler, message, project string) map[string]any {
	t.Helper()
	resp := h.Handle(Request{Method: "resolve_by_commit", Params: map[string]any{
		"message": message,
		"project": project,
	}})
	if resp.Error != "" {
		t.Fatalf("resolve_by_commit: %s", resp.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestHandleResolveByCommit_Los11d_LiveCaseStaysOpen(t *testing.T) {
	h, s := mustHandler(t)
	wrongID := insertDaemonTask(t, s, "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation")

	out := resolveByCommitResult(t, h, "Los 11: Proxy Rules-Cache und Injektionszähler", "yesmem")
	if out["resolved"] != false {
		t.Fatalf("live case must not resolve, got %+v", out)
	}
	// The RPC must never answer "Not resolved: " with an empty reason.
	if reason, _ := out["reason"].(string); reason == "" {
		t.Error("expected a non-empty reason")
	}

	item, err := s.GetLearning(wrongID)
	if err != nil || item == nil {
		t.Fatalf("get learning: %v", err)
	}
	if item.SupersededBy != nil {
		t.Fatalf("the live case must stay open, #%d was resolved", wrongID)
	}
}

func TestHandleResolveByCommit_Los11d_ResolvesTheNamedTask(t *testing.T) {
	h, s := mustHandler(t)
	insertDaemonTask(t, s, "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation")
	targetID := insertDaemonTask(t, s, "Los 11: Proxy Rules-Cache und Injektionszähler reparieren (YM-260)")

	out := resolveByCommitResult(t, h, "fix(hooks): graded resolver (YM-260)", "yesmem")
	if out["resolved"] != true {
		t.Fatalf("expected a resolution, got %+v", out)
	}
	if int64(out["id"].(float64)) != targetID {
		t.Fatalf("resolved #%v, want #%d", out["id"], targetID)
	}
}

func TestHandleResolveByCommit_Los11d_AnchorsOnTheLearningID(t *testing.T) {
	h, s := mustHandler(t)
	targetID := insertDaemonTask(t, s, "Rebuild the proxy cache ruleset")
	insertDaemonTask(t, s, "document the release notes")

	out := resolveByCommitResult(t, h, "chore: rebuild cache ruleset (#"+strconv.FormatInt(targetID, 10)+")", "yesmem")
	if out["resolved"] != true {
		t.Fatalf("expected the named learning to resolve, got %+v", out)
	}
	if int64(out["id"].(float64)) != targetID {
		t.Fatalf("resolved #%v, want #%d", out["id"], targetID)
	}
	if out["reason"] != "git commit: chore: rebuild cache ruleset (#"+strconv.FormatInt(targetID, 10)+")" {
		t.Fatalf("expected the stored commit reason, got %v", out["reason"])
	}
}
