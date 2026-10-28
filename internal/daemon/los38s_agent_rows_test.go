package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los38sRow creates an agent row for the stop/report tests below.
func los38sRow(t *testing.T, s *storage.Store, id string, fields map[string]any) storage.Agent {
	t.Helper()
	a := storage.Agent{
		ID:        id,
		Project:   "los38s-project",
		Section:   "yesloop-los38s",
		Status:    "running",
		Backend:   "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	for k, v := range fields {
		switch k {
		case "pid":
			a.PID = v.(int)
		case "status":
			a.Status = v.(string)
		case "backend":
			a.Backend = v.(string)
		case "opencode_session_id":
			a.OpencodeSessionID = v.(string)
		case "work_dir":
			a.WorkDir = v.(string)
		case "session_id":
			a.SessionID = v.(string)
		case "caller_session":
			a.CallerSession = v.(string)
		}
	}
	if err := s.AgentCreate(a); err != nil {
		t.Fatalf("AgentCreate(%s): %v", id, err)
	}
	return a
}

// los38sDeadPID returns a PID that no longer holds a process: the child was
// reaped before the test uses its number.
func los38sDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	if isPIDAlive(pid) {
		t.Fatalf("helper pid %d is still alive, the test needs a dead pid", pid)
	}
	return pid
}

// los38sSigtermIgnoringProcess starts a live process the identity gate accepts
// (its path names the claude backend binary) and which survives SIGTERM. That is
// the shape of the incident: the row is closed, the process keeps working.
func los38sSigtermIgnoringProcess(t *testing.T) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-los38s-survivor")
	script := "#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 1; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// The shebang process needs a moment to install its trap; a stop that lands
	// before that would end it for the wrong reason.
	time.Sleep(150 * time.Millisecond)
	return cmd.Process.Pid
}

func los38sStopResult(t *testing.T, h *Handler, id string) map[string]any {
	t.Helper()
	resp := h.Handle(Request{Method: "stop_agent", Params: map[string]any{"to": id}})
	if resp.Error != "" {
		t.Fatalf("stop_agent(%s): %s", id, resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode stop_agent result %s: %v", resp.Result, err)
	}
	return result
}

// TestLos38sStopReportsADecidedDeath: a stop whose process is gone must say so.
// The ticket's failure was a row that read "stopped" while the work continued,
// so the reply must carry more than the word.
func TestLos38sStopReportsADecidedDeath(t *testing.T) {
	h, s := mustHandler(t)
	los38sRow(t, s, "agent-los38s-dead", map[string]any{"pid": los38sDeadPID(t)})

	result := los38sStopResult(t, h, "agent-los38s-dead")

	if got, ok := result["process_gone"]; !ok {
		t.Fatalf("stop_agent does not report process_gone: %v", result)
	} else if gone, _ := got.(bool); !gone {
		t.Errorf("process_gone=%v, want true for a reaped pid", got)
	}
	if got, ok := result["session_active"]; !ok {
		t.Errorf("stop_agent does not report session_active: %v", result)
	} else if got != "unknown" {
		t.Errorf("session_active=%v, want unknown without a bound opencode session", got)
	}
}

// TestLos38sStopReportsAResidualProcess: a live process that survived the signal
// is exactly the invisible agent from the incident. The row is closed, the reply
// must name the process that is still there.
func TestLos38sStopReportsAResidualProcess(t *testing.T) {
	h, s := mustHandler(t)
	pid := los38sSigtermIgnoringProcess(t)
	los38sRow(t, s, "agent-los38s-live", map[string]any{"pid": pid})
	// Since YM-244 the stop escalates SIGTERM to SIGKILL, so a process that merely
	// ignores SIGTERM no longer models "survived the signal" — it would be gone.
	// What this test pins is the report, not the kill: the signals are stubbed and
	// the process stays, which is the incident's shape (a live process behind a
	// closed row).
	prevSignal := agentProcSignal
	agentProcSignal = func(int, syscall.Signal) error { return nil }
	t.Cleanup(func() { agentProcSignal = prevSignal })

	result := los38sStopResult(t, h, "agent-los38s-live")

	if gone, _ := result["process_gone"].(bool); gone {
		t.Fatalf("process_gone=true although the signalled process survived: %v", result)
	}
	residual, _ := result["residual"].(string)
	if !strings.Contains(residual, "PID") {
		t.Errorf("residual=%q, want a note naming the surviving process", residual)
	}
}

// TestLos38sStopReportsASessionStillWorking: the backend session lives in a
// service, independent of the process the daemon started. A stop that leaves a
// working session behind must say so, or the orchestrator trusts a row that
// nothing backs.
func TestLos38sStopReportsASessionStillWorking(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/session/ses_los38s/message") {
			http.NotFound(w, r)
			return
		}
		// The turn has to land after the stop was decided; a stamp fixed before
		// the stop would be a turn the stop already covered.
		done := now
		if live := time.Now().UnixMilli(); live > done {
			done = live
		}
		fmt.Fprintf(w, `{"data":[{"id":"m1","type":"assistant","time":{"created":%d,"completed":%d}}]}`, done, done)
	}))
	defer srv.Close()
	pinned := opencodeV2Service
	opencodeV2Service = func() *opencode.Client {
		return opencode.NewClient(opencode.Service{ID: "los38s-stub", URL: srv.URL})
	}
	t.Cleanup(func() { opencodeV2Service = pinned })

	los38sRow(t, s, "agent-los38s-session", map[string]any{
		"pid":                 los38sDeadPID(t),
		"backend":             "opencode",
		"opencode_session_id": "ses_los38s",
	})

	result := los38sStopResult(t, h, "agent-los38s-session")

	if got := result["session_active"]; got != "yes" {
		t.Fatalf("session_active=%v, want yes for a turn completed after the stop: %v", got, result)
	}
	if residual, _ := result["residual"].(string); !strings.Contains(residual, "session") {
		t.Errorf("residual=%q, want a note about the working session", residual)
	}
}

