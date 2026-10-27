package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-267 (2): fireCapHandler built the command, then rebuilt it through
// exec.CommandContext to attach the deadline — and the rebuild carried only argv
// and the environment, so cmd.Dir was dropped and every cap ran in the daemon's
// cwd. Under ai-jail 2.x that cwd is the user's home, which replaces the
// sandbox's private tmpfs home with a writable bind of the real one.
func TestHandleExecuteCapKeepsTheJobWorkDir(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	los34bPinAiJailV2(t)
	isolateHome(t)
	h, _ := mustHandler(t)
	insertCapWithSandbox(t, h, "cwd_probe", "pwd", "standard")

	resp := h.handleExecuteCap(map[string]any{"name": "cwd_probe", "fn": "run", "args": "{}"})
	if resp.Error != "" {
		t.Fatalf("execute error: %s", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	got, _ := result["output"].(string)

	// The cap now runs in its own directory (one per cap, YM-286). The assertion is
	// that cmd.Dir survives the command rebuild, not which directory policy chose.
	want := filepath.Join(RuntimeDir(h.dataDir), runWorkDirName, jobDirName("cap:cwd_probe"))
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	gotResolved := got
	if resolved, err := filepath.EvalSymlinks(got); err == nil {
		gotResolved = resolved
	}
	if strings.TrimSpace(gotResolved) != want {
		t.Fatalf("cap ran in %q, want the job work dir %q", got, want)
	}
}

// los34bPinAiJailV2 pins the sandbox at the 2.x branch: only there does a run
// get the job work dir at all (0.9.x deliberately keeps the daemon's cwd). The
// binary is a stand-in that drops ai-jail's flags and execs the command the flags
// precede, so the run needs no real ai-jail and no 2.x download.
func los34bPinAiJailV2(t *testing.T) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ai-jail")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != \"bash\" ]; do shift; done\nshift\nexec bash \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write stand-in ai-jail: %v", err)
	}

	origLookup, origProbe := sandboxLookPath, sandboxVersionProbe
	sandboxLookPath = func(string) (string, error) { return binary, nil }
	sandboxVersionProbe = func(string) int { return 2 }
	t.Cleanup(func() {
		sandboxLookPath, sandboxVersionProbe = origLookup, origProbe
	})
}
