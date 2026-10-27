package storage

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTrackingStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tracking.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedTrackedEnd(t *testing.T, s *Store, project, sessionID, reason string, at time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT OR REPLACE INTO session_tracking (project, session_id, reason, timestamp) VALUES (?, ?, ?, ?)`,
		project, sessionID, reason, at.UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed session_tracking: %v", err)
	}
}

const trackingProject = "/proj/tracked"

func TestGetPendingRecoveryDeliversOnce(t *testing.T) {
	s := openTrackingStore(t)
	if err := s.TrackSessionEnd(trackingProject, "sess-1", "clear"); err != nil {
		t.Fatalf("TrackSessionEnd: %v", err)
	}

	sid, reason, err := s.GetPendingRecovery(trackingProject, time.Hour)
	if err != nil {
		t.Fatalf("GetPendingRecovery: %v", err)
	}
	if sid != "sess-1" || reason != "clear" {
		t.Fatalf("handoff: got (%q, %q), want (sess-1, clear)", sid, reason)
	}

	sid, _, err = s.GetPendingRecovery(trackingProject, time.Hour)
	if err != nil {
		t.Fatalf("second GetPendingRecovery: %v", err)
	}
	if sid != "" {
		t.Errorf("handoff delivered twice: %q", sid)
	}
}

// Rows written before the consumed_at column existed, and clears whose follow-up
// session never started, are all unconsumed. Delivering whoever is newest and
// sweeping the rest keeps them from surfacing one per briefing.
func TestGetPendingRecoverySweepsOlderRows(t *testing.T) {
	s := openTrackingStore(t)
	seedTrackedEnd(t, s, trackingProject, "sess-old", "clear", time.Now().Add(-2*time.Hour))
	seedTrackedEnd(t, s, trackingProject, "sess-new", "clear", time.Now().Add(-1*time.Minute))

	sid, _, err := s.GetPendingRecovery(trackingProject, 24*time.Hour)
	if err != nil {
		t.Fatalf("GetPendingRecovery: %v", err)
	}
	if sid != "sess-new" {
		t.Fatalf("handoff: got %q, want the newest session", sid)
	}

	sid, _, err = s.GetPendingRecovery(trackingProject, 24*time.Hour)
	if err != nil {
		t.Fatalf("second GetPendingRecovery: %v", err)
	}
	if sid != "" {
		t.Errorf("an older unconsumed row was delivered after the newest: %q", sid)
	}
}

func TestGetPendingRecoveryStaysWithinProjectAndReason(t *testing.T) {
	s := openTrackingStore(t)
	seedTrackedEnd(t, s, trackingProject, "sess-logout", "logout", time.Now())
	seedTrackedEnd(t, s, "/proj/elsewhere", "sess-other", "clear", time.Now())

	if sid, _, err := s.GetPendingRecovery(trackingProject, time.Hour); err != nil || sid != "" {
		t.Errorf("recovered (%q, %v), want nothing: only /clear of this project counts", sid, err)
	}
	if sid, _, err := s.GetPendingRecovery("/proj/elsewhere", time.Hour); err != nil || sid != "sess-other" {
		t.Errorf("other project: got (%q, %v), want sess-other", sid, err)
	}
}

func TestGetPendingRecoveryDropsStaleRows(t *testing.T) {
	s := openTrackingStore(t)
	seedTrackedEnd(t, s, trackingProject, "sess-stale", "clear", time.Now().Add(-48*time.Hour))

	sid, _, err := s.GetPendingRecovery(trackingProject, 24*time.Hour)
	if err != nil {
		t.Fatalf("GetPendingRecovery: %v", err)
	}
	if sid != "" {
		t.Errorf("stale handoff delivered: %q", sid)
	}
	// Consumed as well, so it cannot surface on the next briefing either.
	sid, _, err = s.GetPendingRecovery(trackingProject, 24*time.Hour)
	if err != nil || sid != "" {
		t.Errorf("stale handoff delivered on the second call: (%q, %v)", sid, err)
	}
}

// A hook can deliver the same SessionEnd twice. The second delivery must not
// re-open the handoff a briefing has already used.
func TestTrackSessionEndDoesNotRearmAUsedHandoff(t *testing.T) {
	s := openTrackingStore(t)
	if err := s.TrackSessionEnd(trackingProject, "sess-again", "clear"); err != nil {
		t.Fatalf("TrackSessionEnd: %v", err)
	}

	if sid, _, err := s.GetPendingRecovery(trackingProject, time.Hour); err != nil || sid != "sess-again" {
		t.Fatalf("first handoff: got (%q, %v), want sess-again", sid, err)
	}

	if err := s.TrackSessionEnd(trackingProject, "sess-again", "clear"); err != nil {
		t.Fatalf("re-track: %v", err)
	}
	if sid, _, err := s.GetPendingRecovery(trackingProject, time.Hour); err != nil || sid != "" {
		t.Errorf("re-tracked session delivered again: (%q, %v)", sid, err)
	}
}

// Two briefings for the same project can be generated concurrently (two
// terminals, a session start next to a proxy refresh). Exactly one of them may
// receive the handoff.
func TestGetPendingRecoveryIsAtomicUnderConcurrency(t *testing.T) {
	s := openTrackingStore(t)
	if err := s.TrackSessionEnd(trackingProject, "sess-race", "clear"); err != nil {
		t.Fatalf("TrackSessionEnd: %v", err)
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, errs[i] = s.GetPendingRecovery(trackingProject, time.Hour)
		}(i)
	}
	wg.Wait()

	delivered := 0
	for i, sid := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if sid != "" {
			delivered++
			if sid != "sess-race" {
				t.Errorf("caller %d got %q, want sess-race", i, sid)
			}
		}
	}
	if delivered != 1 {
		t.Errorf("handoff delivered to %d callers, want exactly 1", delivered)
	}
}
