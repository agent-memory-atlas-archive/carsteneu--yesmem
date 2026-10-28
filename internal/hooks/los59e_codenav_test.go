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

// YM-431 finding 1: RunCheck returned at its no-gotcha gate before the code-nav
// check, so the guard only blocked in a project that happened to carry an active
// gotcha. The same command blocked in one session and slipped through in the next.
// The check must run on its own, without any gotcha row.
//
// blockCodeNav ends the process with os.Exit(2), and there is no exit seam, so the
// proof runs the real entry point in a child process of this test binary. The child
// announces itself with its own variable: a stray data-dir variable in the parent
// environment must not turn this test into a silent pass.
func TestLos59eCodeNavBlocksInProjectWithoutActiveGotchas(t *testing.T) {
	// Child half: the parent hands over the data dir and the hook payload on stdin.
	if os.Getenv("LOS59E_HOOK_CHILD") == "1" {
		dir := os.Getenv("LOS59E_HOOK_DATA_DIR")
		if dir == "" {
			// A child without the data dir would read a relative database and prove
			// nothing; fail loudly instead of passing.
			os.Exit(1)
		}
		RunCheck(dir)
		return
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	project := codescan.ScanCacheKey(dir)
	if _, err := store.DB().Exec(
		`INSERT OR REPLACE INTO project_scan_files (project, path, base) VALUES (?, ?, ?)`,
		project, "internal/hooks/check.go", "check.go",
	); err != nil {
		t.Fatalf("seed scan index: %v", err)
	}
	store.Close()

	// Harness precondition: this project really carries no gotcha.
	check, err := storage.Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	var gotchas int
	if err := check.DB().QueryRow(`SELECT count(*) FROM learnings WHERE category = 'gotcha'`).Scan(&gotchas); err != nil {
		t.Fatalf("count gotchas: %v", err)
	}
	check.Close()
	if gotchas != 0 {
		t.Fatalf("harness precondition: project has %d gotchas, want 0", gotchas)
	}

	payload, err := json.Marshal(HookInput{
		SessionID: "sess-los59e",
		CWD:       dir,
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"cat internal/hooks/check.go"}`),
	})
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos59eCodeNavBlocksInProjectWithoutActiveGotchas$")
	cmd.Env = append(os.Environ(), "LOS59E_HOOK_CHILD=1", "LOS59E_HOOK_DATA_DIR="+dir)
	cmd.Stdin = strings.NewReader(string(payload))
	out, runErr := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		t.Fatalf("test child did not start: %v", runErr)
	}

	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("code-nav guard did not block without active gotchas: exit %d, output %q", code, out)
	}
	if !strings.Contains(string(out), `"decision":"block"`) {
		t.Fatalf("block decision missing, output %q", out)
	}
	if !strings.Contains(string(out), "check.go") {
		t.Errorf("block message does not name the indexed file, output %q", out)
	}
}

// YM-431 finding 2: CheckCodeNav resolved a target against hook.CWD and then asked
// the index with a path relative to that same directory, so a command run in a
// subdirectory never reached a file of the enclosing project: "cat ../daemon/x.go"
// left the cwd and was skipped, while the opencode plugin (which works from its
// project directory) blocked it. The containment question is about the checkout.
func TestLos59eCodeNavBlocksEscapeIntoTheEnclosingCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	sub := filepath.Join(root, "internal", "hooks")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	var probed []string
	reason, block := CheckCodeNav(
		"cat ../daemon/wiki_tick.go",
		sub,
		root,
		"sess-los59e",
		func(project, path string) bool {
			probed = append(probed, path)
			return path == "internal/daemon/wiki_tick.go"
		},
		false,
	)
	if !block {
		t.Fatalf("a navigation into the enclosing checkout must block; probed %v", probed)
	}
	if !strings.Contains(reason, "wiki_tick.go") {
		t.Errorf("block message must name the file, got: %s", reason)
	}

	// The other side of the same rule: a target outside the checkout stays out of
	// reach, exactly as the plugin's containment check does.
	if _, block := CheckCodeNav(
		"cat ../../../outside.go",
		sub,
		root,
		"sess-los59e",
		func(project, path string) bool { return true },
		false,
	); block {
		t.Error("a target outside the checkout must not be judged as a project file")
	}
}

// A scan can be rooted below the checkout: the scanner walks the caller's project
// directory and the rows are keyed under the checkout. For a command that runs in or
// below that subdirectory the index holds the subdirectory-relative spelling, so the
// lookup has to ask it too. Asking only the checkout-relative path silently stopped
// matching such an index (YM-431 review). The gap is only halved, not closed: a
// command run at the checkout root while the scan was rooted below it still asks one
// spelling the rows do not carry.
func TestLos59eCodeNavMatchesASubdirectoryRootedIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	sub := filepath.Join(root, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	var probed []string
	_, block := CheckCodeNav(
		"cat daemon/wiki_tick.go",
		sub,
		root,
		"sess-los59e",
		func(project, path string) bool {
			probed = append(probed, path)
			// The rows of a scan rooted at <root>/internal.
			return path == "daemon/wiki_tick.go"
		},
		false,
	)
	if !block {
		t.Fatalf("a subdirectory-rooted index must still match; probed %v", probed)
	}
}

// The index is anchored at the enclosing checkout, which is what the opencode plugin
// uses as well. The ascent must stop at the checkout that actually carries .git: as
// a directory in a normal clone, as a file in a linked worktree.
func TestLos59eCodeNavRoot(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	worktree := filepath.Join(root, "worktree")
	sub := filepath.Join(clone, "internal", "hooks")
	for _, dir := range []string{filepath.Join(clone, ".git"), sub, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("write gitdir file: %v", err)
	}

	cases := []struct{ name, cwd, want string }{
		{"checkout root", clone, clone},
		{"subdirectory", sub, clone},
		{"linked worktree keeps its own root", worktree, worktree},
		{"empty cwd", "", ""},
		{"relative cwd stays as written", "internal/hooks", "internal/hooks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeNavRoot(tc.cwd); got != tc.want {
				t.Errorf("codeNavRoot(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}

	t.Run("no checkout keeps the cleaned path", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "plain")
		if err := os.MkdirAll(plain, 0o755); err != nil {
			t.Fatalf("create plain: %v", err)
		}
		// An enclosing checkout above the temp directory would make this case
		// meaningless; skip instead of asserting a wrong answer.
		for dir := plain; ; {
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				t.Skipf("temp directory lies inside a checkout (%s)", dir)
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		if got := codeNavRoot(plain); got != filepath.Clean(plain) {
			t.Errorf("codeNavRoot(%q) = %q, want the cleaned path", plain, got)
		}
	})
}
