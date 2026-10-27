package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los31dChildDataDirEnv marks the child process runCheckBlocking starts.
const los31dChildDataDirEnv = "LOS31D_HOOK_DATA_DIR"

// TestLos31DCheckChildProcess is the re-entry point for runCheckBlocking. The
// block paths end with os.Exit(2), which would take the test binary down, so the
// only way to prove a block is to ask a child process for its exit status.
func TestLos31DCheckChildProcess(t *testing.T) {
	dir := os.Getenv(los31dChildDataDirEnv)
	if dir == "" {
		t.Skip("helper process for runCheckBlocking")
	}
	RunCheck(dir)
}

// runCheckBlocking runs RunCheck in a child test process and returns the hook
// output together with the exit status.
func runCheckBlocking(t *testing.T, dataDir string, hook HookInput) (string, int) {
	t.Helper()
	payload, err := json.Marshal(hook)
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLos31DCheckChildProcess$")
	cmd.Env = append(os.Environ(), los31dChildDataDirEnv+"="+dataDir)
	cmd.Stdin = bytes.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	code := 0
	if runErr := cmd.Run(); runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			t.Fatalf("child process: %v", runErr)
		}
		code = exit.ExitCode()
	}
	return out.String(), code
}

// TestLos31DMultiEditSeesTheSettingsGuard covers finding 1 of YM-310 for the
// warning half: MultiEdit carries a file_path and a list of surgical old/new
// pairs, so it is an Edit and must reach the settings guard. Before the fix its
// name fell into the default branch, which saw no guard at all.
func TestLos31DMultiEditSeesTheSettingsGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := openTempStore(t, func(*storage.Store) {})

	out := runCheckWithHook(t, dir, HookInput{
		ToolName: "MultiEdit",
		CWD:      home,
		ToolInput: json.RawMessage(`{"file_path":"` + filepath.Join(home, ".claude", "settings.json") +
			`","edits":[{"old_string":"a","new_string":"b"}]}`),
	})
	if !strings.Contains(out, "yesmem setup") {
		t.Errorf("MultiEdit of the settings file must warn, got: %q", out)
	}
}

// TestLos31DMultiEditBlocksTheGeneratedOpsFile covers finding 1 for the blocking
// half, on the file every write path blocks today.
func TestLos31DMultiEditBlocksTheGeneratedOpsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := openTempStore(t, func(*storage.Store) {})

	out, code := runCheckBlocking(t, dir, HookInput{
		ToolName:  "MultiEdit",
		CWD:       home,
		ToolInput: json.RawMessage(`{"file_path":"` + filepath.Join(home, ".claude", "yesmem-ops.md") + `","edits":[]}`),
	})
	if code != 2 {
		t.Errorf("MultiEdit of yesmem-ops.md must be blocked, exit = %d, out = %q", code, out)
	}
	if !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("expected a block decision for the generated ops file, got: %q", out)
	}
}

// TestLos31DNotebookEditReachesTheGotchaLookup covers the second tool of finding
// 1: NotebookEdit names its target in notebook_path, and it fell into the default
// branch as well, so no gotcha was ever consulted for it.
func TestLos31DNotebookEditReachesTheGotchaLookup(t *testing.T) {
	dir := openTempStore(t, func(s *storage.Store) {
		insertGotcha(t, s, "data analysis notebook crashes the kernel", "claude_suggested", "")
	})

	out := runCheckWithHook(t, dir, HookInput{
		ToolName:  "NotebookEdit",
		CWD:       "/x/data",
		ToolInput: json.RawMessage(`{"notebook_path":"/x/data/analysis.ipynb","cell_id":"c1","new_source":"x = 1"}`),
	})
	if !strings.Contains(out, "crashes the kernel") {
		t.Errorf("a NotebookEdit must reach the gotcha lookup, got: %q", out)
	}
}

