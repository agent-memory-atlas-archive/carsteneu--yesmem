package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestReapStaleMCPHelpersKillsOnlyAnchoredStaleHelpers: the reap decides on the
// same argv-anchored scan as `yesmem status`. The fixture carries one stale helper
// (fixtureHelperStale, exe replaced) and several processes whose command line
// merely contains the words — none of those may be signalled.
func TestReapStaleMCPHelpersKillsOnlyAnchoredStaleHelpers(t *testing.T) {
	root := buildProcFixture(t)
	now := time.Unix(fixtureBtime+1000, 0)

	var killed []int
	reaped, err := ReapStaleMCPHelpers(root, now, 1, func(pid int) error {
		killed = append(killed, pid)
		return nil
	})
	if err != nil {
		t.Fatalf("ReapStaleMCPHelpers: %v", err)
	}

	want := []int{fixtureHelperStale}
	if len(reaped) != len(want) || reaped[0] != want[0] {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}
	if len(killed) != 1 || killed[0] != fixtureHelperStale {
		t.Fatalf("signalled pids = %v, want only %d", killed, fixtureHelperStale)
	}
}

// TestReapStaleMCPHelpersSparesSelf: a helper whose pid is our own is never
// signalled, otherwise `yesmem reap-stale-mcp` could kill itself mid-sweep.
func TestReapStaleMCPHelpersSparesSelf(t *testing.T) {
	root := buildProcFixture(t)
	now := time.Unix(fixtureBtime+1000, 0)

	reaped, err := ReapStaleMCPHelpers(root, now, fixtureHelperStale, func(pid int) error {
		t.Errorf("signalled our own pid %d", pid)
		return nil
	})
	if err != nil {
		t.Fatalf("ReapStaleMCPHelpers: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none", reaped)
	}
}

// TestReapStaleMCPHelpersDryRunListsWithoutSignalling pins the listing mode: the
// Makefile and the orchestrator's verification use it to look before killing, so it
// must name the same helpers the reap would end.
func TestReapStaleMCPHelpersDryRunListsWithoutSignalling(t *testing.T) {
	root := buildProcFixture(t)
	now := time.Unix(fixtureBtime+1000, 0)

	reaped, err := ReapStaleMCPHelpers(root, now, 1, nil)
	if err != nil {
		t.Fatalf("ReapStaleMCPHelpers: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != fixtureHelperStale {
		t.Fatalf("dry run listed %v, want [%d]", reaped, fixtureHelperStale)
	}
}

// TestReapStaleMCPHelpersFailingKillIsNotReportedAsReaped: the count is what the
// deploy log claims, so a signal that failed must not appear in it.
func TestReapStaleMCPHelpersFailingKillIsNotReportedAsReaped(t *testing.T) {
	root := buildProcFixture(t)
	now := time.Unix(fixtureBtime+1000, 0)

	reaped, err := ReapStaleMCPHelpers(root, now, 1, func(pid int) error {
		return errors.New("operation not permitted: " + strconv.Itoa(pid))
	})
	if err != nil {
		t.Fatalf("ReapStaleMCPHelpers: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none after a failed signal", reaped)
	}
}

// TestStillStaleHelperRejectsARecycledPID: the second look before the signal is
// what separates "the helper the scan saw" from "whatever holds that pid now".
func TestStillStaleHelperRejectsARecycledPID(t *testing.T) {
	root := buildProcFixture(t)
	if !stillStaleHelper(root, fixtureHelperStale, 1) {
		t.Fatal("a stale helper of the fixture was not recognised")
	}
	os.RemoveAll(filepath.Join(root, strconv.Itoa(fixtureHelperStale)))
	if stillStaleHelper(root, fixtureHelperStale, 1) {
		t.Fatal("a pid whose entry vanished still counted as a stale helper")
	}
}

// TestOwnedByCurrentUserGuardsTheReap: a reap under sudo must not end another
// account's helpers. The negative case here is a pid the process table does not
// know; the foreign-uid case cannot be built without root (chown), so it stays
// untested — the check itself is one syscall.Stat field.
func TestOwnedByCurrentUserGuardsTheReap(t *testing.T) {
	root := buildProcFixture(t)
	if !ownedByCurrentUser(root, fixtureHelperStale) {
		t.Error("a fixture entry owned by this user was not recognised")
	}
	if ownedByCurrentUser(root, 999999) {
		t.Error("a pid without an entry counted as our own process")
	}
}
