package storage

import (
	"errors"
	"testing"
)

// TestYm297RecoverKeepsResumableOpencodeRow is the regression test for YM-297
// (3). After a daemon restart, an agent row whose *process* is gone but whose
// opencode session still exists must survive: the session keeps running in the
// shared opencode service, and deleting the row drops it out of supervision
// (relay, stop, whoami, send_to). Only rows with neither a process nor a session
// may be deleted.
func TestYm297RecoverKeepsResumableOpencodeRow(t *testing.T) {
	s := mustOpen(t)

	live := Agent{
		ID:                "agent-ym297-live",
		Project:           "p",
		Section:           "yesloop-ym297",
		SessionID:         "sess-live",
		Status:            "running",
		Backend:           "opencode",
		OpencodeSessionID: "ses_live",
		WorkDir:           "/w/live",
	}
	gone := Agent{
		ID:                "agent-ym297-gone",
		Project:           "p",
		Section:           "yesloop-ym297-gone",
		SessionID:         "sess-gone",
		Status:            "running",
		Backend:           "opencode",
		OpencodeSessionID: "ses_gone",
		WorkDir:           "/w/gone",
	}
	for _, a := range []Agent{live, gone} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate %s: %v", a.ID, err)
		}
	}

	// No PID alive: the daemon restart is the scenario.
	var gotDirs []string
	resumable := map[string]bool{"ses_live": true}
	reconnected, reattachable, deleted, err := s.AgentRecoverOrphaned(
		func(int) bool { return false },
		func(sid, workDir string) (bool, error) {
			gotDirs = append(gotDirs, workDir)
			return resumable[sid], nil
		},
	)
	if err != nil {
		t.Fatalf("AgentRecoverOrphaned: %v", err)
	}
	if reconnected != 0 {
		t.Errorf("reconnected = %d, want 0 (no PID is alive)", reconnected)
	}
	if reattachable != 1 {
		t.Errorf("reattachable = %d, want 1 (the row with a live opencode session)", reattachable)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the row with neither process nor session) — deleting a resumable opencode row is the YM-297 harm", deleted)
	}

	kept, err := s.AgentGet(live.ID)
	if err != nil || kept == nil {
		t.Fatalf("AgentGet(%s) = %v, %v — the resumable row must survive", live.ID, kept, err)
	}
	if kept.Status != "stopped" {
		t.Errorf("kept row status = %q, want stopped (no process owns it, and resume_agent only accepts stopped/paused/finished)", kept.Status)
	}
	if kept.OpencodeSessionID != "ses_live" {
		t.Errorf("kept row opencode_session_id = %q, want ses_live — resume needs it", kept.OpencodeSessionID)
	}
	if kept.PID != 0 || kept.SockPath != "" {
		t.Errorf("kept row pid=%d sock=%q, want 0 and empty (no process, no socket)", kept.PID, kept.SockPath)
	}
	if kept.Progress == "stopped" {
		t.Errorf("kept row progress = %q — a live session must not be reported as ended work", kept.Progress)
	}

	if _, err := s.AgentGet(gone.ID); err == nil {
		t.Errorf("row %s has neither a process nor a session and must be deleted", gone.ID)
	}

	// The lookup must see the row's own work dir: that is the second identity
	// fact that keeps a stale, shared session id from holding a row alive.
	found := false
	for _, d := range gotDirs {
		if d == "/w/live" {
			found = true
		}
	}
	if !found {
		t.Errorf("sessionResumable received work dirs %v, want the rows' own /w/live", gotDirs)
	}
}

// TestYm297RecoverKeepsRowWhenLookupFails is the fail-safe half: an unreadable
// session store is not evidence that a session is gone, so the row must be kept,
// not deleted. Deleting on an infrastructure error would reintroduce the exact
// YM-297 harm silently.
func TestYm297RecoverKeepsRowWhenLookupFails(t *testing.T) {
	s := mustOpen(t)
	if err := s.AgentCreate(Agent{
		ID:                "agent-ym297-unverifiable",
		Project:           "p",
		Section:           "yesloop-ym297-unverifiable",
		SessionID:         "sess-u",
		Status:            "running",
		Backend:           "opencode",
		OpencodeSessionID: "ses_u",
		WorkDir:           "/w/u",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	_, reattachable, deleted, err := s.AgentRecoverOrphaned(
		func(int) bool { return false },
		func(string, string) (bool, error) { return false, errors.New("session store unreadable") },
	)
	if err != nil {
		t.Fatalf("AgentRecoverOrphaned: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("lookup error deleted %d row(s), want 0 — an unreadable store must keep the row", deleted)
	}
	if reattachable != 1 {
		t.Errorf("reattachable = %d, want 1 (kept with the unverified reason)", reattachable)
	}
	kept, err := s.AgentGet("agent-ym297-unverifiable")
	if err != nil || kept == nil {
		t.Fatalf("row must survive a failed lookup: %v, %v", kept, err)
	}
	if kept.Status != "stopped" || kept.Progress != agentOrphanProgress {
		t.Errorf("kept row status=%q progress=%q, want stopped/%s", kept.Status, kept.Progress, agentOrphanProgress)
	}
}

// TestYm297RecoverKeepsRowWithoutSessionID covers the pre-opencode and
// claude-backend rows: no session id means no session evidence, so the row is
// deleted as before. This pins that the new keep-path does not swallow them.
func TestYm297RecoverDeletesRowWithoutSessionID(t *testing.T) {
	s := mustOpen(t)
	if err := s.AgentCreate(Agent{
		ID:      "agent-ym297-claude",
		Project: "p",
		Section: "yesloop-ym297-claude",
		Status:  "running",
		Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	_, _, deleted, err := s.AgentRecoverOrphaned(
		func(int) bool { return false },
		func(string, string) (bool, error) { return true, nil },
	)
	if err != nil {
		t.Fatalf("AgentRecoverOrphaned: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (a row without a session id has no reattach path)", deleted)
	}
}