// TestLos31DMultiEditIsFileOpTool keeps the destructive-filter in step with the
// new tool names: a gotcha whose action is an rm belongs to a file op, and both
// tools can write files.
func TestLos31DMultiEditIsFileOpTool(t *testing.T) {
	g := models.Learning{Content: "rm -rf the worktree", Actions: []string{"rm -rf"}}
	for _, tool := range []string{"MultiEdit", "NotebookEdit"} {
		if !isRelevantForTool(&g, tool) {
			t.Errorf("%s must keep a destructive gotcha relevant", tool)
		}
	}
}

// TestLos31DBashWritePathsOfTheProtectedFiles covers finding 2: the shell routes
// into both protected files. A redirect, a tee, a copy or a move replaces the
// file's whole content and is blocked like a Write; an in-place editor can only
// touch what it targets and is warned about like an Edit.
func TestLos31DBashWritePathsOfTheProtectedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	ops := filepath.Join(home, ".claude", "yesmem-ops.md")

	blocked := []struct{ name, command string }{
		{"redirect with a separate operator", `echo '{}' > ` + settings},
		{"append redirect", `echo '{}' >> ` + settings},
		{"redirect on the operator", `echo '{}' >` + settings},
		{"redirect glued to its left operand", `echo x>` + settings},
		{"tilde path", `echo '{}' > ~/.claude/settings.json`},
		{"HOME variable", `echo '{}' >> $HOME/.claude/settings.json`},
		{"HOME with braces", `echo '{}' | tee ${HOME}/.claude/settings.json`},
		{"tee", `echo '{}' | tee ` + settings},
		{"copy onto it", `cp /x/other.json ` + settings},
		{"move onto it", `mv /x/settings.json ` + settings},
		{"copy into its directory", `cp /x/settings.json ` + filepath.Join(home, ".claude") + `/`},
		{"move into its directory", `mv /x/settings.json ` + filepath.Join(home, ".claude") + `/`},
		{"behind sudo", `sudo cp /x/other.json ` + settings},
		{"behind sudo with a value flag", `sudo -u root cp /x/other.json ` + settings},
		{"behind env with an assignment", `env YESMEM=1 cp /x/other.json ` + settings},
		{"behind env ignoring the environment", `env -i cp /x/other.json ` + settings},
		{"behind timeout", `timeout 30 cp /x/other.json ` + settings},
		{"behind timeout with a unit", `timeout 30s cp /x/other.json ` + settings},
		{"behind nice", `nice -n 10 cp /x/other.json ` + settings},
		{"behind xargs", `xargs -I{} cp /x/other.json ` + settings},
		{"dd with of=", `dd if=/x/other.json of=` + settings},
		{"truncate", `truncate -s 0 ` + settings},
		{"install", `install -m 600 /x/other.json ` + settings},
		{"shred", `shred ` + settings},
		{"after a change of directory", `cd ` + filepath.Join(home, ".claude") + ` && echo '{}' > settings.json`},
		{"a quoted argument holding a non-breaking space", "echo '{}' > " + settings + " \"a\u00a0b\""},
		{"inside a shell", `bash -c 'cat > ` + settings + `'`},
		{"append to the ops file", `echo '- new' >> ` + ops},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			dir := openTempStore(t, func(*storage.Store) {})
			out, code := runCheckBlocking(t, dir, HookInput{
				ToolName:  "Bash",
				CWD:       home,
				ToolInput: json.RawMessage(`{"command":` + quoteJSON(tt.command) + `}`),
			})
			if code != 2 {
				t.Errorf("%q must be blocked, exit = %d, out = %q", tt.command, code, out)
			}
			if !strings.Contains(out, `"decision":"block"`) {
				t.Errorf("expected a block decision for %q, got: %q", tt.command, out)
			}
		})
	}

	t.Run("in-place editor warns", func(t *testing.T) {
		dir := openTempStore(t, func(*storage.Store) {})
		out := runCheckWithHook(t, dir, HookInput{
			ToolName:  "Bash",
			CWD:       home,
			ToolInput: json.RawMessage(`{"command":` + quoteJSON(`sed -i 's/"a":1/"a":2/' `+settings) + `}`),
		})
		if !strings.Contains(out, "yesmem setup") {
			t.Errorf("an in-place edit of the settings file must warn, got: %q", out)
		}
	})

	notAWrite := []struct{ name, command string }{
		{"reading it", `cat ` + settings},
		{"copying it away", `cp ` + settings + ` /x/backup.json`},
		{"grepping the ops file", `grep -n hook ` + ops},
		{"a commit message quoting the command", `git commit -m "restore: cp x > ` + settings + `"`},
		{"a read with a module flag", `perl -MList::Util -e 'print 1' ` + settings},
		{"a read behind sudo", `sudo cat ` + settings},
		{"a message naming a write verb", `echo mv ` + settings},
		{"a same-named file moved within a directory", `mv /x/yesmem-ops.md /x/build/other.md`},
	}
	for _, tt := range notAWrite {
		t.Run(tt.name, func(t *testing.T) {
			dir := openTempStore(t, func(*storage.Store) {})
			// Through the child process as well: a regression that starts blocking here
			// would otherwise take the test binary down without a report of its own.
			out, code := runCheckBlocking(t, dir, HookInput{
				ToolName:  "Bash",
				CWD:       home,
				ToolInput: json.RawMessage(`{"command":` + quoteJSON(tt.command) + `}`),
			})
			if code != 0 {
				t.Errorf("%q does not write the file and must not exit %d, out = %q", tt.command, code, out)
			}
			if strings.Contains(out, "yesmem setup") || strings.Contains(out, `"decision"`) {
				t.Errorf("%q does not write the file and must stay silent, got: %q", tt.command, out)
			}
		})
	}
}

