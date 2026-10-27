package extraction

import (
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// insertTrustLearning inserts a learning and pins the trust inputs
// (importance, use_count) so TrustScore/ClassifyTrust are deterministic.
// InsertLearning itself never persists use_count.
func insertTrustLearning(t *testing.T, s *storage.Store, content, category, source string, importance, useCount int) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		SessionID: "test-session", Category: category, Content: content,
		Project: "testproj", Confidence: 1.0, CreatedAt: time.Now(),
		ModelUsed: "test", Source: source, Importance: importance,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", source, err)
	}
	if useCount > 0 {
		if _, err := s.DB().Exec("UPDATE learnings SET use_count = ? WHERE id = ?", useCount, id); err != nil {
			t.Fatalf("set use_count: %v", err)
		}
	}
	return id
}

func assertHighTrust(t *testing.T, s *storage.Store, id int64) {
	t.Helper()
	l, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("get learning %d: %v", id, err)
	}
	if got := storage.ClassifyTrust(storage.TrustScore(l)); got != storage.TrustHigh {
		t.Fatalf("test setup: learning #%d is %v, want TrustHigh", id, got)
	}
}

func assertNotSuperseded(t *testing.T, s *storage.Store, id int64) {
	t.Helper()
	l, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("get learning %d: %v", id, err)
	}
	if l.SupersededBy != nil {
		t.Fatalf("learning #%d must stay active, got superseded_by=%d", id, *l.SupersededBy)
	}
}

func assertRelationEdge(t *testing.T, s *storage.Store, sourceID, targetID int64, relType string) {
	t.Helper()
	assocs, err := s.GetAssociationsByRelationType("learning", strconv.FormatInt(sourceID, 10), relType)
	if err != nil {
		t.Fatalf("get %s associations for #%d: %v", relType, sourceID, err)
	}
	want := strconv.FormatInt(targetID, 10)
	for _, a := range assocs {
		if a.TargetID == want {
			return
		}
	}
	t.Fatalf("expected a %s edge #%d -> #%d", relType, sourceID, targetID)
}

func assertNoRelationEdge(t *testing.T, s *storage.Store, sourceID, targetID int64, relType string) {
	t.Helper()
	assocs, err := s.GetAssociationsByRelationType("learning", strconv.FormatInt(sourceID, 10), relType)
	if err != nil {
		return
	}
	want := strconv.FormatInt(targetID, 10)
	for _, a := range assocs {
		if a.TargetID == want {
			t.Fatalf("unexpected %s edge #%d -> #%d", relType, sourceID, targetID)
		}
	}
}

// TestApplyQualityRankedSupersedeProtectsHighTrust (YM-43): the evolution
// supersede path must not kill a high-trust learning. It stays active and the
// conflict is recorded as a contradicts edge. The retired pending_confirmation
// status must no longer be written — nothing ever read it.
func TestApplyQualityRankedSupersedeProtectsHighTrust(t *testing.T) {
	s := mustOpenStore(t)

	winnerID := insertTrustLearning(t, s, "Always run the regression baseline before merging", "decision", "user_stated", 5, 2)
	loserID := insertTrustLearning(t, s, "Run the regression baseline before every merge", "decision", "agreed_upon", 5, 1)
	assertHighTrust(t, s, winnerID)
	assertHighTrust(t, s, loserID)

	winner, err := s.GetLearning(winnerID)
	if err != nil {
		t.Fatalf("get winner: %v", err)
	}
	loser, err := s.GetLearning(loserID)
	if err != nil {
		t.Fatalf("get loser: %v", err)
	}

	applyQualityRankedSupersede(s, []models.Learning{*winner, *loser}, "evolution: same rule twice", nil)

	assertNotSuperseded(t, s, loserID)
	if after, err := s.GetLearning(loserID); err == nil && after.SupersedeStatus == "pending_confirmation" {
		t.Fatalf("pending_confirmation status must no longer be written (#%d)", loserID)
	}
	assertRelationEdge(t, s, loserID, winnerID, "contradicts")
}

