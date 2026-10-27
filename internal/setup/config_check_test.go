package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-463: `yesmem update` reports keys the schema cannot read, `yesmem setup` did not. Both now print the
// same lines from the same function, so an existing config.yaml with an invented key is named instead of
// being replaced silently.

func writeConfigForCheck(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestConfigCheckNamesAnInventedKey(t *testing.T) {
	path := writeConfigForCheck(t, "proxy:\n  enabled: true\nmy_invented_key: 1\n")

	var out, warn bytes.Buffer
	PrintConfigCheck(path, &out, &warn)

	if !strings.Contains(out.String(), `Config check: unknown key "my_invented_key"`) {
		t.Fatalf("invented key not named, got %q (warnings %q)", out.String(), warn.String())
	}
	if warn.Len() != 0 {
		t.Fatalf("unexpected warning: %q", warn.String())
	}
}

func TestConfigCheckNamesAMisplacedProxyKey(t *testing.T) {
	// "keep_recent" belongs under proxy:; at the top level the loader ignores it.
	path := writeConfigForCheck(t, "proxy:\n  enabled: true\nkeep_recent: 5\n")

	var out, warn bytes.Buffer
	PrintConfigCheck(path, &out, &warn)

	if !strings.Contains(out.String(), "misplaced key \"keep_recent\" belongs under \"proxy.keep_recent\"") {
		t.Fatalf("misplaced key not named, got %q", out.String())
	}
}

func TestConfigCheckReportsOK(t *testing.T) {
	path := writeConfigForCheck(t, "proxy:\n  enabled: true\n")

	var out, warn bytes.Buffer
	PrintConfigCheck(path, &out, &warn)

	if strings.TrimSpace(out.String()) != "Config check: OK" {
		t.Fatalf("want a single OK line, got %q", out.String())
	}
}

func TestConfigCheckDoesNotClaimOKForAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	var out, warn bytes.Buffer
	PrintConfigCheck(path, &out, &warn)

	if got := strings.TrimSpace(out.String()); got != "Config check: no config.yaml yet" {
		t.Fatalf("want the missing-file line, got %q", got)
	}
}

func TestConfigCheckWarnsOnUnparseableConfig(t *testing.T) {
	path := writeConfigForCheck(t, "proxy:\n\tenabled: true\n  broken: [\n")

	var out, warn bytes.Buffer
	PrintConfigCheck(path, &out, &warn)

	if !strings.Contains(warn.String(), "Config check warning:") {
		t.Fatalf("want a warning line, got %q", warn.String())
	}
	if out.Len() != 0 {
		t.Fatalf("want no stdout lines, got %q", out.String())
	}
}

// The check has to read the file setup replaces, not the template it writes. Moving the call below the
// write would keep every line green and quietly reduce the report to a statement about the template itself.
func TestWriteConfigForInstallChecksBeforeReplacing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("proxy:\n  enabled: true\nmy_invented_key: 1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, warn bytes.Buffer
	if err := writeConfigForInstall(dir, "proxy:\n  enabled: false\n", &out, &warn); err != nil {
		t.Fatalf("writeConfigForInstall: %v", err)
	}

	// The report names a key that only the replaced file carried.
	if !strings.Contains(out.String(), `unknown key "my_invented_key"`) {
		t.Fatalf("key of the replaced file not reported, got %q (warnings %q)", out.String(), warn.String())
	}
	// And the file on disk is now the template, which does not carry it.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if strings.Contains(string(written), "my_invented_key") {
		t.Fatalf("template still carries the replaced key: %q", string(written))
	}
	if !strings.Contains(string(written), "enabled: false") {
		t.Fatalf("template not written, got %q", string(written))
	}
}
