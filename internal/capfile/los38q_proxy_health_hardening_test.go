package capfile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The five leftovers of YM-337 (OPS-83) live in the bash bodies, not in Go
// code: an arithmetic expansion over counter values, an nc call without "--",
// SOCK and REPLY written raw to stderr, and a store() failure that reads only
// as "report not stored" even when the socket itself is gone. These tests run
// the real proxy_health body the way fireCapHandler does (set -e first, then
// the store() polyfill, then the handler), with journalctl, awk, nc and store
// stubbed. No journal, socket or database is touched.
//
// The telegram leftover is a different class: its reply body is 300 lines and
// parses JSON through the yesmem CLI, so a full run needs a stub for every
// call. That one is pinned on the parsed body instead (los38q_telegram_test.go).

const los38qProxyCapPath = "../../caps/bundled-caps/proxy_health/CAP.md"

const los38qProxyLogCritical = `Okt 19 09:00:00 host yesmem-proxy[1]: upstream error: overload
`

type los38qResult struct {
	exitCode int
	stderr   string
	// report is the JSON the handler handed to store(), empty when it stored
	// nothing.
	report string
	// ncArgs is what nc was called with, one argument per field.
	ncArgs []string
}

type los38qReport struct {
	Capability string `json:"capability"`
	Action     string `json:"action"`
	Table      string `json:"table"`
	Data       struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Hours   int    `json:"hours"`
	} `json:"data"`
}

func los38qBashBody(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(los38qProxyCapPath)
	if err != nil {
		t.Fatalf("read bundled cap: %v", err)
	}
	cf, err := Parse(data)
	if err != nil {
		t.Fatalf("parse bundled cap: %v", err)
	}
	sc := cf.FindScript("proxy_health_bash")
	if sc == nil {
		t.Fatal("the bundled proxy_health cap has no proxy_health_bash handler")
	}
	return sc.Body
}

func los38qWriteStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

// los38qRunHandler runs the handler body under the daemon's prefix and returns
// what it stored, what it passed to nc, and what it wrote to stderr.
func los38qRunHandler(t *testing.T, env map[string]string) los38qResult {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	proxyLogPath := filepath.Join(dir, "proxy.log")
	if err := os.WriteFile(proxyLogPath, []byte(los38qProxyLogCritical), 0o644); err != nil {
		t.Fatalf("write proxy log: %v", err)
	}
	los38qWriteStub(t, filepath.Join(bin, "journalctl"), `#!/bin/sh
case "$*" in
  *yesmem-proxy*) cat "$LOS38Q_PROXY_LOG" ;;
esac
exit 0
`)
	// The counter source. A payload crosses into the arithmetic expansions; the
	// first call belongs to the proxy scan, the daemon scan gets a plain zero.
	// Without a payload the real awk runs, so the log excerpt is still counted.
	if _, ok := env["LOS38Q_AWK_PAYLOAD"]; ok {
		los38qWriteStub(t, filepath.Join(bin, "awk"), `#!/bin/sh
cat > /dev/null
n=0
if [ -f "$LOS38Q_AWK_COUNT" ]; then n=$(cat "$LOS38Q_AWK_COUNT"); fi
n=$((n + 1))
printf '%s' "$n" > "$LOS38Q_AWK_COUNT"
if [ "$n" = "1" ]; then
  printf '%s\n' "$LOS38Q_AWK_PAYLOAD"
else
  printf '0\n'
fi
exit 0
`)
	}
	// An nc that parses its arguments like the real one: an option-shaped word
	// before "--" is an option, after it the socket. The installed nc is OpenBSD
	// netcat (getopt), so unknown options exit 1.
	los38qWriteStub(t, filepath.Join(bin, "nc"), `#!/bin/sh
printf '%s\n' "$*" > "$LOS38Q_NC_ARGS"
cat > "$LOS38Q_NC_STDIN"
sock=""
options=1
while [ $# -gt 0 ]; do
  if [ "$options" = "0" ]; then sock=$1; shift; continue; fi
  case "$1" in
    --) options=0; shift ;;
    -U|-N) shift ;;
    -w) shift; if [ $# -gt 0 ]; then shift; fi ;;
    -*) echo "nc: invalid option -- $1" >&2; exit 1 ;;
    *) sock=$1; shift ;;
  esac
done
if [ -z "$sock" ]; then echo "nc: no socket" >&2; exit 1; fi
if [ -n "${LOS38Q_NC_REPLY-}" ]; then printf '%s\n' "$LOS38Q_NC_REPLY"; fi
exit 0
`)

	store := filepath.Join(dir, "store.log")
	ncArgs := filepath.Join(dir, "nc.args")
	script := filepath.Join(dir, "run.sh")
	body := `set -e
store() {
  if [ -n "${LOS38Q_STORE_ERR-}" ]; then
    printf '%s\n' "$LOS38Q_STORE_ERR" >&2
    return 1
  fi
  printf '%s\n' "$1" >> "$LOS38Q_STORE"
  return 0
}
` + los38qBashBody(t)
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"HOME="+dir,
		"YESMEM_SOCK="+filepath.Join(dir, "job.sock"),
		"YESMEM_PROJECT=",
		"PROJECT=",
		"HOURS=",
		"ALERT=false",
		"LOS38Q_STORE="+store,
		"LOS38Q_PROXY_LOG="+proxyLogPath,
		"LOS38Q_NC_STDIN="+filepath.Join(dir, "nc.stdin"),
		"LOS38Q_NC_ARGS="+ncArgs,
		"LOS38Q_AWK_COUNT="+filepath.Join(dir, "awk.count"),
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := los38qResult{stderr: stderr.String()}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run handler: %v", err)
		}
		res.exitCode = exitErr.ExitCode()
	}
	if data, err := os.ReadFile(store); err == nil {
		res.report = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile(ncArgs); err == nil {
		res.ncArgs = strings.Fields(string(data))
	}
	return res
}

