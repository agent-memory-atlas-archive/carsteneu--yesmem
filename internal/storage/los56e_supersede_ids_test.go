package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos56eSupersedeReturnsExactlyTheRowsItChanged pins YM-367 (1): the cleanup
// paths collected their ids on the read pool and then updated on the write pool,
// so the set handed to the vector-store cleanup could differ from the set that
// actually changed. The returned ids are now the changed rows by construction
// (one UPDATE ... RETURNING), and the predicates still spare what they always
// spared.
func TestLos56eSupersedeReturnsExactlyTheRowsItChanged(t *testing.T) {
	s := mustOpen(t)

	insert := func(session, source, content, category string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: session, Category: category, Content: content,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test", Source: source,
		})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return id
	}

	extracted1 := insert("los56e-a", "llm_extracted", "first extracted fact of A", "gotcha")
	extracted2 := insert("los56e-a", "llm_extracted", "second extracted fact of A", "pattern")
	userOwned := insert("los56e-a", "user_stated", "the user said this about A", "preference")
	otherSession := insert("los56e-b", "llm_extracted", "extracted fact of B", "gotcha")
	narrative := insert("los56e-a", "llm_extracted", "narrative of A", "narrative")
	narrativeC := insert("los56e-c", "llm_extracted", "narrative of C", "narrative")

	ids, err := s.SupersedeSessionLearnings("los56e-a")
	if err != nil {
		t.Fatalf("supersede session learnings: %v", err)
	}
	// The narrative of the session belongs to the same replacement class, so the
	// reextract path reports it too. Compared as a set: SQLite makes no promise
	// about the row order an UPDATE ... RETURNING emits.
	want := map[int64]bool{extracted1: true, extracted2: true, narrative: true}
	if len(ids) != len(want) {
		t.Fatalf("returned ids = %v, want the extracted learnings and the narrative of A", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("returned ids = %v, want the extracted learnings and the narrative of A", ids)
		}
		delete(want, id)
	}

	// Every returned id must be superseded, and nothing else of A may be.
	superseded := map[int64]bool{}
	rows, err := s.readerDB().Query(`SELECT id FROM learnings WHERE session_id = 'los56e-a' AND superseded_by IS NOT NULL`)
	if err != nil {
		t.Fatalf("read superseded: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		superseded[id] = true
	}
	rows.Close()
	if !superseded[extracted1] || !superseded[extracted2] {
		t.Errorf("returned ids are not superseded: %v", superseded)
	}
	if superseded[userOwned] {
		t.Error("a user-stated learning must keep its active state")
	}
	if superseded[otherSession] {
		t.Error("a learning of another session must not be touched")
	}
	if superseded[narrativeC] {
		t.Error("a narrative of another session must not be touched")
	}

	// The narrative path reports its own rows only.
	narrIDs, err := s.SupersedeNarrativesBySession("los56e-c", "regenerated")
	if err != nil {
		t.Fatalf("supersede narratives: %v", err)
	}
	if len(narrIDs) != 1 || narrIDs[0] != narrativeC {
		t.Fatalf("narrative ids = %v, want just the narrative row of C", narrIDs)
	}

	// A second run has nothing to report: the returned set is empty, not stale.
	again, err := s.SupersedeSessionLearnings("los56e-a")
	if err != nil {
		t.Fatalf("second supersede: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run returned %v, want no ids", again)
	}
}

// TestLos56eBoundedTTLStaysVisibleUntilItExpires covers the review finding on
// YM-348: FindLearningsByEntityMatch filtered with valid_until IS NULL, which was
// harmless only because the store open wiped every TTL. A learning that
// cmd_volatile bounded keeps valid_until while staying active, so the existence
// check would have hidden it from the commit-time entity match — for the whole
// window and, because the predicate never looked at the date, forever after.
func TestLos56eBoundedTTLStaysVisibleUntilItExpires(t *testing.T) {
	s := mustOpen(t)

	insert := func(content, validUntil string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID:  "los56e-ttl",
			Category:   "gotcha",
			Content:    content,
			Entities:   []string{"handler_docs.go"},
			Confidence: 1.0,
			CreatedAt:  time.Now(),
			ModelUsed:  "test",
			Source:     "llm_extracted",
		})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		if _, err := s.DB().Exec(`UPDATE learnings SET valid_until = ? WHERE id = ?`, validUntil, id); err != nil {
			t.Fatalf("stamp %q: %v", content, err)
		}
		return id
	}

	future := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	past := time.Now().Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
	liveID := insert("bounded but still valid", future)
	expiredID := insert("bounded and expired", past)

	got, err := s.FindLearningsByEntityMatch([]string{"handler_docs.go"}, "")
	if err != nil {
		t.Fatalf("entity match: %v", err)
	}
	seen := map[int64]bool{}
	for _, l := range got {
		seen[l.ID] = true
	}
	if !seen[liveID] {
		t.Error("a learning inside its TTL must stay findable")
	}
	if seen[expiredID] {
		t.Error("a learning past its TTL must not be offered as current knowledge")
	}
}
