package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-442: the dismissal lifts the block, not the advice. The hook skipped the whole
// code-nav branch for a dismissing session, so code_nav_mode "nudge" said nothing in
// this layer while the opencode plugin still delivered its hint — one setting with two
// behaviours, which is the split the ticket closes.
func TestLos61dCodeNavNudgeSurvivesDismissal(t *testing.T) {
	if os.Getenv("LOS61D_NUDGE_CHILD") == "1" {
		dir := os.Getenv("LOS61D_HOOK_DATA_DIR")
		if dir == "" {
			// Without the data dir the child would read a relative database and prove
			// nothing; fail loudly instead of passing.
			os.Exit(1)
		}
		RunCheck(dir)
		os.Exit(0)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("proxy:\n  code_nav_mode: \"nudge\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	const sessionID = "sess-los61d"
	store, err := storage.Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.DB().Exec(
		`INSERT OR REPLACE INTO project_scan_files (project, path, base) VALUES (?, ?, ?)`,
		codescan.ScanCacheKey(dir), "internal/hooks/check.go", "check.go",
	); err != nil {
		t.Fatalf("seed scan index: %v", err)
	}
	// The session already lifted the block. The advice is not what it lifted.
	if err := store.DismissCodeNav(sessionID); err != nil {
		t.Fatalf("dismiss code nav: %v", err)
	}
	store.Close()

	payload, err := json.Marshal(HookInput{
		SessionID: sessionID,
		CWD:       dir,
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"cat internal/hooks/check.go"}`),
	})
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos61dCodeNavNudgeSurvivesDismissal$")
	cmd.Env = append(os.Environ(), "LOS61D_NUDGE_CHILD=1", "LOS61D_HOOK_DATA_DIR="+dir)
	cmd.Stdin = strings.NewReader(string(payload))
	out, runErr := cmd.Output()
	if cmd.ProcessState == nil {
		t.Fatalf("test child did not start: %v", runErr)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("a dismissing session must not be blocked in nudge mode: exit %d, output %q", code, out)
	}

	var answer struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("hook output is not one JSON answer (%v): %q", err, out)
	}
	hint := answer.HookSpecificOutput.AdditionalContext
	if !strings.HasPrefix(hint, "HINT:") {
		t.Fatalf("the dismissal swallowed the hint, got %q", hint)
	}
	if !strings.Contains(hint, "check.go") {
		t.Errorf("the hint does not name the indexed file: %q", hint)
	}
}