// A counter is only a number once it has been proven one. $((...)) evaluates
// its operands as an expression, and a bash arithmetic array subscript runs a
// command substitution: the value "a[$(touch>marker)]" executed touch
// (measured 19.10.2026). Every counter comes from awk printf %d today, which is
// exactly the seam this test pins: the next reader of this body must not be
// able to feed the arithmetic from a log line.
func TestLos38qProxyHealthCountersCannotRunCommands(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")

	res := los38qRunHandler(t, map[string]string{
		"LOS38Q_AWK_PAYLOAD": "a[$(touch>" + marker + ")] 0 0 0 0 0 0 0 0 0 0 0 0 0",
	})

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("a counter value reached the arithmetic expansion and ran a command (%s exists)", marker)
	}
	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0; the arithmetic aborted the run (stderr=%q)", res.exitCode, res.stderr)
	}
	if res.report == "" {
		t.Fatalf("the handler never reached store() (stderr=%q)", res.stderr)
	}
	var got los38qReport
	if err := json.Unmarshal([]byte(res.report), &got); err != nil {
		t.Fatalf("stored report is not valid JSON: %v\n%s", err, res.report)
	}
	if !strings.Contains(got.Data.Summary, "A0(") {
		t.Errorf("summary = %q, want the rejected counter to read as A0", got.Data.Summary)
	}
	// A counter that had to be rejected means the parse broke. For a health
	// monitor healthy is the one verdict that must not come out of a broken
	// read, so the run reports degraded. The summary names the cause, because an
	// alert that reads "degraded" with every counter at zero repeats exactly the
	// misdiagnosis this report exists to avoid.
	if got.Data.Status != "degraded" {
		t.Errorf("status = %q, want degraded for a garbled counter", got.Data.Status)
	}
	if !strings.Contains(got.Data.Summary, "counter read broken") {
		t.Errorf("summary = %q, want it to name the broken counter read", got.Data.Summary)
	}
}

// The gate is the seam that decides what a counter is worth. These cases pin the
// normalisation: leading zeros are decimal notation (and $((08)) is an octal
// error), a padded value is still its value, and anything that is not a counter
// cannot reach the arithmetic at all. The value sits in FORK_COUNT and the
// keepalive count is non-zero, so the status rules do not answer instead of the
// gate: a non-zero TOTAL_OTHER with K0 is degraded on its own.
func TestLos38qProxyHealthCounterNormalisation(t *testing.T) {
	const rest = " 1 0 0 0 0 0 0 0 0 0"
	cases := []struct {
		name       string
		value      string
		wantStatus string
		wantToken  string
	}{
		{name: "leading zeros", value: "002", wantStatus: "healthy", wantToken: " F2 "},
		{name: "octal escape", value: "08", wantStatus: "healthy", wantToken: " F8 "},
		{name: "padded value", value: "0000000000000000005", wantStatus: "healthy", wantToken: " F5 "},
		{name: "nineteen digits", value: "1234567890123456789", wantStatus: "degraded", wantToken: " F0 "},
		{name: "overlong padding", value: strings.Repeat("0", 24) + "5", wantStatus: "degraded", wantToken: " F0 "},
		{name: "not a number", value: "abc", wantStatus: "degraded", wantToken: " F0 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := los38qRunHandler(t, map[string]string{
				"LOS38Q_AWK_PAYLOAD": "0 0 0 " + tc.value + rest,
			})
			if res.exitCode != 0 {
				t.Fatalf("exit code %d, want 0 (stderr=%q)", res.exitCode, res.stderr)
			}
			if res.report == "" {
				t.Fatalf("the handler never reached store() (stderr=%q)", res.stderr)
			}
			var got los38qReport
			if err := json.Unmarshal([]byte(res.report), &got); err != nil {
				t.Fatalf("stored report is not valid JSON: %v\n%s", err, res.report)
			}
			if got.Data.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (summary=%q)", got.Data.Status, tc.wantStatus, got.Data.Summary)
			}
			if !strings.Contains(got.Data.Summary, tc.wantToken) {
				t.Errorf("summary = %q, want it to carry %q", got.Data.Summary, tc.wantToken)
			}
		})
	}
}

