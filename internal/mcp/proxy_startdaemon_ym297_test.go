package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestYm297StartDaemonLeavesActiveUnitAlone is the regression test for YM-297
// (2): an MCP client whose socket probe failed must not start a second daemon
// while systemd owns an active unit. The old body spawned "daemon --replace"
// here, which killed the unit's daemon and restarted the cascade.
func TestYm297StartDaemonLeavesActiveUnitAlone(t *testing.T) {
	var spawned, systemctlCalls int
	deps := daemonStartDeps{
		unitState:  func() yesmemUnitState { return yesmemUnitActive },
		systemctl:  func(...string) error { systemctlCalls++; return nil },
		executable: func() (string, error) { return "/opt/yesmem/yesmem", nil },
		spawn: func(string, string, []string, []string) error {
			spawned++
			return nil
		},
	}

	err := startDaemonWith(t.TempDir(), deps)
	if !errors.Is(err, errDaemonOwnedBySystemd) {
		t.Fatalf("active unit: startDaemonWith = %v, want an errDaemonOwnedBySystemd error so the caller fails fast instead of waiting out the dial timeout", err)
	}
	if !strings.Contains(err.Error(), "systemctl --user restart yesmem") {
		t.Errorf("active unit: error %q does not tell the operator how to recover", err)
	}
	if spawned != 0 {
		t.Errorf("active unit: spawned %d daemon process(es), want 0 — systemd owns the restart", spawned)
	}
	if systemctlCalls != 0 {
		t.Errorf("active unit: called systemctl %d time(s), want 0", systemctlCalls)
	}
}

// TestYm297StartDaemonStartsStoppedUnit covers the second branch: a unit that
// exists but is not running is started through systemctl, never by spawning a
// daemon this process would then have to fight.
func TestYm297StartDaemonStartsStoppedUnit(t *testing.T) {
	var spawned int
	var got []string
	deps := daemonStartDeps{
		unitState:  func() yesmemUnitState { return yesmemUnitInactive },
		systemctl:  func(args ...string) error { got = args; return nil },
		executable: func() (string, error) { return "/opt/yesmem/yesmem", nil },
		spawn: func(string, string, []string, []string) error {
			spawned++
			return nil
		},
	}

	if err := startDaemonWith(t.TempDir(), deps); err != nil {
		t.Fatalf("inactive unit: startDaemonWith returned %v, want nil", err)
	}
	if spawned != 0 {
		t.Errorf("inactive unit: spawned %d daemon process(es), want 0", spawned)
	}
	if strings.Join(got, " ") != "start yesmem" {
		t.Errorf("systemctl args = %v, want [start yesmem]", got)
	}
}

// TestYm297StartDaemonNeverReplaces covers the third branch: an installation
// without a user unit lets the client spawn the daemon itself, and the argv may
// not carry --replace. A daemon the client cannot see is not necessarily dead,
// and a client must never kill what it cannot identify.
func TestYm297StartDaemonNeverReplaces(t *testing.T) {
	var gotExe string
	var gotArgs []string
	deps := daemonStartDeps{
		unitState:  func() yesmemUnitState { return yesmemUnitAbsent },
		systemctl:  func(...string) error { return nil },
		executable: func() (string, error) { return "/opt/yesmem/yesmem", nil },
		spawn: func(_ string, exe string, args, _ []string) error {
			gotExe, gotArgs = exe, args
			return nil
		},
	}

	if err := startDaemonWith(t.TempDir(), deps); err != nil {
		t.Fatalf("no unit: startDaemonWith returned %v, want nil", err)
	}
	if gotExe != "/opt/yesmem/yesmem" {
		t.Errorf("spawned exe = %q, want /opt/yesmem/yesmem", gotExe)
	}
	if len(gotArgs) != 1 || gotArgs[0] != "daemon" {
		t.Fatalf("spawned args = %v, want exactly [daemon]", gotArgs)
	}
	for _, a := range gotArgs {
		if strings.Contains(a, "replace") {
			t.Errorf("spawned args %v carry --replace; a client must not replace a daemon it cannot see", gotArgs)
		}
	}
}

// TestYm297StartDaemonPropagatesSpawnError keeps the failure path honest: a
// spawn that fails must surface, not look like success.
func TestYm297StartDaemonPropagatesSpawnError(t *testing.T) {
	want := errors.New("no exec")
	deps := daemonStartDeps{
		unitState:  func() yesmemUnitState { return yesmemUnitAbsent },
		systemctl:  func(...string) error { return nil },
		executable: func() (string, error) { return "/opt/yesmem/yesmem", nil },
		spawn:      func(string, string, []string, []string) error { return want },
	}
	if err := startDaemonWith(t.TempDir(), deps); !errors.Is(err, want) {
		t.Fatalf("startDaemonWith returned %v, want the spawn error", err)
	}
}

// TestYm297DetectUnitStateParsesSystemd covers the parser against the two
// shapes systemd prints. It does not call systemctl.
func TestYm297DetectUnitStateParsesSystemd(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want yesmemUnitState
	}{
		{"active", "LoadState=loaded\nActiveState=active\n", yesmemUnitActive},
		{"activating", "LoadState=loaded\nActiveState=activating\n", yesmemUnitActive},
		{"inactive", "LoadState=loaded\nActiveState=inactive\n", yesmemUnitInactive},
		{"failed", "LoadState=loaded\nActiveState=failed\n", yesmemUnitInactive},
		{"not-found", "LoadState=not-found\nActiveState=inactive\n", yesmemUnitAbsent},
		{"empty", "", yesmemUnitAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseYesmemUnitState(tc.out); got != tc.want {
				t.Errorf("parse(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestYm297SystemctlBinaryRefusesWritableCandidate pins the PATH hardening: a
// group- or world-writable systemctl is refused, a clean one is accepted. Same
// heuristic as sqlite3Binary.
func TestYm297SystemctlBinaryRefusesWritableCandidate(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := systemctlBinary()
	if err != nil {
		t.Fatalf("clean 0755 candidate refused: %v", err)
	}
	if got != bin {
		t.Errorf("systemctlBinary = %q, want %q", got, bin)
	}

	if err := os.Chmod(bin, 0777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := systemctlBinary(); err == nil {
		t.Fatal("world-writable systemctl accepted — any local writer could replace it")
	}
}
