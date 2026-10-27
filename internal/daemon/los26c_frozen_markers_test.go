package daemon

import "testing"

// Regression tests for Los 26c (YM-315, PRX-P37), finding 1: the proxy
// invalidates a frozen prefix by writing an empty value into proxy_state. The
// archive ranges that prefix advertised (the [Archiv: Messages a-b] block) were
// dropped with it, so get_compacted_stubs answered with an empty list even
// though the archived messages were still in messages.db.

const los26cFrozenWithArchive = `{
	"messages": [
		{"role": "user", "content": "-"},
		{"role": "user", "content": "[Archiv: Messages 1-725 (725 msgs) — get_compacted_stubs('t-inv', 1, 725) zum Reinzoomen]\nTools: bash(238), read(57)"}
	],
	"cutoff": 726,
	"boundary_hash": "abc",
	"prefix_hash": "def",
	"tokens": 45000,
	"raw_tokens": 120000
}`

func TestLos26c_InvalidationKeepsArchiveMarkers(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("frozen:t-inv", los26cFrozenWithArchive); err != nil {
		t.Fatalf("seed frozen row: %v", err)
	}

	// The invalidation signal of FrozenStubs.Invalidate.
	resp := h.handleSetProxyState(map[string]any{"key": "frozen:t-inv", "value": ""})
	if resp.Error != "" {
		t.Fatalf("invalidate: %s", resp.Error)
	}

	// The stored value must be empty: the lazy DB load refuses it, so no process
	// can resurrect the prefix. The row itself stays until the janitor or the
	// next freeze overwrites it, which is what keeps the invalidation cheap.
	if raw, err := store.GetProxyState("frozen:t-inv"); err != nil || raw != "" {
		t.Fatalf("frozen row still carries a prefix after the invalidation: %q (err=%v)", raw, err)
	}

	resp = h.handleGetCompactedStubs(map[string]any{"thread_id": "t-inv"})
	if resp.Error != "" {
		t.Fatalf("get_compacted_stubs: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) == 0 {
		t.Fatal("get_compacted_stubs returned no archive markers after the invalidation")
	}
	block, _ := blocks[0].(map[string]any)
	if start, _ := block["start_idx"].(float64); start != 1 {
		t.Fatalf("start_idx = %v, want 1", block["start_idx"])
	}
	if end, _ := block["end_idx"].(float64); end != 725 {
		t.Fatalf("end_idx = %v, want 725", block["end_idx"])
	}
	if content, _ := block["content"].(string); content == "" {
		t.Fatal("marker block carries no content")
	}
}

// TestLos26c_NewFreezeDropsStaleMarkers pins the counterpart of the capture: a
// later prefix supersedes the ranges of the invalidated one, so the record must
// not shadow a live prefix or the legacy block table until the janitor runs.
func TestLos26c_NewFreezeDropsStaleMarkers(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("frozen:t-refreeze", los26cFrozenWithArchive); err != nil {
		t.Fatalf("seed frozen row: %v", err)
	}
	if resp := h.handleSetProxyState(map[string]any{"key": "frozen:t-refreeze", "value": ""}); resp.Error != "" {
		t.Fatalf("invalidate: %s", resp.Error)
	}
	if raw, _ := store.GetProxyState("frozen_markers:t-refreeze"); raw == "" {
		t.Fatal("test premise broken: the invalidation captured no markers")
	}

	// The next freeze writes a fresh prefix without an archive range.
	fresh := `{"messages": [{"role": "user", "content": "new stub"}], "cutoff": 4, "prefix_hash": "x", "tokens": 120}`
	if resp := h.handleSetProxyState(map[string]any{"key": "frozen:t-refreeze", "value": fresh}); resp.Error != "" {
		t.Fatalf("refreeze: %s", resp.Error)
	}

	if raw, _ := store.GetProxyState("frozen_markers:t-refreeze"); raw != "" {
		t.Fatalf("stale marker record survived a new freeze: %q", raw)
	}
	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-refreeze"})
	if blocks := resultSlice(t, resp); len(blocks) != 0 {
		t.Fatalf("get_compacted_stubs served %d stale markers after the freeze", len(blocks))
	}
}

// TestLos26c_MarkerScanIgnoresToolInput pins the boundary of the raw scan: an
// archive marker quoted inside a tool_use input is not an archive range. A plain
// byte scan reported it, and get_compacted_stubs then offered a range that never
// existed. The Write shape matters too: its input carries a "content" field, so
// anchoring on the key name alone is not enough.
func TestLos26c_MarkerScanIgnoresToolInput(t *testing.T) {
	payload := `{
		"messages": [
			{"role": "assistant", "content": [{"type": "tool_use", "name": "read", "input": {"command": "grep '[Archiv: Messages 5-9 (5 msgs)' log"}}]},
			{"role": "assistant", "content": [{"type": "tool_use", "name": "write", "input": {"content": "[Archiv: Messages 40-60 (21 msgs) — example from the docs]"}}]},
			{"role": "user", "content": "[Archiv: Messages 1-3 (3 msgs) — get_compacted_stubs('t-tool', 1, 3) zum Reinzoomen]"}
		],
		"cutoff": 4
	}`
	h, store := mustHandler(t)
	if err := store.SetProxyState("frozen:t-tool", payload); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if resp := h.handleSetProxyState(map[string]any{"key": "frozen:t-tool", "value": ""}); resp.Error != "" {
		t.Fatalf("invalidate: %s", resp.Error)
	}

	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-tool"})
	blocks := resultSlice(t, resp)
	if len(blocks) != 1 {
		t.Fatalf("markers = %d, want 1 (only the real archive block counts, not the quoted ones in tool inputs)", len(blocks))
	}
	block, _ := blocks[0].(map[string]any)
	if start, _ := block["start_idx"].(float64); start != 1 {
		t.Fatalf("start_idx = %v, want 1", block["start_idx"])
	}
}

// TestLos26c_InvalidationWithoutMarkersDropsRow keeps the tombstone cheap: a
// prefix that never advertised an archive range must not leave a second row
// behind just because it was invalidated.
func TestLos26c_InvalidationWithoutMarkersDropsRow(t *testing.T) {
	h, store := mustHandler(t)
	plain := `{"messages": [{"role": "user", "content": "stub"}], "cutoff": 3, "prefix_hash": "def", "tokens": 100}`
	if err := store.SetProxyState("frozen:t-plain", plain); err != nil {
		t.Fatalf("seed frozen row: %v", err)
	}

	if resp := h.handleSetProxyState(map[string]any{"key": "frozen:t-plain", "value": ""}); resp.Error != "" {
		t.Fatalf("invalidate: %s", resp.Error)
	}

	if raw, _ := store.GetProxyState("frozen_markers:t-plain"); raw != "" {
		t.Fatalf("invalidation of a marker-free prefix left a marker row behind: %q", raw)
	}
	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-plain"})
	if blocks := resultSlice(t, resp); len(blocks) != 0 {
		t.Fatalf("get_compacted_stubs invented %d blocks for a marker-free thread", len(blocks))
	}
}