// quoteJSON is a test-local helper for embedding a shell command in a hook body.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("quoteJSON: " + err.Error())
	}
	return string(b)
}

// TestLos31DTildeKeywordsMatchAbsoluteKeywords is the host-independent half of
// finding 5: the same command written with "~" and with the expanded home must
// produce the same keywords, otherwise the destructive command class scores lower
// for one spelling than for the other.
func TestLos31DTildeKeywordsMatchAbsoluteKeywords(t *testing.T) {
	// A fixed home under /home makes the path-only fallback deterministic on any host:
	// "home" is path noise and the user segment is one by homeUserNames, so the
	// command reduces to its path words exactly as it does on a real installation.
	t.Setenv("HOME", "/home/los31d")
	home := "/home/los31d"

	tilde := extractKeywords("rm -rf ~/.claude/yesmem")
	absolute := extractKeywords("rm -rf " + home + "/.claude/yesmem")
	if len(tilde) == 0 || len(absolute) == 0 {
		t.Fatalf("harness precondition: empty keyword set, tilde=%v absolute=%v", tilde, absolute)
	}

	got := make(map[string]bool, len(tilde))
	for _, k := range tilde {
		got[k] = true
	}
	for _, k := range absolute {
		if !got[k] {
			t.Errorf("keyword %q from the absolute form is missing in the tilde form %v", k, tilde)
		}
		delete(got, k)
	}
	if len(got) > 0 {
		t.Errorf("tilde form carries keywords the absolute form does not: %v", got)
	}
}

