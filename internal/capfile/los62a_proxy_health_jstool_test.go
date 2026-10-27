package capfile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// YM-447 changed two handlers: the scheduled bash job and the JS tool that the
// MCP call reaches. Both carry their own copy of the frame rule, and a rule that
// drifts between them is exactly how the cap went deaf in the previous rounds
// (the fork preview and the tool_call_id endings were found by tests, not by
// reading). These tests run the real JS body with a stubbed journalctl, so the
// tool variant is covered by the same fixtures as the bash variant instead of by
// a hand copy of its regexes.

// los62aJSRunner writes the JS body from the bundled cap to a module and runs it
// with node. The stub journalctl makes the sh() pipeline inside the handler run
// the real greps over a canned window.
func los62aJSRunner(t *testing.T) func(proxyLog string) map[string]any {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JS variant of the cap can only be run with it")
	}
	data, err := os.ReadFile(los32eCapPath)
	if err != nil {
		t.Fatalf("read bundled cap: %v", err)
	}
	cf, err := Parse(data)
	if err != nil {
		t.Fatalf("parse bundled cap: %v", err)
	}
	sc := cf.FindScript("proxy_health")
	if sc == nil {
		t.Fatal("the bundled proxy_health cap has no proxy_health tool script")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	los32eWriteStub(t, filepath.Join(bin, "journalctl"), `#!/bin/sh
case "$*" in
  *yesmem-proxy*) cat "$LOS62A_PROXY_LOG" ;;
  *) cat "$LOS62A_DAEMON_LOG" ;;
esac
exit 0
`)
	proxyPath := filepath.Join(dir, "proxy.log")
	daemonPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(daemonPath, nil, 0o644); err != nil {
		t.Fatalf("write daemon log: %v", err)
	}

	module := filepath.Join(dir, "cap.mjs")
	script := `import { execSync } from 'node:child_process';
const sh = (cmd) => execSync(cmd, {encoding: 'utf8'}).toString();
const mcp__yesmem__cap_store = async () => ({ok: true});
const mcp__yesmem__broadcast = async () => ({ok: true});
const handler = ` + sc.Body + `;
const out = await handler({hours: 1, alert: true, project: '/home/tester/memory/yesmem'});
console.log(JSON.stringify(out));
`
	if err := os.WriteFile(module, []byte(script), 0o644); err != nil {
		t.Fatalf("write module: %v", err)
	}

	return func(proxyLog string) map[string]any {
		t.Helper()
		if err := os.WriteFile(proxyPath, []byte(proxyLog), 0o644); err != nil {
			t.Fatalf("write proxy log: %v", err)
		}
		cmd := exec.Command(node, module)
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"LOS62A_PROXY_LOG="+proxyPath,
			"LOS62A_DAEMON_LOG="+daemonPath,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run the cap module: %v\n%s", err, out)
		}
		var res map[string]any
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
			t.Fatalf("parse the report: %v\n%s", err, out)
		}
		return res
	}
}

// los62aJSAPIErrors reads the counter out of the report the handler returns.
func los62aJSAPIErrors(t *testing.T, res map[string]any) (total float64, byType map[string]any) {
	t.Helper()
	api, ok := res["api_errors"].(map[string]any)
	if !ok {
		t.Fatalf("report has no api_errors: %v", res)
	}
	byType, _ = api["by_type"].(map[string]any)
	total, _ = api["total"].(float64)
	return total, byType
}

func TestLos62aHealthJSCountsAColouredTransportFailure(t *testing.T) {
	run := los62aJSRunner(t)
	res := run(los32eProxyLine("\x1b[31mupstream error: dial tcp 1.2.3.4:443: connection refused\x1b[0m") +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`))

	total, _ := los62aJSAPIErrors(t, res)
	if total != 1 {
		t.Errorf("api_errors.total = %v, want 1: the colour is decoration, not the message", total)
	}
}

// The selection in the JS variant runs before the cap. A forged line that
// survives the grep must not take one of the fifty slots away from a real one:
// sixty commit subjects naming an error plus one real failure still report one
// failure.
func TestLos62aHealthJSIsNotCrowdedOutByCommitSubjects(t *testing.T) {
	run := los62aJSRunner(t)
	res := run(los32eResponseLines(
		`[req 1 yesmem tid=abc] git commit detected: 1234abc fix upstream 429: rate_limit handling`, 60) +
		los32eProxyLine(`[req 2 yesmem tid=abc] upstream 503: real failure`) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`))

	total, byType := los62aJSAPIErrors(t, res)
	if total != 1 {
		t.Errorf("api_errors.total = %v, want 1 (by_type=%v): the forged lines crowded the real one out", total, byType)
	}
}

func TestLos62aHealthJSIgnoresCommitSubjectLines(t *testing.T) {
	run := los62aJSRunner(t)
	res := run(los32eResponseLines(
		`[req 1 yesmem tid=abc] git commit detected: 1234abc log listening on 127.0.0.1:3456 threshold=525000`, 6) +
		los32eResponseLines(
			`[req 1 yesmem tid=abc] git commit detected: 1234abc fix error: i/o timeout in the daemon retry`, 4) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`))

	if res["status"] != "healthy" {
		t.Errorf("status = %v, want healthy: a commit subject is not a proxy signal (%v)", res["status"], res)
	}
}

// A continuation line of a foreign value has no journal header and no prefix: the
// proxy did not write it, so it may not move a counter. Without this rule a
// newline inside a fork preview forged a keepalive error.
func TestLos62aHealthJSIgnoresHeaderlessContinuationLines(t *testing.T) {
	run := los62aJSRunner(t)
	res := run(los32eProxyLine(`[fork] extract_and_evaluate: 1200 in / 600 cached / 300 out`) +
		"[keepalive] ping error tid=zzz HTTP 429 body=injected\n" +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`))

	total, _ := los62aJSAPIErrors(t, res)
	if total != 0 {
		t.Errorf("api_errors.total = %v, want 0: the continuation line is not a proxy record", total)
	}
}
