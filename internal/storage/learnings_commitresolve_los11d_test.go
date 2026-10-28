package storage

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// YM-260 (Los 11d): the commit hook, the CLI and the v2 plugin resolved the
// first hit of an OR search with no threshold at all. Los 11c showed the exact
// damage (learning #66092).
//
// Two rules were available: a graded word-coverage fallback or an explicit
// anchor in the commit subject. The fallback was implemented and measured
// against the 150 most recent subjects of this repository: it resolved twice and
// both resolutions were wrong. So the tests below pin the anchor rule: a commit
// closes an open task only when its subject names that task by learning id or by
// ticket key, and never on prose overlap alone.

// los11dCommitSubject is the subject that closed #66092 by mistake.
const los11dCommitSubject = "Los 11: Proxy Rules-Cache und Injektionszähler"

// los11dWrongTarget reproduces the wording of #66092: it shares "proxy" with the
// subject above and nothing else.
const los11dWrongTarget = "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation"

func openTask(t *testing.T, s *Store, content string) int64 {
	t.Helper()
	return insertTestLearningForProject(t, s, content, "unfinished", "yesmem", "yesmem")
}

func TestResolveTargetForCommit_Los11d_LiveCaseStaysOpen(t *testing.T) {
	s := mustOpen(t)
	wrongID := openTask(t, s, los11dWrongTarget)

	got, err := s.ResolveTargetForCommit(los11dCommitSubject, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("the live case must not resolve #%d", wrongID)
	}
	// No reference in the subject: the hook stays silent instead of printing a
	// line into every commit, so an empty reason is part of the contract.
	if got.Reason != "" {
		t.Errorf("expected no reason for a subject without a reference, got %q", got.Reason)
	}
}

// Even a subject that covers an open item word for word is no resolve basis:
// prose overlap was measured to be wrong more often than right.
func TestResolveTargetForCommit_Los11d_ProseOverlapAloneResolvesNothing(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Los 11: Proxy Rules-Cache und Injektionszähler reparieren")

	got, err := s.ResolveTargetForCommit(los11dCommitSubject, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("a subject without an anchor must not close #%d", id)
	}
}

