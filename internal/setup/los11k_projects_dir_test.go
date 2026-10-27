package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-357 (Los 11k): Setup-Wizard und Uninstall bauten $HOME/.claude/projects von
// Hand, obwohl paths.claude_projects seit Los 11j ueber
// config.Config.ClaudeProjectsDir() aufloesbar ist. Beide Pfade lesen jetzt die
// Konfiguration aus dem Datenverzeichnis.
func TestClaudeProjectsDirFromConfigReadsTheConfiguredValue_Los11k(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dataDir := t.TempDir()
	configured := filepath.Join(dataDir, "elsewhere", "projects")
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"),
		[]byte("paths:\n  claude_projects: "+configured+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if got := claudeProjectsDirFromConfig(dataDir); got != configured {
		t.Errorf("claudeProjectsDirFromConfig = %q, want the configured %q", got, configured)
	}
}

// Ohne config.yaml gilt der Home-Default, damit Setup und Uninstall auf einer
// frischen oder bereits abgeraeumten Installation denselben Baum sehen wie der
// Daemon.
func TestClaudeProjectsDirFromConfigFallsBackToHome_Los11k(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := claudeProjectsDirFromConfig(t.TempDir()), filepath.Join(home, ".claude", "projects"); got != want {
		t.Errorf("claudeProjectsDirFromConfig without config.yaml = %q, want %q", got, want)
	}
}

// Der Sweep laeuft jetzt ueber das Verzeichnis, das der Aufrufer uebergibt, und
// raeumt den markierten Abschnitt aus jedem Projekt.
func TestCleanProjectMemoryFilesUsesTheGivenDirectory_Los11k(t *testing.T) {
	projectsDir := t.TempDir()
	projDir := filepath.Join(projectsDir, "-home-x-proj", "memory")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	memPath := filepath.Join(projDir, "MEMORY.md")
	body := "User text\n" + yesmemMemoryMarker + "\nGenerated block\n"
	if err := os.WriteFile(memPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}

	cleanProjectMemoryFiles(projectsDir)

	data, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read MEMORY.md: %v", err)
	}
	if got := string(data); got != "User text\n" {
		t.Errorf("MEMORY.md = %q, want only the user text left", got)
	}
}
