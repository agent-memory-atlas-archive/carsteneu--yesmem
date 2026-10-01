package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// evidenceDBPath returns a writable dummy db path — the default queryOpencodeSession
// is stubbed, but VerifySubagentEvidence still stats the file to guard against
// infra outages.
func evidenceDBPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(p, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// evidenceFake returns a queryOpencodeSession stub mapping session ids to
// (parent_id, agent) rows. Missing ids return found=false (agent fault), not
// an error (infra fault).
func evidenceFake(rows map[string][2]string) func(dbPath, sessionID string) (bool, string, string, error) {
	return func(dbPath, sessionID string) (bool, string, string, error) {
		row, ok := rows[sessionID]
		if !ok {
			return false, "", "", nil
		}
		return true, row[0], row[1], nil
	}
}

func withEvidenceRows(t *testing.T, rows map[string][2]string) {
	t.Helper()
	orig := queryOpencodeSession
	queryOpencodeSession = evidenceFake(rows)
	t.Cleanup(func() { queryOpencodeSession = orig })
}

func evidenceFixtureRows() map[string][2]string {
	return map[string][2]string{
		"ses_cold": {"ses_parent", "reviewer"},
		"ses_cons": {"ses_parent", "general"},
		"ses_sec":  {"ses_parent", "reviewer"},
	}
}

func evidenceFixtureEvidence() map[string]string {
	return map[string]string{
		"cold-review": "ses_cold",
		"consequence": "ses_cons",
		"security":    "ses_sec",
	}
}

func TestVerifySubagentEvidence_AllChecksPass(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", evidenceFixtureEvidence())
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestVerifySubagentEvidence_RowMissing(t *testing.T) {
	rows := evidenceFixtureRows()
	delete(rows, "ses_sec")
	withEvidenceRows(t, rows)
	errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", evidenceFixtureEvidence())
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got: %v", errs)
	}
	if errs[0].Field != "security" {
		t.Errorf("expected error field security, got %q", errs[0].Field)
	}
	if !strings.Contains(errs[0].Detail, "ses_sec") {
		t.Errorf("expected detail to name the session id, got: %s", errs[0].Detail)
	}
}

func TestVerifySubagentEvidence_ParentMismatch(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_other_parent", evidenceFixtureEvidence())
	if len(errs) != 3 {
		t.Fatalf("expected 3 errors (one per sub-phase), got: %v", errs)
	}
	for _, fe := range errs {
		if !strings.Contains(fe.Detail, "parent_id") {
			t.Errorf("expected parent_id naming in detail, got: %s", fe.Detail)
		}
	}
}

func TestVerifySubagentEvidence_AgentNotAllowed(t *testing.T) {
	rows := evidenceFixtureRows()
	rows["ses_sec"] = [2]string{"ses_parent", "explore"}
	withEvidenceRows(t, rows)
	errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", evidenceFixtureEvidence())
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got: %v", errs)
	}
	if errs[0].Field != "security" {
		t.Errorf("expected error field security, got %q", errs[0].Field)
	}
	if !strings.Contains(errs[0].Detail, "explore") {
		t.Errorf("expected detail to name the offending agent, got: %s", errs[0].Detail)
	}
}

func TestVerifySubagentEvidence_DuplicateIDs(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	evidence := evidenceFixtureEvidence()
	evidence["consequence"] = "ses_cold"
	evidence["security"] = "ses_cold"
	errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", evidence)
	if len(errs) != 2 {
		t.Fatalf("expected 2 duplicate errors (consequence, security), got: %v", errs)
	}
	seen := map[string]bool{}
	for _, fe := range errs {
		seen[fe.Field] = true
		if !strings.Contains(fe.Detail, "ses_cold") {
			t.Errorf("expected detail to name duplicated id, got: %s", fe.Detail)
		}
	}
	if seen["cold-review"] {
		t.Errorf("cold-review must not be flagged for duplicates of other keys: %v", errs)
	}
	if !seen["consequence"] {
		t.Errorf("consequence (duplicated) must be flagged: %v", errs)
	}
	if !seen["security"] {
		t.Errorf("security (duplicated) must be flagged: %v", errs)
	}
}

func TestVerifySubagentEvidence_InfraErrorSkipsVerification(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	orig := queryOpencodeSession
	queryOpencodeSession = func(dbPath, sessionID string) (bool, string, string, error) {
		return false, "", "", context.DeadlineExceeded
	}
	t.Cleanup(func() { queryOpencodeSession = orig })
	if errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", evidenceFixtureEvidence()); len(errs) != 0 {
		t.Fatalf("infra error must skip verification (not fail the agent), got: %v", errs)
	}
}

func TestVerifySubagentEvidence_EmptyDBPathSkipsVerification(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	if errs := VerifySubagentEvidence("", "ses_parent", evidenceFixtureEvidence()); len(errs) != 0 {
		t.Fatalf("empty dbPath must skip verification, got: %v", errs)
	}
}

func TestVerifySubagentEvidence_EmptyParentSkipsVerification(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	if errs := VerifySubagentEvidence(evidenceDBPath(t), "", evidenceFixtureEvidence()); len(errs) != 0 {
		t.Fatalf("empty parent session id must skip verification, got: %v", errs)
	}
}

func TestVerifySubagentEvidence_NoEvidenceSkipsVerification(t *testing.T) {
	withEvidenceRows(t, evidenceFixtureRows())
	if errs := VerifySubagentEvidence(evidenceDBPath(t), "ses_parent", nil); len(errs) != 0 {
		t.Fatalf("empty evidence must skip verification, got: %v", errs)
	}
}

// TestVerifySubagentEvidence_AgainstSQLite is the integration proof: the
// default queryOpencodeSession talks to a real opencode-style sqlite DB via
// the sqlite3 CLI — the same access pattern as opencodeSessionDir.
func TestVerifySubagentEvidence_AgainstSQLite(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	sql := "CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, agent TEXT, title TEXT, time_created INTEGER);" +
		"INSERT INTO session (id, parent_id, agent, title, time_created) VALUES ('ses_parent', '', 'opencode', 'loop', 1);" +
		"INSERT INTO session (id, parent_id, agent, title, time_created) VALUES ('ses_cold', 'ses_parent', 'reviewer', 'cold', 2);" +
		"INSERT INTO session (id, parent_id, agent, title, time_created) VALUES ('ses_cons', 'ses_parent', 'general', 'cons', 3);" +
		"INSERT INTO session (id, parent_id, agent, title, time_created) VALUES ('ses_sec', 'ses_parent', 'reviewer', 'sec', 4);"
	if out, err := exec.Command("sqlite3", dbPath, sql).CombinedOutput(); err != nil {
		t.Fatalf("failed to build fixture db: %v: %s", err, out)
	}

	if errs := VerifySubagentEvidence(dbPath, "ses_parent", evidenceFixtureEvidence()); len(errs) != 0 {
		t.Fatalf("expected no errors against fixture db, got: %v", errs)
	}

	evidence := evidenceFixtureEvidence()
	evidence["security"] = "ses_unknown"
	errs := VerifySubagentEvidence(dbPath, "ses_parent", evidence)
	if len(errs) != 1 || errs[0].Field != "security" {
		t.Fatalf("expected 1 error for security, got: %v", errs)
	}
}
