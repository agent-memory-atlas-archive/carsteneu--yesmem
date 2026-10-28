package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los32bSeedBashErrorJob registers one job with a project and queues one failed run for
// it. The project is what makes a run addressable for a broadcast at all.
func los32bSeedBashErrorJob(t *testing.T, h *Handler, s *storage.Store, jobID, capName string) {
	t.Helper()
	if h.scheduler == nil {
		h.scheduler = newScheduler(nil, nil)
	}
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: jobID, Name: "broken-" + jobID, Project: "/srv/los32b/" + jobID, IntervalSeconds: 300,
		CapName: capName, AutoCorrect: capName != "",
	}); err != nil {
		t.Fatalf("add job %s: %v", jobID, err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: jobID, JobName: "broken-" + jobID, CapName: capName,
		Command: "exit 1", Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run %s: %v", jobID, err)
	}
}

// YM-264: without a model client the pass returned before it read the queue at
// all. Nothing was reported, and because the batch is the five OLDEST failures,
// those five rows hid every newer failure for good. Past the startup grace window
// the failure has to reach the operator, undiagnosed, and the row has to leave the
// queue.
func TestProcessBashJobErrors_ReportsWithoutAClientPastTheGraceWindow(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.startedAt = time.Now().Add(-bashErrorNoClientGrace - time.Minute)

	los32bSeedBashErrorJob(t, h, s, "no-client", "")

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d unprocessed runs: a row that can never be diagnosed must leave the queue, or it blocks every newer failure", len(runs))
	}

	msgs, err := s.GetUnreadBroadcasts("los32b-session", "/srv/los32b/no-client")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var reported bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "broken-no-client") && strings.Contains(m.Content, "not diagnosed") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the failure must still be reported without a client; got %d broadcasts", len(msgs))
	}
}

// The grace window itself: a run that failed during startup keeps its chance at
// a diagnosis, because the client is wired by an async startup goroutine.
func TestProcessBashJobErrors_KeepsRunsQueuedInsideTheGraceWindow(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	los32bSeedBashErrorJob(t, h, s, "starting", "")

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("got %d unprocessed runs, want the row still queued while the client is being wired", len(runs))
	}
}

// YM-264: the pass runs in its own goroutine, which was not bound to the daemon
// context. A model call or a verification run started while the daemon shuts down
// outlives it. A cancelled context must stop the pass before it touches the queue.
func TestProcessBashJobErrors_StopsOnACancelledContext(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	client := &autoCorrectStubClient{reply: "diagnosis text"}
	h.SetAutoCorrectClient(client)

	los32bSeedBashErrorJob(t, h, s, "shutdown", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.processBashJobErrors(ctx)

	if got := client.callCount(); got != 0 {
		t.Errorf("model calls = %d, want 0: a shutting-down daemon must not start new work", got)
	}
	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("got %d unprocessed runs, want the row left for the next start", len(runs))
	}
}
