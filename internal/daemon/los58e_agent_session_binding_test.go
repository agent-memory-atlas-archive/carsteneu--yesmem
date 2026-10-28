package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los58eCapture runs the spawn-time capture for one row exactly as the spawn
// goroutine does and reports the session the row carries afterwards. The budget
// is a single poll attempt: the stub answers immediately, so a longer budget
// would only slow the test down. It returns an error instead of "" for a broken
// fixture — a swallowed read error would satisfy "not bound" just as well as the
// fix does (cold review finding).
func los58eCapture(h *Handler, s *storage.Store, id string) (string, error) {
	a, err := s.AgentGet(id)
	if err != nil || a == nil {
		return "", fmt.Errorf("agent %s unreadable: %v", id, err)
	}
	since, ok := parseAgentCreatedAt(a.CreatedAt)
	if !ok {
		return "", fmt.Errorf("agent %s carries an unparsable created_at %q", id, a.CreatedAt)
	}
	if sid := captureOpencodeSessionID(a.WorkDir, a.ID, a.Section, a.Project, since, 40*time.Millisecond); sid != "" {
		h.bindAgentSession(a, sid, "spawn capture")
	}
	row, err := s.AgentGet(id)
	if err != nil || row == nil {
		return "", fmt.Errorf("agent %s unreadable after the capture: %v", id, err)
	}
	return row.OpencodeSessionID, nil
}

// TestLos58e_TwoConcurrentSpawnsKeepTheirOwnSession is the test YM-419 asks for:
// two agents spawn at the same moment, and each row must end up with its own
// session — never with the other's, and never with a session nobody proved.
//
// The live shape (21.10.2026, 11:52:19) is what makes this hard: every agent of a
// wave records the same work_dir (the repository root), so the directory does not
// tell siblings apart, and opencode creates a session row seconds before the
// injected prompt lands in it. The session that exists without its prompt yet
// carries no marker at all, and "the only blank session in the workdir" was
// accepted as that agent's own: five spawn captures of the wave returned the same
// id, the first writer bound it, and its own session (created later) was refused
// because the row already carried one.
func TestLos58e_TwoConcurrentSpawnsKeepTheirOwnSession(t *testing.T) {
	h, s := mustHandler(t)
	shared := "/repo/shared"
	now := time.Now().Truncate(time.Millisecond)
	stamp := now.Format(time.RFC3339)

	for _, id := range []string{"los58e-b", "los58e-c"} {
		if err := s.AgentCreate(storage.Agent{
			ID: id, Project: "/repo", Section: "sec-" + id, SessionID: "uuid-" + id,
			Status: "running", Backend: "opencode", WorkDir: shared, CreatedAt: stamp,
		}); err != nil {
			t.Fatalf("AgentCreate(%s): %v", id, err)
		}
	}

	// Phase 1: only C's session exists, and its prompt has not been injected yet.
	// B's own session does not exist yet — the sibling that polls first must not
	// adopt C's session.
	blankOnly := v2StubServer(t, []v2Session{
		{ID: "ses_los58e_c", Dir: shared, Agent: "build", Created: now.Add(400 * time.Millisecond).UnixMilli()},
	}, shared, nil)
	withV2Stub(t, blankOnly)
	// A second shape of the same failure: the table fallback of the capture reads
	// session_v2 as well, and binds "the only root session in the workdir" — a
	// user's fresh session in the repository root would become this agent's. The
	// seeded row is inside the capture window for both rows.
	seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session_v2 VALUES ('ses_los58e_foreign', '%s', %d, '');", shared, now.UnixMilli()))

	if got, err := los58eCapture(h, s, "los58e-b"); err != nil {
		t.Fatalf("capture for los58e-b: %v", err)
	} else if got != "" {
		t.Fatalf("bound %q before any marker existed — neither a session without a user message nor the message-blind table scan may give an agent an identity", got)
	}

	// Phase 2: both prompts have landed. Each capture now has a marker to match,
	// and the two spawns run as concurrently as they do in production.
	withMarkers := v2StubServer(t, []v2Session{
		{ID: "ses_los58e_c", Dir: shared, Agent: "build", Created: now.Add(400 * time.Millisecond).UnixMilli()},
		{ID: "ses_los58e_b", Dir: shared, Agent: "build", Created: now.Add(900 * time.Millisecond).UnixMilli()},
	}, shared, map[string]string{
		"ses_los58e_b": "You are working on project '/repo', agent 'los58e-b', section 'sec-los58e-b'.",
		"ses_los58e_c": "You are working on project '/repo', agent 'los58e-c', section 'sec-los58e-c'.",
	})
	stubV2Service(t, withMarkers)

	type result struct {
		sid string
		err error
	}
	got := make(map[string]result, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range []string{"los58e-b", "los58e-c"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sid, err := los58eCapture(h, s, id)
			mu.Lock()
			got[id] = result{sid, err}
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	for id, want := range map[string]string{"los58e-b": "ses_los58e_b", "los58e-c": "ses_los58e_c"} {
		if r := got[id]; r.err != nil {
			t.Errorf("capture for %s: %v", id, r.err)
		} else if r.sid != want {
			t.Errorf("row %s holds %q, want its own %s", id, r.sid, want)
		}
	}
}
