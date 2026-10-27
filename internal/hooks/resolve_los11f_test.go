package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-279 (Los 11f, aus Los 11d): the PostToolUse hook only looked at the command
// text, so a commit that a pre-commit hook rejected, or that had nothing staged,
// resolved an open task anyway. The Bash tool_response carries stdout, stderr and
// interrupted but no exit status, so the repository has to witness the commit.

// los11fRepo creates a git repository with one commit and returns its path.
func los11fRepo(t *testing.T) string {
	t.Helper()
	// The hook resolves the repository through `git rev-parse --show-toplevel`,
	// which on macOS answers /private/var/... for a temp dir handed out as
	// /var/...; a task stored under the unresolved path would never be found.
	raw := t.TempDir()
	dir, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatalf("resolve %s: %v", raw, err)
	}
	los11fGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	los11fGit(t, dir, "add", "file.txt")
	los11fGit(t, dir, "commit", "-qm", "seed")
	return dir
}

func los11fGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=test", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	// A developer's global config can carry core.hooksPath (which would defeat the
	// rejecting fixture) or commit.gpgsign. Neither belongs in this test.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// los11fGitStdout is los11fGit for the commit itself: the output is what Claude
// Code puts into tool_response.stdout, so the tests feed the same value back.
func los11fGitStdout(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=test", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// los11fGitStdin is los11fGit with stdin, for object plumbing.
func los11fGitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=test"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// los11fRejectingCommit runs a commit that the repository's pre-commit hook
// rejects and returns what the shell saw: empty stdout, the refusal on stderr.
func los11fRejectingCommit(t *testing.T, dir, message string) (stdout, stderr string) {
	t.Helper()
	hookDir := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	hook := filepath.Join(hookDir, "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'rejected by policy' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write pre-commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("seed staged file: %v", err)
	}
	los11fGit(t, dir, "add", "staged.txt")

	full := []string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=test", "-c", "commit.gpgsign=false", "commit", "-m", message}
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err == nil {
		t.Fatalf("the pre-commit hook did not reject the commit")
	}
	return out.String(), errOut.String()
}

// los11fPrepare writes an open task into a database in a fresh dataDir.
func los11fPrepare(t *testing.T, project string) (dataDir string, taskID int64) {
	t.Helper()
	dataDir = t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	taskID, err = store.InsertLearning(&models.Learning{
		Category: "unfinished", Content: "YM-279 Commit-Resolve-Restpunkte beheben",
		Project: project, CanonicalProject: project, Confidence: 1.0,
		CreatedAt: time.Now(), ModelUsed: "test", TaskType: "task",
	})
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return dataDir, taskID
}

// los11fRunCheck feeds a PostToolUse payload to the hook core and returns what
// it would hand back to Claude.
func los11fRunCheck(t *testing.T, dataDir, cwd, command, stdout, stderr string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"session_id":      "sess-los11f",
		"cwd":             cwd,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": command},
		"tool_response":   map[string]any{"stdout": stdout, "stderr": stderr, "interrupted": false, "isImage": false},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return resolveCheckOutput(dataDir, payload)
}

// los11fResolved reports whether the task was resolved.
func los11fResolved(t *testing.T, dataDir string, taskID int64) bool {
	t.Helper()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	item, err := store.GetLearning(taskID)
	if err != nil || item == nil {
		t.Fatalf("get learning %d: %v", taskID, err)
	}
	return item.SupersededBy != nil
}

func TestRunResolveCheck_Los11f_RejectedCommitResolvesNothing(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)
	stdout, stderr := los11fRejectingCommit(t, dir, "fix(hooks): YM-279 rejected commit")

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "fix(hooks): YM-279 rejected commit"`, stdout, stderr)
	if strings.Contains(out, "auto-resolved") {
		t.Fatalf("a rejected commit must not resolve anything, hook said: %s", out)
	}
	if los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the open task was resolved by a commit that never landed")
	}
}

// Accepted residue, stated as such: a rejected commit that repeats the subject
// already at HEAD still resolves, because HEAD's subject does name the task and
// neither witness can tell the two commits apart. It needs the same subject
// twice in a row. Pinned so a future change makes the trade-off a decision
// instead of an accident.
func TestRunResolveCheck_Los11f_SilentRejectionWithTheSubjectAlreadyAtHeadResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 duplicate subject"
	los11fGit(t, dir, "commit", "--allow-empty", "-qm", subject)
	stdout, stderr := los11fRejectingCommit(t, dir, subject)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+subject+`"`, stdout, stderr)
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("the recorded residue changed: the hook said %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the recorded residue changed: the task stayed open")
	}
}