// The learning id is an anchor, but "#123" also means "issue 123" in this
// repository's history. A real learning reference shares words with the work it
// closes; a foreign number reference shares none.
func TestResolveTargetForCommit_Los11d_LearningIDAnchorsAMatchingItem(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Rebuild the proxy cache ruleset")
	openTask(t, s, "document the release notes")

	got, err := s.ResolveTargetForCommit("fix(proxy): rebuild ruleset (#"+strconv.FormatInt(targetID, 10)+")", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil {
		t.Fatalf("expected the named learning to resolve, reason: %q", got.Reason)
	}
	if got.Target.ID != targetID {
		t.Fatalf("resolved #%d, want #%d", got.Target.ID, targetID)
	}
}

func TestResolveTargetForCommit_Los11d_ForeignNumberReferenceIsNoAnchor(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Scheduled cleanup of the archive directory")

	got, err := s.ResolveTargetForCommit("feat(proxy): cache rules (#"+strconv.FormatInt(targetID, 10)+")", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("a number reference without shared words must not close #%d", got.Target.ID)
	}
}

// One shared word is exactly the #66092 accident, so an id anchor needs two.
func TestResolveTargetForCommit_Los11d_LearningIDNeedsTwoSharedTerms(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, los11dWrongTarget)

	got, err := s.ResolveTargetForCommit("fix(ci): tune proxy limits (#"+strconv.FormatInt(id, 10)+")", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("one shared word must not anchor #%d", id)
	}
	if !strings.Contains(got.Reason, "shares only 1") {
		t.Errorf("expected a truthful overlap count, got %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "resolve(learning_id=") {
		t.Errorf("expected the reason to point at resolve(learning_id), got %q", got.Reason)
	}
}

// A unique id anchor decides even when another key in the subject is ambiguous.
func TestResolveTargetForCommit_Los11d_UniqueIDBeatsAConflictingKey(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Rebuild the proxy cache ruleset")
	openTask(t, s, "YM-261 part one")
	openTask(t, s, "YM-261 part two")

	got, err := s.ResolveTargetForCommit(
		"fix(proxy): rebuild cache (#"+strconv.FormatInt(targetID, 10)+", YM-261)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil || got.Target.ID != targetID {
		t.Fatalf("the unique id anchor must decide, got %+v (reason %q)", got.Target, got.Reason)
	}
}

// Repeating one reference is not overflow, and it is not ambiguity.
func TestResolveTargetForCommit_Los11d_ManyRepeatsOfOneIDAreNoOverflow(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Rebuild the proxy cache ruleset")
	ref := "#" + strconv.FormatInt(id, 10)
	subject := "fix(proxy): rebuild cache " + strings.Repeat(ref+" ", 9)

	got, err := s.ResolveTargetForCommit(subject, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil || got.Target.ID != id {
		t.Fatalf("expected #%d to resolve, got %+v (reason %q)", id, got.Target, got.Reason)
	}
}

func TestResolveTargetForCommit_Los11d_TooManyDistinctKeysResolveNothing(t *testing.T) {
	s := mustOpen(t)
	openTask(t, s, "Tracked as PROBE-900 in the campaign")

	keys := make([]string, 0, maxAnchorsPerSubject+1)
	for i := 0; i < maxAnchorsPerSubject+1; i++ {
		keys = append(keys, fmt.Sprintf("PROBE-%d", 900+i))
	}
	got, err := s.ResolveTargetForCommit("chore: sweep ("+strings.Join(keys, ", ")+")", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("an over-long key list must not resolve, got #%d", got.Target.ID)
	}
	if !strings.Contains(got.Reason, "different ticket keys") {
		t.Errorf("expected the reason to name the key overflow, got %q", got.Reason)
	}
}

func TestResolveTargetForCommit_Los11d_UnknownLearningIDResolvesNothing(t *testing.T) {
	s := mustOpen(t)
	openTask(t, s, los11dWrongTarget)

	got, err := s.ResolveTargetForCommit(los11dCommitSubject+" (#999999)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("an unknown id must not borrow a resolution, got #%d", got.Target.ID)
	}
}

func TestResolveTargetForCommit_Los11d_ResolvedItemIsNoAnchor(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "something completely different")
	if err := s.ResolveLearning(id, "done"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got, err := s.ResolveTargetForCommit("chore: something different (#"+strconv.FormatInt(id, 10)+")", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("an already closed item must not be anchored, got #%d", got.Target.ID)
	}
}

func TestResolveTargetForCommit_Los11d_TicketKeyAnchorsTheItem(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Open item tracked as YM-260 in the campaign")

	got, err := s.ResolveTargetForCommit("fix(hooks): graded resolver (YM-260)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil {
		t.Fatalf("expected the item carrying the ticket key to resolve, reason: %q", got.Reason)
	}
	if got.Target.ID != targetID {
		t.Fatalf("resolved #%d, want #%d", got.Target.ID, targetID)
	}
}

// The key lookup runs in SQL, so no candidate window can hide the item: this one
// is the oldest of 506 open tasks.
func TestResolveTargetForCommit_Los11d_TicketKeyBeyondAnyCandidateWindow(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Release notes for the PROBE-9 campaign")
	for i := 0; i < 505; i++ {
		openTask(t, s, fmt.Sprintf("filler task number %d", i))
	}

	got, err := s.ResolveTargetForCommit("chore: finish (PROBE-9)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil || got.Target.ID != targetID {
		t.Fatalf("expected #%d outside any window, got %+v (reason %q)", targetID, got.Target, got.Reason)
	}
}

func TestResolveTargetForCommit_Los11d_SharedTicketKeyIsNoAnchor(t *testing.T) {
	s := mustOpen(t)
	openTask(t, s, "YM-261 part one")
	openTask(t, s, "YM-261 part two")

	got, err := s.ResolveTargetForCommit("fix(briefing): dismiss hint (YM-261)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("a ticket key carried by two items must not resolve either, got #%d", got.Target.ID)
	}
	if !strings.Contains(got.Reason, "YM-261") {
		t.Errorf("expected a reason naming the shared key, got %q", got.Reason)
	}
}

func TestResolveTargetForCommit_Los11d_TwoAnchorsResolveNothing(t *testing.T) {
	s := mustOpen(t)
	first := openTask(t, s, "Rebuild the proxy cache ruleset")
	second := openTask(t, s, "Rewrite the proxy cache loader")

	got, err := s.ResolveTargetForCommit(
		fmt.Sprintf("fix(proxy): rebuild cache (#%s, #%s)", strconv.FormatInt(first, 10), strconv.FormatInt(second, 10)),
		"yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("two named items must not resolve either, got #%d", got.Target.ID)
	}
	if len(got.Candidates) != 2 {
		t.Errorf("expected both named items as candidates, got %+v", got.Candidates)
	}
}

// Repeating the same reference is not ambiguity.
func TestResolveTargetForCommit_Los11d_RepeatedAnchorIsOneAnchor(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Rebuild the proxy cache ruleset")
	ref := "#" + strconv.FormatInt(id, 10)

	got, err := s.ResolveTargetForCommit("fix(proxy): rebuild cache "+ref+" "+ref, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil || got.Target.ID != id {
		t.Fatalf("expected #%d to resolve, got %+v (reason %q)", id, got.Target, got.Reason)
	}
}

// Standards tokens are not tickets: without the denylist an ordinary subject
// would anchor the single open item that mentions the same standard.
func TestResolveTargetForCommit_Los11d_StandardTokensAreNoAnchor(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Investigate why the log dump is truncated (UTF-8) on Windows")

	got, err := s.ResolveTargetForCommit("fix(text): encode UTF-8 safely", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("UTF-8 must not anchor #%d", id)
	}
}

// A ticket key carried by more items than the lookup window returns cannot be
// proven unique, so it must not anchor anything.
func TestResolveTargetForCommit_Los11d_FullKeyWindowIsNoAnchor(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "Tracked as PROBE-9 in the campaign")
	for i := 0; i < keyLookupLimit; i++ {
		openTask(t, s, fmt.Sprintf("PROBE-90 filler row %d", i))
	}

	got, err := s.ResolveTargetForCommit("chore: finish (PROBE-9)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("a full lookup window must not anchor #%d", targetID)
	}
	if !strings.Contains(got.Reason, "uniqueness cannot be proven") {
		t.Errorf("expected a reason naming the unprovable uniqueness, got %q", got.Reason)
	}
}

func TestResolveTargetForCommit_Los11d_RevertSubjectResolvesNothing(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Tracked as PROBE-9 in the campaign")

	got, err := s.ResolveTargetForCommit(`Revert "fix(hooks): graded resolver (PROBE-9)"`, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("a revert must not close #%d", id)
	}
	if !strings.Contains(got.Reason, "reverts") {
		t.Errorf("expected a revert reason, got %q", got.Reason)
	}
}

// Unknown scope cannot be guessed, so nothing is resolved.
func TestResolveTargetForCommit_Los11d_UnknownProjectResolvesNothing(t *testing.T) {
	s := mustOpen(t)
	openTask(t, s, "Tracked as PROBE-9 in the campaign")

	got, err := s.ResolveTargetForCommit("chore: finish (PROBE-9)", "")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("an unscoped call must not resolve anything, got #%d", got.Target.ID)
	}
}

// The body is prose. A ticket mentioned there must not compete with the subject.
func TestResolveTargetForCommit_Los11d_BodyDoesNotAnchor(t *testing.T) {
	s := mustOpen(t)
	openTask(t, s, "Follow-up work for YM-255 remains open")
	targetID := openTask(t, s, "graded resolver work in hooks, tracked as YM-260")

	msg := "fix(hooks): graded resolver work (YM-260)\n\nFollow-up: YM-255 stays open for the next loop."
	got, err := s.ResolveTargetForCommit(msg, "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target == nil || got.Target.ID != targetID {
		t.Fatalf("the subject must decide, got %+v (reason %q)", got.Target, got.Reason)
	}
}

// The first resolution closes the item, so a retried or amended commit cannot
// close a second one: the item is simply no longer open.
func TestResolveCommit_Los11d_RepeatResolvesNothing(t *testing.T) {
	s := mustOpen(t)
	targetID := openTask(t, s, "graded resolver work in hooks, tracked as YM-260")
	openTask(t, s, "another iterator task, tracked as YM-261")

	first, err := s.ResolveCommit("fix(hooks): graded resolver work (YM-260)", "yesmem")
	if err != nil {
		t.Fatalf("first ResolveCommit: %v", err)
	}
	if first.Target == nil || first.Target.ID != targetID {
		t.Fatalf("expected #%d to resolve, got %+v (reason %q)", targetID, first.Target, first.Reason)
	}
	if first.Recorded == "" {
		t.Error("expected the recorded supersede reason")
	}

	second, err := s.ResolveCommit("fix(hooks): graded resolver work (YM-260)", "yesmem")
	if err != nil {
		t.Fatalf("second ResolveCommit: %v", err)
	}
	if second.Target != nil {
		t.Fatalf("a retried commit must not close #%d", second.Target.ID)
	}
}

func TestResolveCommit_Los11d_TruncatedReasonStaysValidUTF8(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Release notes for the PROBE-9 campaign")

	// 190 ASCII bytes, then a multi-byte character straddling the 200-byte cut.
	subject := "chore: " + strings.Repeat("a", 192) + "ääää (PROBE-9)"
	decision, err := s.ResolveCommit(subject, "yesmem")
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if decision.Target == nil || decision.Target.ID != id {
		t.Fatalf("expected #%d to resolve, got %+v (reason %q)", id, decision.Target, decision.Reason)
	}

	item, err := s.GetLearning(id)
	if err != nil || item == nil {
		t.Fatalf("get learning: %v", err)
	}
	if !utf8.ValidString(item.SupersedeReason) {
		t.Errorf("supersede reason is not valid UTF-8: %q", item.SupersedeReason)
	}
	if len(item.SupersedeReason) > len(commitReasonPrefix)+commitReasonMaxLen {
		t.Errorf("supersede reason is longer than the cap: %d bytes", len(item.SupersedeReason))
	}
}

func TestTruncateRunes_Los11d_NeverSplitsACharacter(t *testing.T) {
	// Cut positions 1..4 land inside the second character of "ää".
	for cut := 1; cut <= 6; cut++ {
		got := truncateRunes("äää", cut)
		if !utf8.ValidString(got) {
			t.Errorf("truncateRunes(\"äää\", %d) = %q, not valid UTF-8", cut, got)
		}
		if len(got) > cut {
			t.Errorf("truncateRunes(\"äää\", %d) = %q, longer than the cap", cut, got)
		}
	}
	if got := truncateRunes("abcdef", 3); got != "abc" {
		t.Errorf("ascii truncation changed: %q", got)
	}
}

func TestResolveTargetForCommit_Los11d_IgnoresOtherProjects(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "Tracked as PROBE-9 in another project", "unfinished", "/other/project", "/other/project")

	got, err := s.ResolveTargetForCommit("chore: finish (PROBE-9)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("cross-project item must not resolve, got #%d", got.Target.ID)
	}
}

func TestResolveTargetForCommit_Los11d_QuarantinedItemIsNoTarget(t *testing.T) {
	s := mustOpen(t)
	id := openTask(t, s, "Tracked as PROBE-9 in the campaign")
	quarantineTestLearning(t, s, id)

	got, err := s.ResolveTargetForCommit("chore: finish (PROBE-9)", "yesmem")
	if err != nil {
		t.Fatalf("ResolveTargetForCommit: %v", err)
	}
	if got.Target != nil {
		t.Fatalf("quarantined item #%d must never be resolved by a commit", got.Target.ID)
	}
}

func TestContainsTokenFold_Los11d_RespectsUnicodeBoundaries(t *testing.T) {
	tests := []struct {
		text, key string
		want      bool
	}{
		{"closes YM-260 today", "YM-260", true},
		{"closes (YM-260)", "YM-260", true},
		{"closes ym-260 today", "YM-260", true},
		{"closes YM-2601 today", "YM-260", false},
		{"closes XYM-260 today", "YM-260", false},
		{"caféYM-260", "YM-260", false},
		{"中YM-260", "YM-260", false},
		{"café YM-260", "YM-260", true},
	}
	for _, tt := range tests {
		if got := containsTokenFold(tt.text, tt.key); got != tt.want {
			t.Errorf("containsTokenFold(%q, %q) = %v, want %v", tt.text, tt.key, got, tt.want)
		}
	}
}
