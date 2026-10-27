package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-251 (Los 11c): resolve_by_text closed unrelated items because the
// unfinished search OR-joined every query word. These tests pin the new
// contract: a candidate only counts when it contains every query word, and
// only a single such candidate may be resolved automatically.

// los11cLiveQuery is the exact text the Los-11 agent used on 17.10.2026 when it
// resolved learning #66092 (a May proxy-health-check note) by mistake.
const los11cLiveQuery = "Los 11: Proxy Rules-Cache und Injektionszähler"

// los11cUnrelatedNote reproduces the wording of the item that was wrongly
// resolved: it shares only the word "proxy" (and the token "11" from a date)
// with the query above.
const los11cUnrelatedNote = "Scheduled job proxy-health-check (sched-proxy-health-check-54806) was triggered on 2026-05-11 but the session ends after cap activation"

func TestSearchUnfinishedRanked_Los11c_LiveCaseIsNotAFullMatch(t *testing.T) {
	s := newTestStore(t)
	unrelatedID := insertTestLearningForProject(t, s, los11cUnrelatedNote, "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked(los11cLiveQuery, "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if got == nil {
		t.Fatal("expected a result set, got nil")
	}
	if got.FullMatch() {
		t.Fatalf("the live case must not be a full match (matched %d of %d terms)", got.MatchedTerms, got.Terms)
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatalf("live case must not auto-resolve, but it selected #%d (%s)", unrelatedID, los11cUnrelatedNote)
	}
	if reason := got.SkipReason(); reason == "" {
		t.Error("expected a skip reason for a non-full match")
	}
}

func TestSearchUnfinishedRanked_Los11c_PicksTheItemWithEveryTerm(t *testing.T) {
	s := newTestStore(t)
	insertTestLearningForProject(t, s, los11cUnrelatedNote, "unfinished", "yesmem", "yesmem")
	targetID := insertTestLearningForProject(t, s, "Los 11: Proxy Rules-Cache und Injektionszähler reparieren", "unfinished", "yesmem", "yesmem")
	insertTestLearningForProject(t, s, "Add user profile page", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked(los11cLiveQuery, "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if !got.FullMatch() {
		t.Fatalf("expected a full match, got %d of %d terms", got.MatchedTerms, got.Terms)
	}
	if len(got.Items) != 1 {
		t.Fatalf("expected exactly 1 full-coverage candidate, got %d: %+v", len(got.Items), got.Items)
	}
	if got.Items[0].ID != targetID {
		t.Fatalf("expected target #%d, got #%d (%q)", targetID, got.Items[0].ID, got.Items[0].Content)
	}
	target, ok := got.AutoResolveTarget()
	if !ok {
		t.Fatalf("expected the unique full match to be resolvable, skip reason: %q", got.SkipReason())
	}
	if target.ID != targetID {
		t.Fatalf("auto-resolve picked #%d, want #%d", target.ID, targetID)
	}
	if reason := got.SkipReason(); reason != "" {
		t.Errorf("expected no skip reason for a unique full match, got %q", reason)
	}
}

func TestSearchUnfinishedRanked_Los11c_SpreadTermsAreNoFullMatch(t *testing.T) {
	s := newTestStore(t)
	insertTestLearningForProject(t, s, "proxy rules work", "unfinished", "yesmem", "yesmem")
	insertTestLearningForProject(t, s, "cache injektion work", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("proxy rules cache injektion", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if got.FullMatch() {
		t.Fatalf("terms spread over two items must not count as a full match")
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatal("spread terms must not auto-resolve anything")
	}
	if len(got.Items) == 0 {
		t.Error("expected relaxed candidates to still be reported")
	}
}

func TestSearchUnfinishedRanked_Los11c_AmbiguousFullMatchesResolveNothing(t *testing.T) {
	s := newTestStore(t)
	firstID := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")
	secondID := insertTestLearningForProject(t, s, "proxy rules cache document", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if !got.FullMatch() {
		t.Fatalf("expected both items to be full matches, got %d of %d", got.MatchedTerms, got.Terms)
	}
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got.Items))
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatalf("two full matches must not auto-resolve (#%d vs #%d)", firstID, secondID)
	}
	if reason := got.SkipReason(); reason == "" {
		t.Error("expected a skip reason naming the ambiguity")
	}
}

func TestSearchUnfinishedRanked_Los11c_BelowMinimumTermsResolvesNothing(t *testing.T) {
	s := newTestStore(t)
	id := insertTestLearningForProject(t, s, "proxy cache cleanup", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("proxy cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if !got.FullMatch() {
		t.Fatalf("the sole candidate contains both terms, expected a full match")
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatalf("a %d-term query must not auto-resolve #%d", got.Terms, id)
	}
	if reason := got.SkipReason(); reason == "" {
		t.Error("expected a skip reason about the minimum term count")
	}
}

func TestSearchUnfinishedRanked_Los11c_SkipsResolvedItems(t *testing.T) {
	s := newTestStore(t)
	id := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")
	if err := s.ResolveLearning(id, "done"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got, err := s.SearchUnfinishedRanked("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("resolved items must not be candidates, got %+v", got.Items)
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatal("resolved items must not be auto-resolved again")
	}
}

func TestSearchUnfinishedRanked_Los11c_PunctuationOnlyQueryDoesNotError(t *testing.T) {
	s := newTestStore(t)
	insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("?? --", "yesmem")
	if err != nil {
		t.Fatalf("punctuation-only query must not fail the FTS5 match: %v", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("expected no candidates, got %+v", got.Items)
	}
}

func TestSearchUnfinishedRanked_Los11c_LiveCaseReturnsCandidates(t *testing.T) {
	s := newTestStore(t)
	unrelatedID := insertTestLearningForProject(t, s, los11cUnrelatedNote, "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked(los11cLiveQuery, "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if len(got.Items) == 0 {
		t.Fatal("the live case must still list candidates, otherwise the caller gets nothing to resolve by id")
	}
	if got.Items[0].ID != unrelatedID {
		t.Fatalf("expected the shared-word candidate #%d, got #%d", unrelatedID, got.Items[0].ID)
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatal("a single-word fallback candidate must never be auto-resolved")
	}
}

func TestSearchUnfinishedRanked_Los11c_RepeatedWordsDoNotSatisfyTheMinimum(t *testing.T) {
	s := newTestStore(t)
	targetID := insertTestLearningForProject(t, s, "proxy cache cleanup", "unfinished", "yesmem", "yesmem")

	for _, query := range []string{"proxy proxy proxy", "proxy cache cache", "proxy, proxy. proxy!"} {
		got, err := s.SearchUnfinishedRanked(query, "yesmem")
		if err != nil {
			t.Fatalf("SearchUnfinishedRanked(%q): %v", query, err)
		}
		if got.Terms >= MinResolveTerms {
			t.Errorf("%q: repeated words must not reach the minimum, got Terms=%d", query, got.Terms)
		}
		if _, ok := got.AutoResolveTarget(); ok {
			t.Errorf("%q: repeated words must not auto-resolve #%d", query, targetID)
		}
		if reason := got.SkipReason(); reason == "" {
			t.Errorf("%q: expected a skip reason", query)
		}
	}
}

func TestSearchUnfinishedRanked_Los11c_FallbackSurvivesARareTermWithoutOpenTasks(t *testing.T) {
	s := newTestStore(t)
	candidateID := insertTestLearningForProject(t, s, "proxy cache cleanup", "unfinished", "yesmem", "yesmem")
	// The rare word only exists in a closed learning: every AND tier over it finds
	// no open task, which used to leave the caller with an empty candidate list.
	insertTestLearningForProject(t, s, "alpha rollout notes", "gotcha", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("alpha proxy", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if got.MatchedTerms != 0 {
		t.Fatalf("expected the single-word fallback, got MatchedTerms=%d", got.MatchedTerms)
	}
	if len(got.Items) != 1 || got.Items[0].ID != candidateID {
		t.Fatalf("fallback must list the shared-word candidate #%d, got %+v", candidateID, got.Items)
	}
	if got.FullMatch() {
		t.Error("fallback candidates must never count as a full match")
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatal("fallback candidates must never be auto-resolved")
	}
	if reason := got.SkipReason(); reason == "" {
		t.Error("expected a skip reason for the fallback")
	}
}

func TestSearchUnfinishedRanked_Los11c_LongTextNeverLooksComplete(t *testing.T) {
	s := newTestStore(t)
	insertTestLearningForProject(t, s, "alpha beta gamma delta epsilon zeta eta theta iota kappa", "unfinished", "yesmem", "yesmem")

	// Twelve distinct words: only the first ten are searched, so the query must not
	// count as fully matched.
	got, err := s.SearchUnfinishedRanked("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if got.Terms != 12 {
		t.Errorf("expected all 12 distinct words to be counted, got %d", got.Terms)
	}
	if got.FullMatch() {
		t.Error("a truncated query must not count as a full match")
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Error("a truncated query must not auto-resolve")
	}
	if reason := got.SkipReason(); !strings.Contains(reason, "only the first") {
		t.Errorf("expected the skip reason to explain the truncation, got %q", reason)
	}
}

func TestSearchUnfinishedRanked_Los11c_UnknownWordBlocksTheFullMatch(t *testing.T) {
	s := newTestStore(t)
	candidateID := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")

	got, err := s.SearchUnfinishedRanked("proxy rules cache zzzznotinindex", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if got.Terms != 4 || got.IndexedTerms != 3 {
		t.Fatalf("expected 4 terms with 3 indexed, got Terms=%d IndexedTerms=%d", got.Terms, got.IndexedTerms)
	}
	if got.FullMatch() {
		t.Error("a word that occurs nowhere must rule out a full match")
	}
	if _, ok := got.AutoResolveTarget(); ok {
		t.Fatalf("must not resolve #%d while the query references a word the knowledge base does not know", candidateID)
	}
	if reason := got.SkipReason(); !strings.Contains(reason, "occur in no learning") {
		t.Errorf("expected the skip reason to name the unknown word, got %q", reason)
	}
	if len(got.Items) != 1 || got.Items[0].ID != candidateID {
		t.Fatalf("the partial candidate must still be listed, got %+v", got.Items)
	}
}

func TestReopenLearning_Los11c_ResolutionBecomesActiveAgain(t *testing.T) {
	s := newTestStore(t)
	id := insertTestLearningForProject(t, s, "proxy rules cache rebuild", "unfinished", "yesmem", "yesmem")
	if err := s.ResolveLearning(id, "resolved by text match: whatever"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if err := s.ReopenLearning(id); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	l, err := s.GetLearning(id)
	if err != nil || l == nil {
		t.Fatalf("get learning: %v", err)
	}
	if l.SupersededBy != nil {
		t.Errorf("reopened item must be active again, got superseded_by=%d", *l.SupersededBy)
	}
	if l.SupersedeReason != "" {
		t.Errorf("reopened item must carry no supersede reason, got %q", l.SupersedeReason)
	}

	got, err := s.SearchUnfinishedRanked("proxy rules cache", "yesmem")
	if err != nil {
		t.Fatalf("SearchUnfinishedRanked: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != id {
		t.Fatalf("reopened item must be searchable again, got %+v", got.Items)
	}
}

func TestReopenLearning_Los11c_RefusesGenuineSupersedeChain(t *testing.T) {
	s := newTestStore(t)
	oldID := insertTestLearningForProject(t, s, "old proxy cache rule", "pattern", "yesmem", "yesmem")
	newID, err := s.InsertLearning(&models.Learning{
		Category: "pattern", Content: "new proxy cache rule", Project: "yesmem",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SupersedeLearning(oldID, newID, "replaced by newer rule"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if err := s.ReopenLearning(oldID); err == nil {
		t.Fatal("reopen must refuse an item that was superseded by another learning")
	}

	l, err := s.GetLearning(oldID)
	if err != nil || l == nil {
		t.Fatalf("get learning: %v", err)
	}
	if l.SupersededBy == nil || *l.SupersededBy != newID {
		t.Errorf("supersede link must stay intact, got %v", l.SupersededBy)
	}
}

func TestReopenLearning_Los11c_UnknownIDFails(t *testing.T) {
	s := newTestStore(t)
	if err := s.ReopenLearning(999999); err == nil {
		t.Fatal("expected an error for an unknown learning id")
	}
}
