package daemon

import (
	"testing"
)

// YM-280 (COG-C24): session pins are keyed by the session that created them, so a
// /clear removes them wherever they were bound. The hook reports the raw cwd, and
// the pin tool lets the model pass any directory, so a project-string comparison
// would leave the pin behind as a ghost that is still visible in the briefing.
func TestHandleTrackSessionEndClearsOwnPinsAcrossProjectStrings(t *testing.T) {
	h, store := mustHandler(t)
	dir := "/home/testuser/projects/myapp/internal"
	if _, err := store.PinLearning("session", dir, "pin of the ending session", "test", "sess-b"); err != nil {
		t.Fatalf("pin b: %v", err)
	}
	if _, err := store.PinLearning("session", dir, "pin of a live sibling", "test", "sess-a"); err != nil {
		t.Fatalf("pin a: %v", err)
	}

	resp := h.handleTrackSessionEnd(map[string]any{
		"project":    "/home/testuser/projects/myapp",
		"session_id": "sess-b",
		"reason":     "clear",
	})
	if resp.Error != "" {
		t.Fatalf("track session end: %s", resp.Error)
	}

	pins, err := store.GetPinnedLearnings("session", dir)
	if err != nil {
		t.Fatalf("get pins: %v", err)
	}
	if len(pins) != 1 || pins[0].Content != "pin of a live sibling" {
		t.Errorf("want only the sibling's pin left, got %v", pins)
	}
}
