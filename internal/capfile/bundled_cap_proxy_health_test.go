package capfile

import (
	"os"
	"os/exec"
	"testing"
)

// The bundled proxy_health cap ships a scheduled bash handler. It went live
// without one: the script lived only in the database, its JSON payloads were
// built with nested double quotes, and every fire exited 2 with a bash syntax
// error before the job could produce a report. The file is the go:embed source
// of truth, so the handler belongs here — and it has to parse.
func TestProxyHealthBashHandlerParses(t *testing.T) {
	data, err := os.ReadFile("../../caps/bundled-caps/proxy_health/CAP.md")
	if err != nil {
		t.Fatalf("read bundled cap: %v", err)
	}
	cf, err := Parse(data)
	if err != nil {
		t.Fatalf("parse bundled cap: %v", err)
	}
	sc := cf.FindScript("proxy_health_bash")
	if sc == nil {
		t.Fatal("the bundled proxy_health cap has no proxy_health_bash handler; the scheduled job runs script_name=proxy_health_bash")
	}
	if sc.Runtime != "bash" {
		t.Fatalf("proxy_health_bash runtime = %q, want bash", sc.Runtime)
	}

	// The job scans an hour of journal output in one pass per unit, which costs
	// tens of seconds. Without a declared timeout the scheduler falls back to
	// 20 seconds and kills every run.
	if sc.Timeout != 120 {
		t.Errorf("proxy_health_bash must declare the 120s timeout the audit asked for, got %d", sc.Timeout)
	}

	if out, err := exec.Command("bash", "-n", "-c", sc.Body).CombinedOutput(); err != nil {
		t.Errorf("proxy_health_bash does not parse (bash -n): %v\n%s", err, out)
	}
}
