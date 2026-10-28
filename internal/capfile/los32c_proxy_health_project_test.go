package capfile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The scheduled proxy_health run failed live: the daemon refused the bare
// project name "yesmem" as ambiguous, the refusal went into the job output, the
// script carried on and the run only ended when the scheduler killed it. These
// tests run the real bash handler as a process — journalctl, nc and store() are
// stubs, so no daemon, socket or journal is needed — and pin the three
// properties the live run got wrong: the project comes from the job, a daemon
// error answer ends the run non-zero, and the socket call cannot wait forever.

type los32cResult struct {
	exitCode int
	stderr   string
	// broadcast is the JSON the script wrote to nc's stdin, empty when the
	// script never called nc.
	broadcast string
	ncArgs    []string
	// report is the JSON the script handed to store(), empty when it stored
	// nothing.
	report string
}

// los32cRunBash runs the handler with stubbed journalctl/nc on PATH and a stub
// store(), so a test only sees what the script itself does.
func los32cRunBash(t *testing.T, env map[string]string) los32cResult {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	// One overload line makes the run critical, so the alert path is taken.
	writeStub(t, filepath.Join(bin, "journalctl"), `#!/bin/sh
case "$*" in
  *yesmem-proxy*) printf 'Oct 17 12:00:00 host yesmem-proxy[1]: upstream error: overload\n' ;;
esac
exit 0
`)
	writeStub(t, filepath.Join(bin, "nc"), `#!/bin/sh
printf '%s\n' "$*" > "$LOS32C_NC_ARGS"
cat > "$LOS32C_NC_STDIN"
if [ -n "${LOS32C_NC_REPLY-}" ]; then printf '%s\n' "$LOS32C_NC_REPLY"; fi
exit "${LOS32C_NC_EXIT:-0}"
`)

	store := filepath.Join(dir, "store.log")
	ncStdin := filepath.Join(dir, "nc.stdin")
	ncArgs := filepath.Join(dir, "nc.args")
	script := filepath.Join(dir, "run.sh")
	body := `store() {
  printf '%s\n' "$1" >> "$LOS32C_STORE"
  if [ -n "${LOS32C_STORE_FAIL-}" ]; then
    printf '%s\n' "$LOS32C_STORE_FAIL" >&2
    return 1
  fi
  return 0
}
` + bundledScript(t, proxyHealthCapPath, "proxy_health_bash")
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"YESMEM_SOCK="+filepath.Join(dir, "job.sock"),
		// The handler must not take a project from the ambient environment; a
		// test that inherits one would pass for the wrong reason.
		"YESMEM_PROJECT=",
		"PROJECT=",
		"ALERT=true",
		"HOURS=1",
		"LOS32C_STORE="+store,
		"LOS32C_NC_STDIN="+ncStdin,
		"LOS32C_NC_ARGS="+ncArgs,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := los32cResult{stderr: stderr.String()}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run handler: %v", err)
		}
		res.exitCode = exitErr.ExitCode()
	}
	if data, err := os.ReadFile(ncStdin); err == nil {
		res.broadcast = string(data)
	}
	if data, err := os.ReadFile(ncArgs); err == nil {
		res.ncArgs = strings.Fields(string(data))
	}
	if data, err := os.ReadFile(store); err == nil {
		res.report = strings.TrimSpace(string(data))
	}
	return res
}

// The live failure arrived as a daemon error answer. Swallowing it made the run
// look successful while the alert went nowhere.
func TestLos32cProxyHealthBashAbortsOnAnErrorAnswer(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT": "/home/tester/memory/yesmem",
		"LOS32C_NC_REPLY": `{"error":"ambiguous project name \"yesmem\" matches 2 candidates: ` +
			`/home/tester/memory/yesmem, /home/tester/projects/yesmem"}`,
	})

	if res.broadcast == "" {
		t.Fatal("the handler never called nc, the alert path was not exercised")
	}
	if res.exitCode == 0 {
		t.Errorf("exit code 0 after the daemon rejected the broadcast; the run must fail (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.stderr, "ambiguous") {
		t.Errorf("stderr = %q, want the daemon's error text so the failure is diagnosable", res.stderr)
	}
}

// A bare basename is what the daemon refuses. The job exports the project it
// runs in, and that path is what the broadcast must name.
func TestLos32cProxyHealthBashNamesTheJobProject(t *testing.T) {
	const project = "/home/tester/memory/yesmem"
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT":  project,
		"LOS32C_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0 (stderr=%q)", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.broadcast, `"project":"`+project+`"`) {
		t.Errorf("broadcast = %s, want project %q", res.broadcast, project)
	}
	if strings.Contains(res.broadcast, `"project":"yesmem"`) {
		t.Errorf("broadcast = %s still carries the bare name the daemon refuses", res.broadcast)
	}
}

