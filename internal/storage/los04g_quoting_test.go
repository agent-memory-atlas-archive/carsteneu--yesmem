package storage

import (
	"bytes"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"
)

// los04gForgedValue is a value a client can produce: it carries a newline, so an
// unquoted use ends the log line early and passes the rest off as its own entry
// (YM-332, escaped from YM-311).
const los04gForgedValue = "sess\nwarn: forged log line"

// los04gCaptureLog redirects the standard logger into a buffer for one test.
func los04gCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// los04gAssertOneQuotedLine checks that logged holds exactly one line and that
// value appears quoted in it.
func los04gAssertOneQuotedLine(t *testing.T, logged, value string) {
	t.Helper()
	if logged == "" {
		t.Fatal("no log line captured")
	}
	if got := strings.Count(logged, "\n"); got != 1 {
		t.Fatalf("log output spans %d lines, want 1 (a client value forged an entry):\n%s", got, logged)
	}
	if !strings.Contains(logged, strconv.Quote(value)) {
		t.Fatalf("logged value is not quoted; log line: %s", logged)
	}
}

// The stored jsonl_path of a vanished file is client input: the projects tree is
// written by the client, and a file name may carry a newline. Dropping that row
// logs the path, which must not split the line (YM-332).
func TestSessionIndexedByOtherPath_QuotesTheDroppedPath(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().Format(time.RFC3339)

	forgedPath := "/nonexistent/" + los04gForgedValue + ".jsonl"
	sessionID := "sess-" + los04gForgedValue
	for _, path := range []string{forgedPath, "/tmp/los04g-current.jsonl"} {
		if _, err := s.db.Exec(
			`INSERT INTO index_state (jsonl_path, file_size, file_mtime, indexed_at, indexed_session_id)
			 VALUES (?, 1, ?, ?, ?)`,
			path, now, now, sessionID); err != nil {
			t.Fatalf("seed index_state: %v", err)
		}
	}

	logged := los04gCaptureLog(t)
	if _, err := s.SessionIndexedByOtherPath(sessionID, "/tmp/los04g-current.jsonl"); err != nil {
		t.Fatalf("check ownership: %v", err)
	}
	los04gAssertOneQuotedLine(t, logged.String(), forgedPath)
}

// The same path travels in the error the caller logs, not only in the drop line:
// an error built with %s carries the newline into whatever logs it (found by the
// cold review of YM-332).
func TestSessionIndexedByOtherPath_QuotesThePathInTheDropError(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().Format(time.RFC3339)

	forgedPath := "/nonexistent/" + los04gForgedValue + ".jsonl"
	sessionID := "sess-" + los04gForgedValue
	for _, path := range []string{forgedPath, "/tmp/los04g-current.jsonl"} {
		if _, err := s.db.Exec(
			`INSERT INTO index_state (jsonl_path, file_size, file_mtime, indexed_at, indexed_session_id)
			 VALUES (?, 1, ?, ?, ?)`,
			path, now, now, sessionID); err != nil {
			t.Fatalf("seed index_state: %v", err)
		}
	}
	// The drop has to fail for the error to be built; a row-level trigger fires
	// per row, and one row matches.
	if _, err := s.db.Exec(
		`CREATE TRIGGER los04g_block_index_state_delete BEFORE DELETE ON index_state
		 BEGIN SELECT RAISE(ABORT, 'blocked for the test'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, err := s.SessionIndexedByOtherPath(sessionID, "/tmp/los04g-current.jsonl")
	if err == nil {
		t.Fatal("a blocked drop must return an error")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("the error text spans more than one line: %q", err.Error())
	}
	if !strings.Contains(err.Error(), strconv.Quote(forgedPath)) {
		t.Fatalf("the error text does not quote the path: %q", err.Error())
	}
}

// A project path reaches the storage backfill log from the learnings and sessions
// tables, which a client fills (YM-332).
func TestBackfillScanFiles_QuotesTheProjectOnFailure(t *testing.T) {
	s := mustOpen(t)
	project := "proj/" + los04gForgedValue
	// An empty scan document: the rewrite then reaches its marker insert without
	// a path insert, and the trigger below makes that insert fail.
	scanJSON := `{"Packages":[]}`
	if _, err := s.db.Exec(
		`INSERT INTO project_scan (project, scan_json, git_head) VALUES (?, ?, ?)`,
		project, scanJSON, "head"); err != nil {
		t.Fatalf("seed project_scan: %v", err)
	}
	// The backfill only logs when rewriting the per-path index fails. A row-level
	// trigger fires per row, so the marker insert is the row that fails.
	if _, err := s.db.Exec(
		`CREATE TRIGGER los04g_block_scan_files_insert BEFORE INSERT ON project_scan_files
		 BEGIN SELECT RAISE(ABORT, 'blocked for the test'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	logged := los04gCaptureLog(t)
	if s.IsFileInCodeIndex(project, "b.go") {
		t.Fatalf("index lookup of a project with a failing backfill must miss; log: %s", logged.String())
	}
	los04gAssertOneQuotedLine(t, logged.String(), project)
}

// Agent ids come from the agents table, which rows of older versions and foreign
// writers fill; the recovery sweep logs them (YM-332).
func TestAgentRecoverOrphaned_QuotesTheRowItCouldNotClose(t *testing.T) {
	s := mustOpen(t)
	agentID := "agent-" + los04gForgedValue
	if _, err := s.db.Exec(
		`INSERT INTO agents (id, project, section, status, pid, opencode_session_id) VALUES (?, 'p', 'sec', 'running', 0, ?)`,
		agentID, "sid-1"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := s.db.Exec(
		`CREATE TRIGGER los04g_block_agents_update BEFORE UPDATE ON agents
		 BEGIN SELECT RAISE(ABORT, 'blocked for the test'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	logged := los04gCaptureLog(t)
	if _, _, _, err := s.AgentRecoverOrphaned(func(int) bool { return false }, nil); err != nil {
		t.Fatalf("recover orphaned: %v", err)
	}
	los04gAssertOneQuotedLine(t, logged.String(), agentID)
}

func TestAgentRecoverOrphaned_QuotesTheRowItCouldNotDelete(t *testing.T) {
	s := mustOpen(t)
	agentID := "agent-" + los04gForgedValue
	if _, err := s.db.Exec(
		`INSERT INTO agents (id, project, section, status, pid, session_id) VALUES (?, 'p', 'sec', 'running', 0, '')`,
		agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := s.db.Exec(
		`CREATE TRIGGER los04g_block_agents_delete BEFORE DELETE ON agents
		 BEGIN SELECT RAISE(ABORT, 'blocked for the test'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	logged := los04gCaptureLog(t)
	if _, _, _, err := s.AgentRecoverOrphaned(func(int) bool { return false }, nil); err != nil {
		t.Fatalf("recover orphaned: %v", err)
	}
	los04gAssertOneQuotedLine(t, logged.String(), agentID)
}
