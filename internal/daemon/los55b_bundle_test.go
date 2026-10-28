package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los55bStagnationTracked reports whether the stagnation guard keeps bookkeeping
// for an agent id.
func los55bStagnationTracked(agentID string) bool {
	yesloopStagnationAgentsMu.Lock()
	defer yesloopStagnationAgentsMu.Unlock()
	_, ok := yesloopStagnationAgents[agentID]
	return ok
}

// TestLos55bDoneGuardStateIsPrunedWithTheRow: the done-guard map is in-memory and
// grew one entry per agent that ever failed validation. Reaped rows never came
// back, so the entries accumulated until a daemon restart (YM-285).
func TestLos55bDoneGuardStateIsPrunedWithTheRow(t *testing.T) {
	resetDoneGuardState()
	t.Cleanup(resetDoneGuardState)
	h, s := mustHandler(t)
	makeDoneGuardAgent(t, h, s, "los55b-dg-prune", "sess-los55b-dg", allPhasesPhase4Invalid)

	h.checkYesloopDoneGuard()
	if !doneGuardTracked("los55b-dg-prune") {
		t.Fatal("the guard did not track the failing agent at all — the test cannot observe the prune")
	}

	// The row is gone (reaped, or closed and pruned): nothing will ever read the
	// entry again.
	if err := s.AgentDelete("los55b-dg-prune"); err != nil {
		t.Fatalf("AgentDelete: %v", err)
	}
	h.checkYesloopDoneGuard()

	if doneGuardTracked("los55b-dg-prune") {
		t.Error("done-guard kept bookkeeping for a row that no longer exists")
	}
}

// TestLos55bStagnationStateIsPrunedWithTheRow: same leak in the stagnation guard.
func TestLos55bStagnationStateIsPrunedWithTheRow(t *testing.T) {
	resetYesloopStagnationState()
	t.Cleanup(resetYesloopStagnationState)
	h, s := mustHandler(t)
	makeStagnationAgent(t, h, s, "los55b-st-prune", "### Phase 1: ANALYZE\n**Status:** IN PROGRESS\n")

	h.checkYesloopStagnation()
	if !los55bStagnationTracked("los55b-st-prune") {
		t.Fatal("the guard did not track the agent at all — the test cannot observe the prune")
	}

	if err := s.AgentDelete("los55b-st-prune"); err != nil {
		t.Fatalf("AgentDelete: %v", err)
	}
	h.checkYesloopStagnation()

	if los55bStagnationTracked("los55b-st-prune") {
		t.Error("stagnation guard kept bookkeeping for a row that no longer exists")
	}
}

// TestLos55bEvidenceDetailCarriesNoControlChars: the Phase 5 evidence detail
// embeds the session id the agent wrote into its scratchpad. An unterminated
// terminal escape or a BEL in that value reached the relay body and the
// orchestrator message verbatim (YM-285).
func TestLos55bEvidenceDetailCarriesNoControlChars(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write db placeholder: %v", err)
	}
	prev := queryOpencodeSession
	queryOpencodeSession = func(_, _ string) (bool, string, string, error) {
		// parent mismatch and a disallowed agent type: both land in a Detail.
		return true, "parent\x1b[2K", "rev\x07iewer", nil
	}
	t.Cleanup(func() { queryOpencodeSession = prev })

	errs := VerifySubagentEvidence(dbPath, "ses_parent", map[string]string{
		"cold-review": "ses_evil\x1b[2K\x07",
	})
	if len(errs) == 0 {
		t.Fatal("no evidence errors produced — the test cannot observe the sink")
	}

	msg := summarizeErrors(ValidationResult{FieldErrors: errs})
	for _, r := range msg {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("relay text carries control character %U: %q", r, msg)
		}
	}
	// The producer must already be clean: the detail is also rendered by
	// ValidationResult.String and could reach a log without summarizeErrors.
	for _, fe := range errs {
		for _, r := range fe.Detail {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("evidence detail carries control character %U: %q", r, fe.Detail)
			}
		}
	}
	if !strings.Contains(msg, "cold-review") {
		t.Errorf("the message lost the field name: %q", msg)
	}
}

// TestLos55bUpdateAgentCannotWriteAPid: register_agent refuses a foreign bind, but
// update_agent could set the same column in one call, and the store stamps the
// spawn marker for it — so the identity gate could never reject the row
// (cold review 5.4).
func TestLos55bUpdateAgentCannotWriteAPid(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-update-pid", Project: "los55b-project", Section: "update-pid",
		SessionID: "sess-los55b-owner", Status: "running", Backend: "claude",
		PID: 4242, CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.Handle(Request{Method: "update_agent", Params: map[string]any{
		"id": "los55b-update-pid",
		"fields": map[string]any{
			"pid": float64(los55bForeignProcess(t)),
		},
	}})
	if resp.Error == "" {
		t.Fatal("update_agent accepted a pid write — it bypasses the bind ownership check")
	}
	row, err := s.AgentGet("los55b-update-pid")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.PID != 4242 {
		t.Errorf("pid = %d, the refused write must leave the row untouched", row.PID)
	}

	// Clearing is still allowed: the exit tails and the resume path write pid 0.
	resp = h.Handle(Request{Method: "update_agent", Params: map[string]any{
		"id":     "los55b-update-pid",
		"fields": map[string]any{"pid": float64(0)},
	}})
	if resp.Error != "" {
		t.Fatalf("update_agent refused a pid clear: %s", resp.Error)
	}
	if row, _ := s.AgentGet("los55b-update-pid"); row == nil || row.PID != 0 {
		t.Errorf("pid was not cleared")
	}
}

