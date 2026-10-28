package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// los16dFingerprint fails the test when the fingerprint reports an error.
func los16dFingerprint(t *testing.T, s *Store, project string) string {
	t.Helper()
	fp, err := s.ProjectChangeFingerprint(project)
	if err != nil {
		t.Fatalf("fingerprint for %q: %v", project, err)
	}
	return fp
}

// TestProjectChangeFingerprintSeesWorktreeSessionsLos16d (YM-372 [BRF-B14], Befund 3):
// the sessions half of the fingerprint compared project_short against the project
// path only. A session written inside a worktree checkout carries the worktree path
// there, so work in the worktree never moved the parent project's fingerprint and its
// briefing was never refreshed.
func TestProjectChangeFingerprintSeesWorktreeSessionsLos16d(t *testing.T) {
	s := mustOpen(t)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repo marker: %v", err)
	}
	base := filepath.Base(root)

	insertSession := func(id, project string) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := s.db.Exec(`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, project, project, now, "/los16d/"+id+".jsonl", now); err != nil {
			t.Fatalf("insert session %q: %v", id, err)
		}
	}

	fp1 := los16dFingerprint(t, s, root)

	insertSession("sess-root", root)
	fp2 := los16dFingerprint(t, s, root)
	if fp1 == fp2 {
		t.Fatal("fingerprint must change for a session of the project itself")
	}

	// Worktree session: project_short holds the checkout path, only the repository
	// root scope reaches it.
	insertSession("sess-worktree", filepath.Join(root, ".worktrees", "feature"))
	fp3 := los16dFingerprint(t, s, root)
	if fp2 == fp3 {
		t.Error("fingerprint must change for a session inside a worktree checkout")
	}

	// A different repository that shares the folder name must not move it: the
	// prefix test has to stay inside the root, and the basename alone is ambiguous.
	insertSession("sess-other-same-basename", filepath.Join(t.TempDir(), base))
	fp4 := los16dFingerprint(t, s, root)
	if fp3 != fp4 {
		t.Error("fingerprint must not change for a repository sharing the basename")
	}

	insertSession("sess-other-project", "/los16d/other")
	fp5 := los16dFingerprint(t, s, root)
	if fp4 != fp5 {
		t.Error("fingerprint must not change for a session of a different project")
	}

	// A session row that still carries the legacy short name is out of scope on
	// purpose: matching it by basename would pull in every repository that shares
	// the folder name.
	insertSession("sess-legacy-short", base)
	fp6 := los16dFingerprint(t, s, root)
	if fp5 != fp6 {
		t.Error("a legacy short-name session must not move the fingerprint of a named repository")
	}

	// Without a repository marker the scope falls back to the exact path: a
	// session in a subdirectory is a different project, not a worktree of this one.
	plain := t.TempDir()
	fpPlain1 := los16dFingerprint(t, s, plain)
	insertSession("sess-plain-sub", filepath.Join(plain, "sub"))
	fpPlain2 := los16dFingerprint(t, s, plain)
	if fpPlain1 != fpPlain2 {
		t.Error("a non-repository path must not adopt sessions from its subdirectories")
	}
}
