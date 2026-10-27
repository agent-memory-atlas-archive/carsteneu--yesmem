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

// YM-436 finding 1: code_nav_mode knows three values, but only "off" was ever
// compared, so "nudge" (documented as "hint only") took the block path and ended the
// call with exit 2. A configuration value that does the opposite of its name is a
// trap: whoever sets nudge wants a suggestion, not a lock.
//
// blockCodeNav ends the process with os.Exit(2) and there is no exit seam, so the
// proof runs the real entry point in a child process, as the YM-431 test does. The
// value is hand-written configuration, so the capital spelling is part of the case.
func TestLos60cCodeNavNudgeHintsInsteadOfBlocking(t *testing.T) {
	if os.Getenv("LOS60C_NUDGE_CHILD") == "1" {
		dir := os.Getenv("LOS60C_HOOK_DATA_DIR")
		if dir == "" {
			// Without the data dir the child would read a relative database and
			// prove nothing; fail loudly instead of passing.
			os.Exit(1)
		}
		RunCheck(dir)
		// The real entry point returns and the process exits 0; the test binary
		// would append its own "PASS" line to the hook's answer, so the child ends
		// here with the exit code the hook produced.
		os.Exit(0)
	}

	for _, mode := range []string{"nudge", "Nudge", "  NUDGE "} {
		t.Run(mode, func(t *testing.T) {
			hint := los60cRunHookWithMode(t, mode)
			if strings.Contains(hint, "BLOCKED") {
				t.Errorf("the hint still carries a block verdict: %q", hint)
			}
			if !strings.HasPrefix(hint, "HINT:") {
				t.Errorf("the hint does not announce itself as a hint: %q", hint)
			}
			if !strings.Contains(hint, "check.go") {
				t.Errorf("the hint does not name the indexed file: %q", hint)
			}
		})
	}
}

// los60cRunHookWithMode runs the real entry point for one config value and returns
// the additionalContext it answered with. An exit code other than 0 means the mode
// still blocked, which is the failure this test exists for.
func los60cRunHookWithMode(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	config := "proxy:\n  code_nav_mode: \"" + mode + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
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
	store.Close()

	payload, err := json.Marshal(HookInput{
		SessionID: "sess-los60c",
		CWD:       dir,
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"cat internal/hooks/check.go"}`),
	})
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos60cCodeNavNudgeHintsInsteadOfBlocking$")
	cmd.Env = append(os.Environ(), "LOS60C_NUDGE_CHILD=1", "LOS60C_HOOK_DATA_DIR="+dir)
	cmd.Stdin = strings.NewReader(string(payload))
	out, runErr := cmd.Output()
	if cmd.ProcessState == nil {
		t.Fatalf("test child did not start: %v", runErr)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("code_nav_mode %q must not block: exit %d, output %q", mode, code, out)
	}
	if strings.Contains(string(out), `"decision":"block"`) {
		t.Fatalf("nudge mode still reported a block decision: %q", out)
	}

	var answer struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("hook output is not one JSON answer (%v): %q", err, out)
	}
	return answer.HookSpecificOutput.AdditionalContext
}

// The hint names a path the model wrote itself, so it must not carry invisible or
// direction-changing characters into the terminal. NavHint passes the value through
// messageSafe; this pins that the new output surface is covered like the block one.
func TestLos60cNavHintStripsInvisibleCharacters(t *testing.T) {
	// U+202E right-to-left override, U+2060 word joiner, U+FEFF zero-width no-break
	// space, U+061C arabic letter mark, U+00AD soft hyphen.
	const invisible = "\u202e\u2060\ufeff\u061c\u00ad"
	invisibleRunes := []rune{0x202e, 0x2060, 0xfeff, 0x061c, 0x00ad}
	cmd := "cat internal/a" + invisible + "b.go"
	indexed := func(project, path string) bool { return true }

	hint, ok := NavHint(cmd, "/tmp/proj", "/tmp/proj", indexed)
	if !ok {
		t.Fatal("the hint must answer for an indexed file")
	}
	for _, r := range invisibleRunes {
		if strings.ContainsRune(hint, r) {
			t.Errorf("hint carries %U unsanitized: %q", r, hint)
		}
	}
	if !strings.Contains(hint, "ab.go") {
		t.Errorf("the visible part of the name must survive, got: %q", hint)
	}

	// A quote or backtick would end the quoted pseudo-call in the message. The
	// parser drops such an argument before it reaches a target, so the sanitizer is
	// pinned directly here.
	if got := messageSafe("a\"b" + invisible + "c`d"); strings.ContainsAny(got, "\"`") || strings.Contains(got, invisible) {
		t.Errorf("messageSafe kept a character it must strip: %q", got)
	}
}

// YM-436 finding 4, seen from the guard: the index is keyed under the checkout, but
// its rows are written by the scanner relative to the directory it walked. A command
// that runs at the checkout root names a file the way the checkout spells it, while
// a scan rooted at <checkout>/internal stored "daemon/wiki_tick.go". The lookup has
// to resolve that spelling, or the row is unreachable for exactly this command.
func TestLos60cCodeNavBlocksWithASubtreeRootedIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(root, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	project := codescan.ScanCacheKey(root)
	scanJSON := `{"Packages":[{"Files":[{"Path":"daemon/wiki_tick.go"}]}]}`
	if err := store.SaveProjectScan(&storage.ProjectScanRow{
		Project:  project,
		ScanJSON: scanJSON,
		GitHead:  "head1234",
	}); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	reason, block := CheckCodeNav(
		"cat internal/daemon/wiki_tick.go",
		root,
		project,
		"sess-los60c",
		store.IsTreeInCodeIndex,
		false,
	)
	if !block {
		t.Fatal("a command on the checkout root must reach a scan rooted below it")
	}
	if !strings.Contains(reason, "wiki_tick.go") {
		t.Errorf("block message must name the file, got: %s", reason)
	}

	// The other side: a file the index does not know stays unblocked.
	if _, block := CheckCodeNav(
		"cat internal/daemon/other.go",
		root,
		project,
		"sess-los60c",
		store.IsTreeInCodeIndex,
		false,
	); block {
		t.Error("an unindexed file must not block")
	}
}
