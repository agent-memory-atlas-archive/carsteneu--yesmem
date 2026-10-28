package hooks

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func mustOpenHooksDB(t *testing.T) *storage.Store {
	t.Helper()
	tmpDir := t.TempDir()
	s, err := storage.Open(filepath.Join(tmpDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResolveMatchingTask_IgnoresCrossProject(t *testing.T) {
	store := mustOpenHooksDB(t)

	store.InsertLearning(&models.Learning{
		Category: "unfinished", Content: "fix autoRestore.js API calls",
		Project: "/home/testuser/projects/myproject", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})

	msg := resolveMatchingTask(store, "fix autoRestore API calls", "/home/testuser/projects/yesmem")
	if msg != "" {
		t.Errorf("cross-project item was resolved: %s", msg)
	}
}

// YM-260 (Los 11d) changed the contract: prose overlap alone no longer resolves,
// the commit subject has to name the task. This keeps the same-project case.
func TestResolveMatchingTask_ResolvesSameProject(t *testing.T) {
	store := mustOpenHooksDB(t)

	id, err := store.InsertLearning(&models.Learning{
		Category: "unfinished", Content: "fix autoRestore.js API calls",
		Project: "/home/testuser/projects/yesmem", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	msg := resolveMatchingTask(store, "fix autoRestore API calls (#"+strconv.FormatInt(id, 10)+")", "/home/testuser/projects/yesmem")
	if msg == "" {
		t.Error("same-project item was not resolved")
	}
}

func TestIsGitCommit(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"git commit -m 'fix bug'", true},
		{"git commit -m \"$(cat <<'EOF'\nFix TTL\nEOF\n)\"", true},
		{"git status", false},
		{"git push origin main", false},
		{"echo 'git commit'", false}, // the quoted token is not a git call
		{"git -C /other commit -m 'x'", true},
		{"git -c user.email=t@example.com commit -m 'x'", true},
		{"git log -1 && git commit -m 'x'", true},
		{"ls -la", false},
	}
	for _, tt := range tests {
		if got := isGitCommit(tt.cmd); got != tt.want {
			t.Errorf("isGitCommit(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestExtractCommitMessage(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		{`git commit -m "Fix the login bug"`, "Fix the login bug"},
		{`git commit -m 'Add TTL filter'`, "Add TTL filter"},
		{`git commit -m "$(cat <<'EOF'
NEW: unfinished TTL filter
EOF
)"`, "NEW: unfinished TTL filter"},
		{`git status`, ""},
		{`git commit --amend`, ""},
	}
	for _, tt := range tests {
		got := extractCommitMessage(tt.cmd)
		if got != tt.want {
			t.Errorf("extractCommitMessage(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// YM-279: the message flag also arrives in a combined short-flag cluster or in
// its long form, next to a git path that is not a bare token.
func TestExtractCommitMessage_Los11f_FlagShapes(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`git commit -qm "Fix TTL"`, "Fix TTL"},
		{`git commit -am 'Fix TTL'`, "Fix TTL"},
		{`git commit --message="Fix TTL"`, "Fix TTL"},
		{`git commit --message "Fix TTL"`, "Fix TTL"},
		{`git commit --amend`, ""},
	}
	for _, tc := range cases {
		if got := extractCommitMessage(tc.cmd); got != tc.want {
			t.Errorf("extractCommitMessage(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
	if !isGitCommit(`/usr/bin/git commit -m "x"`) {
		t.Errorf("an absolute git path must be detected")
	}
}
