package capfile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The JS variant of the bundled proxy_health cap builds its journalctl window
// from the caller's hours argument and hands the result to sh(), which runs
// `sh -c` (internal/daemon/handler_cap_exec.go). hours is an MCP parameter, so
// anything a caller passes used to land in that string verbatim: the cap runs
// without a sandbox profile, which makes it a command injection on the host.
// These tests run the real body from bundle source of truth with a stubbed
// journalctl on PATH and pin that only a number reaches the shell (OPS-68).

func los38hBunPath(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".bun", "bin", "bun")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("needs bun on PATH or in ~/.bun/bin")
	return ""
}

// los38hHarness is the smallest environment the JS body needs: an sh() that
// runs the command it is given, and no-op store/broadcast calls. The stub sh
// ignores the exit code on purpose; this test asks which string reaches the
// shell, not how the handler reports a grep without matches.
const los38hHarness = `(async () => {
globalThis.sh = async function(cmd) {
  var p = Bun.spawn(['sh', '-c', cmd], { stdout: 'pipe', stderr: 'pipe' });
  var out = await new Response(p.stdout).text();
  await p.exited;
  return out;
};
globalThis.mcp__yesmem__cap_store = async function() { return {}; };
globalThis.mcp__yesmem__broadcast = async function() { return {}; };
var _handler = __BODY__;
var _r = _handler(JSON.parse(process.env.CAP_ARGS));
if (_r && typeof _r.then === 'function') { _r = await _r; }
console.log(JSON.stringify(_r));
})().catch(function(e) { console.error(e && e.message || String(e)); process.exit(1); });
`

// los38hRun executes the JS body with the given hours value and returns its
// stdout plus the exit error.
func los38hRun(t *testing.T, hours any) (string, error) {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	stub := `#!/bin/sh
echo "Oct 17 12:00:00 host yesmem-proxy[1]: listening on 127.0.0.1:9378 threshold=150000"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write journalctl stub: %v", err)
	}

	harness := strings.Replace(los38hHarness, "__BODY__", bundledScript(t, proxyHealthCapPath, "proxy_health"), 1)
	script := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(script, []byte(harness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	args, err := json.Marshal(map[string]any{"hours": hours, "alert": false})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	cmd := exec.Command(los38hBunPath(t), script)
	cmd.Env = append(os.Environ(),
		"CAP_ARGS="+string(args),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, runErr := cmd.Output()
	return string(out), runErr
}

func TestProxyHealthJS_HoursCannotReachTheShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")

	payload := `1" ; touch ` + marker + ` ; echo "`
	out, err := los38hRun(t, payload)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("hours reached the shell: %s was created by the injected command\noutput: %s", marker, out)
	}
	if err != nil {
		t.Fatalf("handler failed: %v\noutput: %s", err, out)
	}
}

func TestProxyHealthJS_HoursIsStoredAsNumber(t *testing.T) {
	cases := []struct {
		name  string
		hours any
		want  float64
	}{
		{"numeric string", "3", 3},
		{"upper bound", 168, 168},
		{"above bound falls back", 720, 1},
		{"zero falls back", 0, 1},
		{"fraction falls back", 0.5, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := los38hRun(t, tc.hours)
			if err != nil {
				t.Fatalf("handler failed: %v\noutput: %s", err, out)
			}
			line := strings.TrimSpace(out)
			if line == "" {
				t.Fatal("handler printed nothing")
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(line), &result); err != nil {
				t.Fatalf("unmarshal result %q: %v", line, err)
			}
			hours, ok := result["hours"].(float64)
			if !ok {
				t.Fatalf("hours is not a number after normalization: %#v (%T)", result["hours"], result["hours"])
			}
			if hours != tc.want {
				t.Fatalf("hours %v: expected %v, got %v", tc.hours, tc.want, hours)
			}
		})
	}
}
