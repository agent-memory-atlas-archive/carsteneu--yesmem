package briefing

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-282 [MEM-F35]: the metamemory block counts active rows whose validity
// window ran out. Legacy writers stored valid_until in RFC3339 (fmtTime), so a
// lexical comparison against datetime('now') sees 'T' at position 10 and never
// counts the row: an expired tombstone reads as "not expired".
func TestLos14cMetamemoryCountsExpiredRFC3339ValidUntil(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// The trap only shows on the expiry day itself: while the date matches,
	// the RFC3339 'T' at position 10 compares greater than the space in
	// datetime('now'), so the row reads as "not expired".
	now := time.Now().UTC()
	past := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 1, 0, time.UTC)
	if !past.Before(now) {
		t.Skip("run started in the first second of a UTC day")
	}
	pastText := past.Format(time.RFC3339)
	future := now.Add(48 * time.Hour).Format(time.RFC3339)
	if _, err := store.DB().Exec(`
		INSERT INTO learnings (category, content, project, canonical_project, source, confidence, created_at, model_used, superseded_by, valid_until)
		VALUES ('gotcha', 'expired in rfc3339', 'los14c', 'los14c', 'claude_suggested', 1.0, ?, 'test', NULL, ?),
		       ('gotcha', 'still valid in rfc3339', 'los14c', 'los14c', 'claude_suggested', 1.0, ?, 'test', NULL, ?)`,
		past, pastText, past, future); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	gen := New(store, 3)
	text := gen.GenerateMetamemory("los14c")
	if !strings.Contains(text, "1 possibly outdated") {
		t.Fatalf("expired RFC3339 valid_until not counted, metamemory = %q", text)
	}
}
