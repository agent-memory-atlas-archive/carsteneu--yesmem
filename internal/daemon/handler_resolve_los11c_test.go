package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-251 (Los 11c). The live damage on 17.10.2026: an agent called
// resolve_by_text with the Los-11 session title and the OR-based search
// returned learning #66092, a May proxy-health-check note that shares only the
// word "proxy" with the query.
const (
	los11cLiveQuery      = "Los 11: Proxy Rules-Cache und Injektionszähler"
	los11cUnrelatedNote  = "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation"
	los11cProject        = "yesmem"
	los11cTargetTaskText = "Los 11: Proxy Rules-Cache und Injektionszähler reparieren"
)

func los11cInsertUnfinished(t *testing.T, s *storage.Store, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "unfinished", Content: content, Project: los11cProject,
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})
	if err != nil {
		t.Fatalf("insert unfinished %q: %v", content, err)
	}
	return id
}

// TestHandleResolveByText_Los11c_LiveCaseResolvesNothing is the red regression
// test for the live case: with no item containing every query word, nothing may
// be resolved. Before the fix this resolved the unrelated May note.
func TestHandleResolveByText_Los11c_LiveCaseResolvesNothing(t *testing.T) {
	h, s := mustHandler(t)
	unrelatedID := los11cInsertUnfinished(t, s, los11cUnrelatedNote)

	resp := h.Handle(Request{
		Method: "resolve_by_text",
		Params: map[string]any{"text": los11cLiveQuery, "project": los11cProject},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["id"] != nil {
		t.Fatalf("resolve_by_text resolved #%v for a query that matches no item completely", result["id"])
	}
	if resolved, ok := result["resolved"].(bool); !ok || resolved {
		t.Errorf("expected resolved=false, got %v", result["resolved"])
	}

	// The handler must still hand back what it found, so the caller can resolve
	// one item by id instead of guessing (YM-251).
	candidates, _ := result["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("expected the shared-word candidate to be listed, got %+v", result["candidates"])
	}
	entry, _ := candidates[0].(map[string]any)
	if id, ok := entry["id"].(float64); !ok || int64(id) != unrelatedID {
		t.Fatalf("expected candidate #%d, got %+v", unrelatedID, candidates[0])
	}

	active, err := s.GetActiveLearnings("unfinished", los11cProject, "", "", 0)
	if err != nil {
		t.Fatalf("active learnings: %v", err)
	}
	if len(active) != 1 || active[0].ID != unrelatedID {
		t.Fatalf("the unrelated item must stay open, got %+v", active)
	}
}

// TestHandleResolveByText_Los11c_ResolvesTheCompleteMatch keeps the useful part
// of the old behaviour: when exactly one open item contains every query word,
// it is resolved.
func TestHandleResolveByText_Los11c_ResolvesTheCompleteMatch(t *testing.T) {
	h, s := mustHandler(t)
	los11cInsertUnfinished(t, s, los11cUnrelatedNote)
	targetID := los11cInsertUnfinished(t, s, los11cTargetTaskText)

	resp := h.Handle(Request{
		Method: "resolve_by_text",
		Params: map[string]any{"text": los11cLiveQuery, "project": los11cProject},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, ok := result["id"].(float64)
	if !ok || int64(id) != targetID {
		t.Fatalf("expected #%d to be resolved, got %v", targetID, result["id"])
	}
	if resolved, ok := result["resolved"].(bool); !ok || !resolved {
		t.Errorf("expected resolved=true, got %v", result["resolved"])
	}
}

// TestHandleResolveByText_Los11c_ReportsCandidatesInsteadOfGuessing pins the
// ambiguity guard: two open items containing every query word stay untouched
// and both come back as candidates.
func TestHandleResolveByText_Los11c_ReportsCandidatesInsteadOfGuessing(t *testing.T) {
	h, s := mustHandler(t)
	firstID := los11cInsertUnfinished(t, s, "Proxy Rules-Cache Injektionszähler Variante eins")
	secondID := los11cInsertUnfinished(t, s, "Proxy Rules-Cache Injektionszähler Variante zwei")

	resp := h.Handle(Request{
		Method: "resolve_by_text",
		Params: map[string]any{"text": "Proxy Rules-Cache Injektionszähler", "project": los11cProject},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["id"] != nil {
		t.Fatalf("ambiguous query must not resolve, got #%v", result["id"])
	}
	candidates, _ := result["candidates"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	seen := map[int64]bool{}
	for _, c := range candidates {
		entry, _ := c.(map[string]any)
		if id, ok := entry["id"].(float64); ok {
			seen[int64(id)] = true
		}
	}
	if !seen[firstID] || !seen[secondID] {
		t.Fatalf("candidates must name both items (#%d, #%d), got %+v", firstID, secondID, candidates)
	}
	active, _ := s.GetActiveLearnings("unfinished", los11cProject, "", "", 0)
	if len(active) != 2 {
		t.Fatalf("ambiguous query must leave both items open, got %d", len(active))
	}
}

// TestHandleReopen_Los11c_MakesAResolvedItemActiveAgain covers the missing
// reopen path: without an RPC there is no way to undo a wrong resolve.
func TestHandleReopen_Los11c_MakesAResolvedItemActiveAgain(t *testing.T) {
	h, s := mustHandler(t)
	id := los11cInsertUnfinished(t, s, "proxy cache cleanup")

	resolveResp := h.Handle(Request{
		Method: "resolve",
		Params: map[string]any{"learning_id": float64(id), "reason": "wrongly resolved"},
	})
	if resolveResp.Error != "" {
		t.Fatalf("resolve failed: %s", resolveResp.Error)
	}

	reopenResp := h.Handle(Request{
		Method: "reopen",
		Params: map[string]any{"learning_id": float64(id)},
	})
	if reopenResp.Error != "" {
		t.Fatalf("reopen: %s", reopenResp.Error)
	}

	l, err := s.GetLearning(id)
	if err != nil || l == nil {
		t.Fatalf("get learning: %v", err)
	}
	if l.SupersededBy != nil {
		t.Errorf("reopened item must be active, got superseded_by=%d", *l.SupersededBy)
	}
	active, _ := s.GetActiveLearnings("unfinished", los11cProject, "", "", 0)
	if len(active) != 1 || active[0].ID != id {
		t.Fatalf("reopened item must be listed again, got %+v", active)
	}
}
