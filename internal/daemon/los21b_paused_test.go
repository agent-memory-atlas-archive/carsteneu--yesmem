package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-255: "paused" means a live process waiting for a relay (Learning #81181), but
// AgentGetActiveBySection counts a paused row as the owner of its section. A pause
// whose process is gone therefore blocked the section: no spawn, and a relay only
// reached a socket nobody held. The spawn path takes such a section over when the
// death is decided, and closes the row through the same transition the periodic
// reaper (reapGoneAgents) uses.
func TestHandleSpawnAgent_TakesOverSectionWhosePauseIsDead(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los21b-paused-gone", Project: "proj", Section: "sec-paused-gone",
		SessionID: "uuid-paused-gone", PID: 0, Status: "paused", Backend: "claude",
		SockPath:  filepath.Join(t.TempDir(), "dead-pause.sock"),
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "work_dir": t.TempDir(), "section": "sec-paused-gone",
		"backend": "claude",
	})
	if resp.Error != "" {
		t.Fatalf("spawn on a section whose pause has no process was refused: %s", resp.Error)
	}

	old, err := s.AgentGet("los21b-paused-gone")
	if err != nil {
		t.Fatalf("get old agent: %v", err)
	}
	if old.Status != "stopped" {
		t.Fatalf("old row status = %q, want stopped: the section was taken over", old.Status)
	}
	if old.PID != 0 || old.SockPath != "" {
		t.Errorf("old row still advertises pid=%d sock_path=%q, want both cleared", old.PID, old.SockPath)
	}
	if !strings.Contains(old.Progress, "no process") && !strings.Contains(old.Progress, "never had a process") {
		t.Errorf("progress = %q, want the closure reason", old.Progress)
	}

	active, err := s.AgentGetActiveBySection("proj", "sec-paused-gone")
	if err != nil || active == nil {
		t.Fatalf("the new agent does not own the section (err=%v, active=%v)", err, active)
	}
	if active.ID == "los21b-paused-gone" {
		t.Fatalf("the section is still reported as owned by the closed row")
	}
}

// The invariant behind the removed second closer: crashRecovery must not touch a
// paused row. reapGoneAgents is the one periodic path that closes a dead pause and
// its grace (goneAgentReapAfter) is a recorded decision; a second closer running on
// a faster tick would override it and take the DONE-guard's paused rows away before
// an operator ever sees the escalation.
func TestCrashRecovery_LeavesAPauseToTheReaper(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "los21b-paused-reaper", Project: "proj", Section: "sec-paused-reaper",
		SessionID: "uuid-paused-reaper", PID: 0, Status: "paused", Backend: "claude",
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	h.crashRecovery()

	got, err := s.AgentGet("los21b-paused-reaper")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused: crashRecovery is not the pause closer", got.Status)
	}
}

// The other side: a pause whose process is alive owns its section. Freeing it would
// start a second process for the same session, which is why only a decided death
// (pidDecidedlyDead) qualifies for a takeover.
func TestHandleSpawnAgent_RefusesSectionWhosePauseHasLiveProcess(t *testing.T) {
	h, s := mustHandler(t)
	child, _ := startBystander(t)
	pinProcessTable(t)
	stubProcCmdline(t, agentProcCmdlineStub)
	stubProcStartTime(t, time.Now().Add(-time.Minute))

	if err := s.AgentCreate(storage.Agent{
		ID: "los21b-paused-live", Project: "proj", Section: "sec-paused-live",
		SessionID: "uuid-paused-live", PID: child.Process.Pid, Status: "paused",
		Backend: "opencode", CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "work_dir": t.TempDir(), "section": "sec-paused-live",
		"backend": "claude",
	})
	if resp.Error == "" {
		t.Fatalf("spawn was accepted although the paused row's process is alive")
	}
	if !strings.Contains(resp.Error, "already has active agent") {
		t.Errorf("error = %q, want the section-ownership refusal", resp.Error)
	}
	got, err := s.AgentGet("los21b-paused-live")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused: the row was not touched", got.Status)
	}
}
