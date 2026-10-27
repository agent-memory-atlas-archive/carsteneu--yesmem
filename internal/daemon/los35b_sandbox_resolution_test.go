package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/models"
)

// YM-230 (OPS-52): an omitted sandbox line is not a decision. With
// default_sandbox_profile=none the effective profile of such a script IS none,
// and the project lock has to see that — it used to compare the literal string
// "none" only, so the lock let the inherited none through.
func TestLos35bProjectBoundSaveSeesInheritedNone(t *testing.T) {
	h, _ := mustHandler(t)
	h.defaultSandboxProfile = ProfileNone
	h.sandboxDefaultSet = true

	scripts, err := json.Marshal([]ScriptMeta{{
		Name: "run", Kind: "handler", Runtime: "bash", Body: "echo hi",
	}})
	if err != nil {
		t.Fatalf("marshal scripts: %v", err)
	}
	resp := h.handleSaveCap(map[string]any{
		"name":        "los35b_inherited_none",
		"description": "project-bound cap with an unset script profile",
		"project":     t.TempDir(),
		"scripts":     string(scripts),
	})
	if resp.Error == "" {
		t.Fatal("project-bound save accepted a script whose effective profile is none")
	}
	if !strings.Contains(resp.Error, "sandbox") {
		t.Errorf("error must name the sandbox rule, got: %s", resp.Error)
	}
}

// YM-230 (OPS-52, E5): ExportAllCaps wrote `sandbox: none` for a project-scoped
// cap, and capfile.Parse refuses exactly that combination — so the export of
// such a row could never be imported again.
func TestLos35bExportOfProjectBoundNoneCapIsReimportable(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	los35bInsertCap(t, h, "los35b_proj_none", project, "echo hi", "none")

	meta, _, err := h.loadCapMeta("los35b_proj_none", project)
	if err != nil {
		t.Fatalf("load cap: %v", err)
	}
	dir := t.TempDir()
	if err := WriteCapToDisk(meta, project, dir, h.store, h.configuredDefaultSandbox()); err != nil {
		t.Fatalf("write CAP.md: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "los35b_proj_none", "CAP.md"))
	if err != nil {
		t.Fatalf("read CAP.md: %v", err)
	}

	cf, err := capfile.Parse(data)
	if err != nil {
		t.Fatalf("exported CAP.md is not parseable: %v", err)
	}
	params := CapFileToParams(*cf)
	params["project"] = project
	if resp := h.handleSaveCap(params); resp.Error != "" {
		t.Fatalf("exported CAP.md cannot be imported: %s", resp.Error)
	}
}

// YM-230 (OPS-52, E2): a project-bound cap whose script carries no sandbox line
// must stay repairable through the proposal path. Under the new resolution the
// save passes and the script runs under the effective profile; before it ran
// unsandboxed while the row looked fine.
func TestLos35bProposalRepairsProjectBoundCapWithoutSandboxLine(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	los35bInsertCap(t, h, "los35b_prop_repair", project, "echo old", "")

	proposal := CapMeta{
		Name:    "los35b_prop_repair",
		Scripts: []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: "echo repaired"}},
		Version: 1,
		Tested:  true,
	}
	ctx, err := proposal.ToJSON()
	if err != nil {
		t.Fatalf("proposal meta json: %v", err)
	}
	id, err := h.store.InsertLearning(&models.Learning{
		Category:    "cap_proposed",
		Content:     "los35b_prop_repair — auto-correct proposal",
		Context:     ctx,
		Confidence:  1,
		CreatedAt:   time.Now(),
		ModelUsed:   "test",
		TriggerRule: "cap_proposed:los35b_prop_repair",
		Project:     project,
		Keywords:    []string{"script:run"},
	})
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	resp := h.handleCapProposalDecide(Request{
		Method: "cap_proposal_decide",
		Params: map[string]any{"id": float64(id), "decision": "accept"},
	})
	if resp.Error != "" {
		t.Fatalf("proposal repair failed: %s", resp.Error)
	}

	meta, _, err := h.loadCapMeta("los35b_prop_repair", project)
	if err != nil {
		t.Fatalf("load repaired cap: %v", err)
	}
	if len(meta.Scripts) != 1 || !strings.Contains(meta.Scripts[0].Body, "repaired") {
		t.Fatalf("repair did not land: %+v", meta.Scripts)
	}
}