// TestLos55bCmdlineMatchIsNotASubstring: the backend name has to be an argv
// element's base name, not a substring of any argument (cold review 5.4).
func TestLos55bCmdlineMatchIsNotASubstring(t *testing.T) {
	cases := []struct {
		cmdline string
		want    string
		match   bool
	}{
		{"/home/tester/.claude/local/claude\x00--session-id\x00x", "claude", true},
		{"node\x00/home/tester/.nvm/versions/node/v22/bin/codex\x00resume", "codex", true},
		{"sh\x00-c\x00sleep 300\x00claude-notes.md", "claude", false},
		{"/usr/bin/claude-helper\x00", "claude", false},
		{"", "claude", false},
	}
	for _, c := range cases {
		if got := cmdlineNamesBinary(c.cmdline, c.want); got != c.match {
			t.Errorf("cmdlineNamesBinary(%q, %q) = %v, want %v", c.cmdline, c.want, got, c.match)
		}
	}
}

// los55bForeignProcess starts a live process whose command line does not name any
// agent backend: the shape of a PID that a caller hands to register_agent to steal
// a row's process binding.
func los55bForeignProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start foreign helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// TestLos55bBindRefusesAForeignProcess: a bind only ever records a process that
// is the agent's own backend. register_agent takes the PID from its caller, and
// the existence check alone accepted any live process (YM-356).
func TestLos55bBindRefusesAForeignProcess(t *testing.T) {
	pinProcessTable(t)
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-bind-foreign", Project: "los55b-project", Section: "bind-foreign",
		SessionID: "sess-los55b-owner", Status: "spawning", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	bound, err := h.bindAgentProcess("los55b-bind-foreign", los55bForeignProcess(t), "/nonexistent/bind-foreign.sock", "sess-los55b-owner")
	if err != nil {
		t.Fatalf("bindAgentProcess: %v", err)
	}
	if bound {
		t.Fatal("bindAgentProcess bound a PID whose command line names no agent backend")
	}
	row, err := s.AgentGet("los55b-bind-foreign")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.PID != 0 {
		t.Errorf("row pid = %d, a refused bind must leave the row unbound", row.PID)
	}
}

// TestLos55bBindAcceptsItsOwnBackendProcess: the negative control. A process whose
// command line names the row's backend is exactly what the spawn path binds.
func TestLos55bBindAcceptsItsOwnBackendProcess(t *testing.T) {
	pinProcessTable(t)
	// The cmdline gate wants an argv element named like the backend binary; the
	// helper script is named claude-*, which is exactly what the tighter match
	// rejects.
	stubProcCmdline(t, "/home/tester/.claude/local/claude\x00--session-id\x00sess")
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-bind-own", Project: "los55b-project", Section: "bind-own",
		SessionID: "sess-los55b-owner", Status: "spawning", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	bound, err := h.bindAgentProcess("los55b-bind-own", los38sSigtermIgnoringProcess(t), "/nonexistent/bind-own.sock", "sess-los55b-owner")
	if err != nil {
		t.Fatalf("bindAgentProcess: %v", err)
	}
	if !bound {
		t.Fatal("bindAgentProcess refused the row's own backend process")
	}
}

// TestLos55bBindRequiresTheRowsOwner: the session id spawn handed out is the row's
// capability. A caller that does not present it must not take the row over, even
// with a matching PID (YM-356).
func TestLos55bBindRequiresTheRowsOwner(t *testing.T) {
	pinProcessTable(t)
	// The cmdline gate wants an argv element named like the backend binary; the
	// helper script is named claude-*, which is exactly what the tighter match
	// rejects.
	stubProcCmdline(t, "/home/tester/.claude/local/claude\x00--session-id\x00sess")
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-bind-owner", Project: "los55b-project", Section: "bind-owner",
		SessionID: "sess-los55b-owner", Status: "spawning", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	pid := los38sSigtermIgnoringProcess(t)
	bound, err := h.bindAgentProcess("los55b-bind-owner", pid, "/nonexistent/bind-owner.sock", "sess-attacker")
	if err != nil {
		t.Fatalf("bindAgentProcess: %v", err)
	}
	if bound {
		t.Fatal("bindAgentProcess bound a row to a caller that is not its owner")
	}
}