// TestLos38sRespawnRefusesAGoneWorkDir: the incident's third symptom. The
// worktree was removed after the merge, the row still said crashed, and the
// replacement came up in the daemon's run dir without a repository — redoing the
// phases of a finished task. A row whose recorded tree is gone must be closed
// instead of replaced.
func TestLos38sRespawnRefusesAGoneWorkDir(t *testing.T) {
	h, s := mustHandler(t)
	gone := filepath.Join(t.TempDir(), "worktree-that-was-removed")
	los38sRow(t, s, "agent-los38s-ghost", map[string]any{
		"status":   "crashed",
		"work_dir": gone,
	})

	var crashed storage.Agent
	agents, err := s.AgentList("")
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	for _, a := range agents {
		if a.ID == "agent-los38s-ghost" {
			crashed = a
		}
	}
	if crashed.ID == "" {
		t.Fatal("the seed row is missing")
	}

	h.respawnAgent(crashed)

	after, err := s.AgentGet("agent-los38s-ghost")
	if err != nil || after == nil {
		t.Fatalf("AgentGet after respawn: %v", err)
	}
	if after.Status != "stopped" {
		t.Errorf("status=%q, want stopped (the replacement must not come up in a run dir)", after.Status)
	}
	if !strings.Contains(after.Progress, "worktree-that-was-removed") {
		t.Errorf("progress=%q, want the vanished directory named", after.Progress)
	}
	// No active row may own the section: sql.ErrNoRows is the expected answer.
	replacement, _ := s.AgentGetActiveBySection("los38s-project", "yesloop-los38s")
	if replacement != nil {
		t.Errorf("a replacement row %s owns the section although the tree is gone", replacement.ID)
	}
}

