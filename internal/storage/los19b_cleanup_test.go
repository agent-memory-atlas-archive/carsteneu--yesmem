package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-265 [MEM-F33]: CleanupJunkLearnings superseded every row that matched its
// content markers, including the ones the user stated or agreed to. The
// short-session cleanup has spared that set since D5 and cluster distillation
// skips it since YM-228; the junk cleanup spared nothing. A junk marker in a
// user statement is evidence about the statement's shape, not about its value.
func TestLos19b_CleanupJunkSparesUserOwnedLearnings(t *testing.T) {
	s := mustOpen(t)

	insert := func(content, source string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			Category: "gotcha", Content: content, Source: source,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
		})
		if err != nil {
			t.Fatalf("insert (%s): %v", source, err)
		}
		return id
	}

	spared := []struct {
		id     int64
		source string
	}{
		{insert("GEFIXT: Der Cache liegt unter ~/.cache/yesmem", "user_stated"), "user_stated"},
		{insert("ab", "agreed_upon"), "agreed_upon"},
		{insert("Soll ich das aufraeumen?", "user_override"), "user_override"},
	}
	autoID := insert("GEFIXT: Der Cache liegt unter ~/.cache/yesmem", "llm_extracted")
	emptySourceID := insert("ab", "")

	ids, err := s.CleanupJunkLearnings()
	if err != nil {
		t.Fatalf("CleanupJunkLearnings: %v", err)
	}

	for _, row := range spared {
		l, err := s.GetLearning(row.id)
		if err != nil {
			t.Fatalf("get %d: %v", row.id, err)
		}
		if l.SupersededBy != nil {
			t.Errorf("cleanup superseded a %s learning: superseded_by=%v",
				row.source, *l.SupersededBy)
		}
	}

	for _, id := range []int64{autoID, emptySourceID} {
		l, err := s.GetLearning(id)
		if err != nil {
			t.Fatalf("get %d: %v", id, err)
		}
		if l.SupersededBy == nil || *l.SupersededBy != -1 {
			t.Errorf("junk row %d not cleaned: superseded_by=%v: the guard must not switch the cleanup off", id, l.SupersededBy)
		}
	}

	if len(ids) != 2 {
		t.Errorf("cleanup reported %d ids, want 2 (the two non user-owned junk rows)", len(ids))
	}
}
