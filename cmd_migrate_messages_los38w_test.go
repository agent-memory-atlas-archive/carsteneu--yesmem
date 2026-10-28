package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// YM-360 (Los 38w): `yesmem migrate-messages` opened the live database for
// writing, deleted every message, VACUUMed in place and removed the bleve index
// without ever asking whether a daemon or another connection was using the file.
//
// The scenarios below run the command as a child process, because the failure
// contract is an exit code and because a DB lock has to be held by a second
// process to mean anything.
const (
	los38wHelperEnv   = "YESMEM_LOS38W_HELPER"
	los38wArgsEnv     = "YESMEM_LOS38W_ARGS"
	los38wMessageRows = 3
	los38wBulkRows    = 30000
)

const los38wMessagesDDL = `CREATE TABLE IF NOT EXISTS messages (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id   TEXT NOT NULL,
	source_agent TEXT DEFAULT 'claude',
	role         TEXT NOT NULL,
	message_type TEXT NOT NULL,
	content      TEXT,
	content_blob BLOB,
	tool_name    TEXT,
	file_path    TEXT,
	timestamp    TEXT NOT NULL,
	sequence     INTEGER NOT NULL,
	model        TEXT DEFAULT ''
)`

// TestLos38wMigrateCommand is the child process entry point. HOME already points
// at the scenario's installation; runMigrateMessages reads its flags from
// os.Args[2:], so the scenario appends them here.
func TestLos38wMigrateCommand(t *testing.T) {
	if os.Getenv(los38wHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	os.Args = append(os.Args, strings.Fields(os.Getenv(los38wArgsEnv))...)
	runMigrateMessages()
}

func TestLos38wRefusesWhenDaemonSocketAnswers(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	ln, err := net.Listen("unix", filepath.Join(dataDir, "daemon.sock"))
	if err != nil {
		t.Fatalf("listen on daemon.sock: %v", err)
	}
	defer ln.Close()

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 while a daemon answers on the socket; want a refusal")
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "yesmem.db")); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d: a live installation must not be touched", got, los38wMessageRows)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "messages.db")); err == nil {
		t.Error("messages.db was created although the run should have been refused")
	}
}

func TestLos38wRefusesWhenSourceDatabaseIsLocked(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")

	ctx := context.Background()
	holder, err := sql.Open("sqlite", mainDB)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take write lock: %v", err)
	}

	code := los38wRunCommand(t, home, "")

	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	conn.Close()
	holder.Close()

	if code == 0 {
		t.Error("exit code 0 while another connection held a write lock; want a refusal")
	}
	// The copy must not have started: a half-written messages.db is exactly the
	// state the lock check has to prevent.
	if got := los38wMessageCount(t, filepath.Join(dataDir, "messages.db")); got != 0 {
		t.Errorf("target holds %d messages read from a locked source; the refusal must come before any copy", got)
	}
	if got := los38wMessageCount(t, mainDB); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d", got, los38wMessageRows)
	}
}

func TestLos38wForceMigratesDespiteRunningDaemon(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	ln, err := net.Listen("unix", filepath.Join(dataDir, "daemon.sock"))
	if err != nil {
		t.Fatalf("listen on daemon.sock: %v", err)
	}
	defer ln.Close()

	if code := los38wRunCommand(t, home, "--force"); code != 0 {
		t.Errorf("exit code %d with --force; the flag is the deliberate exception to the daemon check", code)
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "messages.db")); got != los38wMessageRows {
		t.Errorf("target messages = %d, want %d", got, los38wMessageRows)
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "yesmem.db")); got != 0 {
		t.Errorf("source messages = %d after a completed migration, want 0", got)
	}
}

// TestLos38wKeepsSourceWhenTargetIsAlreadyPopulated: a target that does not hold
// the source's rows leaves the installation in an ambiguous state, and nothing may
// be deleted. Since YM-369 the run reports that state as a failure instead of
// exiting 0 with "nothing to do": silence there hid an unfinished compaction. The
// fully covered case (the interrupted run) is completed instead, see
// TestLos38yCompletesInterruptedMigration.
func TestLos38wKeepsSourceWhenTargetIsAlreadyPopulated(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	los38ySeedForeignTarget(t, filepath.Join(dataDir, "messages.db"))

	bleveDir := filepath.Join(dataDir, "bleve-index")
	if err := os.MkdirAll(bleveDir, 0o700); err != nil {
		t.Fatalf("seed bleve dir: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 although the target holds foreign rows and the source is complete")
	}
	// No copy ran, so nothing may be deleted: the source still holds the only
	// complete set of messages.
	if got := los38wMessageCount(t, filepath.Join(dataDir, "yesmem.db")); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d: a skipped copy must not delete the source", got, los38wMessageRows)
	}
	if _, err := os.Stat(bleveDir); err != nil {
		t.Errorf("bleve index was removed although nothing was migrated: %v", err)
	}
}

