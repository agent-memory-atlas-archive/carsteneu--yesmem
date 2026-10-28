package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los56hLoopSection is the section shape the loop watchdog judges.
const los56hLoopSection = "yesloop-los-56h-probe"

// TestLos56hLoopAgentInRemovedTreeIsStopped covers YM-385(b): a loop process that
// keeps running after its worktree was removed works in a directory that no longer
// exists, with no owner and no limit watching it. The watchdog ends such a row, and
// leaves a row whose tree is still there alone.
func TestLos56hLoopAgentInRemovedTreeIsStopped(t *testing.T) {
	h, s := mustHandler(t)

	removedRoot := t.TempDir()
	gone := filepath.Join(removedRoot, ".worktrees", los56hLoopSection)
	kept := filepath.Join(t.TempDir(), ".worktrees", los56hLoopSection+"-kept")
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatalf("create kept worktree: %v", err)
	}

	for _, a := range []storage.Agent{
		{ID: "loop-gone", Project: "proj", Section: los56hLoopSection, Status: "paused", WorkDir: gone},
		{ID: "loop-kept", Project: "proj", Section: los56hLoopSection + "-kept", Status: "paused", WorkDir: kept},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("create %s: %v", a.ID, err)
		}
	}

	h.reapAgentsInRemovedTrees()

	after, err := s.AgentGet("loop-gone")
	if err != nil || after == nil {
		t.Fatalf("read loop-gone: %v", err)
	}
	if after.Status != "stopped" {
		t.Errorf("a loop row whose worktree is gone must be stopped, status = %q progress = %q", after.Status, after.Progress)
	}
	if !strings.Contains(after.Progress, "worktree") && !strings.Contains(after.Progress, "tree") {
		t.Errorf("the stop must name the reason, progress = %q", after.Progress)
	}

	untouched, err := s.AgentGet("loop-kept")
	if err != nil || untouched == nil {
		t.Fatalf("read loop-kept: %v", err)
	}
	if untouched.Status != "paused" {
		t.Errorf("a loop row whose tree exists must stay untouched, status = %q", untouched.Status)
	}
}

// TestLos56hSpawnRefusesRemovedLoopWorktree covers YM-385(c): a loop agent started into
// a worktree that no longer exists comes up in the daemon's run directory without a
// repository, which is what a merged section looks like after its worktree was removed.
func TestLos56hSpawnRefusesRemovedLoopWorktree(t *testing.T) {
	h, _ := mustHandler(t)

	gone := filepath.Join(t.TempDir(), ".worktrees", "yesloop-merged")
	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "section": "yesloop-merged", "backend": "claude",
		"work_dir": gone,
	})
	if resp.Error == "" {
		t.Fatalf("a spawn into the removed worktree %q must be refused", gone)
	}
	if !strings.Contains(resp.Error, "work_dir") {
		t.Errorf("the refusal must name the parameter, got %q", resp.Error)
	}

	// The other side: an existing worktree still spawns.
	worktree := filepath.Join(t.TempDir(), ".worktrees", "yesloop-alive")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	ok := h.handleSpawnAgent(map[string]any{
		"project": "proj", "section": "yesloop-alive", "backend": "claude",
		"work_dir": worktree,
	})
	if ok.Error != "" {
		t.Fatalf("a spawn into an existing worktree must pass: %s", ok.Error)
	}
}

