package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateSystemdUnitsDoesNotWriteThroughASymlink: a symlink at the backup path
// is not a copy. Following it would write the unit's contents to whatever it points
// at, so the migration has to refuse and leave the unit alone.
func TestMigrateSystemdUnitsDoesNotWriteThroughASymlink(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)
	target := filepath.Join(home, "elsewhere.conf")
	if err := os.Symlink(target, UnitBackupPath(daemonPath)); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateSystemdUnits(home, t.TempDir()); err == nil {
		t.Fatal("MigrateSystemdUnits reported success with a symlink at the backup path")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the migration wrote through the symlink")
	}
	if got := readFile(t, daemonPath); got != legacyDaemonUnit {
		t.Errorf("unit was rewritten without a usable copy:\n%s", got)
	}
}

// TestKeepUnitBackupKeepsTheUnitsPermissions: a unit the operator hardened must not
// become a world-readable copy just because it was backed up.
func TestKeepUnitBackupKeepsTheUnitsPermissions(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)
	if err := os.Chmod(daemonPath, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateSystemdUnits(home, t.TempDir()); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	info, err := os.Stat(UnitBackupPath(daemonPath))
	if err != nil {
		t.Fatalf("no rollback copy: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("copy mode = %04o, want the unit's 0600", perm)
	}
}

// YM-333 (Los 38o): the watchdog migration is a coupling between the unit and the
// binary, and it has no way back — a downgraded binary never sends READY, so the
// migrated unit blocks the start for TimeoutStartSec and then fails. The binary
// that could undo it is exactly the one that cannot start. The migration therefore
// has to leave the pre-migration unit behind, once, so the way back is a copy.
func TestMigrateSystemdUnitsKeepsThePrenotifyUnitForARollback(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)
	writeUnitFixture(t, home, "yesmem-proxy.service", legacyProxyUnit)

	if _, err := MigrateSystemdUnits(home, t.TempDir()); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}

	backup := UnitBackupPath(daemonPath)
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no rollback copy at %s: %v", backup, err)
	}
	if string(data) != legacyDaemonUnit {
		t.Errorf("rollback copy is not the unit as it was before the migration:\n%s", string(data))
	}

	// A second migration run must not overwrite the copy: it has to keep the
	// original bytes, not the unit of the run before.
	if _, err := MigrateSystemdUnits(home, t.TempDir()); err != nil {
		t.Fatalf("second MigrateSystemdUnits: %v", err)
	}
	if again := readFile(t, backup); again != legacyDaemonUnit {
		t.Errorf("second run rewrote the rollback copy:\n%s", again)
	}
}

// TestMigrateSystemdUnitsRefusesToMigrateWithoutItsRollbackCopy: if the copy cannot
// be written, the irreversible rewrite must not happen. A unit that changed without
// a way back is worse than an unmigrated one.
func TestMigrateSystemdUnitsRefusesToMigrateWithoutItsRollbackCopy(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)

	// A directory where the copy would go: os.WriteFile fails, the unit must stay.
	if err := os.MkdirAll(UnitBackupPath(daemonPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSystemdUnits(home, t.TempDir()); err == nil {
		t.Fatalf("MigrateSystemdUnits reported success although the rollback copy could not be written")
	}
	if got := readFile(t, daemonPath); got != legacyDaemonUnit {
		t.Errorf("unit was rewritten without a rollback copy:\n%s", got)
	}
}

// TestMigrateSystemdUnitsNamesTheWayBack: the copy is only a way back for someone
// who knows it exists. The migrated unit carries the command, because the person
// who sees a failing start looks there, not at the migration output of a run from
// months ago.
func TestMigrateSystemdUnitsNamesTheWayBack(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)
	proxyPath := writeUnitFixture(t, home, "yesmem-proxy.service", legacyProxyUnit)

	if _, err := MigrateSystemdUnits(home, t.TempDir()); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}

	daemon := readFile(t, daemonPath)
	if !strings.Contains(daemon, ".pre-migrate.bak") || !strings.Contains(daemon, "daemon-reload") {
		t.Errorf("migrated unit does not name the way back:\n%s", daemon)
	}
	// The proxy unit is only stripped of the network wait: nothing irreversible
	// happened to it, so it gets neither a hint nor a copy.
	if proxy := readFile(t, proxyPath); strings.Contains(proxy, ".pre-migrate.bak") {
		t.Errorf("proxy unit carries a rollback hint although it was not changed irreversibly:\n%s", proxy)
	}
	if _, err := os.Stat(UnitBackupPath(proxyPath)); err == nil {
		t.Errorf("proxy unit got a rollback copy although only the network wait was removed")
	}
}
