package storage

import (
	"testing"
	"time"
)

// YM-317 Key 3: the old junk cleanup discarded user-owned learnings and left no
// tool able to find them again. The finder of the junk class must report exactly
// those rows, and the restore must reactivate nothing else.
func TestLos19c_JunkCleanupVictimsAreFindable(t *testing.T) {
	s := mustOpen(t)
	seedShortSession(t, s, "sess-lc-repair", 40, 72*time.Hour)

	victim := insertSessionLearning(t, s, "sess-lc-repair", "agreed_upon", "A rule the user agreed to before the source guard existed")
	mixedVictim := insertSessionLearning(t, s, "sess-lc-repair", "Agreed_Upon", "The same claim in mixed case")
	autoJunk := insertSessionLearning(t, s, "sess-lc-repair", "llm_extracted", "Auto-extracted junk from the same cleanup")
	shortReason := insertSessionLearning(t, s, "sess-lc-repair", "user_stated", "A short-session victim, not a junk victim")

	if _, err := s.DB().Exec(
		`UPDATE learnings SET superseded_by = -1, supersede_reason = 'junk cleanup', valid_until = datetime('now')
		 WHERE id IN (?, ?, ?)`, victim, mixedVictim, autoJunk,
	); err != nil {
		t.Fatalf("seed junk supersede: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now') WHERE id = ?`,
		ShortSessionSupersedeReason, shortReason,
	); err != nil {
		t.Fatalf("seed short-session supersede: %v", err)
	}

	found, err := s.FindWronglySupersededJunkLearnings(0)
	if err != nil {
		t.Fatalf("find wrongly superseded junk learnings: %v", err)
	}
	seen := map[int64]bool{}
	for _, l := range found {
		seen[l.ID] = true
	}
	if len(found) != 2 || !seen[victim] || !seen[mixedVictim] {
		t.Fatalf("finder reported %d rows (%v), want the 2 user-owned junk victims %d and %d",
			len(found), seen, victim, mixedVictim)
	}

	restored, err := s.RestoreWronglySupersededJunkLearnings([]int64{victim, mixedVictim, autoJunk, shortReason})
	if err != nil {
		t.Fatalf("restore wrongly superseded junk learnings: %v", err)
	}
	if restored != 2 {
		t.Errorf("restored = %d, want 2", restored)
	}
	requireActive(t, s, victim, "restored user-owned junk victim")
	requireActive(t, s, mixedVictim, "restored mixed-case user-owned junk victim")
	requireSuperseded(t, s, autoJunk, "auto-extracted junk row")
	requireSuperseded(t, s, shortReason, "short-session victim")

	again, err := s.FindWronglySupersededJunkLearnings(0)
	if err != nil {
		t.Fatalf("find after restore: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("expected no remaining junk victims after restore, got %d", len(again))
	}
}
