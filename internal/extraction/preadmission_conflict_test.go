package extraction

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TestCheckPreAdmission_ConflictBranchesAreReachable pins the half of YM-74 that a
// column writer alone does not deliver: the conflict branches compare agent roles,
// and they ran before any insert, so the new learning had no role and the existing
// one was read from a field GetLearning does not select. Both sides were always
// empty, which made the branches dead code.
func TestCheckPreAdmission_ConflictBranchesAreReachable(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// The existing learning belongs to a "code-review" agent.
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-conflict-old", Project: "/repo/conflict", Section: "code-review-7",
		SessionID: "uuid-old", Status: "finished",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		SessionID:  "uuid-old",
		Category:   "gotcha",
		Content:    "the token budget default is applied at spawn time and never at the heartbeat",
		Project:    "/repo/conflict",
		Source:     "claude_suggested",
		Confidence: 0.8,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	// A different kind of agent writes about the same topic in other words.
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-conflict-new", Project: "/repo/conflict", Section: "yesloop-42",
		SessionID: "uuid-new", Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	incoming := &models.Learning{
		SessionID:  "uuid-new",
		Category:   "gotcha",
		Content:    "the token budget default is applied at spawn time and not in the heartbeat",
		Project:    "/repo/conflict",
		Source:     "claude_suggested",
		Confidence: 0.8,
	}

	got := CheckPreAdmission(s, incoming)
	if incoming.AgentRole != "yesloop" {
		t.Fatalf("learning role = %q, want it resolved from the session (yesloop)", incoming.AgentRole)
	}
	// Both roles are present now, and the documented behaviour for a cross-agent
	// divergence is "keep both": the new learning is inserted instead of being
	// suppressed as a duplicate.
	if got.Action != PreAdmissionInsert {
		t.Fatalf("decision = %+v (reason %q), want insert: the two agents' roles differ, so both learnings stay", got, got.Reason)
	}
}