func TestRunResolveCheck_Los11f_LandedCommitStillResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 landed commit"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", subject)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+subject+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a landed commit must keep resolving, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the commit subject stayed open")
	}
}

// `git -C <dir> commit` writes into another repository while the session runs in
// its own. The witness and the resolve scope have to follow the command.
func TestRunResolveCheck_Los11f_CommitInAnotherRepoStillResolves(t *testing.T) {
	target := los11fRepo(t)
	session := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, target)

	const subject = "fix(hooks): YM-279 cross repo"
	stdout := los11fGitStdout(t, target, "commit", "--allow-empty", "-m", subject)

	out := los11fRunCheck(t, dataDir, session, `git -C `+target+` commit -m "`+subject+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a commit that landed in another repository must resolve, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the landed commit stayed open")
	}
}

// A shell `cd <repo> && git commit` moves the commit, not the session.
func TestRunResolveCheck_Los11f_CommitAfterCdStillResolves(t *testing.T) {
	target := los11fRepo(t)
	session := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, target)

	const subject = "fix(hooks): YM-279 cd repo"
	stdout := los11fGitStdout(t, target, "commit", "--allow-empty", "-m", subject)

	out := los11fRunCheck(t, dataDir, session, `cd `+target+` && git commit -m "`+subject+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a commit after cd must resolve, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the landed commit stayed open")
	}
}

// git folds the first paragraph of the message into the subject, so a wrapped
// -m must not defeat the comparison.
func TestRunResolveCheck_Los11f_WrappedSubjectStillResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const message = "fix(hooks): YM-279 wrapped\ncontinued line"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", message)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+message+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a wrapped subject must resolve, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the wrapped commit stayed open")
	}
}

// commitlint/husky ecosystems rewrite the message in a commit-msg hook. The sha
// the tool response reports is unaffected by that.
func TestRunResolveCheck_Los11f_RewrittenSubjectStillResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	hookDir := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	rewrite := "#!/bin/sh\nprintf 'chore: ' > \"$1.tmp\"\ncat \"$1\" >> \"$1.tmp\"\nmv \"$1.tmp\" \"$1\"\n"
	if err := os.WriteFile(filepath.Join(hookDir, "commit-msg"), []byte(rewrite), 0o755); err != nil {
		t.Fatalf("write commit-msg: %v", err)
	}

	const typed = "fix(hooks): YM-279 rewritten"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", typed)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+typed+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a rewritten subject must still resolve via the reported sha, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the commit stayed open")
	}
}

// A -C inside the commit message is a message, not a directory argument.
func TestRunResolveCheck_Los11f_DashCInTheMessageIsNoDirectory(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 handle -C flag"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", subject)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+subject+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a -C inside the message must not redirect the witness, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the commit stayed open")
	}
}

// YM-279 security review (Sub-phase 5.4): `git log` verifies HEAD's signature on
// every call when log.showSignature is set, and it invokes gpg.program to do it.
// Both keys come from the TARGET REPOSITORY's config, and gpg.program may be a
// repo-relative path, so a repository the hook merely points at could execute a
// program shipped with it.
func TestHeadCommit_Los11f_DoesNotRunRepoConfiguredPrograms(t *testing.T) {
	dir := los11fRepo(t)

	marker := filepath.Join(t.TempDir(), "gpg-ran")
	runner := filepath.Join(dir, "evil-gpg.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	los11fGit(t, dir, "config", "log.showSignature", "true")
	los11fGit(t, dir, "config", "gpg.program", "./evil-gpg.sh")

	// HEAD has to carry a signature header, otherwise git has nothing to verify.
	tree := strings.TrimSpace(los11fGit(t, dir, "write-tree"))
	object := "tree " + tree + "\nauthor test <t@example.com> 1700000000 +0000\ncommitter test <t@example.com> 1700000000 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQIzBAABCAAdFiEE\n -----END PGP SIGNATURE-----\n\nfix: YM-279 signed subject\n"
	hash := strings.TrimSpace(los11fGitStdin(t, dir, object, "hash-object", "-w", "-t", "commit", "--stdin"))
	los11fGit(t, dir, "update-ref", "HEAD", hash)

	head, err := headCommit(dir)
	if err != nil {
		t.Fatalf("headCommit: %v", err)
	}
	if head.sha != hash || head.subject != "fix: YM-279 signed subject" {
		t.Fatalf("headCommit = %q, %q", head.sha, head.subject)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("headCommit executed the program the repository config names")
	}
}

// The reflog names the operation that produced HEAD, which is what keeps a
// "[branch sha]" line printed by another operation from passing as this
// command's commit.
func TestHeadCommit_Los11f_ReportsTheReflogOperation(t *testing.T) {
	dir := los11fRepo(t)

	head, err := headCommit(dir)
	if err != nil {
		t.Fatalf("headCommit: %v", err)
	}
	if head.operation != "commit" {
		t.Fatalf("operation = %q, want commit", head.operation)
	}

	los11fGit(t, dir, "revert", "--no-edit", "HEAD")
	head, err = headCommit(dir)
	if err != nil {
		t.Fatalf("headCommit after revert: %v", err)
	}
	if head.operation != "revert" {
		t.Fatalf("a revert must report its own operation, got %q", head.operation)
	}
}

// The sha a command prints is self-reported: `git commit … || echo "[main $(git
// rev-parse HEAD)]"` names the current HEAD without creating a commit. HEAD also
// has to be younger than the command.
func TestRunResolveCheck_Los11f_PrintedShaOfAnOldHeadResolvesNothing(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 old head"
	old := time.Now().Add(-time.Hour).Format(time.RFC3339)
	seed := exec.Command("git", "-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "--allow-empty", "-m", subject)
	seed.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE="+old, "GIT_COMMITTER_DATE="+old)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed old commit: %v (%s)", err, out)
	}

	head := strings.TrimSpace(los11fGit(t, dir, "rev-parse", "HEAD"))
	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+subject+`" || echo "[main `+head+`]"`, "[main "+head+"]\n", "")
	if strings.Contains(out, "auto-resolved") {
		t.Fatalf("a printed sha of an untouched HEAD must not resolve, hook said: %q", out)
	}
	if los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task was resolved by a command that committed nothing")
	}
}