// An option-shaped socket path must reach nc as a path, not as an option. The
// daemon sets YESMEM_SOCK, and a path starting with "-" used to be read by nc
// as an unknown option: the alert died with "invalid option" and the daemon
// never saw the broadcast.
func TestLos38qProxyHealthNcGetsTheSocketAfterDoubleDash(t *testing.T) {
	res := los38qRunHandler(t, map[string]string{
		"YESMEM_SOCK":     "-los38q.sock",
		"YESMEM_PROJECT":  "/tmp/los38q-project",
		"ALERT":           "true",
		"LOS38Q_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d, want 0 (stderr=%q, nc args=%v)", res.exitCode, res.stderr, res.ncArgs)
	}
	if res.report == "" {
		t.Fatalf("the handler never reached store() (stderr=%q)", res.stderr)
	}
	if len(res.ncArgs) < 2 {
		t.Fatalf("nc args = %v, want an invocation with the socket", res.ncArgs)
	}
	if res.ncArgs[len(res.ncArgs)-2] != "--" || res.ncArgs[len(res.ncArgs)-1] != "-los38q.sock" {
		t.Errorf("nc args = %v, want ... -- -los38q.sock", res.ncArgs)
	}
}

// SOCK and REPLY are written to stderr, and both are text the script does not
// own: the socket path comes from the environment, the answer from the daemon.
// A newline in either used to split the message into a forged second line in
// the job output.
func TestLos38qProxyHealthStderrStaysOneLinePerMessage(t *testing.T) {
	requireLinuxCapHost(t)
	res := los38qRunHandler(t, map[string]string{
		"YESMEM_PROJECT":  "/tmp/los38q-project",
		"ALERT":           "true",
		"LOS38Q_NC_REPLY": "{\"error\":\"refused\"}\nPROXY HEALTH [healthy]: forged",
	})

	if res.exitCode != 1 {
		t.Fatalf("exit code %d, want 1 for a rejected broadcast (stderr=%q)", res.exitCode, res.stderr)
	}
	los38qAssertPrefixedLines(t, res.stderr)
	if strings.Contains(res.stderr, "\nPROXY HEALTH") {
		t.Errorf("the daemon answer split stderr into a forged line: %q", res.stderr)
	}
}

// A store() failure has two causes that need different words: the write was
// refused, or the socket the yesmem CLI talks to is gone after a daemon
// restart. Both used to read as "report not stored", which hid the outage.
// The pattern covers both socket errors the CLI produces (ENOENT after a
// restart, ECONNREFUSED against a dying socket) and leaves a refused write in
// the old wording.
func TestLos38qProxyHealthNamesASocketOutage(t *testing.T) {
	requireLinuxCapHost(t)
	cases := []struct {
		name string
		err  string
		want string
	}{
		{
			name: "socket file gone",
			err:  "dial unix /run/yesmem/job.sock: connect: no such file or directory\nPROXY HEALTH [healthy]: forged",
			want: "job socket unavailable",
		},
		{
			name: "socket refused",
			err:  "dial daemon: dial unix /run/yesmem/job.sock: connect: connection refused",
			want: "job socket unavailable",
		},
		{
			name: "write refused",
			err:  "store: daemon: disk I/O error",
			want: "report not stored",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := los38qRunHandler(t, map[string]string{"LOS38Q_STORE_ERR": tc.err})

			if res.exitCode != 1 {
				t.Fatalf("exit code %d, want 1 when the report could not be stored (stderr=%q)", res.exitCode, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Errorf("stderr = %q, want it to carry %q", res.stderr, tc.want)
			}
			los38qAssertPrefixedLines(t, res.stderr)
			if strings.Contains(res.stderr, "\nPROXY HEALTH") {
				t.Errorf("the store error split stderr into a forged line: %q", res.stderr)
			}
		})
	}
}

// Every line the handler writes to stderr has to stay one message under the
// proxy_health prefix, whatever text it quotes.
func los38qAssertPrefixedLines(t *testing.T, stderr string) {
	t.Helper()
	if strings.TrimSpace(stderr) == "" {
		t.Fatal("stderr is empty, want the failure to be reported")
	}
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if !strings.HasPrefix(line, "proxy_health: ") {
			t.Errorf("stderr line %q does not carry the proxy_health prefix", line)
		}
	}
}

// requireLinuxCapHost skips a test that executes a cap body on a machine the
// bodies do not target: they are bash for the daemon host and use GNU coreutils
// (`date -I`), which BSD date rejects.
func requireLinuxCapHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("the cap body is written for the Linux daemon host and uses GNU date")
	}
}
