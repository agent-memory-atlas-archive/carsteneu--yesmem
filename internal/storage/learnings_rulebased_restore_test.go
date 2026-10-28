package storage

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// ruleBasedVictim inserts a learning and displaces it with the given reason.
// A displacerID of 0 writes the sentinel the substance-free retire uses.
func ruleBasedVictim(t *testing.T, s *Store, source, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category:  "decision",
		Content:   content,
		Source:    source,
		ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// displace writes the supersede edge a rule-based dedup leaves behind.
func displace(t *testing.T, s *Store, id, displacer int64, reason string) {
	t.Helper()
	if _, err := s.DB().Exec(
		`UPDATE learnings SET superseded_by = ?, supersede_reason = ?, valid_until = datetime('now') WHERE id = ?`,
		displacer, reason, id,
	); err != nil {
		t.Fatalf("displace %d: %v", id, err)
	}
}

// The finder must report exactly the rows a rule-based dedup displaced although
// the user owns them, and nothing else. The selection is deliberately narrow:
// only a reason that starts with "rule-based", only a real displacer that is
// still active, only a displacer the user does not own himself (a chain between
// two user-owned rows is a legitimate supersede, not a rule-based displacement),
// and only a row whose content survived.
func TestFindRuleBasedSupersededUserLearnings_SelectionIsConservative(t *testing.T) {
	s := mustOpen(t)

	activeDisplacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	userDisplacer := ruleBasedVictim(t, s, "user_stated", "a user statement that won")
	laterDisplacedDisplacer := ruleBasedVictim(t, s, "llm_extracted", "a winner that was itself replaced")

	userStated := ruleBasedVictim(t, s, "user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	agreedUpon := ruleBasedVictim(t, s, "agreed_upon", "Cap frontmatter version is a dedup key")
	embeddingDup := ruleBasedVictim(t, s, "user_stated", "Chunked extraction runs before chunking")
	displace(t, s, userStated, activeDisplacer, "rule-based: near-duplicate")
	displace(t, s, agreedUpon, activeDisplacer, "rule-based: near-duplicate")
	displace(t, s, embeddingDup, activeDisplacer, "rule-based: embedding near-duplicate")

	// Not the user's: the rule-based dedup is allowed to replace it.
	autoOwned := ruleBasedVictim(t, s, "llm_extracted", "machine-written near-duplicate loser")
	displace(t, s, autoOwned, activeDisplacer, "rule-based: near-duplicate")

	// Both ends user-owned: a legitimate supersede chain.
	userToUser := ruleBasedVictim(t, s, "user_stated", "an older user statement, replaced by a newer one")
	displace(t, s, userToUser, userDisplacer, "rule-based: near-duplicate")

	// The displacer is not active any more, so clearing the edge would resurrect a
	// row whose successor was itself retired.
	displace(t, s, laterDisplacedDisplacer, activeDisplacer, "rule-based: near-duplicate")
	orphanVictim := ruleBasedVictim(t, s, "user_stated", "victim of a displacer that was replaced")
	displace(t, s, orphanVictim, laterDisplacedDisplacer, "rule-based: near-duplicate")

	// Emptied content: nothing to bring back.
	emptyContent := ruleBasedVictim(t, s, "user_stated", "   ")
	displace(t, s, emptyContent, activeDisplacer, "rule-based: near-duplicate")

	// Another cleanup's reason: not this command's business.
	otherReason := ruleBasedVictim(t, s, "user_stated", "victim of the short-session cleanup")
	displace(t, s, otherReason, activeDisplacer, ShortSessionSupersedeReason)

	// No displacer at all (the substance-free retire writes 0).
	substanzlos := ruleBasedVictim(t, s, "user_stated", "a statement the dedup found substance-free")
	displace(t, s, substanzlos, 0, "rule-based: substanzlos")

	scan, err := s.FindRuleBasedSupersededUserLearnings(0)
	if err != nil {
		t.Fatalf("find rule-based superseded: %v", err)
	}

	want := map[int64]bool{userStated: true, agreedUpon: true, embeddingDup: true}
	if len(scan.Victims) != len(want) {
		t.Fatalf("finder reported %d victims, want %d: %+v", len(scan.Victims), len(want), scan.Victims)
	}
	for _, v := range scan.Victims {
		if !want[v.ID] {
			t.Errorf("finder reported #%d (%s), which is outside the damaged class", v.ID, v.Reason)
		}
		delete(want, v.ID)
	}
	for id := range want {
		t.Errorf("finder missed #%d", id)
	}

	// The report names the displacer and its source, so the operator can judge
	// what the row lost against what replaced it.
	for _, v := range scan.Victims {
		if v.DisplacerID != activeDisplacer {
			t.Errorf("#%d: displacer = %d, want %d", v.ID, v.DisplacerID, activeDisplacer)
		}
		if v.DisplacerSource != "llm_extracted" {
			t.Errorf("#%d: displacer source = %q, want llm_extracted", v.ID, v.DisplacerSource)
		}
		if v.Source == "" || v.CreatedAt.IsZero() {
			t.Errorf("#%d: report is incomplete: %+v", v.ID, v)
		}
	}

	// A legitimate chain between two user-owned rows is kept out of the victims
	// and reported separately, so the reason it was skipped stays visible.
	if scan.SkippedHighTrustDisplacer != 1 {
		t.Errorf("SkippedHighTrustDisplacer = %d, want 1 (the user-to-user chain)", scan.SkippedHighTrustDisplacer)
	}
	// Every other exclusion is counted too. A report that names the victims and
	// silently drops the rest reads as complete while it is not: the operator
	// decides on this list alone.
	if scan.SkippedInactiveDisplacer != 1 {
		t.Errorf("SkippedInactiveDisplacer = %d, want 1 (the displacer that was itself replaced)", scan.SkippedInactiveDisplacer)
	}
	if scan.SkippedEmptyContent != 1 {
		t.Errorf("SkippedEmptyContent = %d, want 1 (the emptied row)", scan.SkippedEmptyContent)
	}
	if scan.UnlistedByLimit != 0 {
		t.Errorf("UnlistedByLimit = %d, want 0 without a limit", scan.UnlistedByLimit)
	}

	// The no-successor retire is its own damaged class, not an exclusion: it is
	// listed, so an operator can decide on it, and it needs its own apply.
	if len(scan.NoDisplacer) != 1 || scan.NoDisplacer[0].ID != substanzlos {
		t.Errorf("NoDisplacer = %+v, want exactly #%d", scan.NoDisplacer, substanzlos)
	}
	if got := scan.Skipped(); got != 3 {
		t.Errorf("Skipped() = %d, want 3 — the no-displacer class is listed, not excluded", got)
	}

	// The classification in Go and the SQL re-check in the applier must select the
	// same rows. Restoring exactly the reported victims proves it: anything the
	// update refuses would show up as an unrestored remainder.
	ids := make([]int64, len(scan.Victims))
	for i, v := range scan.Victims {
		ids[i] = v.ID
	}
	restored, err := s.RestoreRuleBasedSupersededUserLearnings(ids)
	if err != nil {
		t.Fatalf("restore the reported victims: %v", err)
	}
	if restored != int64(len(ids)) {
		t.Errorf("restore cleared %d of %d reported victims — the finder and the applier disagree", restored, len(ids))
	}
}

// A limit truncates the listing but not the truth: the rows beyond it are
// counted, so a limited run cannot pass for a complete one.
func TestFindRuleBasedSupersededUserLearnings_CountsRowsBeyondTheLimit(t *testing.T) {
	s := mustOpen(t)

	displacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	for i, content := range []string{
		"first user statement that was deduped away",
		"second user statement that was deduped away",
		"third user statement that was deduped away",
	} {
		id := ruleBasedVictim(t, s, "user_stated", content)
		displace(t, s, id, displacer, "rule-based: near-duplicate "+string(rune('a'+i)))
	}

	scan, err := s.FindRuleBasedSupersededUserLearnings(2)
	if err != nil {
		t.Fatalf("find rule-based superseded: %v", err)
	}
	if len(scan.Victims) != 2 {
		t.Fatalf("victims = %d, want 2 (the limit)", len(scan.Victims))
	}
	if scan.UnlistedByLimit != 1 {
		t.Errorf("UnlistedByLimit = %d, want 1 — the row beyond the limit must be counted, not dropped", scan.UnlistedByLimit)
	}
	if got := scan.Skipped(); got != 1 {
		t.Errorf("Skipped() = %d, want 1", got)
	}
}

// The apply path clears the supersede edge for the ids it is given, and only for
// rows that still belong to the damaged class: an id the operator picked by hand
// cannot be turned into a general "reactivate anything" switch.
func TestRestoreRuleBasedSupersededUserLearnings_OnlyTouchesTheDamagedClass(t *testing.T) {
	s := mustOpen(t)

	displacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	victim := ruleBasedVictim(t, s, "user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	displace(t, s, victim, displacer, "rule-based: near-duplicate")
	if _, err := s.DB().Exec(`UPDATE learnings SET supersede_status = 'pending_confirmation' WHERE id = ?`, victim); err != nil {
		t.Fatalf("seed retired status: %v", err)
	}

	autoOwned := ruleBasedVictim(t, s, "llm_extracted", "machine-written near-duplicate loser")
	displace(t, s, autoOwned, displacer, "rule-based: near-duplicate")

	restored, err := s.RestoreRuleBasedSupersededUserLearnings([]int64{victim, autoOwned})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored %d rows, want 1 (the user-owned victim only)", restored)
	}

	after, err := s.GetLearning(victim)
	if err != nil || after == nil {
		t.Fatalf("GetLearning(%d): %v", victim, err)
	}
	if after.SupersededBy != nil || after.SupersedeReason != "" || after.ValidUntil != nil {
		t.Errorf("restored row still carries the supersede edge: by=%v reason=%q valid_until=%v",
			after.SupersededBy, after.SupersedeReason, after.ValidUntil)
	}
	if after.Category != "decision" || after.Content == "" {
		t.Errorf("restore damaged the row: %+v", after)
	}

	stillSuperseded, err := s.GetLearning(autoOwned)
	if err != nil || stillSuperseded == nil {
		t.Fatalf("GetLearning(%d): %v", autoOwned, err)
	}
	if stillSuperseded.SupersededBy == nil || *stillSuperseded.SupersededBy == 0 {
		t.Errorf("restore reactivated a row the user does not own")
	}
}

// The opposite direction: an id the finder does not report must stay superseded,
// even when an operator passes it by hand. The SQL guard and the Go
// classification are two definitions of one condition; the test above only
// catches the case where the guard refuses a reported victim, never the case
// where it clears a row the report kept out.
func TestRestoreRuleBasedSupersededUserLearnings_RefusesRowsTheFinderDoesNotReport(t *testing.T) {
	s := mustOpen(t)

	activeDisplacer := ruleBasedVictim(t, s, "llm_extracted", "active machine winner")
	userDisplacer := ruleBasedVictim(t, s, "user_stated", "active user winner")
	doomed := ruleBasedVictim(t, s, "llm_extracted", "a winner that was itself replaced")

	highTrust := ruleBasedVictim(t, s, "user_stated", "kept out: the displacer is user-owned")
	displace(t, s, highTrust, userDisplacer, "rule-based: near-duplicate")

	inactive := ruleBasedVictim(t, s, "user_stated", "kept out: the displacer was replaced")
	displace(t, s, doomed, activeDisplacer, "rule-based: near-duplicate")
	displace(t, s, inactive, doomed, "rule-based: near-duplicate")

	noDisplacer := ruleBasedVictim(t, s, "user_stated", "kept out: retired as substance-free")
	displace(t, s, noDisplacer, 0, "rule-based: substanzlos")

	emptied := ruleBasedVictim(t, s, "user_stated", " \t\n ")
	displace(t, s, emptied, activeDisplacer, "rule-based: near-duplicate")

	scan, err := s.FindRuleBasedSupersededUserLearnings(0)
	if err != nil {
		t.Fatalf("find rule-based superseded: %v", err)
	}
	if len(scan.Victims) != 0 {
		t.Fatalf("the fixture must hold no candidates, found %+v", scan.Victims)
	}
	if len(scan.NoDisplacer) != 1 || scan.NoDisplacer[0].ID != noDisplacer {
		t.Fatalf("NoDisplacer = %+v, want exactly #%d", scan.NoDisplacer, noDisplacer)
	}
	if scan.Skipped() != 3 {
		t.Fatalf("Skipped() = %d, want 3 (one per excluded class)", scan.Skipped())
	}

	// The displacer applier clears none of them — including the no-displacer row,
	// which has its own apply and stays retired here.
	restored, err := s.RestoreRuleBasedSupersededUserLearnings(
		[]int64{highTrust, inactive, noDisplacer, emptied})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 0 {
		t.Errorf("restore cleared %d rows the finder never reported", restored)
	}
	for _, id := range []int64{highTrust, inactive, noDisplacer, emptied} {
		l, err := s.GetLearning(id)
		if err != nil || l == nil {
			t.Fatalf("GetLearning(%d): %v", id, err)
		}
		if l.SupersededBy == nil {
			t.Errorf("#%d was reactivated although the finder keeps it out of the candidate list", id)
		}
	}
}

// A second applied run finds nothing: the edges it cleared are gone, so the
// command is idempotent.
func TestRestoreRuleBasedSupersededUserLearnings_IsIdempotent(t *testing.T) {
	s := mustOpen(t)

	displacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	victim := ruleBasedVictim(t, s, "user_stated", "an agreed fact that was deduped away")
	displace(t, s, victim, displacer, "rule-based: near-duplicate")

	if _, err := s.RestoreRuleBasedSupersededUserLearnings([]int64{victim}); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	restored, err := s.RestoreRuleBasedSupersededUserLearnings([]int64{victim})
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if restored != 0 {
		t.Errorf("second restore touched %d rows, want 0", restored)
	}

	scan, err := s.FindRuleBasedSupersededUserLearnings(0)
	if err != nil {
		t.Fatalf("find after restore: %v", err)
	}
	if len(scan.Victims) != 0 {
		t.Errorf("finder still reports %d rows after the restore", len(scan.Victims))
	}
}

// TestFindRuleBasedSupersededUserLearnings_SeparatesTheNoDisplacerClass (YM-467):
// the substance-free retire leaves no successor, so its victims cannot be paired
// with a displacer. They are listed as their own class instead of being folded
// into a skipped counter: the dry run is the only surface an operator decides on.
// A row that is already active keeps a leftover reason at most; it is not
// superseded, so the class must not claim it.
func TestFindRuleBasedSupersededUserLearnings_SeparatesTheNoDisplacerClass(t *testing.T) {
	s := mustOpen(t)

	displacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	withDisplacer := ruleBasedVictim(t, s, "user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	displace(t, s, withDisplacer, displacer, "rule-based: near-duplicate")

	substanzlos := ruleBasedVictim(t, s, "user_stated", "Immer main")
	displace(t, s, substanzlos, 0, "rule-based: substanzlos")

	// A negative sentinel is a different record: a cleanup, a resolve or a
	// hard-deleted successor wrote it, so the reason still says why the row is
	// gone. Resurrecting it would repeat what the displacer branch refuses.
	deletedSuccessor := ruleBasedVictim(t, s, "user_stated", "a row whose winner was hard-deleted")
	displace(t, s, deletedSuccessor, -4, "rule-based: embedding near-duplicate")

	activeWithReason := ruleBasedVictim(t, s, "user_stated", "Kaum gesagt, schon wieder aktiv")
	if _, err := s.DB().Exec(
		`UPDATE learnings SET supersede_reason = 'rule-based: substanzlos' WHERE id = ?`, activeWithReason,
	); err != nil {
		t.Fatalf("seed leftover reason: %v", err)
	}

	// A limit is a listing bound per class, not a bound across both.
	scan, err := s.FindRuleBasedSupersededUserLearnings(1)
	if err != nil {
		t.Fatalf("find rule-based superseded: %v", err)
	}
	if len(scan.Victims) != 1 || scan.Victims[0].ID != withDisplacer {
		t.Errorf("Victims = %+v, want exactly #%d", scan.Victims, withDisplacer)
	}
	if len(scan.NoDisplacer) != 1 || scan.NoDisplacer[0].ID != substanzlos {
		t.Errorf("NoDisplacer = %+v, want exactly #%d", scan.NoDisplacer, substanzlos)
	}
	if scan.SkippedSentinel != 1 {
		t.Errorf("SkippedSentinel = %d, want 1 (the row whose successor was deleted)", scan.SkippedSentinel)
	}
	if scan.Skipped() != 1 {
		t.Errorf("Skipped() = %d, want 1 — only the sentinel row is excluded here", scan.Skipped())
	}
}

// TestRestoreNoDisplacerClass_OnlyOnItsOwnApply (YM-467): the second class needs
// a decision of its own. The displacer applier refuses its rows, its own applier
// refuses the displacer class, and both re-check the conservative conditions
// instead of trusting the operator's id list.
func TestRestoreRuleBasedSupersededUserLearnings_NoDisplacerClassNeedsItsOwnApply(t *testing.T) {
	s := mustOpen(t)

	displacer := ruleBasedVictim(t, s, "llm_extracted", "the near-duplicate that won")
	withDisplacer := ruleBasedVictim(t, s, "user_stated", "Pi-Bolt JSC: operand tables per codeblock")
	displace(t, s, withDisplacer, displacer, "rule-based: near-duplicate")

	substanzlos := ruleBasedVictim(t, s, "user_stated", "Immer main")
	displace(t, s, substanzlos, 0, "rule-based: substanzlos")
	if _, err := s.DB().Exec(`UPDATE learnings SET supersede_status = 'pending_confirmation' WHERE id = ?`, substanzlos); err != nil {
		t.Fatalf("seed retired status: %v", err)
	}

	// The machine-written twin: the substance-free retire is allowed to remove it.
	autoSubstanzlos := ruleBasedVictim(t, s, "llm_extracted", "Ja genau so")
	displace(t, s, autoSubstanzlos, 0, "rule-based: substanzlos")

	// A negative sentinel: the successor was hard-deleted, so the reason is the
	// only record of why the row is gone. Restoring it would resurrect a row whose
	// winner no longer exists.
	deletedSuccessor := ruleBasedVictim(t, s, "user_stated", "a row whose winner was hard-deleted")
	displace(t, s, deletedSuccessor, -4, "rule-based: embedding near-duplicate")

	// The displacer applier leaves the class alone, even when it is handed the ids.
	if cleared, err := s.RestoreRuleBasedSupersededUserLearnings([]int64{substanzlos}); err != nil {
		t.Fatalf("displacer applier: %v", err)
	} else if cleared != 0 {
		t.Errorf("displacer applier cleared %d no-displacer rows", cleared)
	}

	restored, err := s.RestoreRuleBasedSupersededUserLearningsNoDisplacer(
		[]int64{withDisplacer, substanzlos, autoSubstanzlos, deletedSuccessor})
	if err != nil {
		t.Fatalf("restore no-displacer: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored %d rows, want 1 (the user-owned substance-free row)", restored)
	}

	after, err := s.GetLearning(substanzlos)
	if err != nil || after == nil {
		t.Fatalf("GetLearning(%d): %v", substanzlos, err)
	}
	if after.SupersededBy != nil || after.SupersedeReason != "" || after.ValidUntil != nil || after.SupersedeStatus != "" {
		t.Errorf("restored row still carries the retire: by=%v reason=%q valid_until=%v status=%q",
			after.SupersededBy, after.SupersedeReason, after.ValidUntil, after.SupersedeStatus)
	}
	if after.Content != "Immer main" || after.Category != "decision" {
		t.Errorf("restore damaged the row: %+v", after)
	}

	for _, id := range []int64{withDisplacer, autoSubstanzlos, deletedSuccessor} {
		l, err := s.GetLearning(id)
		if err != nil || l == nil {
			t.Fatalf("GetLearning(%d): %v", id, err)
		}
		if l.SupersededBy == nil {
			t.Errorf("#%d was reactivated although it does not belong to the no-displacer class", id)
		}
	}

	// Idempotent: the cleared rows are no longer in the class.
	if again, err := s.RestoreRuleBasedSupersededUserLearningsNoDisplacer([]int64{substanzlos}); err != nil {
		t.Fatalf("second restore: %v", err)
	} else if again != 0 {
		t.Errorf("second restore touched %d rows, want 0", again)
	}

	scan, err := s.FindRuleBasedSupersededUserLearnings(0)
	if err != nil {
		t.Fatalf("find after restore: %v", err)
	}
	if len(scan.NoDisplacer) != 0 {
		t.Errorf("finder still lists %+v after the restore", scan.NoDisplacer)
	}
}