func TestLos38wMigratesAndSwapsSourceAtomically(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")

	before, err := os.Stat(mainDB)
	if err != nil {
		t.Fatalf("stat source: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Fatalf("exit code %d for a clean installation, want 0", code)
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "messages.db")); got != los38wMessageRows {
		t.Errorf("target messages = %d, want %d", got, los38wMessageRows)
	}
	if got := los38wMessageCount(t, mainDB); got != 0 {
		t.Errorf("source messages = %d, want 0", got)
	}

	after, err := os.Stat(mainDB)
	if err != nil {
		t.Fatalf("stat source after migration: %v", err)
	}
	// An in-place VACUUM rewrites the live file and can leave it half compacted if
	// it fails. VACUUM INTO builds a new file that is renamed into place, so the
	// inode must change.
	if os.SameFile(before, after) {
		t.Error("source database kept its inode: want VACUUM INTO plus an atomic rename over the live file")
	}
	if _, err := os.Stat(mainDB + ".vacuum"); err == nil {
		t.Error("temporary vacuum file was left behind")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(mainDB + suffix); err == nil {
			t.Errorf("%s survived the swap and would be applied to the new database", suffix)
		}
	}
	// Every row has to be searchable before the bleve index may go.
	if got := los38wFTSRowCount(t, filepath.Join(dataDir, "messages.db")); got != los38wMessageRows {
		t.Errorf("messages_fts rows = %d, want %d: the rebuild is incomplete and must keep bleve", got, los38wMessageRows)
	}
	if got := los38wQueryInt(t, filepath.Join(dataDir, "messages.db"),
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'blob'"); got != 1 {
		t.Errorf("blob bodies found in messages_fts = %d, want 1", got)
	}
	// The write path also mirrors a thinking blob into content; readers that do not
	// fall back to the blob would otherwise show an empty snippet.
	if got := los38wQueryInt(t, filepath.Join(dataDir, "messages.db"),
		"SELECT COUNT(*) FROM messages WHERE content = 'blob-body-1' AND CAST(content_blob AS TEXT) = 'blob-body-1'"); got != 1 {
		t.Errorf("thinking rows with mirrored content = %d, want 1", got)
	}
	// The copy has to carry the whole row, not the columns this command happens to
	// know about: a dropped source_agent is permanent misattribution.
	if got := los38wQueryInt(t, filepath.Join(dataDir, "messages.db"),
		"SELECT COUNT(*) FROM messages WHERE source_agent = 'codex' AND model = 'gpt-x'"); got != los38wMessageRows {
		t.Errorf("copied rows with their source_agent/model = %d, want %d", got, los38wMessageRows)
	}
}

// TestLos38wNoOpWithoutDatabase: a fresh installation has nothing to move, and the
// run must not create the database it would otherwise open.
func TestLos38wNoOpWithoutDatabase(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Errorf("exit code %d without a database, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "yesmem.db")); err == nil {
		t.Error("yesmem.db was created by a run that had nothing to migrate")
	}
}

// TestLos38wReportsInterruptedCopy: a target with fewer rows than the source is a
// half finished migration, not an idempotent repeat. Reporting success there would
// hide the missing rows from every reader of messages.db.
func TestLos38wReportsInterruptedCopy(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	los38wSeedMessagesN(t, filepath.Join(dataDir, "messages.db"), 1)

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 although the target holds 1 of 3 messages; want a failure")
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "yesmem.db")); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d: the source is the only complete set", got, los38wMessageRows)
	}
}

// TestLos38wKeepsDatabaseOwnerOnly: VACUUM INTO writes the copy under the process
// umask, so the swap would otherwise turn a hardened database into 0644. The source
// keeps 0640 (not 0600) so the assertion fails under any permissive umask.
func TestLos38wKeepsDatabaseOwnerOnly(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")
	if err := os.Chmod(mainDB, 0o640); err != nil {
		t.Fatalf("harden source: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	info, err := os.Stat(mainDB)
	if err != nil {
		t.Fatalf("stat source: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("source mode = %04o after the swap, want 0640", got)
	}
	for _, p := range []string{"messages.db", "messages.db-wal", "messages.db-shm"} {
		path := filepath.Join(dataDir, p)
		targetInfo, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := targetInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", p, got)
		}
	}
}

// TestLos38wIgnoresYESMEM_SOCK: the guard must not be switchable off by an
// environment variable. IsDaemonRunning follows YESMEM_SOCK, so a stale value
// would have reported "no daemon" while one was answering.
func TestLos38wIgnoresYESMEM_SOCK(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	ln, err := net.Listen("unix", filepath.Join(dataDir, "daemon.sock"))
	if err != nil {
		t.Fatalf("listen on daemon.sock: %v", err)
	}
	defer ln.Close()

	stale := filepath.Join(t.TempDir(), "stale.sock")
	if code := los38wRunCommand(t, home, "", "YESMEM_SOCK="+stale); code == 0 {
		t.Error("exit code 0 with a bogus YESMEM_SOCK although the real socket answers")
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "yesmem.db")); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d", got, los38wMessageRows)
	}
}