// TestLos55bRegisterCannotCloseAForeignRow: register_agent closes the row of a
// refused bind so a half-spawned row cannot own its section forever. Without an
// ownership check any socket caller could close another agent's spawning row
// (YM-356).
func TestLos55bRegisterCannotCloseAForeignRow(t *testing.T) {
	pinProcessTable(t)
	// The cmdline gate wants an argv element named like the backend binary; the
	// helper script is named claude-*, which is exactly what the tighter match
	// rejects.
	stubProcCmdline(t, "/home/tester/.claude/local/claude\x00--session-id\x00sess")
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-register-victim", Project: "los55b-project", Section: "register-victim",
		SessionID: "sess-los55b-owner", Status: "spawning", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleRegisterAgent(map[string]any{
		"id":         "los55b-register-victim",
		"pid":        float64(los38sSigtermIgnoringProcess(t)),
		"sock_path":  "/nonexistent/register-victim.sock",
		"session_id": "sess-attacker",
	})
	if resp.Error == "" {
		t.Fatal("register_agent accepted a caller that does not own the row")
	}

	row, err := s.AgentGet("los55b-register-victim")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "spawning" {
		t.Errorf("status = %q, a foreign caller must not close the row", row.Status)
	}
	if row.PID != 0 {
		t.Errorf("row pid = %d, a foreign caller must not bind it", row.PID)
	}
}

// TestLos55bRegisterAcceptsItsOwner: the negative control at the handler level.
func TestLos55bRegisterAcceptsItsOwner(t *testing.T) {
	pinProcessTable(t)
	// The cmdline gate wants an argv element named like the backend binary; the
	// helper script is named claude-*, which is exactly what the tighter match
	// rejects.
	stubProcCmdline(t, "/home/tester/.claude/local/claude\x00--session-id\x00sess")
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-register-own", Project: "los55b-project", Section: "register-own",
		SessionID: "sess-los55b-owner", Status: "spawning", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	pid := los38sSigtermIgnoringProcess(t)
	resp := h.handleRegisterAgent(map[string]any{
		"id":         "los55b-register-own",
		"pid":        float64(pid),
		"sock_path":  "/nonexistent/register-own.sock",
		"session_id": "sess-los55b-owner",
	})
	if resp.Error != "" {
		t.Fatalf("register_agent refused the row's owner: %s", resp.Error)
	}
	row, err := s.AgentGet("los55b-register-own")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.PID != pid || row.Status != "running" {
		t.Errorf("row = pid %d status %q, want pid %d running", row.PID, row.Status, pid)
	}
}

// TestLos55bReadPathsDoNotExposeTheRowSession: the ownerSession check is only
// worth anything if the row's session id is not readable. get_agent and
// list_agents used to return it, so any socket caller could read the capability
// and then take the row over (cold review F2, YM-356).
func TestLos55bReadPathsDoNotExposeTheRowSession(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-leak", Project: "los55b-project", Section: "leak",
		SessionID: "sess-los55b-secret", Status: "running", Backend: "claude",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"get_agent", map[string]any{"to": "los55b-leak"}},
		{"list_agents", map[string]any{"project": "los55b-project"}},
	} {
		resp := h.Handle(Request{Method: call.method, Params: call.params})
		if resp.Error != "" {
			t.Fatalf("%s: %s", call.method, resp.Error)
		}
		if strings.Contains(string(resp.Result), "sess-los55b-secret") {
			t.Errorf("%s exposes the row's session id — it is the ownership capability", call.method)
		}
		if !strings.Contains(string(resp.Result), "los55b-leak") {
			t.Errorf("%s lost the agent entirely: %s", call.method, resp.Result)
		}
	}
}

// TestLos55bResumeWithExplicitClaudeSessionBindsTheProcess: the spawn path must
// not use the resume session as the ownership key. Before the fix it passed the
// explicit claude session as ownerSession, the row's own session_id never matched,
// and the freshly started backend was torn down while the handler answered
// "resuming" (cold review F1, YM-356).
func TestLos55bResumeWithExplicitClaudeSessionBindsTheProcess(t *testing.T) {
	h, s := mustHandler(t)
	spawn := stubRestartSpawnSync(t, "5")
	// This row runs the claude backend, so the identity gate needs a cmdline
	// naming it (the helper stubs the opencode one).
	stubProcCmdline(t, "/home/tester/.claude/local/claude\x00--session-id\x00sess-row-owner")
	// The resume derives the socket path from dataDir; a TempDir path is longer
	// than a unix socket allows, and the bridge would fail before the bind.
	h.dataDir = shortSockDir(t)
	h.disableAgentProcesses = false
	dir := t.TempDir()

	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-resume-explicit", Project: "los55b-project", Section: "resume-explicit",
		SessionID: "sess-row-owner", Status: "stopped", Backend: "claude",
		WorkDir: dir, CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleResumeAgent(map[string]any{
		"to": "los55b-resume-explicit", "session_id": "claude-explicit-session",
	})
	if resp.Error != "" {
		t.Fatalf("resume_agent refused the row: %s", resp.Error)
	}

	var got *storage.Agent
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := s.AgentGet("los55b-resume-explicit"); err == nil && a != nil && a.PID != 0 {
			got = a
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the resumed process was never bound — the spawn path used a session id the row does not carry")
	}
	if want := spawn.PID(); want == 0 {
		t.Fatal("the resume created no backend process at all")
	} else if got.PID != want {
		t.Errorf("row pid = %d, want the started backend %d", got.PID, want)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running", got.Status)
	}
}

