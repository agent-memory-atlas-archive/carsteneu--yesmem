package daemon

import (
	"strings"
	"testing"
)

// YM-405 (Los 57d): store_compacted_block was the only writer of the
// compacted_blocks table, and nothing called it. The collapse path keeps its
// archive in the frozen prefix (proxy_state), so a write RPC into the legacy
// table only made the dead path look live. The table survives as a read-only
// legacy fallback for get_compacted_stubs and expand_context.
func TestStoreCompactedBlockRPCMustNotComeBack_Los57d(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.Handle(Request{
		Method: "store_compacted_block",
		Params: map[string]any{
			"thread_id": "t-legacy",
			"start_idx": float64(0),
			"end_idx":   float64(50),
			"content":   "archived summary",
		},
	})
	if !strings.Contains(resp.Error, "unknown method") {
		t.Fatalf("store_compacted_block is still dispatched (error %q); the write path into compacted_blocks is dead and must not be reachable", resp.Error)
	}
}
