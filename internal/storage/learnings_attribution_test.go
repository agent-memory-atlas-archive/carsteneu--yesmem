package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// readProvenance reads the two provenance columns straight from the row: neither
// is part of GetLearning's column list, and that is the point of the finding.
func readProvenance(t *testing.T, s *Store, id int64) (string, int64) {
	t.Helper()
	var role string
	var dialog sql.NullInt64
	if err := s.db.QueryRow(`SELECT COALESCE(agent_role, ''), dialog_id FROM learnings WHERE id = ?`, id).Scan(&role, &dialog); err != nil {
		t.Fatalf("read provenance of learning %d: %v", id, err)
	}
	return role, dialog.Int64
}

// TestInsertLearningBatch_RecordsAgentRole pins YM-74 (part a): agent_role and
// dialog_id existed as columns, conflict detection and the wiki read them, and no
// code path ever wrote them. A learning stored by a spawned agent now carries the
// kind of agent that produced it.
func TestInsertLearningBatch_RecordsAgentRole(t *testing.T) {
	s := newTestStore(t)
	if err := s.AgentCreate(Agent{
		ID: "agent-prov-1", Project: "p", Section: "yesloop-prov",
		SessionID: "uuid-prov", OpencodeSessionID: "ses_prov", Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "opencode:ses_prov",
		Category:   "gotcha",
		Content:    "an agent-written learning carries its role",
		Project:    "p",
		Source:     "claude_suggested",
		Confidence: 0.8,
		CreatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	role, dialogID := readProvenance(t, s, id)
	if role != "yesloop" {
		t.Errorf("agent_role = %q, want yesloop (the bound agent's section kind)", role)
	}
	if dialogID != 0 {
		t.Errorf("dialog_id = %d, want 0: the agent is not in a dialog", dialogID)
	}
}

// TestInsertLearningBatch_RecordsDialogID pins the second half: a learning from a
// session that is in a dialog records that dialog as its context.
func TestInsertLearningBatch_RecordsDialogID(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertSession(&models.Session{ID: "sess-in-dialog", Project: "p", StartedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	dialogID, err := s.StartDialog("sess-in-dialog", "sess-partner", "provenance topic")
	if err != nil {
		t.Fatalf("StartDialog: %v", err)
	}

	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "sess-in-dialog",
		Category:   "gotcha",
		Content:    "a learning from a dialog session records the dialog",
		Project:    "p",
		Source:     "claude_suggested",
		Confidence: 0.8,
		CreatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	_, gotDialog := readProvenance(t, s, id)
	if gotDialog != dialogID {
		t.Errorf("dialog_id = %d, want %d", gotDialog, dialogID)
	}
}

// TestInsertLearningBatch_KeepsExplicitProvenance: a caller that knows better (an
// import, a backfill) keeps its values.
func TestInsertLearningBatch_KeepsExplicitProvenance(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertSession(&models.Session{ID: "sess-explicit", Project: "p", StartedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, err := s.StartDialog("sess-explicit", "sess-other", "some topic"); err != nil {
		t.Fatalf("StartDialog: %v", err)
	}

	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "sess-explicit",
		Category:   "gotcha",
		Content:    "explicit provenance is not overwritten",
		Project:    "p",
		Source:     "claude_suggested",
		Confidence: 0.8,
		CreatedAt:  time.Now(),
		AgentRole:  "review",
		DialogID:   4242,
	})
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	role, dialogID := readProvenance(t, s, id)
	if role != "review" || dialogID != 4242 {
		t.Errorf("provenance = (%q, %d), want (review, 4242)", role, dialogID)
	}
}
