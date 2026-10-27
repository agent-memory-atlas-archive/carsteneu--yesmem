package mcp

import (
	"encoding/json"
	"testing"
)

// The daemon answers scratchpad_delete with {status, deleted_count}
// (internal/daemon/handler_scratchpad.go). The formatter read {deleted, message}
// instead, so every successful delete of three sections was reported as
// "✓ Deleted 0 entries" (YM-299).
func TestLos38jScratchpadDeleteReportsTheDeletedCount(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"section delete", `{"status":"deleted","deleted_count":3}`, "✓ Deleted 3 entries"},
		{"project delete", `{"status":"deleted","deleted_count":7}`, "✓ Deleted 7 entries"},
		// Deleting a section that does not exist is a success with nothing
		// removed; that zero is real and belongs in the answer.
		{"nothing to delete", `{"status":"deleted","deleted_count":0}`, "✓ Deleted 0 entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatScratchpadDelete(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("formatScratchpadDelete(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A response without the counter must not be rendered as a successful deletion
// of zero entries: that is exactly the failure this ticket fixed, and an invented
// zero would hide the next rename of the field.
func TestLos38jScratchpadDeleteDoesNotInventAZeroCount(t *testing.T) {
	raw := `{"status":"deleted"}`
	got := formatScratchpadDelete(json.RawMessage(raw))
	if got == "✓ Deleted 0 entries" {
		t.Fatalf("a response without deleted_count was reported as %q", got)
	}
	if got != raw {
		t.Fatalf("formatScratchpadDelete(%s) = %q, want the raw payload so the shape mismatch stays visible", raw, got)
	}
}
