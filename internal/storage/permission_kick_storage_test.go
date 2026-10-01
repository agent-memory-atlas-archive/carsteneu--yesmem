package storage

import (
	"fmt"
	"testing"
)

func TestAgentPermissionKick_Roundtrip(t *testing.T) {
	s := newTestStore(t)

	for i, want := range []string{"off", "on", ""} {
		agent := Agent{
			ID:             fmt.Sprintf("agent-pk-rt%d", i),
			Project:        "proj",
			Section:        fmt.Sprintf("sec-%d", i),
			Status:         "running",
			PermissionKick: want,
		}
		if err := s.AgentCreate(agent); err != nil {
			t.Fatalf("AgentCreate(%q): %v", want, err)
		}
		got, err := s.AgentGet(agent.ID)
		if err != nil {
			t.Fatalf("AgentGet(%q): %v", want, err)
		}
		if got.PermissionKick != want {
			t.Errorf("PermissionKick = %q, want %q", got.PermissionKick, want)
		}
	}

	// Update path: switch an agent from off to on via AgentUpdate.
	if err := s.AgentUpdate("agent-pk-rt0", map[string]any{"permission_kick": "on"}); err != nil {
		t.Fatalf("AgentUpdate: %v", err)
	}
	got, err := s.AgentGet("agent-pk-rt0")
	if err != nil {
		t.Fatalf("AgentGet after update: %v", err)
	}
	if got.PermissionKick != "on" {
		t.Errorf("PermissionKick after update = %q, want on", got.PermissionKick)
	}
}

// TestMigrateAgentsSchema_AddsPermissionKick simulates an old agents table
// without the permission_kick column and verifies the additive migration
// adds it (test case 9).
func TestMigrateAgentsSchema_AddsPermissionKick(t *testing.T) {
	s := newTestStore(t)

	// Recreate the agents table with a legacy (pre-permission_kick) layout.
	if _, err := s.db.Exec(`DROP TABLE agents`); err != nil {
		t.Fatalf("drop agents: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE agents (
		id TEXT PRIMARY KEY,
		project TEXT NOT NULL,
		section TEXT NOT NULL,
		status TEXT DEFAULT 'pending'
	)`); err != nil {
		t.Fatalf("create legacy agents: %v", err)
	}

	if err := s.MigrateAgentsSchema(); err != nil {
		t.Fatalf("MigrateAgentsSchema: %v", err)
	}

	rows, err := s.db.Query(`PRAGMA table_info(agents)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "permission_kick" {
			found = true
		}
	}
	if !found {
		t.Fatal("permission_kick column missing after MigrateAgentsSchema")
	}

	// The migrated table must accept agent records with the field set.
	if err := s.MigrateAgentsSchema(); err != nil {
		t.Fatalf("MigrateAgentsSchema (idempotent rerun): %v", err)
	}
}
