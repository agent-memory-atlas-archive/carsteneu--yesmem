package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// --- resolveSessionID fallback for opencode v2 (cwd-based) ---
//
// In opencode v2 every `yesmem mcp` child is spawned by the shared
// `opencode serve --service` process, so the child's PPID — and therefore the
// _caller_pid the MCP layer sends — is that one service PID for every session
// it hosts, never the TUI PID stored in agents.pid. The PID lookups in
// resolveSessionID can consequently not match for a v2 agent. The MCP child's
// own working directory is the session's workdir and is sent as _cwd.
//
// The fixtures seed a real session table rather than recomputing the expected
// directory from the code under test: a loop agent is spawned under its
// repository root while its session lives in a worktree, so the session store —
// not agents.project — is what identifies it.

// opencodeSharedServicePID stands in for the service PID every v2 MCP child
// reports as its parent. It matches no agent row.
const opencodeSharedServicePID = 1982100

func newStoredAgent(t *testing.T, h *Handler, a storage.Agent) *storage.Agent {
	t.Helper()
	if err := h.store.AgentCreate(a); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.AgentGet(a.ID)
	if err != nil || got == nil {
		t.Fatalf("agent %s not stored: %v", a.ID, err)
	}
	return got
}

// seedRootSession appends a root session row to the opencode.db under the HOME
// the caller already isolated. It must append, not create a new HOME — tests
// seeding two sessions need both rows in one database.
func seedRootSession(t *testing.T, dir, sessionID string) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sql := "CREATE TABLE IF NOT EXISTS session_v2 (id TEXT PRIMARY KEY, directory TEXT, parent_id TEXT, time_updated INTEGER);" +
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('%s', '%s', '', 1);", sessionID, dir)
	if out, err := exec.Command("sqlite3", filepath.Join(dbDir, "opencode.db"), sql).CombinedOutput(); err != nil {
		t.Fatalf("seed root session: %v (%s)", err, out)
	}
}

func TestResolveSessionID_OpencodeFallsBackToCallerCWD(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const worktree = "/worktrees/loop-cwd"
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-cwd", Project: "/repo/root", Section: "cwd-sec",
		SessionID: "cwd-sess", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_cwd",
	})
	seedRootSession(t, worktree, "ses_cwd")

	got := h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_cwd":          worktree,
		// The shared service PID: matches no agent, exactly as in production.
		"_caller_pid": float64(opencodeSharedServicePID),
	}, "missing")
	if got != "cwd-sess" {
		t.Fatalf("got %q, want cwd-sess resolved from the session's directory", got)
	}
}

// TestResolveSessionID_CWDMatchesWorktreeNotProject is the regression guard for
// the real fleet layout: project is the repository root, the session runs in a
// worktree. Matching agents.project would never resolve those agents.
func TestResolveSessionID_CWDMatchesWorktreeNotProject(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	agent := newStoredAgent(t, h, storage.Agent{
		ID: "agent-worktree", Project: "/repo/root", Section: "wt-sec",
		SessionID: "wt-sess", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_wt",
	})
	if agent.OpencodeSessionID != "ses_wt" {
		t.Fatalf("fixture agent not stored with its session id: %+v", agent)
	}
	seedRootSession(t, "/repo/root/.worktrees/branch-a", "ses_wt")

	got := h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_cwd":          "/repo/root/.worktrees/branch-a",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "wt-sess" {
		t.Fatalf("got %q, want wt-sess", got)
	}

	// The repository root itself is not where the agent runs.
	got = h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_cwd":          "/repo/root",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "" {
		t.Fatalf("the project root must not resolve an agent that runs in a worktree, got %q", got)
	}
}

func TestResolveSessionID_CWDFallbackIgnoresForeignWorkDirs(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-elsewhere", Project: "/repo/other", Section: "other-sec",
		SessionID: "other-sess", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_other",
	})
	seedRootSession(t, "/worktrees/other", "ses_other")

	got := h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_cwd":          "/worktrees/no-agent-here",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "" {
		t.Fatalf("got %q, want no match for a workdir without an agent", got)
	}
}

// TestResolveSessionID_CWDTwoAgentsIsAmbiguous: never guess between two running
// agents that share a workdir — a wrong answer misattributes telemetry.
func TestResolveSessionID_CWDTwoAgentsIsAmbiguous(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const worktree = "/worktrees/ambiguous"
	seedRootSession(t, worktree, "ses_amb_1")
	seedRootSession(t, worktree, "ses_amb_2")
	for i, id := range []string{"agent-amb-1", "agent-amb-2"} {
		newStoredAgent(t, h, storage.Agent{
			ID: id, Project: "/repo/ambig", Section: "amb-" + id,
			SessionID: "sess-" + id, Status: "running", Backend: "opencode",
			OpencodeSessionID: fmt.Sprintf("ses_amb_%d", i+1),
		})
	}

	got := h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_cwd":          worktree,
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "" {
		t.Fatalf("ambiguous workdir must not resolve, got %q", got)
	}
}

func TestResolveSessionID_CWDFallbackSkipsStoppedAndForeignBackends(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-stopped", Project: "/repo/stopped", Section: "stopped-sec",
		SessionID: "stopped-sess", Status: "stopped", Backend: "opencode",
		OpencodeSessionID: "ses_stopped",
	})
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-claude", Project: "/repo/claude", Section: "claude-sec",
		SessionID: "claude-sess", Status: "running", Backend: "claude",
		OpencodeSessionID: "ses_claude",
	})
	seedRootSession(t, "/worktrees/stopped", "ses_stopped")
	seedRootSession(t, "/worktrees/claude", "ses_claude")

	for _, dir := range []string{"/worktrees/stopped", "/worktrees/claude"} {
		got := h.resolveSessionID(map[string]any{
			"_source_agent": "opencode",
			"_cwd":          dir,
			"_caller_pid":   float64(opencodeSharedServicePID),
		}, "missing")
		if got != "" {
			t.Errorf("%s: expected no resolution, got %q", dir, got)
		}
	}
}

// TestResolveSessionID_ExplicitSessionIDStillWins: the cwd fallback must not
// override an authoritative id the caller named.
func TestResolveSessionID_ExplicitSessionIDStillWins(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-cwd2", Project: "/repo/root2", Section: "cwd-sec2",
		SessionID: "cwd-sess2", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_cwd2",
	})
	seedRootSession(t, "/worktrees/explicit", "ses_cwd2")

	got := h.resolveSessionID(map[string]any{
		"session_id":    "explicit-wins",
		"_session_id":   "mcp-claimed",
		"_source_agent": "opencode",
		"_cwd":          "/worktrees/explicit",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "session_id")
	if got != "explicit-wins" {
		t.Fatalf("got %q, want explicit-wins", got)
	}

	got = h.resolveSessionID(map[string]any{
		"_session_id":   "mcp-claimed",
		"_source_agent": "opencode",
		"_cwd":          "/worktrees/explicit",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "mcp-claimed" {
		t.Fatalf("got %q, want the MCP-claimed id to keep precedence", got)
	}
}
