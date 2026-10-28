package storage

import (
	"fmt"
	"testing"
	"time"
)

// YM-312: nothing ever removed an agent row once it had been closed. Every loop,
// every probe spawn, every crashed agent left a stopped row behind, and the rows
// are read by list_agents, the dashboard and every section lookup. The prune is
// deliberately narrow: a closed row, no process attached, and older than the
// retention window.
func TestPruneStoppedAgentsKeepsLiveAndRecentRows(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)

	rows := []struct {
		id        string
		status    string
		pid       int
		stoppedAt string
		createdAt string
	}{
		// pruned: closed, no process, long gone
		{"agent-old-stopped", "stopped", 0, old, old},
		{"agent-old-failed", "failed", 0, old, old},
		// pruned: legacy row without stopped_at falls back to created_at
		{"agent-old-nostopped", "stopped", 0, "", old},
		// kept: recent
		{"agent-recent-stopped", "stopped", 0, recent, recent},
		// kept: a closed row that still names a process (a stopped row is not
		// proof its process is gone, Learning #101526)
		{"agent-old-with-pid", "stopped", 4242, old, old},
		// kept: closed at daemon start with the reattach marker (YM-297)
		{"agent-old-orphaned", "stopped", 0, old, old},
		// kept: not a terminal status
		{"agent-old-paused", "paused", 0, old, old},
		{"agent-old-running", "running", 0, old, old},
	}
	for _, row := range rows {
		if _, err := s.db.Exec(
			`INSERT INTO agents (id, project, section, status, pid, stopped_at, created_at) VALUES (?, 'p', 's', ?, ?, ?, ?)`,
			row.id, row.status, row.pid, row.stoppedAt, row.createdAt,
		); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	// The daemon-start recovery closes rows with this marker; they are the only
	// handle a still-resumable backend session has (YM-297). Since YM-333 the
	// durable marker is the column; progress carries only the readable reason.
	if _, err := s.db.Exec(`UPDATE agents SET orphan_reattachable = 1, progress = 'orphaned-reattachable' WHERE id = 'agent-old-orphaned'`); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.PruneStoppedAgents(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneStoppedAgents: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	left, err := s.AgentList("")
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	remaining := map[string]bool{}
	for _, a := range left {
		remaining[a.ID] = true
	}
	for _, keep := range []string{"agent-recent-stopped", "agent-old-with-pid", "agent-old-orphaned", "agent-old-paused", "agent-old-running"} {
		if !remaining[keep] {
			t.Errorf("%s was pruned, but it is live, recent, or not terminal", keep)
		}
	}
	for _, gone := range []string{"agent-old-stopped", "agent-old-failed", "agent-old-nostopped"} {
		if remaining[gone] {
			t.Errorf("%s survived the prune", gone)
		}
	}
}

// TestPruneStoppedAgentsIsBoundedByTheCutoff: the cutoff is the only knob, so a
// cutoff in the past must not take the rows that are merely older than nothing.
func TestPruneStoppedAgentsReturnsZeroOnEmptyStore(t *testing.T) {
	s := mustOpen(t)
	deleted, err := s.PruneStoppedAgents(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneStoppedAgents: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d on an empty store, want 0", deleted)
	}
}

// TestPruneStoppedAgentsComparesTimestampsNotStrings: created_at/stopped_at carry
// the writer's offset, so a lexical comparison puts rows in the wrong order. A row
// closed 40 days ago in a +02:00 offset must still be pruned.
func TestPruneStoppedAgentsComparesTimestampsNotStrings(t *testing.T) {
	s := mustOpen(t)
	now := time.Now()
	zone := time.FixedZone("CEST", 2*3600)
	oldOffset := now.Add(-40 * 24 * time.Hour).In(zone).Format(time.RFC3339)

	if _, err := s.db.Exec(
		`INSERT INTO agents (id, project, section, status, pid, stopped_at, created_at) VALUES ('agent-offset', 'p', 's', 'stopped', 0, ?, ?)`,
		oldOffset, oldOffset,
	); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneStoppedAgents(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneStoppedAgents: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (offset-carrying timestamp must be compared as time, not text)", deleted)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE id = 'agent-offset'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal(fmt.Sprintf("row with offset timestamp survived (count %d)", n))
	}
}
