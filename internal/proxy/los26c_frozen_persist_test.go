package proxy

import (
	"encoding/json"
	"testing"
)

// Regression tests for Los 26c (YM-315, PRX-P37): rest points around the frozen
// stub prefix. Names and helpers carry the los26c prefix so parallel loops in
// this package cannot collide on a name.

// TestLos26c_PersistKeepsRawTokens pins finding 7 of YM-315. Store and
// UpdateMessages write raw_tokens into the persisted payload, Persist did not.
// The re-persist path (ensureFrozenPersisted after a failed Store, e.g. during a
// daemon deploy) therefore replaced a complete row with one whose raw token
// estimate was zero, and that row is the one a cold start loads.
func TestLos26c_PersistKeepsRawTokens(t *testing.T) {
	f := NewFrozenStubs()
	persisted := map[string]string{}
	f.SetPersistFunc(func(key, value string) { persisted[key] = value })

	stub := []any{map[string]any{"role": "user", "content": "frozen stub"}}
	boundary := map[string]any{"role": "user", "content": "boundary"}
	f.Store("los26c-raw", stub, 5, boundary, 1200, 48000)

	// Store wrote a complete row; drop it so only Persist can supply the payload.
	delete(persisted, "frozen:los26c-raw")
	f.Persist("los26c-raw")

	raw, ok := persisted["frozen:los26c-raw"]
	if !ok || raw == "" {
		t.Fatal("Persist wrote no row")
	}
	var fp frozenPersisted
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		t.Fatalf("unmarshal persisted row: %v", err)
	}
	if fp.RawTokens != 48000 {
		t.Fatalf("persisted raw_tokens = %d, want 48000 (Persist drops the estimate Store recorded)", fp.RawTokens)
	}
	if fp.Tokens != 1200 {
		t.Fatalf("persisted tokens = %d, want 1200", fp.Tokens)
	}
}
