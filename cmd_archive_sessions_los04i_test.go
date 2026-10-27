package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

const los04iArchiveHelperEnv = "YESMEM_LOS04I_ARCHIVE_HELPER"

// los04iCleanEnv drops every variable that would point the child at another
// installation than the scenario's HOME.
func los04iCleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "HOME", "YESMEM_DATA_DIR", "YESMEM_SOCK", los04iArchiveHelperEnv:
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestLos04iArchiveSessionsCommand is the child process entry point of the fatal
// path test. HOME already points at the scenario's installation.
func TestLos04iArchiveSessionsCommand(t *testing.T) {
	if os.Getenv(los04iArchiveHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	os.Args = []string{"yesmem", "archive-sessions", "--dry-run"}
	runArchiveSessions()
}

// 'archive-sessions' reports a store it cannot open through log.Fatalf with %v.
// The path of that error carries HOME, and a newline in HOME ended the stderr
// line, so the rest read as its own log entry (YM-332, escaped from Los 04h).
// The run fails before the store exists, so every failing step prints the same
// path: the data directory (main.go) and the store open (cmd_archive_sessions.go).
func TestArchiveSessions_QuotesThePathsOfItsFatalErrors(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home\nwarn: forged line")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	// A file where the data directory belongs: MkdirAll, Chmod and the store
	// open all fail, and each of those errors names the path built from HOME.
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write .claude: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos04iArchiveSessionsCommand$")
	cmd.Env = append(los04iCleanEnv(), "HOME="+home, los04iArchiveHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the command reported success although its data directory cannot be created:\n%s", out)
	}

	printed := string(out)
	if strings.Contains(printed, "\nwarn: forged line") {
		t.Fatalf("a path forged a stderr line:\n%s", printed)
	}
	if !strings.Contains(printed, `home\nwarn: forged line`) {
		t.Fatalf("the path is missing from the fatal message, so the failure cannot be diagnosed:\n%s", printed)
	}
}

// The backfill builds the document path from the session id and reports a stat
// error on it. That error wraps an *os.PathError, so %v printed the id raw and a
// newline in it ended the fatal line (YM-332, escaped from Los 04h). A file where
// the project directory belongs makes the stat fail with ENOTDIR, which reaches
// the fatal without a broken installation.
func TestArchiveSessions_QuotesTheDocumentPathOfABackfillError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	sessionID := "sess\nwarn: forged archive doc line"
	project := filepath.Join(home, "proj")
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertSession(&models.Session{
		ID:           sessionID,
		Project:      project,
		MessageCount: 1,
		StartedAt:    time.Now().Add(-time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// A file where the project directory belongs: the stat of the document below
	// it fails with ENOTDIR instead of "does not exist".
	projectDir := filepath.Join(dataDir, "archive", archive.ProjectDirName(project))
	if err := os.MkdirAll(filepath.Dir(projectDir), 0o755); err != nil {
		t.Fatalf("mkdir archive root: %v", err)
	}
	if err := os.WriteFile(projectDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write project dir: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos04iArchiveSessionsCommand$")
	cmd.Env = append(los04iCleanEnv(), "HOME="+home, los04iArchiveHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the command reported success although it could not stat the document:\n%s", out)
	}

	// The document name replaces the separators of the session id, so the colon
	// of the forged text arrives as a dash.
	printed := string(out)
	if strings.Contains(printed, "\nwarn- forged archive doc line") {
		t.Fatalf("the document path forged a stderr line:\n%s", printed)
	}
	if !strings.Contains(printed, `sess\nwarn- forged archive doc line`) {
		t.Fatalf("the document path is missing from the fatal message:\n%s", printed)
	}
}
