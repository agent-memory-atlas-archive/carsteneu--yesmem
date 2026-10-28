package storage

import (
	"testing"
	"time"
)

// TestLos26c_OrphanedFrozenRowsAreReclaimed pins finding 2 of YM-315. A frozen
// prefix row belongs to a thread that will invalidate it or come back for it;
// a thread that does neither leaves the row (several megabytes) in proxy_state
// forever, because the proxy eviction is deliberately in-memory only and
// --reset-cache is the only path that ever deleted those rows.
func TestLos26c_OrphanedFrozenRowsAreReclaimed(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	for _, key := range []string{"frozen:old", "frozen_markers:old", "frozen:new", "decay:old"} {
		if err := s.SetProxyState(key, `{"messages":[]}`); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	stale := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := s.proxyStateDB().Exec(`UPDATE proxy_state SET updated_at = ? WHERE key <> 'frozen:new'`, stale); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	if n, err := s.DeleteProxyStatePrefixOlderThan("frozen:", 24*time.Hour); err != nil {
		t.Fatalf("delete frozen: %v", err)
	} else if n != 1 {
		t.Fatalf("frozen: deleted %d rows, want 1", n)
	}
	if n, err := s.DeleteProxyStatePrefixOlderThan("frozen_markers:", 24*time.Hour); err != nil {
		t.Fatalf("delete markers: %v", err)
	} else if n != 1 {
		t.Fatalf("frozen_markers: deleted %d rows, want 1", n)
	}

	if raw, _ := s.GetProxyState("frozen:new"); raw == "" {
		t.Fatal("the current frozen row was reclaimed")
	}
	if raw, _ := s.GetProxyState("decay:old"); raw == "" {
		t.Fatal("the janitor reached outside its prefix")
	}
	// Count rows directly: GetProxyState returns "" for a deleted row and for an
	// emptied one, so it cannot prove a DELETE happened.
	if n := s.countProxyStateRows(t, "frozen:old"); n != 0 {
		t.Fatal("the orphaned frozen row survived the reclaim")
	}
}

// TestLos26c_UnparseableTimestampIsKeptFailSafe pins the guard against a
// comparison that silently matches everything: a row whose updated_at cannot be
// read must be kept, not deleted.
func TestLos26c_UnparseableTimestampIsKeptFailSafe(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	if err := s.SetProxyState("frozen:odd", `{"messages":[]}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.proxyStateDB().Exec(`UPDATE proxy_state SET updated_at = 'not-a-timestamp' WHERE key = 'frozen:odd'`); err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	n, err := s.DeleteProxyStatePrefixOlderThan("frozen:", 24*time.Hour)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("deleted %d rows with an unreadable timestamp, want 0", n)
	}
	if raw, _ := s.GetProxyState("frozen:odd"); raw == "" {
		t.Fatal("a row whose age is unknown was deleted")
	}
}