// TestLos56hRespawnClaimIsExclusive covers the claim that coordinates a crash
// replacement with a stop (YM-397): only one actor may take the crashed row, and the
// claim must not make the row look like the owner of its section.
func TestLos56hRespawnClaimIsExclusive(t *testing.T) {
	h, s := mustHandler(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "crash-1", Project: "proj", Section: "yesloop-claim", Status: "crashed",
	}); err != nil {
		t.Fatalf("create crashed row: %v", err)
	}

	if !h.claimRespawnFor("crash-1") {
		t.Fatal("the first claim of a crashed row must succeed")
	}
	if h.claimRespawnFor("crash-1") {
		t.Error("a second claim must be refused: the row is already claimed")
	}
	claimed, err := s.AgentGet("crash-1")
	if err != nil || claimed == nil {
		t.Fatalf("read claimed row: %v", err)
	}
	if claimed.Status != "respawn_claimed" {
		t.Errorf("the claim must be visible on the row, status = %q", claimed.Status)
	}
	// The claim must not own the section: handleSpawnAgent refuses a section that has an
	// active row, and the replacement has to be able to take it.
	if active, err := s.AgentGetActiveBySection("proj", "yesloop-claim"); err == nil && active != nil {
		t.Errorf("the claimed row must not count as an active owner, got %s", active.Status)
	}

	if !h.releaseRespawnClaim("crash-1", "spawn refused") {
		t.Fatal("releasing the claim must succeed")
	}
	released, err := s.AgentGet("crash-1")
	if err != nil || released == nil {
		t.Fatalf("read released row: %v", err)
	}
	if released.Status != "crashed" {
		t.Errorf("a released claim returns to crashed, status = %q", released.Status)
	}
	if h.releaseRespawnClaim("crash-1", "spawn refused") {
		t.Error("releasing a row that is not claimed must report false")
	}
}

// TestLos56hStopRevokesTheRespawnClaim covers the interleaving YM-397 is about: the
// stop lands between the claim and the replacement, and the replacement must not
// outlive it. The stop wins, deterministically, because the claim is revoked before the
// replacement is judged.
func TestLos56hStopRevokesTheRespawnClaim(t *testing.T) {
	h, s := mustHandler(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "crash-2", Project: "proj", Section: "yesloop-race", Status: "crashed",
	}); err != nil {
		t.Fatalf("create crashed row: %v", err)
	}
	if !h.claimRespawnFor("crash-2") {
		t.Fatal("claim must succeed")
	}

	// The stop arrives while the replacement is being started.
	resp := h.Handle(Request{Method: "stop_agent", Params: map[string]any{"to": "crash-2", "_verify": false}})
	if resp.Error != "" {
		t.Fatalf("stop_agent must accept a claimed row, got %s", resp.Error)
	}

	// The replacement the spawn already created.
	if err := s.AgentCreate(storage.Agent{
		ID: "replacement", Project: "proj", Section: "yesloop-race", Status: "running",
	}); err != nil {
		t.Fatalf("create replacement: %v", err)
	}

	if h.finishRespawn("crash-2", "replacement") {
		t.Error("a revoked claim must not keep the replacement")
	}
	replacement, err := s.AgentGet("replacement")
	if err != nil || replacement == nil {
		t.Fatalf("read replacement: %v", err)
	}
	if replacement.Status != "stopped" {
		t.Errorf("the replacement of a revoked claim must be stopped, status = %q", replacement.Status)
	}
}

// TestLos56hRespawnKeepsTheReplacement covers the normal run: nothing stopped the row,
// so the claim commits, the crashed row is closed as replaced, and the replacement stays.
func TestLos56hRespawnKeepsTheReplacement(t *testing.T) {
	h, s := mustHandler(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "crash-3", Project: "proj", Section: "yesloop-ok", Status: "crashed",
	}); err != nil {
		t.Fatalf("create crashed row: %v", err)
	}
	if !h.claimRespawnFor("crash-3") {
		t.Fatal("claim must succeed")
	}
	if err := s.AgentCreate(storage.Agent{
		ID: "replacement-3", Project: "proj", Section: "yesloop-ok", Status: "running",
	}); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	if !h.finishRespawn("crash-3", "replacement-3") {
		t.Fatal("an unrevoked claim must keep the replacement")
	}

	closed, err := s.AgentGet("crash-3")
	if err != nil || closed == nil {
		t.Fatalf("read crashed row: %v", err)
	}
	if closed.Status != "stopped" {
		t.Errorf("the replaced row must be closed, status = %q", closed.Status)
	}
	if !strings.Contains(closed.Progress, "replacement-3") {
		t.Errorf("the closed row must name its replacement, progress = %q", closed.Progress)
	}
	kept, err := s.AgentGet("replacement-3")
	if err != nil || kept == nil {
		t.Fatalf("read replacement: %v", err)
	}
	if kept.Status != "running" {
		t.Errorf("the kept replacement must stay running, status = %q", kept.Status)
	}
}

