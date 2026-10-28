package storage

import "testing"

// YM-261 (Los 11d): the unfinished search never looked at quarantined_at, so an
// item from a quarantined session stayed searchable and could be resolved by
// text or by a commit message. Quarantine is the user's signal that a session's
// learnings must not act, so both the candidate list and the resolve decision
// have to skip them.

func quarantineTestLearning(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE learnings SET quarantined_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		t.Fatalf("quarantine #%d: %v", id, err)
	}
}

func TestSearchUnfinishedRanked_Los11d_SkipsQuarantinedItems(t *testing.T) {
	s := mustOpen(t)
	quarantinedID := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")
	quarantineTestLearning(t, s, quarantinedID)

	got, err := s.SearchUnfinishedRanked("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("quarantined items must not be candidates, got %+v", got.Items)
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatalf("quarantined item #%d must never be resolved automatically", quarantinedID)
	}
}

func TestSearchUnfinished_Los11d_DisplayListSkipsQuarantinedItems(t *testing.T) {
	s := mustOpen(t)
	quarantinedID := insertTestLearningForProject(t, s, "proxy rules cache cleanup", "unfinished", "yesmem", "yesmem")
	quarantineTestLearning(t, s, quarantinedID)
	activeID := insertTestLearningForProject(t, s, "proxy rules cache documentation", "unfinished", "yesmem", "yesmem")

	items, err := s.SearchUnfinished("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinished: %v", err)
	}
	if len(items) != 1 || items[0].ID != activeID {
		t.Fatalf("expected only the active item #%d, got %+v", activeID, items)
	}
}

// A quarantined item must not block the resolution of an unambiguous active one:
// it simply does not take part in the ranking.
func TestSearchUnfinishedRanked_Los11d_QuarantinedTwinDoesNotCreateAmbiguity(t *testing.T) {
	s := mustOpen(t)
	quarantinedID := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")
	quarantineTestLearning(t, s, quarantinedID)
	targetID := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	target, ok := got.AutoResolveTarget()
	if !ok {
		t.Fatalf("expected the active item to be resolvable, skip reason: %q", got.SkipReason())
	}
	if target.ID != targetID {
		t.Fatalf("auto-resolve picked #%d, want #%d", target.ID, targetID)
	}
}