// TestLos38wRefusesWhenSourceChangesDuringCopy covers the loss F1: rows that
// disappear while the copy pages through the source used to be deleted from it
// even though they were never copied. The delete below is triggered by the
// command's own progress line, so the count is already fixed and the copy is
// still running.
func TestLos38wRefusesWhenSourceChangesDuringCopy(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")
	los38wSeedBulk(t, mainDB, los38wBulkRows)

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos38wMigrateCommand$")
	cmd.Env = append(los38wCleanEnv(), "HOME="+home, los38wHelperEnv+"=1", los38wArgsEnv+"=")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	var seen strings.Builder
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		seen.WriteString(line)
		if strings.Contains(line, "messages to migrate") {
			break
		}
		if err != nil {
			t.Fatalf("command ended before it started copying: %v (%s)", err, seen.String())
		}
	}
	los38wExec(t, mainDB, `DELETE FROM messages WHERE id BETWEEN 100 AND 5100`)
	for {
		line, err := reader.ReadString('\n')
		seen.WriteString(line)
		if err != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Errorf("exit code 0 although the source shrank during the copy (%s)", seen.String())
	}
	if got := los38wMessageCount(t, mainDB); got == 0 {
		t.Error("the source was emptied by a run that could not copy every row")
	}
}

// TestLos38wDetectsWriteLock pins the detection itself: the scenario tests above
// would also pass if the check returned a generic error instead of "locked".
func TestLos38wDetectsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	seeded, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := seeded.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	seeded.Close()

	if held, err := dbWriteLockHeld(path); err != nil || held {
		t.Fatalf("free database: held=%v err=%v, want false/nil", held, err)
	}

	ctx := context.Background()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take write lock: %v", err)
	}

	held, err := dbWriteLockHeld(path)
	if err != nil {
		t.Fatalf("locked database: unexpected error %v", err)
	}
	if !held {
		t.Error("held = false while another connection holds a write lock")
	}
}

func los38wFTSRowCount(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&n); err != nil {
		t.Fatalf("count messages_fts in %s: %v", path, err)
	}
	return n
}

// los38wInstallation prepares a private installation whose main database holds
// los38wMessageRows messages.
func los38wInstallation(t *testing.T) (home, dataDir string) {
	t.Helper()
	// macOS caps a unix socket path at 104 bytes, and t.TempDir() under
	// /var/folders/... plus .claude/yesmem/daemon.sock exceeds it, so the bind
	// fails with "invalid argument" before the test can prove anything.
	home, err := os.MkdirTemp("", "ym38w")
	if err != nil {
		t.Fatalf("create home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	dataDir = filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	los38wSeedMessages(t, filepath.Join(dataDir, "yesmem.db"))
	return home, dataDir
}

func los38wSeedMessages(t *testing.T, path string) {
	t.Helper()
	los38wSeedMessagesN(t, path, los38wMessageRows)
}

func los38wSeedMessagesN(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(los38wMessagesDDL); err != nil {
		t.Fatalf("create messages in %s: %v", path, err)
	}
	for i := 0; i < rows; i++ {
		content := fmt.Sprintf("hello %d", i)
		var blob any
		if i == 1 {
			// A thinking message: content stays NULL, the body sits in the blob. The
			// write path indexes that text, so the migration has to as well.
			content = ""
			blob = []byte("blob-body-1")
		}
		if _, err := db.Exec(
			`INSERT INTO messages (session_id, source_agent, role, message_type, content, content_blob, timestamp, sequence, model) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("session-%d", i), "codex", "user", "text", content, blob, "2026-10-19T00:00:00Z", i, "gpt-x",
		); err != nil {
			t.Fatalf("insert into %s: %v", path, err)
		}
	}
}

// los38wSeedBulk inserts rows in one statement; the per-row helper is too slow for
// a fixture the paging test needs to outlive.
func los38wSeedBulk(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(los38wMessagesDDL); err != nil {
		t.Fatalf("create messages in %s: %v", path, err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n WHERE x < ?)
		INSERT INTO messages (session_id, source_agent, role, message_type, content, timestamp, sequence, model)
		SELECT 'session-' || x, 'codex', 'user', 'text', 'body ' || x, '2026-10-19T00:00:00Z', x, 'gpt-x' FROM n`, rows); err != nil {
		t.Fatalf("bulk insert into %s: %v", path, err)
	}
}

func los38wExec(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("exec %q on %s: %v", statement, path, err)
	}
}

func los38wMessageCount(t *testing.T, path string) int64 {
	t.Helper()
	return los38wQueryInt(t, path, "SELECT COUNT(*) FROM messages")
}

func los38wQueryInt(t *testing.T, path, query string) int64 {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q in %s: %v", query, path, err)
	}
	return n
}

// los38wRunCommand runs the command in a child process and returns its exit code.
func los38wRunCommand(t *testing.T, home, args string, extraEnv ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLos38wMigrateCommand$")
	cmd.Env = append(los38wCleanEnv(), "HOME="+home, los38wHelperEnv+"=1", los38wArgsEnv+"="+args)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("run command: %v (%s)", err, out)
	return -1
}

// los38wCleanEnv drops every variable that could redirect the child at the live
// installation.
func los38wCleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "HOME", "YESMEM_SOCK", "YESMEM_DATA_DIR", los38wHelperEnv, los38wArgsEnv:
			continue
		}
		out = append(out, kv)
	}
	return out
}
