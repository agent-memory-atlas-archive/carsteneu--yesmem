package setup

// YM-291 point 6: the daemon unit must declare ANTHROPIC_BASE_URL instead of
// inheriting it from the login session's systemd manager environment. A
// sandboxed claude without it talks straight to Anthropic, so memory injection,
// ledger and cost accounting silently stop working.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLos61eProxyBaseURLFromListen(t *testing.T) {
	cases := []struct{ name, listen, want string }{
		{"empty falls back to the default", "", "http://localhost:9099"},
		{"bare port", ":9099", "http://localhost:9099"},
		{"loopback host", "127.0.0.1:9200", "http://127.0.0.1:9200"},
		{"named host", "localhost:9300", "http://localhost:9300"},
		{"spaces are trimmed", "  :9400  ", "http://localhost:9400"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proxyBaseURL(c.listen); got != c.want {
				t.Errorf("proxyBaseURL(%q) = %q, want %q", c.listen, got, c.want)
			}
		})
	}
}

func TestLos61eDaemonUnitCarriesTheProxyBaseURL(t *testing.T) {
	unit := buildDaemonUnit("/usr/local/bin/yesmem", ":0", "/home/u/.Xauthority", "unix:path=/run/user/1000/bus", "http://localhost:9099")
	if !strings.Contains(unit, `Environment="ANTHROPIC_BASE_URL=http://localhost:9099"`) {
		t.Errorf("daemon unit does not declare ANTHROPIC_BASE_URL:\n%s", unit)
	}
}

func TestLos61eMigrateSystemdUnitsAddsTheProxyBaseURL(t *testing.T) {
	stubMigrationSelf(t, "/usr/local/bin/yesmem")
	home := t.TempDir()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("proxy:\n  listen: \":9200\"\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A unit as written before this change: notify type present, no proxy URL.
	unit := "[Unit]\nDescription=YesMem\n\n[Service]\nType=notify\nNotifyAccess=main\nWatchdogSec=90\nTimeoutStartSec=300\nExecStart=/usr/local/bin/yesmem daemon --replace\n\n[Install]\nWantedBy=default.target\n"
	path := filepath.Join(serviceDir, "yesmem.service")
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	if _, err := MigrateSystemdUnits(home, dataDir); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(got), `Environment="ANTHROPIC_BASE_URL=http://localhost:9200"`) {
		t.Errorf("migration did not add the proxy URL from the configured port:\n%s", got)
	}
	if !strings.Contains(string(got), "Type=notify") {
		t.Error("migration dropped an existing directive")
	}
}

func TestLos61eMigrateSystemdUnitsKeepsAnExplicitProxyBaseURL(t *testing.T) {
	stubMigrationSelf(t, "/usr/local/bin/yesmem")
	home := t.TempDir()
	dataDir := t.TempDir()
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unit := "[Service]\nType=notify\nWatchdogSec=90\nTimeoutStartSec=300\nExecStart=/usr/local/bin/yesmem daemon --replace\nEnvironment=\"ANTHROPIC_BASE_URL=http://127.0.0.1:1234\"\n"
	path := filepath.Join(serviceDir, "yesmem.service")
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	if _, err := MigrateSystemdUnits(home, dataDir); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(got), "localhost:9099") {
		t.Errorf("migration overwrote an explicit value:\n%s", got)
	}
	if !strings.Contains(string(got), "http://127.0.0.1:1234") {
		t.Errorf("migration dropped the explicit value:\n%s", got)
	}
}

func TestLos61eMigrateSystemdUnitsSkipsAForeignBinary(t *testing.T) {
	stubMigrationSelf(t, "/usr/local/bin/yesmem")
	home := t.TempDir()
	dataDir := t.TempDir()
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unit := "[Service]\nExecStart=/opt/other/daemon --replace\nType=notify\nWatchdogSec=90\nTimeoutStartSec=300\n"
	path := filepath.Join(serviceDir, "yesmem.service")
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	if _, err := MigrateSystemdUnits(home, dataDir); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if strings.Contains(string(got), "ANTHROPIC_BASE_URL") {
		t.Errorf("migration touched a unit that runs someone else's binary:\n%s", got)
	}
}
