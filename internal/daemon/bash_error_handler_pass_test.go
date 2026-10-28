package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/storage"
)

// autoCorrectBlockingClient holds the first completion until the test releases
// it, so the caller of the pass can be observed returning while the pass is
// still in flight. Los-32 name.
type autoCorrectBlockingClient struct {
	started chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func (c *autoCorrectBlockingClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		select {
		case c.started <- struct{}{}:
		default:
		}
		<-c.release
	}
	return "the job fails because its input file is missing", nil
}

func (c *autoCorrectBlockingClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return c.Complete(system, userMsg, opts...)
}

func (c *autoCorrectBlockingClient) Name() string  { return "auto-correct-blocking" }
func (c *autoCorrectBlockingClient) Model() string { return "auto-correct-blocking-model" }

func (c *autoCorrectBlockingClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// The pass asks a model and (on the fix path) runs a script, which takes tens of
// seconds. It used to run inline on the heartbeat ticker, so relay delivery and
// crash recovery waited behind it. The entry point must return immediately and
// must not start a second pass while one is in flight.
func TestRunBashErrorPass_DoesNotBlockTheCallerAndRefusesAsecondPass(t *testing.T) {
	h, s := mustHandler(t)
	client := &autoCorrectBlockingClient{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.SetAutoCorrectClient(client)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{ID: "async-diag", Name: "broken-script", Project: "/srv/los32d/async", IntervalSeconds: 300}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := s.SaveBashJobRun(storage.BashJobRun{
			JobID:    "async-diag",
			JobName:  "broken-script",
			Command:  "exit 1",
			Status:   "error",
			ExitCode: 1,
			ErrorMsg: "exit status 1",
		}); err != nil {
			t.Fatalf("save run %d: %v", i, err)
		}
	}

	// The call returns although the pass is blocked inside the model call.
	h.runBashErrorPass(context.Background())

	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the model")
	}

	// A tick that arrives mid-pass must not start a second pass: that would pick
	// up the second queued run and ask the model twice.
	h.runBashErrorPass(context.Background())

	close(client.release)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.GetUnprocessedBashErrors(10)
		if err != nil {
			t.Fatalf("get unprocessed: %v", err)
		}
		if len(runs) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runs, err := s.GetUnprocessedBashErrors(10); err != nil || len(runs) != 0 {
		t.Fatalf("the pass did not finish (err=%v, unprocessed=%d)", err, len(runs))
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("expected exactly one diagnosis per queued run (2), got %d model calls", got)
	}
}