// YM-230 (OPS-52): the resolution chain outside a job. A script that names a
// profile keeps it; an omitted one inherits the configured default; a configured
// none stays a decision and is honored.
func TestLos35bScriptSandboxResolution(t *testing.T) {
	h, _ := mustHandler(t)

	cases := []struct {
		in   string
		want SandboxProfile
		err  bool
	}{
		{"", ProfileStandard, false},
		{"none", ProfileNone, false},
		{"standard", ProfileStandard, false},
		{"strict", ProfileStrict, false},
		{"yolo", ProfileNone, true},
	}
	for _, tt := range cases {
		got, err := h.resolveScriptSandbox(tt.in)
		if tt.err {
			if err == nil {
				t.Errorf("resolveScriptSandbox(%q) expected error, got %v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveScriptSandbox(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolveScriptSandbox(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}

	h.defaultSandboxProfile = ProfileStrict
	h.sandboxDefaultSet = true
	if got, err := h.resolveScriptSandbox(""); err != nil || got != ProfileStrict {
		t.Errorf("configured strict: resolveScriptSandbox(\"\") = %v, %v; want strict", got, err)
	}

	h.defaultSandboxProfile = ProfileNone
	if got, _ := h.resolveScriptSandbox(""); got != ProfileNone {
		t.Errorf("configured none: resolveScriptSandbox(\"\") = %v, want none", got)
	}
}

// YM-230: the startup line names per cap how many scripts inherit, and it names
// a project-bound cap whose effective profile is none instead of staying silent.
func TestLos35bLoadLogReportsInheritedProfiles(t *testing.T) {
	h, _ := mustHandler(t)
	los35bInsertCap(t, h, "los35b_inherit", t.TempDir(), "echo hi", "")
	los35bInsertCap(t, h, "los35b_explicit", t.TempDir(), "echo hi", "standard")

	if inherited, unsandboxed := logSandboxProfilesOnLoad(h); inherited != 1 || unsandboxed != 0 {
		t.Errorf("inherited=%d unsandboxed=%d, want 1 and 0", inherited, unsandboxed)
	}

	h.defaultSandboxProfile = ProfileNone
	h.sandboxDefaultSet = true
	if inherited, unsandboxed := logSandboxProfilesOnLoad(h); inherited != 0 || unsandboxed != 1 {
		t.Errorf("with a configured none: inherited=%d unsandboxed=%d, want 0 and 1", inherited, unsandboxed)
	}
}

// YM-230 (OPS-52, E5 review finding 1): with default_sandbox_profile=none a
// project-scoped cap whose script names no profile is unsandboxed too, so the
// export has to repair that case as well — otherwise the file cannot come back.
func TestLos35bExportOfInheritedNoneUnderConfiguredNoneIsReimportable(t *testing.T) {
	h, _ := mustHandler(t)
	h.defaultSandboxProfile = ProfileNone
	h.sandboxDefaultSet = true
	project := t.TempDir()
	los35bInsertCap(t, h, "los35b_proj_inherit", project, "echo hi", "")

	meta, _, err := h.loadCapMeta("los35b_proj_inherit", project)
	if err != nil {
		t.Fatalf("load cap: %v", err)
	}
	dir := t.TempDir()
	if err := WriteCapToDisk(meta, project, dir, h.store, h.configuredDefaultSandbox()); err != nil {
		t.Fatalf("write CAP.md: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "los35b_proj_inherit", "CAP.md"))
	if err != nil {
		t.Fatalf("read CAP.md: %v", err)
	}
	cf, err := capfile.Parse(data)
	if err != nil {
		t.Fatalf("exported CAP.md is not parseable: %v", err)
	}
	if got := cf.Scripts[0].Sandbox; got != "standard" {
		t.Errorf("exported sandbox = %q, want standard (the row inherits none)", got)
	}
	params := CapFileToParams(*cf)
	params["project"] = project
	if resp := h.handleSaveCap(params); resp.Error != "" {
		t.Fatalf("exported CAP.md cannot be imported: %s", resp.Error)
	}
}

// YM-230: the point of the change is that an omitted sandbox line reaches the
// sandbox. A tracing stand-in for ai-jail records whether the run went through it.
func TestLos35bCapWithoutSandboxLineRunsInTheSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	marker := los35bPinAiJail2xTrace(t)
	isolateHome(t)
	h, _ := mustHandler(t)
	insertCapWithSandbox(t, h, "los35b_sbx", "echo ok", "")

	resp := h.handleExecuteCap(map[string]any{"name": "los35b_sbx", "fn": "run", "args": "{}"})
	if resp.Error != "" {
		t.Fatalf("execute error: %s", resp.Error)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the cap ran without the sandbox binary: %v", err)
	}
}

// los35bPinAiJail2xTrace pins the sandbox at a stand-in binary that records its
// own invocation and then execs the command the flags precede. It returns the
// marker path, so a test can tell a sandboxed run from a direct one.
func los35bPinAiJail2xTrace(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ai-jail-called")
	binary := filepath.Join(t.TempDir(), "ai-jail")
	script := "#!/bin/sh\nprintf x >> '" + marker + "'\n" +
		"while [ $# -gt 0 ] && [ \"$1\" != \"bash\" ]; do shift; done\nshift\nexec bash \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write stand-in ai-jail: %v", err)
	}
	origLookup, origProbe := sandboxLookPath, sandboxVersionProbe
	sandboxLookPath = func(string) (string, error) { return binary, nil }
	sandboxVersionProbe = func(string) int { return 2 }
	t.Cleanup(func() {
		sandboxLookPath, sandboxVersionProbe = origLookup, origProbe
	})
	return marker
}

// los35bUnsandboxed makes an omitted script profile mean none for one handler.
// A test that executes a cap without a sandbox stand-in has to say that out
// loud now: an absent profile inherits the configured default (YM-230), which is
// standard. The config value is where "run unsandboxed" is decided, and it also
// covers the legacy handler_bash path, which carries no sandbox field.
func los35bUnsandboxed(t *testing.T, h *Handler) {
	t.Helper()
	h.defaultSandboxProfile = ProfileNone
	h.sandboxDefaultSet = true
}

// los35bInsertCap seeds one active cap row directly, so the test controls the
// stored sandbox value and the project binding (handleSaveCap would apply the
// lock on the way in).
func los35bInsertCap(t *testing.T, h *Handler, name, project, body, sandbox string) {
	t.Helper()
	meta := CapMeta{
		Name:        name,
		Description: "test cap",
		Scripts:     []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: body, Sandbox: sandbox}},
		Version:     1,
		Tested:      true,
		Scope:       "project",
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
