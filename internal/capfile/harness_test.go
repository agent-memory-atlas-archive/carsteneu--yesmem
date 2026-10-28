package capfile

// Shared fixtures for the capfile package: the bundled cap files the tests read,
// and the two operations they all repeat (write an executable stub, pull one
// script body out of a cap).

import (
	"os"
	"runtime"
	"testing"
)

const (
	proxyHealthCapPath = "../../caps/bundled-caps/proxy_health/CAP.md"
	telegramCapPath    = "../../caps/bundled-caps/telegram/CAP.md"
)

// writeStub writes an executable stub onto a fake PATH entry.
func writeStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

// bundledScript reads a bundled cap from disk and returns one script body. The
// cap file is the source of truth for the cap tests: the assertion must see what
// ships, not a copy.
func bundledScript(t *testing.T, capPath, script string) string {
	t.Helper()
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatalf("read bundled cap %s: %v", capPath, err)
	}
	cf, err := Parse(data)
	if err != nil {
		t.Fatalf("parse bundled cap %s: %v", capPath, err)
	}
	sc := cf.FindScript(script)
	if sc == nil {
		t.Fatalf("%s has no %s script", capPath, script)
	}
	return sc.Body
}

// requireLinuxCapHost marks a test that runs a cap body for real. The bodies
// target the Linux daemon host: they use GNU date and the host's bash. The
// darwin run showed both symptoms at once, GNU date rejects `date -I<s>` (BSD
// date takes no suffix) and the `${s//[$'\001'-...]/ }` neutralisation left the
// newline in place, so foreign text kept writing its own log line. Either one
// makes a log-line assertion meaningless there.
func requireLinuxCapHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("the cap body is written for the Linux daemon host: it uses GNU date and the host's bash")
	}
}
