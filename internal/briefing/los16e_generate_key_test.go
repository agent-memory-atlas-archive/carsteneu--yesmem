package briefing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TestGenerateForKeyUsesTheKeyItselfLos16e (YM-382, Review-Runde 6): the refresh
// loop holds the row identity and must render that project, not the ancestor
// OwningProject climbs to. The fixture gives an ancestor a session of its own, which
// is exactly the live shape of a repository whose work happens in worktrees.
func TestGenerateForKeyUsesTheKeyItselfLos16e(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	parent := filepath.Join(t.TempDir(), "collection")
	root := filepath.Join(parent, "los16e-key")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := s.UpsertSession(&models.Session{
		ID: "sess-los16e-key-wt", Project: worktree, ProjectShort: worktree,
		MessageCount: 6, StartedAt: time.Now().Add(-time.Hour),
		JSONLPath: "/sess-los16e-key-wt.jsonl", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.UpsertSession(&models.Session{
		ID: "sess-los16e-key-parent", Project: parent, ProjectShort: parent,
		MessageCount: 6, StartedAt: time.Now().Add(-time.Hour),
		JSONLPath: "/sess-los16e-key-parent.jsonl", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert parent session: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		Content: "KEY_ONLY_WORKTREE_LEARNING", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-key-wt", Project: worktree, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	// Since YM-393 a checkout root resolves to itself, so the refresh loop may key
	// a row on it and the row is readable. Before that fix this fixture made
	// OwningProject climb to the parent, and the same assertion failed — which is
	// exactly what the test now guards.
	if got := s.OwningProject(root); got != root {
		t.Fatalf("precondition: a checkout root must be its own project, got %q", got)
	}

	g := NewFromConfig(s, &config.Config{})
	if got := g.GenerateForKey(root); !strings.Contains(got, "KEY_ONLY_WORKTREE_LEARNING") {
		t.Fatalf("GenerateForKey must render the row's own project, got %d bytes without the learning", len(got))
	}
}
