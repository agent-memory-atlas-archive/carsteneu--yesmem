package storage

import (
	"database/sql"
	"testing"
	"time"
)

// Rows damaged before the fix carry the cleanup reason but a user-stated source.
// The finder must report exactly those, and the restore must reactivate nothing else.
func TestRestoreWronglySupersededShortSessionLearnings(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-old", 1, 500*time.Hour)
	damagedUser := insertSessionLearning(t, s, "sess-old", "user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	damagedAgreed := insertSessionLearning(t, s, "sess-old", "agreed_upon", "Cap frontmatter version is a dedup key")
	legitAuto := insertSessionLearning(t, s, "sess-old", "llm_extracted", "Auto-extracted noise from a trivial session")
	junkUser := insertSessionLearning(t, s, "sess-old", "user_stated", "Junk cleanup victim, not a length victim")
	damagedOverride := insertSessionLearning(t, s, "sess-old", "user_override", "Forced trait carried by the old cleanup")

	// A learning may carry no session at all (learnings.session_id is nullable);
	// the finder must still report it instead of failing on the scan.
	res, err := s.DB().Exec(
		`INSERT INTO learnings (category, content, created_at, model_used, source,
			superseded_by, supersede_reason, valid_until)
		 VALUES ('decision', 'Session-less user statement', '2026-10-01T00:00:00+02:00', 'self',
			'user_stated', -1, ?, datetime('now'))`, ShortSessionSupersedeReason)
	if err != nil {
		t.Fatalf("seed session-less learning: %v", err)
	}
	sessionless, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	// Historical damage: written by the old cleanup, before the fix existed.
	for _, id := range []int64{damagedUser, damagedAgreed, damagedOverride, legitAuto} {
		if _, err := s.DB().Exec(
			`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now') WHERE id = ?`,
			ShortSessionSupersedeReason, id,
		); err != nil {
			t.Fatalf("seed historical supersede on %d: %v", id, err)
		}
	}
	// One damaged row also carries the retired confirmation status; the restore
	// must hand back a row that looks active in every column.
	if _, err := s.DB().Exec(
		`UPDATE learnings SET supersede_status = 'pending_confirmation' WHERE id = ?`, damagedOverride,
	); err != nil {
		t.Fatalf("seed retired status: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE learnings SET superseded_by = -1, supersede_reason = 'junk cleanup', valid_until = datetime('now') WHERE id = ?`,
		junkUser,
	); err != nil {
		t.Fatalf("seed junk supersede: %v", err)
	}

	found, err := s.FindWronglySupersededShortSessionLearnings(0)
	if err != nil {
		t.Fatalf("find wrongly superseded: %v", err)
	}
	if len(found) != 4 {
		t.Fatalf("expected the 4 user-sourced victims, got %d: %+v", len(found), found)
	}
	seen := map[int64]bool{}
	for _, l := range found {
		seen[l.ID] = true
	}
	if !seen[damagedUser] || !seen[damagedAgreed] || !seen[damagedOverride] || !seen[sessionless] {
		t.Errorf("finder returned wrong rows: %+v (want %d, %d, %d and %d)",
			seen, damagedUser, damagedAgreed, damagedOverride, sessionless)
	}

	// Restore is handed the reviewed ids; it must refuse the auto-extracted and
	// the junk-cleanup row even though they are listed here.
	restored, err := s.RestoreWronglySupersededShortSessionLearnings(
		[]int64{damagedUser, damagedAgreed, damagedOverride, sessionless, legitAuto, junkUser})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 4 {
		t.Errorf("restored = %d, want 4", restored)
	}
	requireActive(t, s, damagedUser, "restored user_stated learning")
	requireActive(t, s, damagedAgreed, "restored agreed_upon learning")
	requireActive(t, s, damagedOverride, "restored user_override learning")
	requireActive(t, s, sessionless, "restored session-less learning")
	requireSuperseded(t, s, legitAuto, "auto-extracted learning")
	requireSuperseded(t, s, junkUser, "junk-cleanup learning")

	st := readSupersedeState(t, s, damagedUser)
	if st.reason.Valid || st.validUntil.Valid {
		t.Errorf("restored row must clear reason and valid_until, got reason=%v valid_until=%v",
			st.reason, st.validUntil)
	}
	var status sql.NullString
	if err := s.DB().QueryRow(
		`SELECT supersede_status FROM learnings WHERE id = ?`, damagedOverride,
	).Scan(&status); err != nil {
		t.Fatalf("read supersede_status: %v", err)
	}
	if status.Valid {
		t.Errorf("restored row must clear the retired supersede_status, got %q", status.String)
	}

	// A second run must find nothing left to restore.
	again, err := s.FindWronglySupersededShortSessionLearnings(0)
	if err != nil {
		t.Fatalf("find after restore: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("expected no remaining victims after restore, got %d", len(again))
	}
}