// TestLos31DTildeAndAbsoluteHomeReachTheSameVerdict is the behavioural half of
// finding 5: the escalated gotcha about deleting the installation must decide the
// same way for both spellings of the same command.
func TestLos31DTildeAndAbsoluteHomeReachTheSameVerdict(t *testing.T) {
	t.Setenv("HOME", "/home/los31d")
	home := "/home/los31d"
	dir := openTempStore(t, func(s *storage.Store) {
		id := insertGotcha(t, s, "rm -rf "+home+"/.claude/yesmem loescht alle Learnings", "hook_auto_learned", "bash")
		for i := 0; i < blockThreshold; i++ {
			if err := s.IncrementFailCounts([]int64{id}); err != nil {
				t.Fatalf("escalate fail count: %v", err)
			}
		}
	})

	absoluteOut, absoluteCode := runCheckBlocking(t, dir, HookInput{
		ToolName:  "Bash",
		CWD:       home,
		ToolInput: json.RawMessage(`{"command":` + quoteJSON("rm -rf "+home+"/.claude/yesmem") + `}`),
	})
	if !strings.Contains(absoluteOut, "loescht alle Learnings") {
		t.Fatalf("harness precondition: the absolute form did not reach the gotcha, exit = %d, out = %q", absoluteCode, absoluteOut)
	}

	tildeOut, tildeCode := runCheckBlocking(t, dir, HookInput{
		ToolName:  "Bash",
		CWD:       home,
		ToolInput: json.RawMessage(`{"command":` + quoteJSON("rm -rf ~/.claude/yesmem") + `}`),
	})

	// The absolute form is the reference the finding names ("blocks"), so the test
	// asks for a block and fails when the guard stops producing one.
	blocked := func(out string, code int) bool {
		return code == 2 && strings.Contains(out, `"decision":"block"`)
	}
	if !blocked(absoluteOut, absoluteCode) {
		t.Fatalf("the absolute form must block, exit = %d, out = %q", absoluteCode, absoluteOut)
	}
	if !blocked(tildeOut, tildeCode) {
		t.Errorf("the tilde form must block like the absolute one: exit = %d out = %q (absolute exit = %d)",
			tildeCode, tildeOut, absoluteCode)
	}
	if !strings.Contains(tildeOut, "loescht alle Learnings") {
		t.Errorf("the tilde form must reach the escalated gotcha, got: %q", tildeOut)
	}
}

// TestLos31DReplDoesNotConsumeSaveCount covers finding 3. Every REPL call sends a
// different code body, so a changed body is the normal case and not evidence that
// a gotcha warning changed the approach. Crediting it inflated save_count the same
// way the WebFetch host change did before YM-269.
func TestLos31DReplDoesNotConsumeSaveCount(t *testing.T) {
	var id int64
	dir := openTempStore(t, func(s *storage.Store) {
		id = insertGotcha(t, s, "Bash error: `cat ab.txt` → No such file", "hook_auto_learned", "")
	})

	saveCount := func() int {
		t.Helper()
		store, err := storage.Open(filepath.Join(dir, "yesmem.db"))
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer store.Close()
		l, err := store.GetLearning(id)
		if err != nil {
			t.Fatalf("GetLearning: %v", err)
		}
		return l.SaveCount
	}

	run := func(code string) string {
		return runCheckWithHook(t, dir, HookInput{
			SessionID: "los31d-repl",
			CWD:       "/x/data",
			ToolName:  "REPL",
			ToolInput: json.RawMessage(`{"code":` + quoteJSON(code) + `}`),
		})
	}

	first := run("sh('cat ab.txt')")
	if !strings.Contains(first, "Bash error") {
		t.Fatalf("harness precondition: REPL did not inject the gotcha, got: %q", first)
	}
	run("sh('cat cd.txt')")

	if got := saveCount(); got != 0 {
		t.Errorf("a second REPL body must not credit a save, save_count = %d, want 0", got)
	}
}

// TestLos31DGotchaCountCapStopsRepeatInjection covers finding 4: the per-session
// injection cap had no test. It already existed, so this pins the behaviour
// instead of fixing it: three injections per session and gotcha, then silence,
// and the counter belongs to the session.
func TestLos31DGotchaCountCapStopsRepeatInjection(t *testing.T) {
	dir := openTempStore(t, func(s *storage.Store) {
		insertGotcha(t, s, "sandbox blocked by the ai-jail profile", "claude_suggested", "")
	})

	run := func(session string) string {
		return runCheckWithHook(t, dir, HookInput{
			SessionID: session,
			CWD:       "/x/data",
			ToolName:  "Bash",
			ToolInput: json.RawMessage(`{"command":"go test ./sandbox"}`),
		})
	}

	for i := 1; i <= 3; i++ {
		if out := run("los31d-cap"); !strings.Contains(out, "ai-jail profile") {
			t.Fatalf("injection %d of 3 is missing, got: %q", i, out)
		}
	}
	if out := run("los31d-cap"); out != "" {
		t.Errorf("the fourth injection in the same session must be capped, got: %q", out)
	}
	if out := run("los31d-cap-other"); !strings.Contains(out, "ai-jail profile") {
		t.Errorf("the cap must be per session, another session got: %q", out)
	}
}
