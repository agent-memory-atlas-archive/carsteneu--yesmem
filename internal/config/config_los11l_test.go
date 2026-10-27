package config

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-381 (Los 11l): paths.archive hatte keinen Produktionsleser. Der Daemon baute
// das Archiv aus cfg.DataDir (daemon.go:235), die Vorlage versprach aber einen
// konfigurierbaren Pfad. ArchiveDir ist der Leser; die Entscheidung gehoert hierher
// und nicht an die Aufrufstelle, weil ein leerer Wert und der Loader-Default
// verschiedene Dinge sind: der Default ist auf ~/.claude/yesmem verankert, auch
// wenn der Daemon gegen ein anderes Datenverzeichnis laeuft.
func TestArchiveDirResolvesConfiguredPath_Los11l(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(t.TempDir(), "data")

	loaded, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	custom := filepath.Join(t.TempDir(), "custom-archive")
	configured := &Config{Paths: PathsConfig{Archive: custom}}

	cases := []struct {
		name    string
		cfg     *Config
		dataDir string
		want    string
	}{
		{"nil config falls back to data dir", nil, dataDir, filepath.Join(dataDir, "archive")},
		{"empty paths.archive falls back to data dir", &Config{}, dataDir, filepath.Join(dataDir, "archive")},
		{"configured paths.archive wins", configured, dataDir, custom},
		{"loader default is not a configuration", loaded, dataDir, filepath.Join(dataDir, "archive")},
		{"loader default under the default data dir stays", loaded, filepath.Join(home, ".claude", "yesmem"), filepath.Join(home, ".claude", "yesmem", "archive")},
		{"relative value has no anchor", &Config{Paths: PathsConfig{Archive: "archive"}}, dataDir, filepath.Join(dataDir, "archive")},
	}
	for _, tc := range cases {
		if got := tc.cfg.ArchiveDir(tc.dataDir); got != tc.want {
			t.Errorf("%s: ArchiveDir(%q) = %q, want %q", tc.name, tc.dataDir, got, tc.want)
		}
	}
}

// Ohne HOME kann Load das "~/" der Vorlage nicht aufloesen; der Literalwert darf
// nicht als konfiguriert gelten, sonst legt der Daemon ein Verzeichnis namens "~"
// unter seinem Arbeitsverzeichnis an.
func TestArchiveDirWithoutHomeFallsBack_Los11l(t *testing.T) {
	t.Setenv("HOME", "")
	dataDir := filepath.Join(t.TempDir(), "data")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("paths:\n  archive: ~/.claude/yesmem/archive\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dataDir, "archive"); cfg.ArchiveDir(dataDir) != want {
		t.Errorf("ArchiveDir = %q, want the data-dir fallback %q", cfg.ArchiveDir(dataDir), want)
	}
}

// Ein konfigurierter Pfad muss auch durch Load hindurch ankommen; sonst waere
// ArchiveDir nur ein Umweg um denselben toten Knopf.
func TestArchiveDirSeesLoadedConfigValue_Los11l(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	custom := filepath.Join(t.TempDir(), "archive-custom")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("paths:\n  archive: "+custom+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ArchiveDir(filepath.Join(home, ".claude", "yesmem")); got != custom {
		t.Errorf("ArchiveDir = %q, want the configured %q", got, custom)
	}
}
