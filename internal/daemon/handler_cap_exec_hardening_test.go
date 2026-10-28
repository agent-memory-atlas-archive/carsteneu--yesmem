package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-13: cap args are rendered into the handler script. A value or key that
// reached the shell unescaped turns into executable code — `$(cmd)` in a value
// ran, and a key was spliced in as a bare shell word.
func TestHandleExecuteCap_BashArgsCannotInjectCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	los35bUnsandboxed(t, h)
	marker := filepath.Join(t.TempDir(), "pwned")

	h.handleSaveCap(map[string]any{
		"name":         "inject_probe",
		"description":  "arg injection probe",
		"handler_bash": "printf '%s\\n' \"$ARGS_msg\"",
		"scope":        "user",
	})

	resp := h.handleExecuteCap(map[string]any{
		"name": "inject_probe",
		"args": `{"msg":"$(touch ` + marker + `)"}`,
	})

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("argument injection executed: %s was created", marker)
	}
	if resp.Error != "" {
		t.Fatalf("execute error: %s", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	output, _ := result["output"].(string)
	if !strings.Contains(output, marker) {
		t.Errorf("payload must survive as literal text, output = %q", output)
	}
}

func TestHandleExecuteCap_BashArgKeysAreValidated(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	h, _ := mustHandler(t)
	los35bUnsandboxed(t, h)
	marker := filepath.Join(t.TempDir(), "keypwn")

	h.handleSaveCap(map[string]any{
		"name":         "key_probe",
		"description":  "arg key probe",
		"handler_bash": "echo done",
		"scope":        "user",
	})

	resp := h.handleExecuteCap(map[string]any{
		"name": "key_probe",
		"args": `{"a=1;touch ` + marker + `;#":"v"}`,
	})

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("argument key injection executed: %s was created", marker)
	}
	if resp.Error == "" {
		t.Fatalf("expected rejection of invalid arg key, got %+v", resp)
	}
}

// The existing contract (see TestHandleExecuteCap_BashHandler) is that the
// outer JSON quotes are removed by the shell. Harmless values must keep working
// unchanged.
func TestRenderCapBashVars_KeepsPlainValuesIntact(t *testing.T) {
	vars, err := renderCapBashVars(`{"message":"world","count":3}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(vars, `ARGS_message="world"`) {
		t.Errorf("vars = %q, want ARGS_message=\"world\"", vars)
	}
	if !strings.Contains(vars, "ARGS_count=3") {
		t.Errorf("vars = %q, want ARGS_count=3", vars)
	}
}

func TestRenderCapBashVars_NeutralizesExpansion(t *testing.T) {
	vars, err := renderCapBashVars(`{"msg":"$(id)` + "`id`" + `$HOME"}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(vars, `"$(id)`) || strings.Contains(vars, "`id`") {
		t.Errorf("expansion must be neutralized, vars = %q", vars)
	}
	if !strings.Contains(vars, `\$`) {
		t.Errorf("expected escaped dollar sign, vars = %q", vars)
	}
}

func TestRenderCapBashVars_RejectsInvalidKey(t *testing.T) {
	if _, err := renderCapBashVars(`{"bad key":"v"}`); err == nil {
		t.Error("expected error for a key that is not a shell identifier")
	}
}

// YM-14: the mapped binaries and the daemon socket must be read-only inside the
// sandbox — a cap that can rewrite the yesmem binary escapes the sandbox. Since
// YM-314 the paths are registered on the Sandbox instead of being rendered at the
// call site, so the assertion runs through the flags the sandbox renders.
func TestCapExecMountPathsAreReadOnly(t *testing.T) {
	paths := capExecMountPaths("/run/yesmem.sock", "/usr/local/bin/yesmem", "")
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want the binary and the socket (an empty binary produces no path)", paths)
	}
	los34cPinAiJail(t, 0)
	sb := NewSandbox(SandboxConfig{Enabled: true, ReadOnlyMounts: paths})

	joined := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	if strings.Contains(joined, "--rw-map") {
		t.Errorf("cap mounts must never be writable, got %v", joined)
	}
	if !strings.Contains(joined, "--map /usr/local/bin/yesmem") {
		t.Errorf("binary must be mapped read-only, got %v", joined)
	}
	if !strings.Contains(joined, "--map /run/yesmem.sock") {
		t.Errorf("daemon socket must be mapped read-only, got %v", joined)
	}
	if count := strings.Count(joined, "--map "); count != 2 {
		t.Errorf("empty binary paths must not produce a flag, got %v", joined)
	}
}

// YM-16: without ai-jail a cap that asked for a sandbox profile must not run
// unsandboxed. execute_cap builds its sandbox with FallbackUnsandboxed=false.
func TestHandleExecuteCap_FailsClosedWhenSandboxUnavailable(t *testing.T) {
	_, cleanup := withLookupSpy(t, true)
	defer cleanup()

	h, _ := mustHandler(t)
	insertCapWithSandbox(t, h, "sbx_probe", "echo ran-unsandboxed", "standard")

	resp := h.handleExecuteCap(map[string]any{"name": "sbx_probe", "fn": "run", "args": "{}"})

	if resp.Error == "" {
		t.Fatalf("expected fail-closed error, got %+v", resp)
	}
	if strings.Contains(resp.Error, "ran-unsandboxed") {
		t.Errorf("cap must not run unsandboxed: %s", resp.Error)
	}
	// Today this path fails only because the mount flags are handed to bash as
	// arguments (YM-14). The error must name the missing sandbox instead.
	if !strings.Contains(resp.Error, "sandbox") {
		t.Errorf("error must name the missing sandbox, got: %s", resp.Error)
	}
}

func insertCapWithSandbox(t *testing.T, h *Handler, name, body, sandbox string) {
	t.Helper()
	meta := CapMeta{Name: name, Scripts: []ScriptMeta{{
		Name:    "run",
		Kind:    "handler",
		Runtime: "bash",
		Body:    body,
		Sandbox: sandbox,
	}}, Version: 1, Tested: true}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category:   "cap",
		Content:    name,
		Context:    ctx,
		Confidence: 1,
		CreatedAt:  time.Now(),
		ModelUsed:  "test",
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}