// TestLos56hStaleRespawnClaimIsClosed covers the consequence finding on YM-397: a daemon
// that dies between the claim and the replacement leaves a row in "respawn_claimed", and no
// other pass looks at that status. On main the row stayed "crashed" and the abandoned-crash
// closer collected it; the claim must not take that owner away.
func TestLos56hStaleRespawnClaimIsClosed(t *testing.T) {
	h, s := mustHandler(t)

	// The row is old: a claim that is still there hours later belongs to a row that was
	// created long before, and the closer measures from its newest stamp.
	old := time.Now().Add(-2 * time.Hour)
	if err := s.AgentCreate(storage.Agent{
		ID: "stale-claim", Project: "proj", Section: "yesloop-stale", Status: "respawn_claimed",
		CreatedAt: old.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("create claimed row: %v", err)
	}
	// The claim was taken long before this pass: the closer reads the claim's own stamp,
	// so the claim is taken here with a timestamp two hours old instead of trusting a
	// column update (last_restart_at is not on update_agent's allowlist).
	stale := old.Format("2006-01-02 15:04:05")
	if err := s.AgentUpdate("stale-claim", map[string]any{"status": "crashed"}); err != nil {
		t.Fatalf("return the row to crashed: %v", err)
	}
	if claimed, err := s.AgentClaimRespawn("stale-claim", "respawn in flight", stale, stale); err != nil || !claimed {
		t.Fatalf("claim with a stale stamp: claimed=%v err=%v", claimed, err)
	}

	h.crashRecovery()

	row, err := s.AgentGet("stale-claim")
	if err != nil || row == nil {
		t.Fatalf("read row: %v", err)
	}
	if row.Status == "respawn_claimed" {
		t.Errorf("a claim older than the abandoned grace must be closed, status = %q", row.Status)
	}

	// The other side: a fresh claim is left alone, its replacement may still be starting.
	if err := s.AgentCreate(storage.Agent{
		ID: "fresh-claim", Project: "proj", Section: "yesloop-fresh", Status: "respawn_claimed",
	}); err != nil {
		t.Fatalf("create fresh claim: %v", err)
	}
	if err := s.AgentUpdate("fresh-claim", map[string]any{"status": "crashed"}); err != nil {
		t.Fatalf("return the fresh row to crashed: %v", err)
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	if claimed, err := s.AgentClaimRespawn("fresh-claim", "respawn in flight", now, now); err != nil || !claimed {
		t.Fatalf("claim a fresh row: claimed=%v err=%v", claimed, err)
	}
	h.crashRecovery()
	fresh, err := s.AgentGet("fresh-claim")
	if err != nil || fresh == nil {
		t.Fatalf("read fresh row: %v", err)
	}
	if fresh.Status != "respawn_claimed" {
		t.Errorf("a fresh claim must stay, status = %q", fresh.Status)
	}
}

// TestLos56hUpdateAgentRefusesDaemonOwnedStatus covers the security finding on YM-397:
// "status" is on update_agent's allowlist, and respawn_claimed is a token the daemon owns.
// A caller that could write it would leave its section unowned (AgentGetActiveBySection
// ignores the value) and hand its own row to the abandoned-claim closer, which closes it
// without a signal.
func TestLos56hUpdateAgentRefusesDaemonOwnedStatus(t *testing.T) {
	h, s := mustHandler(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "caller-row", Project: "proj", Section: "yesloop-caller", Status: "running",
	}); err != nil {
		t.Fatalf("create row: %v", err)
	}

	resp := h.Handle(Request{Method: "update_agent", Params: map[string]any{
		"id":     "caller-row",
		"fields": map[string]any{"status": "respawn_claimed"},
	}})
	if resp.Error == "" {
		t.Error("update_agent must refuse the daemon-owned status respawn_claimed")
	}
	row, err := s.AgentGet("caller-row")
	if err != nil || row == nil {
		t.Fatalf("read row: %v", err)
	}
	if row.Status != "running" {
		t.Errorf("the refused write must not land, status = %q", row.Status)
	}

	// The statuses a caller legitimately reports still work.
	ok := h.Handle(Request{Method: "update_agent", Params: map[string]any{
		"id":     "caller-row",
		"fields": map[string]any{"status": "paused", "progress": "waiting for a relay"},
	}})
	if ok.Error != "" {
		t.Fatalf("a caller-reported status must stay allowed: %s", ok.Error)
	}
}