// TestRuleBasedDedupProtectsHighTrustLoser (YM-45): the rule-based dedup must
// not supersede a high-trust learning. Before the fix it called
// store.SupersedeLearning directly, bypassing the trust guard that the
// evolution path applies. The pair is a near-duplicate, so it is linked as
// relates_to — labelling it a contradiction would inflate the contradiction
// count in the briefing health.
func TestRuleBasedDedupProtectsHighTrustLoser(t *testing.T) {
	s := mustOpenStore(t)

	const winnerContent = "always run the full regression baseline test before you merge into the main branch"
	const loserContent = "always run the full regression baseline test before you merge into the main tree"

	winnerID := insertTrustLearning(t, s, winnerContent, "decision", "user_stated", 5, 2)
	loserID := insertTrustLearning(t, s, loserContent, "decision", "agreed_upon", 5, 1)
	assertHighTrust(t, s, winnerID)
	assertHighTrust(t, s, loserID)

	// Precondition: the pair is a bigram near-duplicate above the 0.85 threshold
	// used by the rule-based dedup, otherwise the test would be vacuous.
	if sim := BigramJaccard(winnerContent, loserContent); sim <= 0.85 {
		t.Fatalf("test setup: bigram similarity %.3f must exceed 0.85", sim)
	}
	if winnerID >= loserID {
		t.Fatalf("test setup: winner #%d must have the lower ID than loser #%d", winnerID, loserID)
	}

	ext := NewExtractor(&mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
		return `{"actions":[]}`, nil
	}}, s)

	scope := map[string]map[string]struct{}{"testproj": {"decision": {}}}
	ext.RunEvolutionForScope(s, scope, nil)

	assertNotSuperseded(t, s, loserID)
	assertRelationEdge(t, s, loserID, winnerID, "relates_to")
	assertNoRelationEdge(t, s, loserID, winnerID, "contradicts")
}

// TestSubstanceFreeRetireSparesUserOwnedLearning (YM-467): the substance-free
// pass of the rule-based consolidation replaces a learning with no successor at
// all, so there is nothing to even out later — before the fix it did that to
// user-owned rows too (17 live rows). The guard is the source predicate the
// cleanups use, not the trust score: the user's own statement stays active even
// when it carries no weight (importance 1, never retrieved), while a
// machine-written row keeps being retired with "rule-based: substanzlos".
func TestSubstanceFreeRetireSparesUserOwnedLearning(t *testing.T) {
	s := mustOpenStore(t)

	// Both contents are substance-free by the rule the pass applies (under 15
	// runes), otherwise the test would never reach the path it is about.
	const userContent = "Immer main"
	const autoContent = "Ja genau so"
	for _, c := range []string{userContent, autoContent} {
		if !IsSubstanzlos(c) {
			t.Fatalf("test setup: %q must count as substance-free", c)
		}
	}

	userID := insertTrustLearning(t, s, userContent, "decision", "user_stated", 1, 0)
	autoID := insertTrustLearning(t, s, autoContent, "decision", "llm_extracted", 1, 0)

	RunConsolidation(s, nil, nil, ConsolidateConfig{MaxRounds: 1, RuleBasedOnly: true})

	assertNotSuperseded(t, s, userID)
	if after, err := s.GetLearning(userID); err != nil || after == nil {
		t.Fatalf("get user-owned learning: %v", err)
	} else if after.SupersedeReason != "" {
		t.Errorf("user-owned row kept the retire reason %q", after.SupersedeReason)
	}

	retired, err := s.GetLearning(autoID)
	if err != nil || retired == nil {
		t.Fatalf("get machine-written learning: %v", err)
	}
	if retired.SupersededBy == nil || *retired.SupersededBy != 0 {
		t.Errorf("machine-written row must stay retired with the 0 sentinel, got superseded_by=%v", retired.SupersededBy)
	}
	if retired.SupersedeReason != "rule-based: substanzlos" {
		t.Errorf("machine-written row reason = %q, want rule-based: substanzlos", retired.SupersedeReason)
	}
}

// TestConsolidationRuleBasedProtectsHighTrustLoser (YM-45): the daemon's
// automatic consolidation path runs its own rule-based dedup. It must honour
// the trust guard too — otherwise a high-trust learning is protected in the
// extraction path and destroyed in the other automatic path.
func TestConsolidationRuleBasedProtectsHighTrustLoser(t *testing.T) {
	s := mustOpenStore(t)

	const winnerContent = "always run the full regression baseline test before you merge into the main branch"
	const loserContent = "always run the full regression baseline test before you merge into the main tree"

	winnerID := insertTrustLearning(t, s, winnerContent, "decision", "user_stated", 5, 2)
	loserID := insertTrustLearning(t, s, loserContent, "decision", "agreed_upon", 5, 1)
	assertHighTrust(t, s, winnerID)
	assertHighTrust(t, s, loserID)
	if sim := BigramJaccard(winnerContent, loserContent); sim <= 0.85 {
		t.Fatalf("test setup: bigram similarity %.3f must exceed 0.85", sim)
	}

	RunConsolidation(s, nil, nil, ConsolidateConfig{MaxRounds: 1, RuleBasedOnly: true})

	assertNotSuperseded(t, s, loserID)
	assertRelationEdge(t, s, loserID, winnerID, "relates_to")
}
