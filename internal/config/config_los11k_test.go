package config

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-357 (Los 11k): Load uebernahm paths.db, paths.archive, paths.bleve_index
// und paths.opencode_db unexpandiert. Die Installer-Vorlage schreibt sie in der
// "~/"-Form (setup.go:1546-1550). Ein literales "~" ist kein Verzeichnis: der
// strings-Pfad des Daemons (filepath.Dir(Paths.DB), daemon.go:816 und :1558),
// cmd_claudemd und der Opencode-Scanner (Paths.OpencodeDB, daemon.go:303) liefen
// damit ins Leere.
func TestLoadExpandsHomeInEveryConfiguredPath_Los11k(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `paths:
  db: ~/.claude/yesmem/yesmem.db
  bleve_index: ~/.claude/yesmem/bleve-index
  archive: ~/.claude/yesmem/archive
  claude_projects: ~/.claude/projects
  opencode_db: ~/.local/share/opencode/opencode.db
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct{ name, got, want string }{
		{"paths.db", cfg.Paths.DB, filepath.Join(home, ".claude", "yesmem", "yesmem.db")},
		{"paths.bleve_index", cfg.Paths.BleveIndex, filepath.Join(home, ".claude", "yesmem", "bleve-index")},
		{"paths.archive", cfg.Paths.Archive, filepath.Join(home, ".claude", "yesmem", "archive")},
		{"paths.claude_projects", cfg.Paths.ClaudeProjects, filepath.Join(home, ".claude", "projects")},
		{"paths.opencode_db", cfg.Paths.OpencodeDB, filepath.Join(home, ".local", "share", "opencode", "opencode.db")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (a configured \"~/\" path must reach the consumer absolute)", tc.name, tc.got, tc.want)
		}
	}
}

// Absolute Werte bleiben unangetastet, leere bleiben leer: fuer paths.db ist ""
// die Aussage "nicht konfiguriert", und ein erfundener Default wuerde den
// Aufrufer eine Datei oeffnen lassen, die niemand genannt hat.
func TestLoadKeepsAbsoluteAndEmptyPaths_Los11k(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	custom := filepath.Join(t.TempDir(), "custom")
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "paths:\n  db: " + custom + "/yesmem.db\n  archive: \"\"\n  opencode_db: /srv/opencode.db\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := custom + "/yesmem.db"; cfg.Paths.DB != want {
		t.Errorf("paths.db = %q, want the untouched absolute value %q", cfg.Paths.DB, want)
	}
	if cfg.Paths.Archive != "" {
		t.Errorf("paths.archive = %q, want the explicit empty value kept", cfg.Paths.Archive)
	}
	if cfg.Paths.OpencodeDB != "/srv/opencode.db" {
		t.Errorf("paths.opencode_db = %q, want /srv/opencode.db", cfg.Paths.OpencodeDB)
	}
}

// Ohne HOME gibt es nichts zu expandieren: das "~" bleibt stehen, statt in einen
// Pfad relativ zum Arbeitsverzeichnis zu rutschen.
func TestLoadWithoutHomeLeavesTilde_Los11k(t *testing.T) {
	t.Setenv("HOME", "")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("paths:\n  db: ~/.claude/yesmem/yesmem.db\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Paths.DB != "~/.claude/yesmem/yesmem.db" {
		t.Errorf("paths.db without HOME = %q, want the literal value back", cfg.Paths.DB)
	}
}
