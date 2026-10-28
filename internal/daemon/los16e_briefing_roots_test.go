package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// TestBriefingRefreshIncludesWorktreeParentProjectLos16e (YM-382 [BRF-B15]):
// the refresh worklist came from ListProjects, which knows only projects with
// sessions of their own. A repository whose work happened entirely inside worktree
// checkouts therefore never got a briefing, and a session opened at the repository
// root paid the refinement call on demand, if it got one at all.
func TestBriefingRefreshIncludesWorktreeParentProjectLos16e(t *testing.T) {
	s := mustStore(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("userhomedir: %v", err)
	}
	// A real path: isLiveProjectPath rejects anything missing on disk.
	root, err := os.MkdirTemp(home, ".yesmem-test-los16e-root-*")
	if err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	insertSessionRow(t, s, "sess-los16e-wt", worktree, time.Now().Add(-time.Hour).Format(time.RFC3339))
	// The worktree project carries a stale cache, so it stays a target on its own.
	if err := s.SaveRefinedBriefing(worktree, "stale-hash", "old prose", "opus"); err != nil {
		t.Fatalf("save refined briefing: %v", err)
	}

	targets, err := listProjectsNeedingBriefingRefresh(s, &config.Config{})
	if err != nil {
		t.Fatalf("listProjectsNeedingBriefingRefresh: %v", err)
	}

	var gotRoot, gotWorktree bool
	for _, tt := range targets {
		switch tt.Project.ProjectShort {
		case root:
			gotRoot = true
		case worktree:
			gotWorktree = true
		default:
			t.Errorf("unexpected target: short=%q project=%q", tt.Project.ProjectShort, tt.Project.Project)
		}
	}
	if !gotWorktree {
		t.Errorf("the worktree project itself must stay a refresh target, targets=%v", targets)
	}
	if !gotRoot {
		t.Errorf("the repository root of a worktree-only project must be a refresh target, got %d targets", len(targets))
	}
}

// TestBriefingRefreshSkipsRootBelowOwnedAncestorLos16e (YM-382, Review-Runde 7):
// the live shape of a worktree-only repository is an ancestor that carries sessions
// of its own. A session opened at the root then resolves to that ancestor, so a
// briefing cached under the root key would never be read; the root is no target.
// This test states the remaining gap of BRF-B15 instead of hiding it.
func TestBriefingRefreshSkipsRootBelowOwnedAncestorLos16e(t *testing.T) {
	s := mustStore(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("userhomedir: %v", err)
	}
	parent, err := os.MkdirTemp(home, ".yesmem-test-los16e-owned-*")
	if err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })

	root := filepath.Join(parent, "repo")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	now := time.Now().Format(time.RFC3339)
	insertSessionRow(t, s, "sess-los16e-owned-wt", worktree, now)
	insertSessionRow(t, s, "sess-los16e-owned-parent", parent, now)

	targets, err := listProjectsNeedingBriefingRefresh(s, &config.Config{})
	if err != nil {
		t.Fatalf("listProjectsNeedingBriefingRefresh: %v", err)
	}
	for _, tt := range targets {
		if tt.Project.ProjectShort == root {
			t.Fatalf("a root below an ancestor with sessions is never read, so it must not be refreshed")
		}
	}
}

// TestBriefingRefreshSkipsDormantWorktreeParentLos16e (YM-382, Kaltrreview-Befund 2):
// the synthetic root row owns no session, so the exact-match activity lookup returns
// zero and the dormancy filter failed open. A repository dormant for over a month
// would then be refined on every daemon start and every 24h tick, because worktree
// activity keeps moving its fingerprint. The row's last_active is the only source
// for this case and must feed the filter.
func TestBriefingRefreshSkipsDormantWorktreeParentLos16e(t *testing.T) {
	s := mustStore(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("userhomedir: %v", err)
	}
	root, err := os.MkdirTemp(home, ".yesmem-test-los16e-dormant-*")
	if err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	// Well past briefingInactivityThreshold (30 days).
	dormant := time.Now().Add(-400 * 24 * time.Hour).Format(time.RFC3339)
	insertSessionRow(t, s, "sess-los16e-dormant", worktree, dormant)
	if err := s.SaveRefinedBriefing(worktree, "stale-hash", "old prose", "opus"); err != nil {
		t.Fatalf("save refined briefing: %v", err)
	}

	targets, err := listProjectsNeedingBriefingRefresh(s, &config.Config{})
	if err != nil {
		t.Fatalf("listProjectsNeedingBriefingRefresh: %v", err)
	}
	for _, tt := range targets {
		if tt.Project.ProjectShort == root {
			t.Errorf("a dormant repository must not be refreshed just because it gained a root row, got target %q", root)
		}
	}
}
