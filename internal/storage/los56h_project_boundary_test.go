package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los56hCheckout creates a directory tree below dir and marks the given levels as
// checkouts by creating their ".git" entry.
func los56hCheckout(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mark %s as a checkout: %v", dir, err)
	}
	return dir
}

// TestLos56hOwningProjectStopsAtANestedCheckout covers YM-393: OwningProject climbed to
// the nearest ancestor with sessions without looking at where one checkout ends. A
// vendor clone without sessions of its own therefore shared the enclosing repository's
// project identity, and every pin and learning written there was attributed to the wrong
// repository.
func TestLos56hOwningProjectStopsAtANestedCheckout(t *testing.T) {
	s := mustOpenFiles(t)
	root := t.TempDir()
	parent := los56hCheckout(t, filepath.Join(root, "parent"))
	nested := los56hCheckout(t, filepath.Join(parent, "vendor", "lib"))
	plain := filepath.Join(parent, "pkg") // inside the parent, no checkout of its own
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir plain: %v", err)
	}

	if err := s.UpsertSession(&models.Session{ID: "los56h-parent", Project: parent}); err != nil {
		t.Fatalf("seed parent session: %v", err)
	}

	if got := s.OwningProject(nested); got != nested {
		t.Errorf("OwningProject(%q) = %q, want the nested checkout %q: a vendor clone is its own project", nested, got, nested)
	}
	// Inside the nested checkout the identity is the clone, not the enclosing repo.
	if got := s.OwningProject(filepath.Join(nested, "internal", "deep")); got != nested {
		t.Errorf("OwningProject(below the nested checkout) = %q, want %q", got, nested)
	}
	// A plain directory of the parent still belongs to the parent.
	if got := s.OwningProject(plain); got != parent {
		t.Errorf("OwningProject(%q) = %q, want the enclosing project %q", plain, got, parent)
	}
}

// TestLos56hOwningProjectKeepsASubdirectoryWithItsOwnSessions is the other side of the
// checkout boundary: the root is a FLOOR for the ascent, not a jump to it. A directory
// inside the checkout that carries sessions of its own keeps its identity; otherwise every
// existing project registered below a checkout would move to the repository root (cold
// review Los 56h, measured on the live rows).
func TestLos56hOwningProjectKeepsASubdirectoryWithItsOwnSessions(t *testing.T) {
	s := mustOpenFiles(t)
	root := t.TempDir()
	parent := los56hCheckout(t, filepath.Join(root, "parent"))
	plans := filepath.Join(parent, "docs", "plans")
	if err := os.MkdirAll(plans, 0o755); err != nil {
		t.Fatalf("mkdir plans: %v", err)
	}
	if err := s.UpsertSession(&models.Session{ID: "los56h-parent-2", Project: parent}); err != nil {
		t.Fatalf("seed parent session: %v", err)
	}
	if err := s.UpsertSession(&models.Session{ID: "los56h-plans", Project: plans}); err != nil {
		t.Fatalf("seed plans session: %v", err)
	}

	if got := s.OwningProject(plans); got != plans {
		t.Errorf("OwningProject(%q) = %q, want itself: a subdirectory with its own sessions keeps its identity", plans, got)
	}
	if got := s.OwningProject(filepath.Join(plans, "deep")); got != plans {
		t.Errorf("OwningProject(below %q) = %q, want %q", plans, got, plans)
	}
}