// TestLos38sRestartWorkDirDecision pins the three answers the restart gate must
// keep apart: a usable tree, a tree that was recorded and is gone, and a row that
// never recorded one (which keeps the old project-resolution fallback).
func TestLos38sRestartWorkDirDecision(t *testing.T) {
	h, _ := mustHandler(t)

	present := t.TempDir()
	if dir, refusal := h.agentRestartWorkDir(storage.Agent{WorkDir: present}); refusal != "" || dir != present {
		t.Errorf("present tree: dir=%q refusal=%q, want the recorded dir and no refusal", dir, refusal)
	}

	gone := filepath.Join(t.TempDir(), "removed-worktree")
	dir, refusal := h.agentRestartWorkDir(storage.Agent{WorkDir: gone, Backend: "claude"})
	if dir != "" {
		t.Errorf("gone tree: dir=%q, want no directory", dir)
	}
	if !strings.Contains(refusal, "removed-worktree") {
		t.Errorf("gone tree: refusal=%q, want the vanished directory named", refusal)
	}

	if dir, refusal := h.agentRestartWorkDir(storage.Agent{}); refusal != "" || dir != "" {
		t.Errorf("no recorded tree: dir=%q refusal=%q, want the unresolved fallback", dir, refusal)
	}
}

// TestLos38sResidualReachesTheRow: the reply is not enough. A later reader of the
// row (get_agent, list_agents, an orchestrator after a session loss) has to see
// that the stop left something behind, otherwise the row keeps lying in the DB.
func TestLos38sResidualReachesTheRow(t *testing.T) {
	h, s := mustHandler(t)
	pid := los38sSigtermIgnoringProcess(t)
	los38sRow(t, s, "agent-los38s-row", map[string]any{"pid": pid})
	// Same reason as in TestLos38sStopReportsAResidualProcess: with the YM-244
	// escalation the process would be killed, so the signals are stubbed to keep
	// the "survived the stop" shape this test is about.
	prevSignal := agentProcSignal
	agentProcSignal = func(int, syscall.Signal) error { return nil }
	t.Cleanup(func() { agentProcSignal = prevSignal })

	los38sStopResult(t, h, "agent-los38s-row")

	row, err := s.AgentGet("agent-los38s-row")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "stopped" {
		t.Errorf("status = %q, want stopped", row.Status)
	}
	if !strings.Contains(row.Progress, "still held") {
		t.Errorf("progress = %q, want the surviving process recorded in the row", row.Progress)
	}
}

// TestLos38sStopIgnoresAnAbortedTurn: an open turn stays open forever when it was
// aborted, so "working" alone would report a residual on every stop of a session
// that ended that way. Only a young open turn counts.
func TestLos38sStopIgnoresAnAbortedTurn(t *testing.T) {
	h, s := mustHandler(t)

	started := time.Now().Add(-2 * time.Hour).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/session/ses_los38s_aborted/message") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"m1","type":"assistant","time":{"created":%d,"completed":0}}]}`, started)
	}))
	defer srv.Close()
	pinned := opencodeV2Service
	opencodeV2Service = func() *opencode.Client {
		return opencode.NewClient(opencode.Service{ID: "los38s-stub", URL: srv.URL})
	}
	t.Cleanup(func() { opencodeV2Service = pinned })

	los38sRow(t, s, "agent-los38s-aborted", map[string]any{
		"pid":                 los38sDeadPID(t),
		"backend":             "opencode",
		"opencode_session_id": "ses_los38s_aborted",
	})

	result := los38sStopResult(t, h, "agent-los38s-aborted")

	if got := result["session_active"]; got != "no" {
		t.Fatalf("session_active=%v, want no for a turn that has been open for two hours: %v", got, result)
	}
	if residual, _ := result["residual"].(string); residual != "" {
		t.Errorf("residual=%q, want none for an aborted turn", residual)
	}
}

