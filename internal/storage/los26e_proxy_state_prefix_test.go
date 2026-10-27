package storage

import "testing"

// DeleteProxyStatePrefix matched the prefix with LIKE and therefore read the
// characters _ and % as wildcards. A reset of "frozen_markers:" also deleted
// every key whose seventh character happens to differ from the underscore.
func TestLos26e_DeleteProxyStatePrefixTreatsUnderscoreLiterally(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	for _, key := range []string{"frozen_markers:a", "frozenXmarkers:foreign", "frozen:a"} {
		if err := s.SetProxyState(key, `{}`); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	n, err := s.DeleteProxyStatePrefix("frozen_markers:")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	if got := s.los26eCountProxyStateRows(t, "frozenXmarkers:foreign"); got != 1 {
		t.Errorf("rows of frozenXmarkers:foreign after the delete = %d, want 1 (the underscore matched a foreign key)", got)
	}
	if raw, _ := s.GetProxyState("frozen:a"); raw == "" {
		t.Error("the delete reached outside its prefix")
	}
}

// The same read applies to %: a literal percent sign in the prefix must not turn
// the whole key space into a match.
func TestLos26e_DeleteProxyStatePrefixTreatsPercentLiterally(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	for _, key := range []string{"project:a%b:state", "project:axb:state", "project:a%b"} {
		if err := s.SetProxyState(key, `{}`); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	n, err := s.DeleteProxyStatePrefix("project:a%b:")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	if raw, _ := s.GetProxyState("project:axb:state"); raw == "" {
		t.Error("percent sign in the prefix deleted a key without the literal prefix")
	}
	if raw, _ := s.GetProxyState("project:a%b"); raw == "" {
		t.Error("a key that is a prefix of the pattern but lacks the trailing colon was deleted")
	}
}

// substr() counts characters, len() counts bytes. A prefix outside ASCII must
// not end up deleting nothing.
func TestLos26e_DeleteProxyStatePrefixHandlesMultiBytePrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	for _, key := range []string{"projekt:é:state", "projekt:éx:state"} {
		if err := s.SetProxyState(key, `{}`); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	n, err := s.DeleteProxyStatePrefix("projekt:é:")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	if raw, _ := s.GetProxyState("projekt:éx:state"); raw == "" {
		t.Error("the multi-byte prefix reached a key that does not carry it")
	}
}

// An empty prefix matches every key as a prefix and would empty the whole
// proxy_state table. The RPC boundary rejects it, the store must not depend on
// that.
func TestLos26e_DeleteProxyStatePrefixRejectsEmptyPrefix(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	for _, key := range []string{"frozen:a", "decay:b"} {
		if err := s.SetProxyState(key, `{}`); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	n, err := s.DeleteProxyStatePrefix("")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty prefix deleted %d rows, want 0", n)
	}
	for _, key := range []string{"frozen:a", "decay:b"} {
		if raw, _ := s.GetProxyState(key); raw == "" {
			t.Errorf("empty prefix deleted %s", key)
		}
	}
}

// los26eCountProxyStateRows counts rows by exact key: GetProxyState returns ""
// for a deleted row and for an emptied one, so it cannot prove a DELETE.
func (s *Store) los26eCountProxyStateRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := s.proxyStateDB().QueryRow(`SELECT count(*) FROM proxy_state WHERE key = ?`, key).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", key, err)
	}
	return n
}
