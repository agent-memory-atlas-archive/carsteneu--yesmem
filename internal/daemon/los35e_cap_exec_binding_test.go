package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-353 (OPS-87, key 1): execute_cap resolved the script's effective sandbox
// profile and then ran the body, without asking the lock that save_cap and the
// scheduler already ask. A project-bound cap whose script names sandbox=none ran
// unsandboxed through execute_cap while the same cap was refused on save and on
// the job path.
func TestLos35eProjectBoundNoneCapIsRefusedOnExecute(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	project := t.TempDir()
	los35bInsertCap(t, h, "los35e_proj_none_exec", project, "echo executed-unsandboxed", "none")

	resp := h.handleExecuteCap(map[string]any{"name": "los35e_proj_none_exec", "fn": "run", "args": "{}", "project": project})

	if resp.Error == "" {
		t.Fatalf("execute_cap ran a project-bound cap with sandbox=none: %+v", resp)
	}
	if strings.Contains(resp.Error, "executed-unsandboxed") {
		t.Errorf("the body must not run at all, error carries its output: %s", resp.Error)
	}
	if !strings.Contains(resp.Error, "sandbox") {
		t.Errorf("error must name the sandbox rule, got: %s", resp.Error)
	}
}

// The binding has two halves and a row can satisfy only the project one: a cap
// stored with a project but a user scope. A predicate joined with && would pass
// every other test here.
func TestLos35eProjectColumnAloneRefusesUnsandboxedExecute(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	project := t.TempDir()
	los35eInsertCapRow(t, h, "los35e_proj_column_none", project, "echo executed-unsandboxed", "none", "user")

	resp := h.handleExecuteCap(map[string]any{"name": "los35e_proj_column_none", "fn": "run", "args": "{}", "project": project})
	if resp.Error == "" {
		t.Fatalf("a cap row that carries a project ran unsandboxed: %+v", resp)
	}
	if strings.Contains(resp.Error, "executed-unsandboxed") {
		t.Errorf("the body must not run at all, error carries its output: %s", resp.Error)
	}
}

// The lock is narrower than "no sandbox at all": a user-scoped cap may run
// unsandboxed, which is exactly what save_cap allows for it. The guard must not
// swallow that case.
func TestLos35eUserScopedNoneCapStillRunsOnExecute(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	insertCapWithSandbox(t, h, "los35e_user_none_exec", "echo user-cap-ran", "none")

	resp := h.handleExecuteCap(map[string]any{"name": "los35e_user_none_exec", "fn": "run", "args": "{}"})
	if resp.Error != "" {
		t.Fatalf("user-scoped cap with sandbox=none must keep running: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "user-cap-ran") {
		t.Errorf("the cap did not run, result = %s", resp.Result)
	}
}

// The refusal must come before the sandbox build: newSandbox materialises mask
// placeholders, so the old order let a refused run leave its marks behind.
func TestLos35eRefusedExecuteLeavesNoSandboxMarks(t *testing.T) {
	home := isolateHome(t)
	h, _ := mustHandler(t)
	project := t.TempDir()
	los35bInsertCap(t, h, "los35e_proj_none_marks", project, "echo executed", "none")

	resp := h.handleExecuteCap(map[string]any{"name": "los35e_proj_none_marks", "fn": "run", "args": "{}", "project": project})
	if resp.Error == "" {
		t.Fatalf("execute_cap ran the cap: %+v", resp)
	}

	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("a refused run materialised the ~/.claude.json mask placeholder")
	}
	runtimeDir, err := EnsureRuntimeDir(h.dataDir)
	if err != nil {
		t.Fatalf("runtime dir: %v", err)
	}
	work := filepath.Join(runtimeDir, runWorkDirName, jobDirName("cap:los35e_proj_none_marks"))
	if _, err := os.Stat(work); err == nil {
		t.Errorf("a refused run created its work dir %s", work)
	}
}

// los35eInsertCapRow seeds one cap with an explicit project column and scope, so a
// test can take the two halves of the binding apart.
func los35eInsertCapRow(t *testing.T, h *Handler, name, project, body, sandbox, scope string) {
	t.Helper()
	meta := CapMeta{
		Name:        name,
		Description: "test cap",
		Scripts:     []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: body, Sandbox: sandbox}},
		Version:     1,
		Tested:      true,
		Scope:       scope,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category:    "cap",
		Content:     name + " — test cap",
		Context:     ctx,
		Confidence:  1,
		CreatedAt:   time.Now(),
		ModelUsed:   "test",
		TriggerRule: "cap:" + name,
		Project:     project,
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}
