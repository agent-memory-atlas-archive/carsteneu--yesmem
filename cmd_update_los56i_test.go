package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/setup"
)

// YM-386 [PLG-F34] Teil 1 (Los 56i): "yesmem update" frischte Skills, Caps,
// Plugin, Window-Plugin und Themes auf, aber nicht die opencode-Commands. Nach
// einem Uninstall stellte nur ein vollstaendiges "yesmem setup" sie wieder her,
// und eine Bestandsinstallation bekam sie durch ein Update nie.
//
// runMigrateCmd selbst ist nicht testbar: es oeffnet die Live-DB, ruft systemctl
// und schreibt in ~/.claude. Der Test bindet deshalb die Aufrufstelle im
// Migrationspfad (dasselbe Vorgehen wie beim Deploy-Lock-Test der Makefile) und
// prueft daneben, dass der Installer in einem Temp-HOME tatsaechlich Dateien
// anlegt.
func TestLos56i_MigrationInstallsTheOpencodeCommands(t *testing.T) {
	source, err := os.ReadFile("cmd_update.go")
	if err != nil {
		t.Fatalf("read cmd_update.go: %v", err)
	}
	if !strings.Contains(string(source), "setup.InstallOpencodeCommands(") {
		t.Error("runMigrateCmd does not call setup.InstallOpencodeCommands: a fresh install writes the commands, an update never restores them")
	}

	home := t.TempDir()
	n, err := setup.InstallOpencodeCommands(home)
	if err != nil {
		t.Fatalf("install into a temp home: %v", err)
	}
	if n == 0 {
		t.Fatal("the installer wrote no command files")
	}
	entries, err := os.ReadDir(filepath.Join(home, ".config", "opencode", "commands"))
	if err != nil {
		t.Fatalf("read installed commands: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the commands directory is empty after a successful install")
	}
}
