package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/storage"
)

// autoCorrectStubClient is the daemon LLM client for these tests: it records
// every prompt it is asked and answers with a canned completion. Los-32 name
// (bare "stubClient" would collide with the other loops in this package).
type autoCorrectStubClient struct {
	reply string
	err   error

	mu      sync.Mutex
	prompts []string
}

func (c *autoCorrectStubClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, system+"\n"+userMsg)
	c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	return c.reply, nil
}

func (c *autoCorrectStubClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return c.Complete(system, userMsg, opts...)
}

func (c *autoCorrectStubClient) Name() string  { return "auto-correct-stub" }
func (c *autoCorrectStubClient) Model() string { return "auto-correct-stub-model" }

func (c *autoCorrectStubClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prompts)
}

func (c *autoCorrectStubClient) lastPrompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.prompts) == 0 {
		return ""
	}
	return c.prompts[len(c.prompts)-1]
}

// errAutoCorrectStub is what a stubbed model call returns in the failure tests.
var errAutoCorrectStub = errors.New("stub: model unavailable")

// autoCorrectPassVerifier replaces the sandbox test-run for the flow tests: the
// real one needs ai-jail, the assertion here is about the persist path.
func autoCorrectPassVerifier(t *testing.T) func(string, int, SandboxProfile) (int, error) {
	t.Helper()
	return func(string, int, SandboxProfile) (int, error) { return 0, nil }
}

// The diagnosis used to shell out to `claude --bare -p`, which bypassed the
// provider, the budget and the api gate. With no claude binary on PATH at all,
// the diagnosis must still run — through the daemon's own LLM client.
func TestDiagnoseBashError_UsesTheDaemonLLMClient(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{ID: "diag-llm", Name: "broken-script", Project: "/srv/los32d/diag-llm", IntervalSeconds: 300}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	stub := &autoCorrectStubClient{reply: "the job exits 1 because its input file is missing"}
	h.SetAutoCorrectClient(stub)

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "diag-llm",
		JobName:  "broken-script",
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		Output:   "boom",
		ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if got := stub.callCount(); got != 1 {
		t.Fatalf("diagnosis must go through the daemon LLM client exactly once, got %d calls", got)
	}
	if !strings.Contains(stub.lastPrompt(), "boom") {
		t.Errorf("the diagnosis prompt must carry the job output, got %q", stub.lastPrompt())
	}

	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/diag-llm")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "input file is missing") {
			found = true
		}
	}
	if !found {
		t.Errorf("the diagnosis must be broadcast, got %d broadcasts", len(msgs))
	}
}

// The fix pass used to shell out to `claude --bare -p` with the prompt in argv.
// It must go through the daemon LLM client instead: no claude binary on PATH,
// the stub answers, and the prompt never leaves through a process argument.
func TestAutoCorrectBashCap_UsesTheDaemonLLMClient(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	stub := &autoCorrectStubClient{reply: patchReply("echo broken\nexit 1", "echo fixed\nexit 0")}
	h.SetAutoCorrectClient(stub)
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	seedCap(t, h, "llm_fix", "", "echo broken\nexit 1\n", []string{"llm_fix"})

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-llm",
		Name:        "llm-fix-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "llm_fix",
		ScriptName:  "llm_fix",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:      "j-llm",
		JobName:    "llm-fix-job",
		CapName:    "llm_fix",
		ScriptName: "llm_fix",
		Command:    "echo broken\nexit 1\n",
		Status:     "error",
		ExitCode:   1,
		ErrorMsg:   "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if got := stub.callCount(); got != 1 {
		t.Fatalf("the fix pass must go through the daemon LLM client exactly once, got %d calls", got)
	}
	if !strings.Contains(stub.lastPrompt(), "llm_fix") {
		t.Errorf("the fix prompt must address the cap, got %q", stub.lastPrompt())
	}
	if strings.Contains(stub.lastPrompt(), "echo broken") {
		t.Errorf("the fix prompt must not carry the script body, got %q", stub.lastPrompt())
	}
}

// Without a daemon LLM client there is no model to ask. The pass must stay inert
// rather than fall back to a `claude` process or drain the row: the client is
// wired asynchronously, and a run that failed during startup still deserves its
// one diagnosis.
func TestAutoCorrectBashCap_WithoutClientLeavesRunQueued(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	seedCap(t, h, "no_client", "", "echo broken\nexit 1\n", []string{"no_client"})

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-noclient",
		Name:        "no-client-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "no_client",
		ScriptName:  "no_client",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:      "j-noclient",
		JobName:    "no-client-job",
		CapName:    "no_client",
		ScriptName: "no_client",
		Command:    "echo broken\nexit 1\n",
		Status:     "error",
		ExitCode:   1,
		ErrorMsg:   "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.startedAt = time.Now() // inside the grace window, not at the clock's mercy
	h.processBashJobErrors(context.Background())

	// With no client the pass is inert: the run stays queued for the tick after
	// the client is wired, instead of being drained as "processed".
	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("a run must stay queued while there is no client; got %d unprocessed", len(runs))
	}
}

// The verification run must see the body the job would run: fireJobBash wraps it
// in prepareBashCommand (the store()/llm() adapter prelude). Verifying the bare
// body accepted a fix whose only broken part was the adapter path and stamped
// tested:true on it.
func TestAutoCorrect_VerifiesTheBodyTheJobWouldRun(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nstore '{\"capability\":\"x\",\"action\":\"upsert\"}'\nexit 0\n"

	seedCap(t, h, "adapter_cap", "", body, []string{"adapter_cap"})
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("set -eu", "set -eu -o pipefail")})

	var verified string
	h.autoCorrectVerify = func(b string, _ int, _ SandboxProfile) (int, error) {
		verified = b
		return 0, nil
	}

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-adapter", Name: "adapter", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "adapter_cap", ScriptName: "adapter_cap", AutoCorrect: true,
		Project: "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-adapter", JobName: "adapter", CapName: "adapter_cap", ScriptName: "adapter_cap",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if verified == "" {
		t.Fatal("the verification run never happened")
	}
	if !strings.Contains(verified, "YESMEM_WORKER_IN") {
		t.Errorf("the verified body lacks the adapter prelude the job gets: %.120q", verified)
	}
}
