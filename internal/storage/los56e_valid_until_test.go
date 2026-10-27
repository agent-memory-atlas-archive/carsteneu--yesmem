package storage

import (
	"path/filepath"
	"testing"
)

// TestLos56eStoreOpenKeepsValidUntilOnActiveRows is the regression test for
// YM-348 (1): the v0.68 repair statement ran on every store open and cleared
// valid_until on every active row, not only on the reactivated ones it was
// written for. cmd_volatile sets a TTL by writing valid_until on a row that
// stays active (superseded_by IS NULL), so the next open of the daemon wiped the
// TTL of every bounded volatile learning — the mechanism could never take
// effect.
func TestLos56eStoreOpenKeepsValidUntilOnActiveRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "yesmem.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const ttl = "2026-11-19 23:00:00"
	if _, err := s.DB().Exec(`INSERT INTO learnings (session_id, category, content, confidence, created_at, model_used, source, valid_until, staleness_type)
		VALUES ('los56e', 'gotcha', 'bounded volatile row', 1.0, '2026-10-20T10:00:00+02:00', 'test', 'llm_extracted', ?, 'volatile')`, ttl); err != nil {
		t.Fatalf("insert bounded row: %v", err)
	}
	// The row the statement was written for: reactivated, its invalidation
	// metadata still attached.
	if _, err := s.DB().Exec(`INSERT INTO learnings (session_id, category, content, confidence, created_at, model_used, source, supersede_reason)
		VALUES ('los56e', 'gotcha', 'reactivated row', 1.0, '2026-10-20T10:00:00+02:00', 'test', 'llm_extracted', 'old reason')`); err != nil {
		t.Fatalf("insert reactivated row: %v", err)
	}
	s.Close()

	// Every daemon start reopens the store, which replays the migration list.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var got string
	if err := s2.readerDB().QueryRow(`SELECT COALESCE(valid_until, '') FROM learnings WHERE content = 'bounded volatile row'`).Scan(&got); err != nil {
		t.Fatalf("read valid_until: %v", err)
	}
	if got != ttl {
		t.Fatalf("valid_until = %q after reopen, want %q: the TTL must survive the store open", got, ttl)
	}

	var reason string
	if err := s2.readerDB().QueryRow(`SELECT COALESCE(supersede_reason, '') FROM learnings WHERE content = 'reactivated row'`).Scan(&reason); err != nil {
		t.Fatalf("read supersede_reason: %v", err)
	}
	if reason != "" {
		t.Fatalf("supersede_reason = %q on an active row, want it cleared", reason)
	}
}
