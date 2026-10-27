package capfile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon prepends "set -e" to every bash handler
// (handler_cap_exec.go, fireCapHandler). The bundled proxy_health body was
// written for a shell without it: the loop that defaults the counters ran
// eval "[ -z ... ] && $v=0", a simple command whose status is 1 whenever the
// counter was already set, so the run ended before store() ever ran (YM-327,
// OPS-78). The neighbouring "[ test ] && assignment" lines are exempt from
// set -e and never were the problem. The existing tests run the body without
// the prefix, which is why the abort was invisible.
//
// These tests run the real body the way fireCapHandler does: "set -e" first,
// then the store() polyfill, then the handler code. journalctl, nc and store are
// stubs on PATH and in the script, so no journal, socket or database is read.

const los38nProxyLogHealthy = `Okt 18 15:00:00 host yesmem-proxy[1]: [keepalive] ping tid=a1b2 cache_write=512
Okt 18 15:00:05 host yesmem-proxy[1]: cache: 1234 in / 900 cached (73% hit)
Okt 18 15:05:00 host yesmem-proxy[1]: fork extract_and_evaluate: 1200 in / 600 cached / 300 out
Okt 18 15:05:00 host yesmem-proxy[1]: [fork] result: 3 learnings, 1 evaluations
`

const los38nDaemonLogQuiet = `Okt 18 15:00:00 host yesmem[1]: scheduler: bash job "proxy-health-check" completed
`

const los38nProxyLogCritical = `Okt 18 15:10:00 host yesmem-proxy[1]: upstream error: overload
`

type los38nResult struct {
	exitCode int
	stderr   string
	// report is the JSON the handler handed to store(), empty when it stored
	// nothing.
	report string
	// broadcast is the JSON the handler wrote to nc, empty when the alert
	// branch did not run.
	broadcast string
}

type los38nReport struct {
	Capability string `json:"capability"`
	Action     string `json:"action"`
	Table      string `json:"table"`
	Data       struct {
		Timestamp string  `json:"timestamp"`
		Status    string  `json:"status"`
		Summary   string  `json:"summary"`
		Hours     float64 `json:"hours"`
	} `json:"data"`
}

func los38nRunHandler(t *testing.T, proxyLog, daemonLog string, env map[string]string) los38nResult {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	proxyLogPath := filepath.Join(dir, "proxy.log")
	daemonLogPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(proxyLogPath, []byte(proxyLog), 0o644); err != nil {
		t.Fatalf("write proxy log: %v", err)
	}
	if err := os.WriteFile(daemonLogPath, []byte(daemonLog), 0o644); err != nil {
		t.Fatalf("write daemon log: %v", err)
	}
	los32cWriteStub(t, filepath.Join(bin, "journalctl"), `#!/bin/sh
case "$*" in
  *yesmem-proxy*) cat "$LOS38N_PROXY_LOG" ;;
  *yesmem*) cat "$LOS38N_DAEMON_LOG" ;;
esac
exit 0
`)
	// An awk that yields no counter fields leaves every variable empty. The stub
	// only exists for the case that exercises the defaulting loop, so the other
	// cases still count through the real awk.
	if env["LOS38N_NO_COUNTERS"] == "1" {
		los32cWriteStub(t, filepath.Join(bin, "awk"), "#!/bin/sh\ncat > /dev/null\nexit 0\n")
	}
	// The alert path writes its request to nc's stdin; the stub keeps that
	// request so a test can tell a taken branch from a skipped one, and answers
	// the way the daemon does, so no socket is needed.
	ncStdin := filepath.Join(dir, "nc.stdin")
	los32cWriteStub(t, filepath.Join(bin, "nc"), `#!/bin/sh
cat > "$LOS38N_NC_STDIN"
if [ -n "${LOS38N_NC_REPLY-}" ]; then printf '%s\n' "$LOS38N_NC_REPLY"; fi
exit 0
`)

	store := filepath.Join(dir, "store.log")
	script := filepath.Join(dir, "run.sh")
	// The order the daemon uses: set -e, then the polyfills, then the body.
	body := "set -e\n" + `store() {
  printf '%s\n' "$1" >> "$LOS38N_STORE"
  return 0
}
` + los32cBashBody(t)
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		// A temp HOME keeps the default socket path away from the real install.
		"HOME="+dir,
		"YESMEM_SOCK="+filepath.Join(dir, "job.sock"),
		"YESMEM_PROJECT=",
		"PROJECT=",
		// The scheduled job passes no arguments, so HOURS falls back to one in
		// the handler itself; setting it empty keeps an ambient value out.
		"HOURS=",
		"ALERT=false",
		"LOS38N_STORE="+store,
		"LOS38N_PROXY_LOG="+proxyLogPath,
		"LOS38N_DAEMON_LOG="+daemonLogPath,
		"LOS38N_NC_STDIN="+ncStdin,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := los38nResult{stderr: stderr.String()}
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
	if data, err := os.ReadFile(ncStdin); err == nil {
		res.broadcast = strings.TrimSpace(string(data))
	}
	return res
}

