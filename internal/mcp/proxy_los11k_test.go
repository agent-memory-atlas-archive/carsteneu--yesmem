package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-357 (Los 11k): der Selbst-Spawn setzte YESMEM_PROJECTS_DIR auf ein selbst
// gebautes $HOME/.claude/projects. Die Variable hatte keinen Leser: der Daemon
// loest das Verzeichnis aus der config.yaml (main.go, appCfg.ClaudeProjectsDir).
// Eine zweite Ableitung im Environment konnte nur von der echten abweichen, also
// wird sie nicht mehr gesetzt — auch nicht aus der Config.
func TestLos11kStartDaemonPassesNoProjectsDir(t *testing.T) {
	dataDir := t.TempDir()
	// Eine Config mit abweichendem Wert darf den Spawn nicht mehr beeinflussen.
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"),
		[]byte("paths:\n  claude_projects: "+filepath.Join(dataDir, "configured-projects")+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("HOME", t.TempDir())

	var gotEnv []string
	deps := daemonStartDeps{
		unitState:  func() yesmemUnitState { return yesmemUnitAbsent },
		systemctl:  func(...string) error { return nil },
		executable: func() (string, error) { return "/opt/yesmem/yesmem", nil },
		spawn: func(_ string, _ string, _ []string, env []string) error {
			gotEnv = env
			return nil
		},
	}

	if err := startDaemonWith(dataDir, deps); err != nil {
		t.Fatalf("startDaemonWith returned %v, want nil", err)
	}
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "YESMEM_PROJECTS_DIR=") {
			t.Fatalf("spawned env carries %q; the daemon reads the projects dir from config.yaml itself, a second copy in the environment was never read and could drift", kv)
		}
	}
}
