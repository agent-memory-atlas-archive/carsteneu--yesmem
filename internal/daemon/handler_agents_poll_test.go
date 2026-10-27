package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger into a buffer for the duration
// of the test and restores it afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestPollOpencodeSessionID_LogsWhenDBMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := captureLog(t)

	if got := pollOpencodeSessionID("/some/workdir", time.Time{}, time.Time{}, 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "opencode.db") || !strings.Contains(out, "not found") {
		t.Errorf("expected log about missing opencode.db, got: %q", out)
	}
}

// TestPollOpencodeSessionID_LogsTimeoutWithCandidates: the timeout hint must
// name where opencode is creating sessions *now*. The v1 `session` table has
// been frozen since 2026-10-12, so a hint read from it points at directories no
// current spawn could have used — the v2 store is the answer.
func TestPollOpencodeSessionID_LogsTimeoutWithCandidates(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbDir := home + "/.local/share/opencode"
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);
INSERT INTO session_v2 VALUES ('ses_other', '/other/project', 1, '');
CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER);
INSERT INTO session VALUES ('ses_frozen', '/frozen/project', 1);`
	cmd := exec.Command("sqlite3", dbDir+"/opencode.db", seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed db: %v (%s)", err, out)
	}
	buf := captureLog(t)

	if got := pollOpencodeSessionID("/wanted/project", time.Time{}, time.Time{}, 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "/wanted/project") {
		t.Errorf("expected timeout log to name the workDir, got: %q", out)
	}
	if !strings.Contains(out, "/other/project") {
		t.Errorf("expected timeout log to list the v2 session directories, got: %q", out)
	}
	if strings.Contains(out, "/frozen/project") {
		t.Errorf("the frozen v1 session table must not feed the hint, got: %q", out)
	}
}

// TestPollOpencodeSessionID_RespectsTimeBound: the session-table fallback must
// not bind a session from an earlier run of the same workdir.
func TestPollOpencodeSessionID_RespectsTimeBound(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	old := now.Add(-15 * time.Minute).UnixMilli()
	fresh := now.UnixMilli()
	home := seedSessionDB(t, "CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
		"INSERT INTO session VALUES ('ses_old', '/wd/loop', "+fmt.Sprint(old)+", '');"+
		"INSERT INTO session VALUES ('ses_fresh', '/wd/loop', "+fmt.Sprint(fresh)+", '');")
	t.Setenv("HOME", home)

	if got := pollOpencodeSessionID("/wd/loop", now, time.Time{}, time.Second); got != "ses_fresh" {
		t.Fatalf("expected the session created after the bound, got %q", got)
	}
	if got := pollOpencodeSessionID("/wd/loop", now.Add(time.Minute), time.Time{}, time.Second); got != "" {
		t.Fatalf("expected no session after a later bound, got %q", got)
	}
}

// TestPollOpencodeSessionID_RejectsSubagentAndLaterRuns: the fallback must only
// ever return a root session inside the window. A task() subagent session is
// never the agent's own session, and binding one hands the agent a foreign
// conversation on resume.
func TestPollOpencodeSessionID_RejectsSubagentAndLaterRuns(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	window := now.UnixMilli()
	sql := "CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);" +
		// A subagent session inside the window — never a candidate.
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_child', '/wd/loop', %d, 'ses_parent');", window) +
		// A root session well outside the window — never a candidate.
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_late', '/wd/loop', %d, '');", now.Add(time.Hour).UnixMilli())
	seedSessionDB(t, sql)

	got := pollOpencodeSessionID("/wd/loop", now, now.Add(markerWindowAfter), 600*time.Millisecond)
	if got != "" {
		t.Fatalf("neither a subagent nor an out-of-window session may be bound, got %q", got)
	}
}

// TestPollOpencodeSessionID_DeclinesAnAmbiguousDirectory: this table query reads
// no messages, so it cannot see the section marker — the newest row in the
// workdir is a guess when the directory holds more than one session inside the
// window. A wrong guess resumes the agent into somebody else's conversation, so
// the answer has to be unique. (A migrated session exists in both tables, which
// must not read as two candidates.)
func TestPollOpencodeSessionID_DeclinesAnAmbiguousDirectory(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	home := seedSessionDB(t, "CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
		fmt.Sprintf("INSERT INTO session VALUES ('ses_other_run', '/wd/loop', %d, '');", now.UnixMilli()-1000)+
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_other_run', '/wd/loop', %d, '');", now.UnixMilli()-1000)+
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_mine', '/wd/loop', %d, '');", now.UnixMilli()))
	t.Setenv("HOME", home)
	buf := captureLog(t)

	if got := pollOpencodeSessionID("/wd/loop", now.Add(-markerSinceSlack), now.Add(markerWindowAfter), 600*time.Millisecond); got != "" {
		t.Fatalf("two root sessions in the window are an ambiguity, got %q", got)
	}
	// The window is anchored to the agent row, so the ambiguity cannot resolve
	// itself on a later attempt — the reason has to be in the log, otherwise an
	// operator reads "no session" as a timing problem.
	if out := buf.String(); !strings.Contains(out, "more than one root session") {
		t.Errorf("expected the timeout log to name the ambiguity, got: %q", out)
	}
}

// TestPollOpencodeAnyDirSessionID_BindsOnlyAUniqueMatch: the last resort for an
// undateable-directory agent must decline when it cannot be sure.
func TestPollOpencodeAnyDirSessionID_BindsOnlyAUniqueMatch(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	created := now.UnixMilli()
	home := seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session_v2 VALUES ('ses_only', '/repo/proj/.worktrees/a', %d, '');", created))
	t.Setenv("HOME", home)
	until := now.Add(markerWindowAfter)

	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), until, time.Second); got != "ses_only" {
		t.Fatalf("expected the unique session in the window, got %q", got)
	}
	// A second root session inside the window makes it ambiguous.
	if _, err := exec.Command("sqlite3", filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_second', '/repo/proj/.worktrees/b', %d, '');", created+1000)).CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), until, 600*time.Millisecond); got != "" {
		t.Fatalf("an ambiguous window must not be bound, got %q", got)
	}
}

// TestPollOpencodeAnyDirSessionID_RejectsAnotherProjectsSession: a session
// outside the agent's own tree is never its session, however well it fits the
// window. The bounded window alone is not enough — a spawn batch can leave
// exactly one root session in the window, belonging to another repository.
func TestPollOpencodeAnyDirSessionID_RejectsAnotherProjectsSession(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	home := seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session_v2 VALUES ('ses_foreign_repo', '/other/repo/.worktrees/x', %d, '');", now.UnixMilli()))
	t.Setenv("HOME", home)

	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), now.Add(markerWindowAfter), 600*time.Millisecond); got != "" {
		t.Fatalf("a session from another project must not be bound, got %q", got)
	}
}

// TestPollOpencodeAnyDirSessionID_DeduplicatesAcrossTables: a migrated v1
// session exists in both tables, so counting rows would read as ambiguous and
// decline an unambiguous answer.
func TestPollOpencodeAnyDirSessionID_DeduplicatesAcrossTables(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	home := seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session VALUES ('ses_migrated', '/repo/proj/wt', %d, '');"+
			"INSERT INTO session_v2 VALUES ('ses_migrated', '/repo/proj/wt', %d, '');", now.UnixMilli(), now.UnixMilli()))
	t.Setenv("HOME", home)

	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), now.Add(markerWindowAfter), time.Second); got != "ses_migrated" {
		t.Fatalf("the same session in both tables is one candidate, got %q", got)
	}
}

// TestPollOpencodeAnyDirSessionID_RejectsTheProjectRoot: the project root is
// where the orchestrator and the user keep their own sessions. The marker-less
// fallback cannot tell one root session from another, so it must at least never
// accept the root directory itself — that is the one foreign session which is
// always in range for a loop agent working in a worktree below it.
func TestPollOpencodeAnyDirSessionID_RejectsTheProjectRoot(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	home := seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session_v2 VALUES ('ses_orchestrator', '/repo/proj', %d, '');", now.UnixMilli()))
	t.Setenv("HOME", home)

	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), now.Add(markerWindowAfter), time.Second); got != "" {
		t.Fatalf("the project root's own session must not be bound, got %q", got)
	}
}

// TestPollOpencodeAnyDirSessionID_StillBindsAWorktreeSession keeps the stricter
// rule honest: a session strictly below the project root stays a candidate, so
// the root exclusion does not silently disable the fallback altogether.
func TestPollOpencodeAnyDirSessionID_StillBindsAWorktreeSession(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	now := time.Now()
	home := seedSessionDB(t, fmt.Sprintf(
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);"+
			"INSERT INTO session_v2 VALUES ('ses_worktree', '/repo/proj/.worktrees/loop', %d, '');", now.UnixMilli()))
	t.Setenv("HOME", home)

	if got := pollOpencodeAnyDirSessionID("/repo/proj", now.Add(-markerSinceSlack), now.Add(markerWindowAfter), time.Second); got != "ses_worktree" {
		t.Fatalf("a worktree session must still be bound, got %q", got)
	}
}

func TestPollCodexSessionID_LogsTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := captureLog(t)

	if got := pollCodexSessionID("/some/workdir", 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "/some/workdir") {
		t.Errorf("expected timeout log to name the workDir, got: %q", out)
	}
}