func TestCommitDir_Los11f_FollowsTheCommand(t *testing.T) {
	cases := []struct{ command, want string }{
		{`git commit -m "fix: x"`, "/session"},
		{`git -C /other commit -m "fix: x"`, "/other"},
		{`git -C ../rel commit -m "fix: x"`, "/rel"},
		{`git -c user.email=t@example.com commit -m "x"`, "/session"},
		{`cd /repo && git commit -m "x"`, "/repo"},
		{`git commit -m "handle -C flag"`, "/session"},
		{`git commit -m "x" && git -C /other log`, "/session"},
		{`git add -A && git -C /other commit -m "x"`, "/other"},
		{`git status --short && cd /repo && git commit -m "x"`, "/repo"},
	}
	for _, tc := range cases {
		if got := commitDir(tc.command, "/session"); got != tc.want {
			t.Errorf("commitDir(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

// A quiet commit prints nothing, so its stderr is the only stream left. A
// pre-commit framework that writes progress there on success must not stop the
// resolution.
func TestRunResolveCheck_Los11f_QuietCommitWithNoisyStderrResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 quiet commit"
	los11fGit(t, dir, "commit", "--allow-empty", "-qm", subject)

	out := los11fRunCheck(t, dataDir, dir, `git commit -qm "`+subject+`"`, "", "running linters\n")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a quiet commit with stderr noise must resolve, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the quiet commit stayed open")
	}
}

// Two commits in one Bash call report two shas; HEAD is the second.
func TestRunResolveCheck_Los11f_SecondCommitInOneCallResolves(t *testing.T) {
	dir := los11fRepo(t)
	dataDir, taskID := los11fPrepare(t, dir)

	const first = "chore: unrelated sweep"
	const second = "fix(hooks): YM-279 second commit"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", first)
	stdout += los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", second)

	out := los11fRunCheck(t, dataDir, dir, `git commit -m "`+second+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("the commit that is actually HEAD must resolve, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the second commit stayed open")
	}
}

// A commit made from a subdirectory of the repository closes the project's task:
// the witness is the command's directory, the scope is the repository root.
func TestRunResolveCheck_Los11f_CommitFromASubdirectoryResolves(t *testing.T) {
	dir := los11fRepo(t)
	sub := filepath.Join(dir, "internal", "hooks")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	dataDir, taskID := los11fPrepare(t, dir)

	const subject = "fix(hooks): YM-279 subdir commit"
	stdout := los11fGitStdout(t, dir, "commit", "--allow-empty", "-m", subject)

	out := los11fRunCheck(t, dataDir, sub, `git commit -m "`+subject+`"`, stdout, "")
	if !strings.Contains(out, "auto-resolved") {
		t.Fatalf("a commit from a subdirectory must resolve the project's task, hook said: %q", out)
	}
	if !los11fResolved(t, dataDir, taskID) {
		t.Fatalf("the task named by the commit stayed open")
	}
}
