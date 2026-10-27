package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// YM-304 [AGT-MA37] key 2: the Phase 5 evidence check was fail-open twice — a
// failed session lookup skipped its key, and an unreadable opencode.db skipped the
// whole check. Both answers read as "compliant", so an agent whose subagent ids
// were never verified could claim DONE. An unverifiable claim is not a verified
// one; the guard pauses (and unpauses itself once the scratchpad is compliant), so
// the closed direction is the recoverable one.
func TestVerifySubagentEvidence_LookupFailureIsNotCompliantLos55a(t *testing.T) {
	prev := queryOpencodeSession
	queryOpencodeSession = func(string, string) (bool, string, string, error) {
		return false, "", "", errors.New("sqlite3: database is locked")
	}
	t.Cleanup(func() { queryOpencodeSession = prev })

	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(dbPath, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	errs := VerifySubagentEvidence(dbPath, "ses_parent", map[string]string{"cold-review": "ses_child"})
	if len(errs) == 0 {
		t.Fatal("a failed lookup was accepted as compliant — an unverified subagent id passes the gate")
	}
	if errs[0].Phase != 5 {
		t.Errorf("error phase = %d, want 5", errs[0].Phase)
	}
}

// An unreadable database is an infrastructure failure, but it is the guard's only
// source of truth: skipping it means every id in the block is taken on the agent's
// word.
func TestVerifySubagentEvidence_MissingDatabaseIsNotCompliantLos55a(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "absent-opencode.db")

	errs := VerifySubagentEvidence(dbPath, "ses_parent", map[string]string{"cold-review": "ses_child"})
	if len(errs) == 0 {
		t.Fatal("a missing opencode.db skipped the evidence check instead of failing it")
	}
}

// Without a database path at all the check cannot run by configuration, not by
// failure — that case keeps skipping, or every agent on such a host would be
// paused forever with no way to prove anything.
func TestVerifySubagentEvidence_NoDatabaseConfiguredSkipsLos55a(t *testing.T) {
	if errs := VerifySubagentEvidence("", "ses_parent", map[string]string{"cold-review": "ses_child"}); len(errs) != 0 {
		t.Errorf("unconfigured database reported %v, want no findings", errs)
	}
}
