package storage

import "testing"

// TestProjectChangeFingerprintReportsQueryErrorsLos16d (YM-372 [BRF-B14], Befund 4):
// the four reads scanned their result and dropped the error. A failing query left
// every value at its zero and produced a constant fingerprint, which the refresh
// loop compares against the stored hash — the briefing then looks unchanged and
// stays stale without a single log line.
func TestProjectChangeFingerprintReportsQueryErrorsLos16d(t *testing.T) {
	t.Run("sessions", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.ProjectChangeFingerprint("/los16d/repo"); err != nil {
			t.Fatalf("baseline fingerprint: %v", err)
		}
		if _, err := s.db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatalf("drop sessions: %v", err)
		}
		if _, err := s.ProjectChangeFingerprint("/los16d/repo"); err == nil {
			t.Error("a failing sessions query must be reported, not folded into a fingerprint")
		}
	})

	t.Run("learnings", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.db.Exec(`DROP TABLE learnings`); err != nil {
			t.Fatalf("drop learnings: %v", err)
		}
		if _, err := s.ProjectChangeFingerprint("/los16d/repo"); err == nil {
			t.Error("a failing learnings query must be reported, not folded into a fingerprint")
		}
	})

	t.Run("persona_directives", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.db.Exec(`DROP TABLE persona_directives`); err != nil {
			t.Fatalf("drop persona_directives: %v", err)
		}
		if _, err := s.ProjectChangeFingerprint("/los16d/repo"); err == nil {
			t.Error("a failing persona_directives query must be reported, not folded into a fingerprint")
		}
	})
}
