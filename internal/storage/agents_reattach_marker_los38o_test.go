package storage

import (
	"testing"
	"time"
)

// YM-333 (Los 38o): the reattach marker lived in agents.progress. progress is
// userland data — the stop path overwrites it, and update_agent accepts it from
// any socket caller — so one later stop turned a kept recovery row into a row the
// prune was allowed to delete, and with it the only handle a still-running backend
// session had (YM-297). The marker moves into a column only the recovery writes.
func TestRecoverOrphanedSetsTheReattachColumn(t *testing.T) {
	s := mustOpen(t)
	if err := s.AgentCreate(Agent{
		ID:                "agent-los38o-kept",
		Project:           "p",
		Section:           "yesloop-los38o-kept",
		SessionID:         "sess-kept",
		Status:            "running",
		Backend:           "opencode",
		OpencodeSessionID: "ses_kept",
		WorkDir:           "/w/kept",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	if _, _, _, err := s.AgentRecoverOrphaned(
		func(int) bool { return false },
		func(string, string) (bool, error) { return true, nil },
	); err != nil {
		t.Fatalf("AgentRecoverOrphaned: %v", err)
	}

	var marked int
	if err := s.db.QueryRow(`SELECT COALESCE(orphan_reattachable, 0) FROM agents WHERE id = 'agent-los38o-kept'`).Scan(&marked); err != nil {
		t.Fatalf("read reattach column: %v", err)
	}
	if marked != 1 {
		t.Fatalf("orphan_reattachable = %d, want 1 on the row the recovery kept", marked)
	}
}

// TestPruneStoppedAgentsKeepsAReattachRowWhoseProgressWasOverwritten is the
// regression: the identical row is prunable when the marker sits in progress,
// because the first stop after the recovery rewrites that field.
func TestPruneStoppedAgentsKeepsAReattachRowWhoseProgressWasOverwritten(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)

	if _, err := s.db.Exec(
		`INSERT INTO agents (id, project, section, status, pid, stopped_at, created_at) VALUES ('agent-los38o-stop', 'p', 's', 'stopped', 0, ?, ?)`,
		old, old,
	); err != nil {
		t.Fatal(err)
	}
	// The daemon-start recovery keeps the row: its backend session still exists.
	if _, err := s.db.Exec(`UPDATE agents SET orphan_reattachable = 1 WHERE id = 'agent-los38o-stop'`); err != nil {
		t.Fatal(err)
	}
	// A later stop writes its reason into progress, as every stop path does.
	if _, err := s.db.Exec(`UPDATE agents SET progress = 'stopped: max_runtime 48h0m0s exceeded, no verified process' WHERE id = 'agent-los38o-stop'`); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.PruneStoppedAgents(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneStoppedAgents: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("prune deleted %d row(s), want 0: the reattach marker must survive a later stop overwriting progress", deleted)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE id = 'agent-los38o-stop'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reattach row count = %d, want 1", n)
	}
}
