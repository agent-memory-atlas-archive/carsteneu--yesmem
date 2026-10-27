package briefing

import (
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestLos56eMetamemorySeesExpiredTTLAfterReopen is the regression test for
// YM-348 (2): the "possibly outdated" counter in the self-assessment could never
// fire, because every store open nulled valid_until on all active rows (see the
// sibling test in storage). A row whose TTL has passed must be counted, and the
// count must survive the reopen a daemon start performs.
func TestLos56eMetamemorySeesExpiredTTLAfterReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "yesmem.db")

	s, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Past TTL, still active: exactly what a bounded volatile learning looks like
	// after its window closed.
	if _, err := s.DB().Exec(`INSERT INTO learnings (session_id, category, content, confidence, created_at, model_used, source, valid_until, canonical_project, staleness_type)
		VALUES ('los56e', 'gotcha', 'expired bounded row', 1.0, '2026-10-01T10:00:00+02:00', 'test', 'llm_extracted', '2026-10-02 10:00:00', 'los56e-proj', 'volatile')`); err != nil {
		t.Fatalf("insert expired row: %v", err)
	}
	s.Close()

	s2, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	g := &Generator{store: s2, strings: DefaultStrings()}
	got := g.loadMetamemory("los56e-proj")
	if got == "" {
		t.Fatal("an expired active learning must reach the self-assessment, got no text at all")
	}
}
