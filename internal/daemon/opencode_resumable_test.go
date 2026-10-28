package daemon

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestYm297OpencodeSessionResumable verifies the restart-recovery lookup against
// a fixture database (sqlite3 CLI, same access pattern as the session capture).
// The question it answers is narrow on purpose: does the session still exist in
// opencode's store *for this agent's work dir*, so that resume_agent can reattach
// to it?
func TestYm297OpencodeSessionResumable(t *testing.T) {
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
	// Both stores, because a v2 session lives in session_v2 and a migrated one
	// in both.
	mustRun("CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)")
	mustRun("CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT)")
	mustRun("INSERT INTO session_v2 VALUES ('ses_v2_live', '/w/live')")
	mustRun("INSERT INTO session VALUES ('ses_v1_live', '/w/v1')")
	// A stale, shared id: the session exists, but in somebody else's checkout.
	mustRun("INSERT INTO session_v2 VALUES ('ses_foreign', '/w/other')")

	cases := []struct {
		name    string
		dbPath  string
		session string
		workDir string
		want    bool
		wantErr bool
	}{
		{"v2 session", dbPath, "ses_v2_live", "/w/live", true, false},
		{"v1 session", dbPath, "ses_v1_live", "/w/v1", true, false},
		{"prefixed id", dbPath, "opencode:ses_v2_live", "/w/live", true, false},
		{"unknown session", dbPath, "ses_absent", "/w/live", false, false},
		{"foreign work dir", dbPath, "ses_foreign", "/w/mine", false, false},
		// A row from before the work_dir migration has no directory to compare,
		// so the lookup falls back to the id alone. Over-keeping, never deletion.
		{"empty work dir", dbPath, "ses_foreign", "", true, false},
		{"empty session", dbPath, "", "/w/live", false, true},
		{"missing db", filepath.Join(t.TempDir(), "missing.db"), "ses_v2_live", "/w/live", false, true},
		{"empty db path", "", "ses_v2_live", "/w/live", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := opencodeSessionResumable(tc.dbPath, tc.session, tc.workDir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("opencodeSessionResumable err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("opencodeSessionResumable(%q, %q, %q) = %v, want %v", tc.dbPath, tc.session, tc.workDir, got, tc.want)
			}
		})
	}
}
