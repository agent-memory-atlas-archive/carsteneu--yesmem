package storage

import (
	"testing"
	"time"
)

// YM-280: opencode and codex have no SessionEnd hook, so their attributed session
// pins are never cleared. Age is the only bound that reaches them.
//
// YM-325 (6): the same bound now covers the unattributed project-less rows. They
// are global (they show up in every briefing) and no event can ever reach them: no
// session id to match a /clear, no project to match a project-scoped /clear. Age is
// their only cleanup path, and a session pin is session-scoped by definition.
// Project-bound unattributed rows keep their own path: /clear removes them.
func TestPurgeSessionPinsBefore_BoundsEveryUnreachablePin(t *testing.T) {
	s := mustOpenFiles(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	fresh := time.Now().Add(-1 * time.Hour)

	seedPin(t, s, "session", "", "sess-a", "old attributed project-less", old)
	seedPin(t, s, "session", los28cProject, "sess-a", "old attributed project pin", old)
	seedPin(t, s, "session", los28cProject, "sess-a", "fresh attributed project pin", fresh)
	seedPin(t, s, "session", "", "", "old unattributed project-less", old)
	seedPin(t, s, "session", "", "", "fresh unattributed project-less", fresh)
	seedPin(t, s, "session", los28cProject, "", "old unattributed project pin", old)
	seedPin(t, s, "permanent", "", "sess-a", "old permanent pin", old)

	removed, err := s.PurgeSessionPinsBefore(time.Now().Add(-SessionPinMaxAge))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (two attributed pins and the unreachable project-less row)", removed)
	}

	got := pinContents(t, s, "session", los28cProject)
	if pinHasContent(got, "old attributed project pin") || pinHasContent(got, "old attributed project-less") {
		t.Errorf("expired attributed pins must be purged, got: %v", got)
	}
	if pinHasContent(got, "old unattributed project-less") {
		t.Errorf("a project-less unattributed pin has no other cleanup path and must expire, got: %v", got)
	}
	if !pinHasContent(got, "fresh attributed project pin") {
		t.Errorf("fresh attributed pin must survive, got: %v", got)
	}
	if !pinHasContent(got, "fresh unattributed project-less") {
		t.Errorf("fresh unattributed project-less pin must survive, got: %v", got)
	}
	if !pinHasContent(got, "old unattributed project pin") {
		t.Errorf("project-bound unattributed pins keep dying with /clear, not by age, got: %v", got)
	}
	permanent := pinContents(t, s, "permanent", los28cProject)
	if !pinHasContent(permanent, "old permanent pin") {
		t.Errorf("permanent pins must never be purged, got: %v", permanent)
	}
}