// TestLos38sLimitStopClaimsNoResidual covers the caller side of the switch: the
// heartbeat's own limit stop must not write a residual claim, and that depends on
// the parameter it actually sends. A test that drives handleStopAgent directly
// would stay green while the wiring is broken.
func TestLos38sLimitStopClaimsNoResidual(t *testing.T) {
	h, s := mustHandler(t)
	pid := los38sSigtermIgnoringProcess(t)
	los38sRow(t, s, "agent-los38s-limit", map[string]any{"pid": pid})
	if err := s.AgentUpdate("agent-los38s-limit", map[string]any{"token_budget": 1000}); err != nil {
		t.Fatalf("budget: %v", err)
	}
	// Over budget: 1100 output tokens (the unit the limit counts).
	if err := s.AgentUpdateTelemetry("agent-los38s-limit", 1, 600, 1100); err != nil {
		t.Fatalf("telemetry: %v", err)
	}

	h.enforceAgentLimits()

	row, err := s.AgentGet("agent-los38s-limit")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "stopped" {
		t.Fatalf("status = %q, want stopped", row.Status)
	}
	if strings.Contains(row.Progress, "still held") {
		t.Errorf("progress = %q, the heartbeat stop must not claim a residual from an unverified snapshot", row.Progress)
	}
	if !strings.Contains(row.Progress, "token_budget") {
		t.Errorf("progress = %q, want the limit recorded", row.Progress)
	}
}

// TestLos38sSkippedVerificationClaimsNothing: the heartbeat stops skip the
// post-signal verification. Such a stop must not write a snapshot taken right
// after SIGTERM into the row — the process it just signalled would read as "still
// held", and the field would become timing noise on the most frequent stop path.
func TestLos38sSkippedVerificationClaimsNothing(t *testing.T) {
	h, s := mustHandler(t)
	pid := los38sSigtermIgnoringProcess(t)
	los38sRow(t, s, "agent-los38s-quiet", map[string]any{"pid": pid})

	resp := h.Handle(Request{Method: "stop_agent", Params: map[string]any{
		"to": "agent-los38s-quiet", "_verify": false,
	}})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := result["process_gone"]; ok {
		t.Errorf("a stop without verification reports process_gone: %v", result)
	}
	if _, ok := result["session_active"]; ok {
		t.Errorf("a stop without verification reports session_active: %v", result)
	}

	row, err := s.AgentGet("agent-los38s-quiet")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if strings.Contains(row.Progress, "still held") {
		t.Errorf("progress = %q, want no residual claim from a skipped verification", row.Progress)
	}
}

// TestLos38sRecentOpenTurnCounts: the age bound comes from the idle guard, which
// counts a long tool run as work. A bound tight enough to hide a ten-minute test
// suite would report "nothing works" exactly during the work it should report.
func TestLos38sRecentOpenTurnCounts(t *testing.T) {
	h, s := mustHandler(t)

	started := time.Now().Add(-10 * time.Minute).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/session/ses_los38s_working/message") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"m1","type":"assistant","time":{"created":%d,"completed":0}}]}`, started)
	}))
	defer srv.Close()
	pinned := opencodeV2Service
	opencodeV2Service = func() *opencode.Client {
		return opencode.NewClient(opencode.Service{ID: "los38s-stub", URL: srv.URL})
	}
	t.Cleanup(func() { opencodeV2Service = pinned })

	los38sRow(t, s, "agent-los38s-working", map[string]any{
		"pid":                 los38sDeadPID(t),
		"backend":             "opencode",
		"opencode_session_id": "ses_los38s_working",
	})

	result := los38sStopResult(t, h, "agent-los38s-working")

	if got := result["session_active"]; got != "yes" {
		t.Fatalf("session_active=%v, want yes for a turn that has been running ten minutes: %v", got, result)
	}
}