func TestLos38nProxyHealthBashReachesStoreUnderSetE(t *testing.T) {
	cases := []struct {
		name          string
		proxyLog      string
		daemonLog     string
		env           map[string]string
		wantState     string
		wantHours     float64
		wantBroadcast bool
	}{
		{
			// The live job passes no arguments, so HOURS is the handler's own
			// default of one. That is the run that ends at the SINCE line when
			// set -e is in effect.
			name:      "default window",
			proxyLog:  los38nProxyLogHealthy,
			daemonLog: los38nDaemonLogQuiet,
			wantState: "healthy",
			wantHours: 1,
		},
		{
			// HOURS above one takes the other side of the SINCE branch and
			// reaches the counter defaulting loop with every counter set.
			name:      "window from the job",
			proxyLog:  los38nProxyLogHealthy,
			daemonLog: los38nDaemonLogQuiet,
			env:       map[string]string{"HOURS": "8"},
			wantState: "healthy",
			wantHours: 8,
		},
		{
			// An overload makes the run critical, which walks every severity
			// comparison in the ladder.
			name:      "critical log",
			proxyLog:  los38nProxyLogCritical,
			daemonLog: los38nDaemonLogQuiet,
			wantState: "critical",
			wantHours: 1,
		},
		{
			// No journal output at all leaves every counter at zero, the case
			// where every "[ n -gt k ] && STATUS=" test is false.
			name:      "empty journal",
			proxyLog:  "",
			daemonLog: "",
			wantState: "healthy",
			wantHours: 1,
		},
		{
			// The live job runs with ALERT unset, so the alert branch is part of
			// the real run: a critical status has to be broadcast and the report
			// still stored.
			name:      "alert branch",
			proxyLog:  los38nProxyLogCritical,
			daemonLog: los38nDaemonLogQuiet,
			env: map[string]string{
				"ALERT":           "true",
				"YESMEM_PROJECT":  "/tmp/los38n-project",
				"LOS38N_NC_REPLY": `{"result":{"status":"broadcast_sent"}}`,
			},
			wantState:     "critical",
			wantHours:     1,
			wantBroadcast: true,
		},
		{
			// A counter source that yields no fields leaves every variable
			// empty, the one case the defaulting loop exists for.
			name:      "no counter output",
			proxyLog:  "",
			daemonLog: "",
			env:       map[string]string{"LOS38N_NO_COUNTERS": "1"},
			wantState: "healthy",
			wantHours: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := los38nRunHandler(t, tc.proxyLog, tc.daemonLog, tc.env)

			if res.exitCode != 0 {
				t.Fatalf("exit code %d under set -e, want 0; the run ended before it finished (stderr=%q, report=%q)",
					res.exitCode, res.stderr, res.report)
			}
			if res.report == "" {
				t.Fatalf("the handler never reached store(), the report is lost (stderr=%q)", res.stderr)
			}
			var got los38nReport
			if err := json.Unmarshal([]byte(res.report), &got); err != nil {
				t.Fatalf("stored report is not valid JSON: %v\n%s", err, res.report)
			}
			if got.Capability != "proxy_health" || got.Action != "upsert" || got.Table != "reports" {
				t.Errorf("stored report addresses %s/%s/%s, want proxy_health/upsert/reports",
					got.Capability, got.Action, got.Table)
			}
			if got.Data.Status != tc.wantState {
				t.Errorf("status = %q, want %q (summary=%q)", got.Data.Status, tc.wantState, got.Data.Summary)
			}
			if got.Data.Hours != tc.wantHours {
				t.Errorf("hours = %v, want %v", got.Data.Hours, tc.wantHours)
			}
			if !strings.HasPrefix(got.Data.Summary, tc.wantState+": ") {
				t.Errorf("summary = %q, want it to start with %q", got.Data.Summary, tc.wantState+": ")
			}
			// A branch that is skipped and one that runs leave the same report,
			// so the alert case has to be checked at nc itself.
			if tc.wantBroadcast && res.broadcast == "" {
				t.Error("the handler never called nc, the alert branch did not run")
			}
			if !tc.wantBroadcast && res.broadcast != "" {
				t.Errorf("broadcast = %s, want no broadcast for this case", res.broadcast)
			}
		})
	}
}

// The healthy excerpt still has to be counted: a handler that reaches store()
// with zeroed counters would pass the exit-code check while reporting nothing.
func TestLos38nProxyHealthBashKeepsItsCountersUnderSetE(t *testing.T) {
	res := los38nRunHandler(t, los38nProxyLogHealthy, los38nDaemonLogQuiet, nil)

	if res.report == "" {
		t.Fatalf("the handler never reached store() (stderr=%q)", res.stderr)
	}
	var got los38nReport
	if err := json.Unmarshal([]byte(res.report), &got); err != nil {
		t.Fatalf("stored report is not valid JSON: %v\n%s", err, res.report)
	}
	// F1 is the fork line, K1 the successful ping, C0/1 the cache line.
	for _, want := range []string{"F1", "K1", "C0/1"} {
		if !strings.Contains(got.Data.Summary, want) {
			t.Errorf("summary = %q, want it to carry %q", got.Data.Summary, want)
		}
	}
}