func los55bStopAllResult(t *testing.T, h *Handler, project string) map[string]any {
	t.Helper()
	resp := h.Handle(Request{Method: "stop_all_agents", Params: map[string]any{"project": project}})
	if resp.Error != "" {
		t.Fatalf("stop_all_agents(%s): %s", project, resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode stop_all_agents result %s: %v", resp.Result, err)
	}
	return result
}

// los55bStopAllReport finds the per-agent report for one id in a stop_all reply.
func los55bStopAllReport(t *testing.T, result map[string]any, agentID string) map[string]any {
	t.Helper()
	rows, ok := result["agents"].([]any)
	if !ok {
		t.Fatalf("stop_all_agents carries no per-agent report: %v", result)
	}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if m["agent_id"] == agentID {
			return m
		}
	}
	t.Fatalf("no report for agent %s in %v", agentID, rows)
	return nil
}

// TestLos55bStopAllReportsProcessAndSession: a bulk stop wrote "stopped" without
// looking at what it left behind, which is exactly how the incident symptom
// reappeared (row closed, process still working). The reply and the row must name
// the residual (YM-356).
func TestLos55bStopAllReportsProcessAndSession(t *testing.T) {
	h, s := mustHandler(t)
	const project = "los55b-bulk-project"
	prevSignal := agentProcSignal
	agentProcSignal = func(int, syscall.Signal) error { return nil }
	t.Cleanup(func() { agentProcSignal = prevSignal })

	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-bulk-dead", Project: project, Section: "bulk-dead",
		Status: "running", Backend: "claude", PID: los38sDeadPID(t),
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate(dead): %v", err)
	} // A live process the identity gate accepts and that survives SIGTERM: the
	// residual process from the incident.
	survivorPID := los38sSigtermIgnoringProcess(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-bulk-survivor", Project: project, Section: "bulk-survivor",
		Status: "running", Backend: "claude", PID: survivorPID,
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate(survivor): %v", err)
	}

	result := los55bStopAllResult(t, h, project)

	if got, _ := result["stopped"].(float64); got != 2 {
		t.Fatalf("stopped = %v, want 2", result["stopped"])
	}

	deadReport := los55bStopAllReport(t, result, "los55b-bulk-dead")
	if gone, ok := deadReport["process_gone"].(bool); !ok || !gone {
		t.Errorf("dead row: process_gone = %v, want true", deadReport["process_gone"])
	}
	if got := deadReport["session_active"]; got != "unknown" {
		t.Errorf("dead row: session_active = %v, want unknown without a bound opencode session", got)
	}

	liveReport := los55bStopAllReport(t, result, "los55b-bulk-survivor")
	if gone, ok := liveReport["process_gone"].(bool); !ok || gone {
		t.Errorf("survivor: process_gone = %v, want false", liveReport["process_gone"])
	}
	if residual, _ := liveReport["residual"].(string); !strings.Contains(residual, "still held") {
		t.Errorf("survivor: residual = %q, want the still-held process named", residual)
	}

	row, err := s.AgentGet("los55b-bulk-survivor")
	if err != nil || row == nil {
		t.Fatalf("AgentGet(survivor): %v", err)
	}
	if !strings.Contains(row.Progress, "still held") {
		t.Errorf("row progress = %q, the durable claim must carry the residual", row.Progress)
	}
}

// TestLos55bResumeRefusesAGoneWorkDir: the restart paths refuse to start a
// backend outside any repository when the recorded work dir vanished. The resume
// path silently fell back to the project path or the daemon's run dir, so the
// invariant held on two of three start paths (YM-356).
func TestLos55bResumeRefusesAGoneWorkDir(t *testing.T) {
	h, s := mustHandler(t)
	gone := filepath.Join(t.TempDir(), "removed-worktree")

	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-resume-gone", Project: "los55b-resume-project", Section: "resume-gone",
		SessionID: "sess-los55b-gone", Status: "stopped", Backend: "claude",
		WorkDir: gone,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleResumeAgent(map[string]any{"to": "los55b-resume-gone"})
	if resp.Error == "" {
		t.Fatal("resume_agent accepted a row whose recorded working directory is gone")
	}
	if !strings.Contains(resp.Error, "gone") && !strings.Contains(resp.Error, "no working directory") {
		t.Errorf("error does not name the reason: %s", resp.Error)
	}
	row, err := s.AgentGet("los55b-resume-gone")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "stopped" {
		t.Errorf("status = %q, a refused resume must not rewrite the row", row.Status)
	}
}