// TestLos38sRestartRefusesWhenTheSessionDirectoryIsAlsoGone: the session is an
// answer to a vanished tree only while its directory exists.
func TestLos38sRestartRefusesWhenTheSessionDirectoryIsAlsoGone(t *testing.T) {
	h, _ := mustHandler(t)
	pinned := opencodeSessionDirFn
	opencodeSessionDirFn = func(string) string {
		return filepath.Join(t.TempDir(), "session-dir-that-is-gone")
	}
	t.Cleanup(func() { opencodeSessionDirFn = pinned })

	vanished := filepath.Join(t.TempDir(), "removed-worktree")
	dir, refusal := h.agentRestartWorkDir(storage.Agent{
		WorkDir:           vanished,
		Backend:           "opencode",
		OpencodeSessionID: "ses_los38s_gone",
	})
	if dir != "" {
		t.Errorf("dir = %q, want none", dir)
	}
	if !strings.Contains(refusal, "removed-worktree") {
		t.Errorf("refusal = %q, want the vanished directory named", refusal)
	}
}

// TestLos38sRestartUsesTheSessionDirectory: a vanished tree is not a refusal when
// the backend session still names the directory it ran in. That is the legitimate
// restart this gate must keep working.
func TestLos38sRestartUsesTheSessionDirectory(t *testing.T) {
	h, _ := mustHandler(t)
	sessionDir := t.TempDir()
	pinned := opencodeSessionDirFn
	opencodeSessionDirFn = func(id string) string {
		if id != "ses_los38s_moved" {
			return ""
		}
		return sessionDir
	}
	t.Cleanup(func() { opencodeSessionDirFn = pinned })

	dir, refusal := h.agentRestartWorkDir(storage.Agent{
		WorkDir:           filepath.Join(t.TempDir(), "removed-worktree"),
		Backend:           "opencode",
		OpencodeSessionID: "ses_los38s_moved",
	})
	if refusal != "" {
		t.Fatalf("refusal = %q, want the session directory to be accepted", refusal)
	}
	if dir != sessionDir {
		t.Errorf("dir = %q, want the session directory %q", dir, sessionDir)
	}
}

// TestLos38sRegisterRefusalClosesTheSpawnRow: the CLI path stops its child and
// moves on, so nothing runs the daemon's exit tail for a refused bind. The row
// must not stay "spawning" with pid 0 — that would own the section forever.
func TestLos38sRegisterRefusalClosesTheSpawnRow(t *testing.T) {
	h, s := mustHandler(t)
	los38sRow(t, s, "agent-los38s-cli", map[string]any{"status": "spawning", "pid": 0, "session_id": "sess-los38s-cli"})

	resp := h.handleRegisterAgent(map[string]any{
		"id": "agent-los38s-cli", "pid": float64(los38sDeadPID(t)), "sock_path": "/tmp/los38s-cli.sock",
		"session_id": "sess-los38s-cli",
	})
	if resp.Error == "" {
		t.Fatal("register_agent accepted a pid that holds no process")
	}

	row, err := s.AgentGet("agent-los38s-cli")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status == "spawning" || row.Status == "pending" {
		t.Errorf("status = %q, the refused bind must not leave the row owning its section", row.Status)
	}
	active, _ := s.AgentGetActiveBySection("los38s-project", "yesloop-los38s")
	if active != nil {
		t.Errorf("row %s still owns the section after the refused bind", active.ID)
	}
}

// TestLos38sBindRefusesADeadPID: agents.pid must only ever name a process that
// exists. register_agent takes the pid from its caller, so a bind that does not
// look leaves a row claiming a process nobody holds.
func TestLos38sBindRefusesADeadPID(t *testing.T) {
	h, s := mustHandler(t)
	los38sRow(t, s, "agent-los38s-bind", map[string]any{"status": "spawning", "pid": 0})

	bound, err := h.bindAgentProcess("agent-los38s-bind", los38sDeadPID(t), "/tmp/los38s-bind.sock", "")
	if err != nil {
		t.Fatalf("bindAgentProcess: %v", err)
	}
	if bound {
		t.Fatal("bindAgentProcess bound a pid whose process is gone")
	}
}
