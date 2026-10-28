package daemon

import (
	"testing"
)

// A bash job's timeout was a hardcoded guess: 20 seconds, or 600 when the
// command merely mentioned claude/llm. A script that declares its own runtime
// (ScriptMeta.Timeout, parsed from the CAP.md `timeout:` metadata) was ignored,
// so a job scanning an hour of journal output was killed at 20 seconds on every
// run. The declared value wins; the heuristic stays as the fallback.
func TestBashJobTimeout(t *testing.T) {
	cases := []struct {
		name          string
		command       string
		scriptTimeout int64
		wantSeconds   int
	}{
		{"no declaration, plain command", "journalctl --user -u yesmem", 0, 20},
		{"no declaration, agent command", "claude -p fix-it", 0, 600},
		{"no declaration, llm adapter", "echo x | llm summarize", 0, 600},
		{"declared runtime wins", "journalctl --user -u yesmem", 120, 120},
		{"declared runtime wins over the agent heuristic", "claude -p fix-it", 90, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bashJobTimeout(tc.command, tc.scriptTimeout); got != tc.wantSeconds {
				t.Errorf("bashJobTimeout(%q, %d) = %d, want %d", tc.command, tc.scriptTimeout, got, tc.wantSeconds)
			}
		})
	}
}

// los35cResolveBashCap hands the declared timeout to the job runner; without it
// the scheduler has no way to know how long the script needs.
func TestResolveBashCommand_ReturnsTheDeclaredScriptTimeout(t *testing.T) {
	h, _ := mustHandler(t)

	scripts := `[{"name":"scan","kind":"handler","runtime":"bash","lang":"bash","timeout":120,"body":"echo scan\nexit 0\n"}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "timed_cap", "description": "declares its runtime", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	job := &ScheduledJob{ID: "j-timeout", CapName: "timed_cap", ScriptName: "scan"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("resolve bash cap: %v", err)
	}
	if timeoutSec := resolved.timeout; timeoutSec != 120 {
		t.Errorf("declared script timeout = %d, want 120", timeoutSec)
	}
}

// A script without a `timeout:` line leaves the choice to the scheduler.
func TestResolveBashCommand_NoDeclaredTimeoutIsZero(t *testing.T) {
	h, _ := mustHandler(t)

	scripts := `[{"name":"plain","kind":"handler","runtime":"bash","lang":"bash","body":"echo plain\nexit 0\n"}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "plain_cap", "description": "no runtime declared", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	job := &ScheduledJob{ID: "j-plain", CapName: "plain_cap", ScriptName: "plain"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("resolve bash cap: %v", err)
	}
	if timeoutSec := resolved.timeout; timeoutSec != 0 {
		t.Errorf("undeclared script timeout = %d, want 0 (let the scheduler decide)", timeoutSec)
	}
}