// TestLos55bResumeKeepsAnExistingWorkDir: the negative control. A row whose
// recorded directory is still there must resume exactly as before.
func TestLos55bResumeKeepsAnExistingWorkDir(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-resume-live", Project: "los55b-resume-project", Section: "resume-live",
		SessionID: "sess-los55b-live", Status: "stopped", Backend: "claude",
		WorkDir: dir,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleResumeAgent(map[string]any{"to": "los55b-resume-live"})
	if resp.Error != "" {
		t.Fatalf("resume_agent refused a row with an existing work dir: %s", resp.Error)
	}
	row, err := s.AgentGet("los55b-resume-live")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "pending" {
		t.Errorf("status = %q, want pending after an accepted resume", row.Status)
	}
	if row.WorkDir != dir {
		t.Errorf("work_dir = %q, want the existing %q", row.WorkDir, dir)
	}
}

func TestLos55bSummarizeErrorsKeepsValidUTF8(t *testing.T) {
	detail := strings.Repeat("a", 139) + "ä" + "tail"
	msg := summarizeErrors(ValidationResult{
		FieldErrors: []FieldError{{Phase: 5, Field: "cold-review", Detail: detail}},
	})
	if !utf8.ValidString(msg) {
		t.Fatalf("summarizeErrors produced invalid UTF-8: %q", msg)
	}
}

// TestLos55bListAgentsKeepsTheNonSecretRowFields: the agentToMap projection is the
// public shape of a row. Dropping the row session id was the point; dropping
// model, theme and proxy_thread_id as well is an API regression — nothing about
// them is secret, and list_agents used to return them (orchestrator follow-up).
func TestLos55bListAgentsKeepsTheNonSecretRowFields(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-fields", Project: "los55b-project", Section: "fields",
		SessionID: "sess-los55b-secret", Status: "running", Backend: "opencode",
		Model: "zai/glm-5.2", Theme: "yesmem-loop", ProxyThreadID: "thread-abc",
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"list_agents", map[string]any{"project": "los55b-project"}},
		{"get_agent", map[string]any{"to": "los55b-fields"}},
	} {
		resp := h.Handle(Request{Method: call.method, Params: call.params})
		if resp.Error != "" {
			t.Fatalf("%s: %s", call.method, resp.Error)
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Result, &body); err != nil {
			t.Fatalf("%s: decode %s: %v", call.method, resp.Result, err)
		}
		row := body
		if call.method == "list_agents" {
			rows, ok := body["agents"].([]any)
			if !ok || len(rows) != 1 {
				t.Fatalf("list_agents returned %v", body["agents"])
			}
			row = rows[0].(map[string]any)
		}
		want := map[string]string{
			"model":           "zai/glm-5.2",
			"theme":           "yesmem-loop",
			"proxy_thread_id": "thread-abc",
		}
		for field, value := range want {
			if got := row[field]; got != value {
				t.Errorf("%s: %s = %v, want %q — the non-secret row fields must survive the projection", call.method, field, got, value)
			}
		}
		if _, leaked := row["session_id"]; leaked {
			t.Errorf("%s: session_id is back in the public projection", call.method)
		}
	}
}

