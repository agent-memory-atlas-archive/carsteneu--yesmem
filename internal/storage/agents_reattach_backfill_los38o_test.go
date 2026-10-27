package storage

import (
	"testing"
	"time"
)

// YM-333 (Los 38o), found by the cold review: the column alone loses the rows an
// installation already kept. A row the previous version closed with the marker in
// progress carries orphan_reattachable = 0 after the ALTER, and the recovery sweep
// only looks at live rows — so the first prune after the upgrade deletes exactly the
// set the marker exists to protect. The migration has to carry the old marker over.
func TestMigrateAgentsSchemaKeepsTheLegacyReattachMarker(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)

	// A row as the previous version left it: marker in progress, column untouched.
	if _, err := s.db.Exec(
		`INSERT INTO agents (id, project, section, status, pid, stopped_at, created_at, progress, orphan_reattachable) VALUES ('agent-legacy-marker', 'p', 's', 'stopped', 0, ?, ?, 'orphaned-reattachable', 0)`,
		old, old,
	); err != nil {
		t.Fatal(err)
	}

	if err := s.MigrateAgentsSchema(); err != nil {
		t.Fatalf("MigrateAgentsSchema: %v", err)
	}

	deleted, err := s.PruneStoppedAgents(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneStoppedAgents: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("prune deleted %d legacy row(s), want 0: the old marker in progress must survive the migration", deleted)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE id = 'agent-legacy-marker'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("legacy reattach row count = %d, want 1", n)
	}
}

// TestMigrateAgentsSchemaDoesNotBackfillLiveOrOrdinaryRows: the backfill is the
// narrow carry-over, not a blanket keep-everything. A running row and a normally
// closed row must stay untouched.
func TestMigrateAgentsSchemaDoesNotBackfillLiveOrOrdinaryRows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)

	for _, row := range []struct{ id, status, progress string }{
		{"agent-legacy-running", "running", "orphaned-reattachable"},
		{"agent-legacy-normal", "stopped", "stopped: token_budget exceeded"},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO agents (id, project, section, status, pid, stopped_at, created_at, progress, orphan_reattachable) VALUES (?, 'p', 's', ?, 0, ?, ?, ?, 0)`,
			row.id, row.status, old, old, row.progress,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.MigrateAgentsSchema(); err != nil {
		t.Fatalf("MigrateAgentsSchema: %v", err)
	}
	for _, id := range []string{"agent-legacy-running", "agent-legacy-normal"} {
		var marked int
		if err := s.db.QueryRow(`SELECT COALESCE(orphan_reattachable, 0) FROM agents WHERE id = ?`, id).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked != 0 {
			t.Errorf("%s got the reattach marker, but it is not a closed recovery row", id)
		}
	}
}
