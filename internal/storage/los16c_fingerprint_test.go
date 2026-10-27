package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProjectChangeFingerprintSeesNonPathKeyedLearningsLos16c (YM-343 [BRF-B13],
// Befund 3): the fingerprint compared learnings.project against the project path
// only. Rows keyed by a legacy short name and rows of a worktree checkout do not
// carry the path there, so the fingerprint stayed constant and the briefing was
// never refreshed for them.
func TestProjectChangeFingerprintSeesNonPathKeyedLearningsLos16c(t *testing.T) {
	s := newTestStore(t)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repo marker: %v", err)
	}
	base := filepath.Base(root)

	insertRow := func(content, project, canonical string, superseded bool) {
		t.Helper()
		supersededBy := any(nil)
		if superseded {
			supersededBy = int64(-1)
		}
		if _, err := s.db.Exec(`INSERT INTO learnings
			(session_id, category, content, project, confidence, created_at, model_used, source, superseded_by, canonical_project)
			VALUES (?, 'gotcha', ?, ?, 1.0, ?, 'test', 'llm_extracted', ?, ?)`,
			"los16c-session", content, project, time.Now().UTC().Format(time.RFC3339), supersededBy, canonical); err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
	}

	fp1 := los16dFingerprint(t, s, root)

	// Legacy row: project holds the short name, canonical_project the basename.
	insertRow("legacy short-name row", base, base, false)
	fp2 := los16dFingerprint(t, s, root)
	if fp1 == fp2 {
		t.Error("fingerprint must change for a row keyed by the legacy short name")
	}

	// Worktree row: project holds the worktree path, only the repository root
	// scope reaches it.
	insertRow("worktree row", filepath.Join(root, ".worktrees", "feature"), base, false)
	fp3 := los16dFingerprint(t, s, root)
	if fp2 == fp3 {
		t.Error("fingerprint must change for a row keyed by a worktree path")
	}

	// A superseded row of the same project must also be counted.
	insertRow("superseded legacy row", base, base, true)
	fp4 := los16dFingerprint(t, s, root)
	if fp3 == fp4 {
		t.Error("fingerprint must change for a superseded row of the project")
	}

	// A different repository that shares the folder name must not move it: the
	// basename is the collision scope models.ProjectMatches exists to prevent.
	insertRow("same-basename other repo", filepath.Join(t.TempDir(), base), base, false)
	fp5 := los16dFingerprint(t, s, root)
	if fp4 != fp5 {
		t.Error("fingerprint must not change for a repository sharing the basename")
	}

	// A row of a differently named project must not move it either.
	insertRow("other repo row", "/los16c/other", "other", false)
	fp6 := los16dFingerprint(t, s, root)
	if fp5 != fp6 {
		t.Error("fingerprint must not change for a row of a different project")
	}
}
