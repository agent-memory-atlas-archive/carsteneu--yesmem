package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-359 (OPS-88, key 1): loadCapMeta read every active cap row regardless of
// its project, so the row of one project answered an execute_cap coming from
// another. The caller's project is the only thing that may reach a bound row;
// the error text tells the two apart, because a found row is refused by the
// sandbox lock while a skipped one is simply not found.
func TestLos35fExecuteCapIgnoresAForeignProjectRow(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_foreign_row", "/tmp/los35f-proj-a", "echo foreign", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_foreign_row",
		"fn":      "run",
		"args":    "{}",
		"project": "/tmp/los35f-proj-b",
	})

	if resp.Error == "" {
		t.Fatalf("execute_cap ran a cap bound to another project: %+v", resp)
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("a row of another project answered the lookup: %s", resp.Error)
	}
}

// The fix must not overshoot: the same project still reaches its own row and
// runs its body.
func TestLos35fExecuteCapStillReachesItsOwnProjectRow(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_own_row", "/tmp/los35f-proj-a", "echo own", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_own_row",
		"fn":      "run",
		"args":    "{}",
		"project": "/tmp/los35f-proj-a",
	})

	if resp.Error != "" {
		t.Fatalf("the own row was not reached: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "own") {
		t.Fatalf("the own row did not run, result = %s", resp.Result)
	}
}

// A caller without any project context reaches user-scoped caps only. This is
// the fail-closed half: an unnamed caller must not be able to pick a bound row.
func TestLos35fUnnamedCallerReachesNoBoundRow(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_unnamed", "/tmp/los35f-proj-a", "echo bound", "", "project")

	resp := h.handleExecuteCap(map[string]any{"name": "los35f_unnamed", "fn": "run", "args": "{}"})

	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("a bound row answered a caller without a project: %q", resp.Error)
	}
}

// A user-scoped cap stays reachable from everywhere, including a caller without
// a project.
func TestLos35fGlobalCapStaysReachable(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_global", "", "echo global-body", "", "user")

	resp := h.handleExecuteCap(map[string]any{"name": "los35f_global", "fn": "run", "args": "{}"})

	if resp.Error != "" {
		t.Fatalf("a user-scoped cap was blocked: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "global-body") {
		t.Fatalf("the user-scoped cap did not run, result = %s", resp.Result)
	}
}

// The own row wins over a user-scoped row of the same name: otherwise a project
// could never shadow a global cap it replaced.
func TestLos35fOwnRowWinsOverAGlobalRow(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_shadowed", "", "echo global-body", "", "user")
	los35eInsertCapRow(t, h, "los35f_shadowed", "/tmp/los35f-proj-a", "echo project-body", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_shadowed",
		"fn":      "run",
		"args":    "{}",
		"project": "/tmp/los35f-proj-a",
	})

	if resp.Error != "" {
		t.Fatalf("execute failed: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "project-body") {
		t.Fatalf("the global row answered a project call, result = %s", resp.Result)
	}
}

// MCP calls carry no project parameter; the injected cwd is the fallback when the
// session row is not readable.
func TestLos35fCwdNamesTheCallersProject(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_cwd_row", "/tmp/los35f-proj-c", "echo cwd-body", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name": "los35f_cwd_row",
		"fn":   "run",
		"args": "{}",
		"_cwd": "/tmp/los35f-proj-c/.worktrees/checkout",
	})

	if resp.Error != "" {
		t.Fatalf("the cwd did not name the caller's project: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "cwd-body") {
		t.Fatalf("the bound cap did not run, result = %s", resp.Result)
	}
}

// The session row is the source that carries in production: an MCP call names no
// project, and the session id arrives from the proxy. It has to resolve to the
// project that owns the row.
func TestLos35fTrustedSessionNamesTheCallersProject(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, store := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_session_row", "/tmp/los35f-proj-s", "echo session-body", "", "project")
	if err := store.UpsertSession(&models.Session{ID: "los35f-session", Project: "/tmp/los35f-proj-s"}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	resp := h.handleExecuteCap(map[string]any{
		"name":        "los35f_session_row",
		"fn":          "run",
		"args":        "{}",
		"_session_id": "los35f-session",
	})

	if resp.Error != "" {
		t.Fatalf("the session did not name the caller's project: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "session-body") {
		t.Fatalf("the bound cap did not run, result = %s", resp.Result)
	}
}

// Two repositories can share a basename. The basename alone made them the same
// project, which is the very leak this ticket closes, one directory level deeper.
func TestLos35fSameBasenameInAnotherRepositoryIsForeign(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	rowProject := filepath.Join(t.TempDir(), "yesmem")
	callerProject := filepath.Join(t.TempDir(), "yesmem")
	los35eInsertCapRow(t, h, "los35f_same_basename", rowProject, "echo elsewhere", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_same_basename",
		"fn":      "run",
		"args":    "{}",
		"project": callerProject,
	})

	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("a same-named repository in another root answered the call: %q", resp.Error)
	}
}

// A worktree is the same project as its main checkout, so the row stays reachable
// from inside the worktree.
func TestLos35fWorktreeOfTheSameRepositoryReachesTheRow(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	root := filepath.Join(t.TempDir(), "yesmem")
	los35eInsertCapRow(t, h, "los35f_worktree_row", root, "echo worktree-body", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name": "los35f_worktree_row",
		"fn":   "run",
		"args": "{}",
		"_cwd": filepath.Join(root, ".worktrees", "some-loop"),
	})

	if resp.Error != "" {
		t.Fatalf("the worktree of the same repository was treated as foreign: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "worktree-body") {
		t.Fatalf("the bound cap did not run, result = %s", resp.Result)
	}
}

// A caller that states a bare name names no repository, so it must not reach a row
// that is bound to a full path: otherwise "yesmem" picks up the cap of any checkout
// of that name, the same leak this ticket closes one level up.
func TestLos35fBareNameCallerReachesNoPathBoundRow(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_path_bound", "/tmp/los35f-elsewhere", "echo elsewhere", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_path_bound",
		"fn":      "run",
		"args":    "{}",
		"project": "los35f-elsewhere",
	})

	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("a bare name reached a path-bound row: %q", resp.Error)
	}
}

// The disk import binds a CAP.md to its directory name, not to a repository: a row
// stored that way has to stay reachable for a caller that also names only the
// project name, which is what the importer itself writes.
func TestLos35fBareNameCallerReachesANameBoundRow(t *testing.T) {
	isolateHome(t)
	los35fRequireAijail(t)
	h, _ := mustHandler(t)
	los35eInsertCapRow(t, h, "los35f_name_bound", "los35f-imported", "echo name-body", "", "project")

	resp := h.handleExecuteCap(map[string]any{
		"name":    "los35f_name_bound",
		"fn":      "run",
		"args":    "{}",
		"project": "los35f-imported",
	})

	if resp.Error != "" {
		t.Fatalf("a name-bound row did not answer a name-bound caller: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "name-body") {
		t.Fatalf("the name-bound cap did not run, result = %s", resp.Result)
	}
}

// los35fRequireAijail skips a test that executes a cap body: execute_cap fails
// closed without ai-jail (handler_cap_exec.go pins FallbackUnsandboxed false), so
// on a runner without it the lookup question cannot be answered at all. The
// refusal paths above do not need this, they never reach the sandbox.
func los35fRequireAijail(t *testing.T) {
	t.Helper()
	if _, err := sandboxLookPath("ai-jail"); err != nil {
		t.Skip("ai-jail is not installed and execute_cap fails closed without it")
	}
}