// No project, no guess: the old script silently broadcast to "yesmem", and an
// inherited PROJECT= must not become the next silent guess.
func TestLos32cProxyHealthBashRefusesToGuessTheProject(t *testing.T) {
	for _, env := range []map[string]string{
		{"LOS32C_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`},
		{
			"PROJECT":         "/tmp/elsewhere",
			"LOS32C_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
		},
	} {
		res := los32cRunBash(t, env)
		if res.broadcast != "" {
			t.Errorf("env %v: broadcast = %s, want no broadcast without YESMEM_PROJECT", env, res.broadcast)
		}
		if res.exitCode == 0 {
			t.Errorf("env %v: exit code 0 without a project; the alert cannot be delivered (stderr=%q)", env, res.stderr)
		}
	}
}

// A reply that merely contains "result" is not a success; the daemon's error
// answer has to win.
func TestLos32cProxyHealthBashRejectsAnErrorReplyMentioningResult(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT":  "/home/tester/memory/yesmem",
		"LOS32C_NC_REPLY": `{"error":"result"}`,
	})

	if res.exitCode == 0 {
		t.Errorf("exit code 0 on an error reply (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.stderr, `"error":"result"`) {
		t.Errorf("stderr = %q, want the daemon's error text", res.stderr)
	}
}

// nc failing means the request never reached the daemon. Silent success would
// hide a dead alert path again.
func TestLos32cProxyHealthBashAbortsWhenTheSocketFails(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT": "/home/tester/memory/yesmem",
		"LOS32C_NC_EXIT": "1",
	})

	if res.exitCode == 0 {
		t.Errorf("exit code 0 although nc failed; the alert is lost (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.stderr, "got no answer") {
		t.Errorf("stderr = %q, want a message about the missing answer", res.stderr)
	}
}

// Losing the report is a failure, but not at the price of the alert: the health
// signal is most needed when the bookkeeping is broken.
func TestLos32cProxyHealthBashStillAlertsWhenTheReportIsNotStored(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT":    "/home/tester/memory/yesmem",
		"LOS32C_STORE_FAIL": "store: daemon unreachable",
		"LOS32C_NC_REPLY":   `{"result":{"status":"broadcast_sent"}}`,
	})

	if res.broadcast == "" {
		t.Error("the alert was suppressed by a failed store; the signal must survive")
	}
	if res.exitCode == 0 {
		t.Errorf("exit code 0 after the store failed; the report is lost (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.stderr, "daemon unreachable") {
		t.Errorf("stderr = %q, want the store error text", res.stderr)
	}
}

// A crafted project must not break the JSON payload or add fields; the value is
// interpolated into the broadcast body.
func TestLos32cProxyHealthBashSanitizesTheProject(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT":  "/tmp/x\tyesmem\",\"sender\":\"victim\n\033[31m",
		"LOS32C_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
	})

	if res.broadcast == "" {
		t.Fatal("the handler never called nc")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.broadcast), &payload); err != nil {
		t.Fatalf("broadcast is not valid JSON: %v\n%s", err, res.broadcast)
	}
	params, _ := payload["params"].(map[string]any)
	if params["sender"] != "HealthChecker" {
		t.Errorf("a crafted project replaced a field: %v", params)
	}
	got, _ := params["project"].(string)
	if strings.ContainsAny(got, "\"\\\n\r\t") {
		t.Errorf("project = %q still carries bytes that break the payload", got)
	}
}

// The daemon answers one request and keeps the connection open, so a plain
// "nc -U" waits for an EOF that never comes — the run then dies on the job
// timeout instead of reporting. -N shuts the write side after stdin ends.
func TestLos32cProxyHealthBashBoundsTheSocketCall(t *testing.T) {
	res := los32cRunBash(t, map[string]string{
		"YESMEM_PROJECT":  "/home/tester/memory/yesmem",
		"LOS32C_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
	})

	if len(res.ncArgs) == 0 {
		t.Fatal("the handler never called nc")
	}
	bounded := false
	for _, arg := range res.ncArgs {
		if arg == "-N" || arg == "-q" {
			bounded = true
		}
	}
	if !bounded {
		t.Errorf("nc args = %v, want -N (or -q) so the call ends at the reply", res.ncArgs)
	}
}
