package daemon

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestOpencodeChildSessionActive verifies the opencode.db child-session query
// against a fixture database (sqlite3 CLI, mirrors handler_agents.go access).
func TestOpencodeChildSessionActive(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	mustRun := func(query string) {
		t.Helper()
		out, err := exec.Command("sqlite3", dbPath, query).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3 %q: %v (%s)", query, err, out)
		}
	}
	mustRun("CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, time_updated INTEGER)")
	now := time.Now().UnixMilli()
	mustRun("INSERT INTO session VALUES ('child-fresh', 'p-fresh', " + strconv.FormatInt(now, 10) + ")")
	mustRun("INSERT INTO session VALUES ('child-stale', 'p-stale', " + strconv.FormatInt(now-10*60*1000, 10) + ")")

	if !opencodeChildSessionActive(dbPath, "p-fresh") {
		t.Errorf("child updated within window should be active")
	}
	if opencodeChildSessionActive(dbPath, "p-stale") {
		t.Errorf("child older than the 90s window must be inactive")
	}
	if opencodeChildSessionActive(dbPath, "p-none") {
		t.Errorf("parent without children must be inactive")
	}
	if opencodeChildSessionActive(filepath.Join(t.TempDir(), "missing.db"), "p-fresh") {
		t.Errorf("missing db must read as inactive")
	}
	if opencodeChildSessionActive(dbPath, "") {
		t.Errorf("empty parent id must read as inactive without touching the db")
	}
	if opencodeChildActiveFn("") {
		t.Errorf("injectable default: empty parent id must read as inactive")
	}
}
