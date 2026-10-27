package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/setup"
)

// YM-333 (Los 38o): the migration keeps the pre-notify unit, but a copy nobody
// names is not a way back. The migration output names it, and only when it exists.
func TestSystemdRollbackHintNamesTheCopyOnlyWhenItExists(t *testing.T) {
	home := t.TempDir()
	if hint := systemdRollbackHint(home); hint != "" {
		t.Fatalf("hint for an install without a rollback copy: %q, want empty", hint)
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(unitDir, "yesmem.service")
	backup := setup.UnitBackupPath(unit)
	if err := os.WriteFile(backup, []byte("[Service]\nType=simple\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hint := systemdRollbackHint(home)
	for _, want := range []string{backup, unit, "daemon-reload"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not name %q: %s", want, hint)
		}
	}
}

// TestSystemdRollbackHintNamesTheGapItCannotClose: a unit an earlier binary migrated
// already has no copy, and no fix can invent one. Saying it is better than silence,
// because the reader would otherwise assume a way back exists.
func TestSystemdRollbackHintNamesTheGapItCannotClose(t *testing.T) {
	home := t.TempDir()
	if hint := systemdRollbackHint(home); hint != "" {
		t.Fatalf("hint without a unit: %q, want empty", hint)
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(unitDir, "yesmem.service")
	if err := os.WriteFile(unit, []byte("[Service]\nType=notify\nWatchdogSec=90\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hint := systemdRollbackHint(home)
	if !strings.Contains(hint, unit) || !strings.Contains(hint, "Type=simple") {
		t.Fatalf("hint does not name the unit and the manual way back: %s", hint)
	}
}

// TestSystemdRollbackHintIsSilentForASimpleUnit: a unit that was never migrated needs
// no rollback advice, and printing some would teach the reader to ignore the line.
func TestSystemdRollbackHintIsSilentForASimpleUnit(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "yesmem.service"), []byte("[Service]\nType=simple\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hint := systemdRollbackHint(home); hint != "" {
		t.Fatalf("hint for an unmigrated unit: %q, want empty", hint)
	}
	if !daemonUnitInstalled(home) {
		t.Error("daemonUnitInstalled did not see the unit")
	}
}