// TestLos55bCrashClaimIsACompareAndSet: the crash recovery reads a snapshot and
// then destroys session data and starts a replacement. A stop (or a bulk stop)
// that closes the row in between must win, so the recovery has to CLAIM the row
// with one conditional statement instead of acting on the snapshot — a re-read
// alone leaves the window open (orchestrator follow-up).
func TestLos55bCrashClaimIsACompareAndSet(t *testing.T) {
	st, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().Format(time.RFC3339)

	if err := st.AgentCreate(storage.Agent{
		ID: "los55b-claim", Project: "p", Section: "claim", Status: "running",
		PID: 4242, Backend: "claude", CreatedAt: now,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// A stop landed first: the row is closed and holds no PID.
	if err := st.AgentUpdate("los55b-claim", map[string]any{
		"status": "stopped", "pid": 0, "stopped_at": now, "progress": "stopped",
	}); err != nil {
		t.Fatalf("AgentUpdate: %v", err)
	}
	claimed, err := st.AgentClaimCrash("los55b-claim", 4242, now, "crash #1 — respawning")
	if err != nil {
		t.Fatalf("AgentClaimCrash: %v", err)
	}
	if claimed {
		t.Fatal("the claim won against a row a stop had already closed")
	}
	row, _ := st.AgentGet("los55b-claim")
	if row == nil || row.Status != "stopped" {
		t.Fatalf("row = %+v, the refused claim must leave it untouched", row)
	}
	if row.RetryCount != 0 {
		t.Errorf("retry_count = %d, a refused claim must not bump it", row.RetryCount)
	}

	// The row is running again with the same PID: now the claim is ours, and a
	// second claim loses (only one actor may run the destructive part).
	if err := st.AgentUpdate("los55b-claim", map[string]any{"status": "running", "pid": 4242}); err != nil {
		t.Fatalf("AgentUpdate: %v", err)
	}
	if claimed, err = st.AgentClaimCrash("los55b-claim", 4242, now, "crash #1 — respawning"); err != nil || !claimed {
		t.Fatalf("claim of a running row: claimed=%v err=%v", claimed, err)
	}
	if claimed, err = st.AgentClaimCrash("los55b-claim", 4242, now, "crash #2"); err != nil || claimed {
		t.Fatalf("second claim: claimed=%v err=%v, want refused", claimed, err)
	}
	row, _ = st.AgentGet("los55b-claim")
	if row == nil || row.Status != "crashed" || row.RetryCount != 1 {
		t.Errorf("row = %+v, want status crashed and retry_count 1", row)
	}
}

// los55bClaimAndStop puts a row into the state the crash path is in after its
// claim, and then lets a stop land: exactly the interleaving the final-failure
// writer has to lose (orchestrator follow-up, cold review heartbeat.go:688).
func los55bClaimAndStop(t *testing.T, s *storage.Store, id string, pid int) storage.Agent {
	t.Helper()
	now := time.Now().Format(time.RFC3339)
	if err := s.AgentCreate(storage.Agent{
		ID: id, Project: "los55b-project", Section: id,
		SessionID: "sess-" + id, Status: "running", Backend: "claude",
		PID: pid, RetryCount: 3, CreatedAt: now,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	claimed, err := s.AgentClaimCrash(id, pid, now, "crash #4 — respawning")
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	written, err := s.AgentFinalizeStop(id, pid, now, "stopped")
	if err != nil || !written {
		t.Fatalf("finalize stop: written=%v err=%v", written, err)
	}
	row, err := s.AgentGet(id)
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	return *row
}

// los55bMessages counts the messages a run addressed to a caller.
func los55bMessages(t *testing.T, s *storage.Store, target string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE target = ?`, target).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestLos55bFinalCrashDoesNotOverwriteAStop: the final-failure write used to be
// unconditional, so a stop that landed between the crash claim and the finish was
// overwritten with "failed" and reported as a final crash (cold review, confirmed
// on code). Both the row and the notifications must lose.
func TestLos55bFinalCrashDoesNotOverwriteAStop(t *testing.T) {
	h, s := mustHandler(t)
	row := los55bClaimAndStop(t, s, "los55b-final-stop", 4242)
	row.CallerSession = "caller-los55b-final"
	if err := s.AgentUpdate(row.ID, map[string]any{"caller_session": row.CallerSession}); err != nil {
		t.Fatalf("set caller_session: %v", err)
	}

	if closed := h.reportFinalCrash(row); closed {
		t.Fatal("reportFinalCrash claimed a row a stop had already closed")
	}

	after, err := s.AgentGet(row.ID)
	if err != nil || after == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if after.Status != "stopped" {
		t.Errorf("status = %q, the stop must win over the final-failure write", after.Status)
	}
	if strings.Contains(after.Error, "retries exhausted") {
		t.Errorf("error = %q, the final-crash note overwrote the stop", after.Error)
	}
	if n := los55bMessages(t, s, row.CallerSession); n != 0 {
		t.Errorf("%d notification(s) reported a final crash for a row a stop had closed", n)
	}
}

// TestLos55bFinalCrashReportsWhenItOwnsTheRow: the negative control. Without a
// stop in between the exhausted row is closed and reported.
func TestLos55bFinalCrashReportsWhenItOwnsTheRow(t *testing.T) {
	h, s := mustHandler(t)
	now := time.Now().Format(time.RFC3339)
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-final-own", Project: "los55b-project", Section: "los55b-final-own",
		SessionID: "sess-final-own", Status: "running", Backend: "claude",
		PID: 4243, RetryCount: 3, CreatedAt: now,
		CallerSession: "caller-los55b-own",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	claimed, err := s.AgentClaimCrash("los55b-final-own", 4243, now, "crash #4 — respawning")
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	row, err := s.AgentGet("los55b-final-own")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}

	if closed := h.reportFinalCrash(*row); !closed {
		t.Fatal("reportFinalCrash refused a row it owns")
	}
	after, _ := s.AgentGet("los55b-final-own")
	if after == nil || after.Status != "failed" {
		t.Fatalf("row = %+v, want status failed", after)
	}
	if !strings.Contains(after.Error, "retries exhausted") {
		t.Errorf("error = %q, want the final-crash note", after.Error)
	}
	if n := los55bMessages(t, s, "caller-los55b-own"); n != 1 {
		t.Errorf("%d message(s) for the caller, want exactly 1 final-failure notice", n)
	}
}

// TestLos55bCrashClaimCoversPIDIdentity: the claim's pid conjunct is what makes
// "the row moved" decisive — a bind or a recycled PID must lose, and the pid-0
// crash (a running row with no process) must win. Both were untested, so a mutant
// without the conjunct or with `pid =` instead of COALESCE(pid,0) survived (cold
// review, orchestrator follow-up).
func TestLos55bCrashClaimCoversPIDIdentity(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	cases := []struct {
		name     string
		rowPID   int
		claimPID int
		want     bool
	}{
		{"pid 0 is a regular crash", 0, 0, true},
		{"a NULL pid is a crash too", -1, 0, true},
		{"matching pid wins", 4242, 4242, true},
		{"a rebound pid loses", 4242, 5252, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, err := storage.Open(":memory:")
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			rowPID := c.rowPID
			if rowPID < 0 {
				rowPID = 0
			}
			if err := st.AgentCreate(storage.Agent{
				ID: "los55b-pid", Project: "p", Section: "pid", Status: "running",
				PID: rowPID, Backend: "claude", CreatedAt: now,
			}); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}
			if c.rowPID < 0 {
				// A legacy row can carry NULL: COALESCE(pid,0) is what makes it
				// match the pid-0 crash, and `pid = ?` would silently refuse it.
				if _, err := st.DB().Exec(`UPDATE agents SET pid = NULL WHERE id = 'los55b-pid'`); err != nil {
					t.Fatalf("null the pid: %v", err)
				}
			}
			got, err := st.AgentClaimCrash("los55b-pid", c.claimPID, now, "crash #1")
			if err != nil {
				t.Fatalf("AgentClaimCrash: %v", err)
			}
			if got != c.want {
				t.Fatalf("claim(row pid %d, claim pid %d) = %v, want %v", c.rowPID, c.claimPID, got, c.want)
			}
			row, _ := st.AgentGet("los55b-pid")
			if c.want && (row.Status != "crashed" || row.RetryCount != 1) {
				t.Errorf("row = %+v, want crashed with retry_count 1", row)
			}
			if !c.want && (row.Status != "running" || row.RetryCount != 0) {
				t.Errorf("row = %+v, a refused claim must leave it running with retry_count 0", row)
			}
		})
	}
}

// TestLos55bExhaustedCrashClosesWithoutRespawn pins the retry boundary end to end:
// a running row whose counter is already at the limit is claimed, closed as failed
// with the incremented counter, gets no replacement, and reports exactly once.
func TestLos55bExhaustedCrashClosesWithoutRespawn(t *testing.T) {
	h, s := mustHandler(t)
	const caller = "caller-los55b-ladder"
	if err := s.AgentCreate(storage.Agent{
		ID: "los55b-ladder", Project: "los55b-project", Section: "ladder",
		SessionID: "sess-ladder", Status: "running", Backend: "claude",
		PID: los38sDeadPID(t), CallerSession: caller,
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	// AgentCreate does not insert retry_count (nor progress/error): the column is
	// written on later updates only. The ladder fixture needs it at the limit.
	if err := s.AgentUpdate("los55b-ladder", map[string]any{"retry_count": 3}); err != nil {
		t.Fatalf("set retry_count: %v", err)
	}

	h.crashRecovery()

	row, err := s.AgentGet("los55b-ladder")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status = %q, want failed at the retry limit", row.Status)
	}
	if row.RetryCount != 4 {
		t.Errorf("retry_count = %d, want 4 (the claim counts this attempt)", row.RetryCount)
	}
	if !strings.Contains(row.Error, "retries exhausted") {
		t.Errorf("error = %q, want the final-crash note", row.Error)
	}
	rows, err := s.AgentList("los55b-project")
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("%d rows, want 1 — the exhausted crash must not start a replacement", len(rows))
	}
	if n := los55bMessages(t, s, caller); n != 1 {
		t.Errorf("%d message(s) for the caller, want exactly 1 final-failure notice", n)
	}
}

// TestLos55bCrashClaimCountsTheAttempt pins the counter for every rung of the
// ladder: the claim is the attempt counter, so rung R becomes R+1 (0..3). The
// terminal rung (R=3, status failed, no replacement) is covered end to end by
// TestLos55bExhaustedCrashClosesWithoutRespawn.
func TestLos55bCrashClaimCountsTheAttempt(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	for rung := 0; rung <= 3; rung++ {
		st, err := storage.Open(":memory:")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		if err := st.AgentCreate(storage.Agent{
			ID: "los55b-rung", Project: "p", Section: "rung", Status: "running",
			PID: 4242, Backend: "claude", CreatedAt: now,
		}); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
		if err := st.AgentUpdate("los55b-rung", map[string]any{"retry_count": rung}); err != nil {
			t.Fatalf("set retry_count: %v", err)
		}
		if claimed, err := st.AgentClaimCrash("los55b-rung", 4242, now, "crash"); err != nil || !claimed {
			t.Fatalf("rung %d: claimed=%v err=%v", rung, claimed, err)
		}
		row, _ := st.AgentGet("los55b-rung")
		if row.RetryCount != rung+1 {
			t.Errorf("rung %d: retry_count = %d, want %d", rung, row.RetryCount, rung+1)
		}
		st.Close()
	}
}

// TestLos55bFinalCrashSuppressesArmedEscalation: the stop-in-between case has to
// suppress the DEAD_AGENT escalation too, not only the FAILED context. A yesloop
// section with partial phase work arms that escalation, so the test proves the
// branch behind the close rather than just the message count of a quiet path
// (cold review 5.2 Minor).
func TestLos55bFinalCrashSuppressesArmedEscalation(t *testing.T) {
	h, s := mustHandler(t)
	const id = "los55b-final-escalation"
	now := time.Now().Format(time.RFC3339)
	if err := s.AgentCreate(storage.Agent{
		ID: id, Project: "los55b-project", Section: "yesloop-" + id,
		SessionID: "sess-" + id, Status: "running", Backend: "claude",
		PID: 4244, RetryCount: 3, CallerSession: "caller-los55b-escalation",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	// Partial phase work: one completed phase, so the escalation is armed.
	if err := s.ScratchpadWrite("los55b-project", "yesloop-"+id,
		"### Phase 1: ANALYZE\n**Status:** COMPLETE\n**Goal understood:** x\n", "owner"); err != nil {
		t.Fatalf("ScratchpadWrite: %v", err)
	}
	// Prove the branch is live: a valid row produces two notifications
	// (escalation + context), so a count of zero below is meaningful.
	if claimed, err := s.AgentClaimCrash(id, 4244, now, "crash #4"); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if written, err := s.AgentFinalizeStop(id, 4244, now, "stopped"); err != nil || !written {
		t.Fatalf("finalize stop: written=%v err=%v", written, err)
	}
	row, err := s.AgentGet(id)
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}

	if closed := h.reportFinalCrash(*row); closed {
		t.Fatal("reportFinalCrash claimed a row a stop had closed")
	}
	if n := los55bMessages(t, s, "caller-los55b-escalation"); n != 0 {
		t.Errorf("%d message(s) after a lost close, want 0 (escalation and context are both gated)", n)
	}
}

// TestLos55bArmedEscalationActuallyFires is the positive control for the test
// above: without the stop the same fixture reports twice.
func TestLos55bArmedEscalationActuallyFires(t *testing.T) {
	h, s := mustHandler(t)
	const id = "los55b-final-escalation-own"
	now := time.Now().Format(time.RFC3339)
	if err := s.AgentCreate(storage.Agent{
		ID: id, Project: "los55b-project", Section: "yesloop-" + id,
		SessionID: "sess-" + id, Status: "running", Backend: "claude",
		PID: 4245, RetryCount: 3, CallerSession: "caller-los55b-esc-own",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.ScratchpadWrite("los55b-project", "yesloop-"+id,
		"### Phase 1: ANALYZE\n**Status:** COMPLETE\n**Goal understood:** x\n", "owner"); err != nil {
		t.Fatalf("ScratchpadWrite: %v", err)
	}
	if claimed, err := s.AgentClaimCrash(id, 4245, now, "crash #4"); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	row, err := s.AgentGet(id)
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if closed := h.reportFinalCrash(*row); !closed {
		t.Fatal("reportFinalCrash refused a row it owns")
	}
	if n := los55bMessages(t, s, "caller-los55b-esc-own"); n != 2 {
		t.Errorf("%d message(s), want 2 (DEAD_AGENT escalation + FAILED context) — without this control the suppression test is vacuous", n)
	}
}

// TestLos55bCloseIfCrashedRefusesANonCrashedRow pins the predicate the final
// close relies on: only a row that is still crashed may be closed. A resume
// cannot move a crashed row into "pending" (handleResumeAgent refuses that
// status), and this test documents the store side of that argument.
func TestLos55bCloseIfCrashedRefusesANonCrashedRow(t *testing.T) {
	st, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().Format(time.RFC3339)
	if err := st.AgentCreate(storage.Agent{
		ID: "los55b-close-pred", Project: "p", Section: "pred", Status: "pending",
		PID: 0, Backend: "claude", CreatedAt: now,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	closed, err := st.AgentCloseIfCrashed("los55b-close-pred", "final crash")
	if err != nil {
		t.Fatalf("AgentCloseIfCrashed: %v", err)
	}
	if closed {
		t.Fatal("the close won against a row that was not crashed")
	}
	row, _ := st.AgentGet("los55b-close-pred")
	if row == nil || row.Status != "pending" || row.Error != "" {
		t.Errorf("row = %+v, the refused close must leave it untouched", row)
	}
}
