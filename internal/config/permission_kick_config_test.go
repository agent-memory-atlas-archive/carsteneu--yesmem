package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig_PermissionKickEnabled(t *testing.T) {
	cfg := Default()
	if !cfg.Agents.PermissionKick {
		t.Error("Agents.PermissionKick default = false, want true")
	}
	if cfg.Agents.PermissionKickDelay != "" {
		t.Errorf("Agents.PermissionKickDelay default = %q, want empty", cfg.Agents.PermissionKickDelay)
	}
	if cfg.Agents.PermissionKickInterval != "" {
		t.Errorf("Agents.PermissionKickInterval default = %q, want empty", cfg.Agents.PermissionKickInterval)
	}
}

func TestLoad_PermissionKickOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "agents:\n  permission_kick: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.PermissionKick {
		t.Error("Agents.PermissionKick = true, want false (explicit override)")
	}
}

func TestLoad_PermissionKickDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "agents:\n  permission_kick: true\n  permission_kick_delay: \"90s\"\n  permission_kick_interval: \"10m\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.PermissionKickDelay != "90s" {
		t.Errorf("PermissionKickDelay = %q, want 90s", cfg.Agents.PermissionKickDelay)
	}
	if cfg.Agents.PermissionKickInterval != "10m" {
		t.Errorf("PermissionKickInterval = %q, want 10m", cfg.Agents.PermissionKickInterval)
	}
}
