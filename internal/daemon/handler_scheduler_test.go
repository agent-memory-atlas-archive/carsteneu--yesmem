package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/sanitize"
	"github.com/carsteneu/yesmem/internal/storage"
)

func TestHeadlessArgs_NoSession(t *testing.T) {
	args := headlessArgs("", "")
	want := []string{"-p", "--output-format", "json", "--verbose", "--max-turns", "3", "--permission-mode", "bypassPermissions"}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i, a := range want {
		if args[i] != a {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], a)
		}
	}
}

func TestHeadlessArgs_WithSession(t *testing.T) {
	args := headlessArgs("sess-abc123", "")
	want := []string{"-p", "--output-format", "json", "--verbose", "--max-turns", "3", "--permission-mode", "bypassPermissions", "--resume", "sess-abc123"}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i, a := range want {
		if args[i] != a {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], a)
		}
	}
}

func TestParseHeadlessOutput_ValidJSON(t *testing.T) {
	output := []byte(`{"type":"result","result":"Hello world","session_id":"sess-xyz789","cost_usd":0.01}`)
	text, sid := parseHeadlessOutput(output)
	if text != "Hello world" {
		t.Errorf("text = %q, want %q", text, "Hello world")
	}
	if sid != "sess-xyz789" {
		t.Errorf("session_id = %q, want %q", sid, "sess-xyz789")
	}
}

func TestParseHeadlessOutput_InvalidJSON(t *testing.T) {
	output := []byte("plain text response without json")
	text, sid := parseHeadlessOutput(output)
	if text != "plain text response without json" {
		t.Errorf("text = %q, want raw output", text)
	}
	if sid != "" {
		t.Errorf("session_id = %q, want empty", sid)
	}
}

func TestParseHeadlessOutput_StreamJSON(t *testing.T) {
	output := []byte("{\"type\":\"assistant\",\"message\":\"thinking...\"}\n{\"type\":\"result\",\"result\":\"Done\",\"session_id\":\"sess-final\"}\n")
	text, sid := parseHeadlessOutput(output)
	if text != "Done" {
		t.Errorf("text = %q, want %q", text, "Done")
	}
	if sid != "sess-final" {
		t.Errorf("session_id = %q, want %q", sid, "sess-final")
	}
}

func TestParseHeadlessOutput_EmptyResult(t *testing.T) {
	output := []byte(`{"type":"result","result":"","session_id":"sess-empty"}`)
	text, sid := parseHeadlessOutput(output)
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if sid != "sess-empty" {
		t.Errorf("session_id = %q, want %q", sid, "sess-empty")
	}
}

func TestResolveJobSandbox(t *testing.T) {
	tests := []struct {
		name    string
		param   string
		dflt    SandboxProfile
		want    SandboxProfile
		wantErr bool
	}{
		{"empty uses default", "", ProfileStandard, ProfileStandard, false},
		{"explicit none", "none", ProfileStandard, ProfileNone, false},
		{"explicit standard", "standard", ProfileNone, ProfileStandard, false},
		{"explicit strict", "strict", ProfileStandard, ProfileStrict, false},
		{"invalid", "yolo", ProfileStandard, ProfileNone, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveJobSandbox(tt.param, tt.dflt)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeadlessArgs_WithModel(t *testing.T) {
	args := headlessArgs("", "claude-opus-4-7")
	found := false
	for i, a := range args {
		if a == "--model" && i+1 < len(args) && args[i+1] == "claude-opus-4-7" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --model claude-opus-4-7 in args: %v", args)
	}
}

func TestHeadlessArgs_EmptyModelOmitted(t *testing.T) {
	args := headlessArgs("", "")
	for _, a := range args {
		if a == "--model" {
			t.Error("empty model should not produce --model flag")
		}
	}
}

func TestPrepareBashCommand_InjectsPreamble(t *testing.T) {
	cmd := `TOKEN=$(store '{"capability":"telegram","action":"query","table":"config"}' | jq -r '.[0].value')
echo $TOKEN`
	result := prepareBashCommand(cmd)
	if result == cmd {
		t.Error("should prepend adapter preamble")
	}
	if !strings.Contains(result, "store()") {
		t.Error("preamble should define store() function")
	}
	if !strings.HasSuffix(result, cmd) {
		t.Error("original command should be at the end")
	}
}

func TestPrepareBashCommand_NoStoreNoPreamble(t *testing.T) {
	cmd := "echo hello"
	result := prepareBashCommand(cmd)
	if result != cmd {
		t.Errorf("should not modify command without store(), got %q", result)
	}
}

func TestStoreBashRun_RedactsOutputWhenRedactorSet(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{
		store:    s,
		redactor: sanitize.NewSecretRedactor(nil),
	}
	job := &ScheduledJob{
		ID:      "test-redact",
		CapName: "test",
	}
	out := "leaked sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa here"
	h.storeBashRun(job, "echo x", out, nil, 0)
	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no run stored")
	}
	if strings.Contains(runs[0].Output, "sk-ant-api03") {
		t.Fatalf("output not redacted: %q", runs[0].Output)
	}
	if !strings.Contains(runs[0].Output, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in output, got %q", runs[0].Output)
	}
}

func TestStoreBashRun_RedactsCommandWhenRedactorSet(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{
		store:    s,
		redactor: sanitize.NewSecretRedactor(nil),
	}
	job := &ScheduledJob{ID: "test-cmd-redact", CapName: "test"}
	cmd := `echo "token: sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`
	h.storeBashRun(job, cmd, "ok", nil, 0)
	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no run stored")
	}
	if strings.Contains(runs[0].Command, "sk-ant-api03") {
		t.Fatalf("command not redacted: %q", runs[0].Command)
	}
	if !strings.Contains(runs[0].Command, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in command, got %q", runs[0].Command)
	}
}

func TestStoreBashRun_RedactsErrorMsgWhenRedactorSet(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{
		store:    s,
		redactor: sanitize.NewSecretRedactor(nil),
	}
	job := &ScheduledJob{ID: "test-err-redact", CapName: "test"}
	bashErr := errors.New("exec failed: stderr contained sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa from upstream")
	h.storeBashRun(job, "cmd", "", bashErr, 1)
	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no run stored")
	}
	if strings.Contains(runs[0].ErrorMsg, "sk-ant-api03") {
		t.Fatalf("error_msg not redacted: %q", runs[0].ErrorMsg)
	}
	if !strings.Contains(runs[0].ErrorMsg, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in error_msg, got %q", runs[0].ErrorMsg)
	}
}

// seedCapWithSandbox stores a cap learning whose bash script carries the given sandbox value.
func seedCapWithSandbox(t *testing.T, h *Handler, capName, scriptName, body, sandbox string) {
	t.Helper()
	meta := CapMeta{
		Name:        capName,
		Description: "Test: " + capName,
		Scripts: []ScriptMeta{
			{Name: scriptName, Kind: "handler", Runtime: "bash", Body: body, Sandbox: sandbox},
		},
		Version: 1,
		Tested:  true,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	l := &models.Learning{
		Content:     capName + " — Test: " + capName,
		Category:    "cap",
		Source:      "user_stated",
		Project:     "",
		Context:     ctx,
		TriggerRule: "cap:" + capName,
	}
	if _, err := h.store.InsertLearning(l); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}

func TestResolveBashCommand_ReturnsScriptSandbox(t *testing.T) {
	h, _ := mustHandler(t)
	seedCapWithSandbox(t, h, "heartbeat", "run", "echo ok", "none")

	job := &ScheduledJob{CapName: "heartbeat", ScriptName: "run"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body := resolved.body; body != "echo ok" {
		t.Errorf("body = %q, want %q", body, "echo ok")
	}
	if scriptSandbox := resolved.scriptSandbox; scriptSandbox != "none" {
		t.Errorf("scriptSandbox = %q, want %q", scriptSandbox, "none")
	}
}

func TestResolveBashCommand_ScriptSandboxEmpty(t *testing.T) {
	h, _ := mustHandler(t)
	seedCapWithSandbox(t, h, "my_cap", "fetch", "curl https://example.com", "")

	job := &ScheduledJob{CapName: "my_cap", ScriptName: "fetch"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body := resolved.body; body != "curl https://example.com" {
		t.Errorf("body = %q, want %q", body, "curl https://example.com")
	}
	if scriptSandbox := resolved.scriptSandbox; scriptSandbox != "" {
		t.Errorf("scriptSandbox = %q, want empty (inherit)", scriptSandbox)
	}
}

// YM-171: executeHeadless ran claude without any deadline. A hanging run kept the
// job marked running forever, so it never fired again, and the job goroutine
// leaked. The run is now bounded; this exercises the bound directly.
func TestRunHeadlessCommand_TimesOutAHangingProcess(t *testing.T) {
	previous := headlessJobTimeout
	headlessJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { headlessJobTimeout = previous })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := runHeadlessCommand("sleep", []string{"30"}, "", t.TempDir(), nil, &stdout, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hanging process must be killed by the timeout, got no error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("returned only after %v, the deadline did not fire", elapsed)
	}
}

// The prompt reaches claude on stdin, not as an argument. The bounded run must
// keep that channel, and must keep the exit code (ai-jail --exec preserves both).
func TestRunHeadlessCommand_FeedsThePromptOnStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runHeadlessCommand("cat", nil, "PROMPT-MARKER", t.TempDir(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v (stderr: %s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "PROMPT-MARKER") {
		t.Fatalf("stdout = %q, want the prompt", stdout.String())
	}
}

// A non-zero exit stays an error, so the caller still logs a failed headless run.
func TestRunHeadlessCommand_KeepsANonZeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runHeadlessCommand("bash", []string{"-c", "exit 3"}, "", t.TempDir(), nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("exit 3 must be reported as an error")
	}
}

// YM-172: scheduleList dropped interval_seconds and recurring, so a job created
// with interval_seconds could not be told apart from a cron job, and a one-shot
// job looked recurring.
func TestScheduleList_ExposesIntervalAndRecurring(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "poll", Name: "poll", Prompt: "echo poll", Enabled: true,
		Recurring: true, IntervalSeconds: 15, Mode: "bash",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	resp := h.Handle(Request{Method: "schedule", Params: map[string]any{"action": "list"}})
	if resp.Error != "" {
		t.Fatalf("schedule list: %s", resp.Error)
	}
	var data struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(data.Jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(data.Jobs))
	}
	if _, ok := data.Jobs[0]["interval_seconds"]; !ok {
		t.Errorf("job entry has no interval_seconds: %v", data.Jobs[0])
	}
	if _, ok := data.Jobs[0]["recurring"]; !ok {
		t.Errorf("job entry has no recurring: %v", data.Jobs[0])
	}
}

func TestScheduleList_IntervalSecondsValue(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "poll", Name: "poll", Prompt: "echo poll", Enabled: true,
		Recurring: false, IntervalSeconds: 15, Mode: "bash",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	resp := h.Handle(Request{Method: "schedule", Params: map[string]any{"action": "list"}})
	var data struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(data.Jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(data.Jobs))
	}
	if got, _ := data.Jobs[0]["interval_seconds"].(float64); got != 15 {
		t.Errorf("interval_seconds = %v, want 15", data.Jobs[0]["interval_seconds"])
	}
	if got, _ := data.Jobs[0]["recurring"].(bool); got {
		t.Errorf("recurring = %v, want false", data.Jobs[0]["recurring"])
	}
}

// YM-173: a configured default_sandbox_profile: none was indistinguishable from
// "no default configured", because ProfileNone is the zero value, so schedule
// create silently turned it into standard.
//
// The cap is user-scoped and carries no project on purpose: a project-bound cap
// may never run unsandboxed, so binding this fixture would test the save lock
// (YM-346) instead of the configured default.
func TestScheduleCreate_HonoursAConfiguredNoneDefault(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	meta := CapMeta{
		Name:        "my_cap",
		Description: "Test: my_cap",
		Scripts: []ScriptMeta{
			{Name: "run", Kind: "handler", Runtime: "bash", Body: "echo hi"},
		},
		Version: 1,
		Tested:  true,
		Scope:   "user",
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Content:  "my_cap — Test: my_cap",
		Category: "cap",
		Source:   "user_stated",
		Context:  ctx,
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
	h.defaultSandboxProfile = ProfileNone
	h.sandboxDefaultSet = true

	resp := h.Handle(Request{
		Method: "schedule",
		Params: map[string]any{
			"action": "create", "name": "no-sandbox", "cron": "* * * * *",
			"mode": "bash", "cap_name": "my_cap", "script_name": "run",
		},
	})
	if resp.Error != "" {
		t.Fatalf("schedule create: %s", resp.Error)
	}

	jobs := h.scheduler.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	if jobs[0].Sandbox != ProfileNone {
		t.Errorf("sandbox = %s, want none (the configured default)", jobs[0].Sandbox)
	}
}

// Without a configured default the job still gets standard, not none.
func TestScheduleCreate_UnsetDefaultStaysStandard(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	seedCapWithSandbox(t, h, "my_cap2", "run", "echo hi", "")

	resp := h.Handle(Request{
		Method: "schedule",
		Params: map[string]any{
			"action": "create", "name": "default-sandbox", "cron": "* * * * *",
			"mode": "bash", "cap_name": "my_cap2", "script_name": "run",
		},
	})
	if resp.Error != "" {
		t.Fatalf("schedule create: %s", resp.Error)
	}

	jobs := h.scheduler.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	if jobs[0].Sandbox != ProfileStandard {
		t.Errorf("sandbox = %s, want standard (no default configured)", jobs[0].Sandbox)
	}
}

// YM-218: the job's log directory is the replacement for /tmp inside the jail, so
// it has to be private. Since YM-276 it lives in the runtime dir beside the data
// dir: the sandbox masks the data dir as a whole, so a log dir below it could not
// be mapped back in.
func TestEnsureJobDir_IsUnderTheRuntimeDir(t *testing.T) {
	h, _ := mustHandler(t)
	job := &ScheduledJob{ID: "sched-proxy-health-check-12345", Name: "proxy-health-check"}

	dir, err := h.ensureJobDir(job)
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	want := filepath.Join(RuntimeDir(h.dataDir), "jobs", jobDirName("sched-proxy-health-check-12345"))
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if !strings.HasPrefix(filepath.Base(dir), "sched-proxy-health-check-12345-") {
		t.Errorf("dir %q does not carry the job key", dir)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("job dir not created: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 700 (job logs are private)", perm)
	}
}

// Two jobs may carry the same name; they must not share a state file.
func TestEnsureJobDir_SameNameGetsSeparateDirs(t *testing.T) {
	h, _ := mustHandler(t)
	first, err := h.ensureJobDir(&ScheduledJob{ID: "sched-poll-1", Name: "poll"})
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	second, err := h.ensureJobDir(&ScheduledJob{ID: "sched-poll-2", Name: "poll"})
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	if first == second {
		t.Fatalf("two jobs with the same name share the directory %q", first)
	}
}

// A job name is user input and must not steer the directory out of the job root.
func TestEnsureJobDir_KeepsTheNameInside(t *testing.T) {
	h, _ := mustHandler(t)
	for _, name := range []string{"../../etc/passwd", "..", ".", "a/b", "with space"} {
		dir, err := h.ensureJobDir(&ScheduledJob{Name: name})
		if err != nil {
			t.Fatalf("ensureJobDir(%q): %v", name, err)
		}
		base := filepath.Join(RuntimeDir(h.dataDir), "jobs")
		rel, relErr := filepath.Rel(base, dir)
		if relErr != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
			t.Errorf("name %q did not stay a single element under the job dir: %q", name, dir)
		}
		if rel == "" || strings.HasPrefix(rel, ".") {
			t.Errorf("name %q produced a hidden or empty element: %q", name, dir)
		}
	}
}

// The job dir is useless under strict: lockdown drops every mount, and a
// YESMEM_JOB_DIR pointing at an unwritable path would make a cap's redirect abort
// the script. Strict therefore keeps the /tmp fallback.
// A job without a project gets an empty variable, so the entry is always there:
// an omitted key would let an inherited YESMEM_PROJECT through mergeEnv.
func TestBashSandboxConfig_JobDirOnlyOutsideStrict(t *testing.T) {
	h, _ := mustHandler(t)
	job := &ScheduledJob{ID: "sched-poll-999", Name: "poll"}

	standard := h.bashSandboxConfig(job, nil, ProfileStandard)
	if len(standard.RWMapPaths) != 1 {
		t.Fatalf("standard must map the job dir, got %+v", standard)
	}
	if _, ok := los32cEnvValue(standard.Env, "YESMEM_JOB_DIR"); !ok {
		t.Errorf("env = %v, want YESMEM_JOB_DIR", standard.Env)
	}

	strict := h.bashSandboxConfig(job, nil, ProfileStrict)
	if len(strict.RWMapPaths) != 0 {
		t.Errorf("strict cannot use a mount and must not promise one, got %+v", strict)
	}
	if _, ok := los32cEnvValue(strict.Env, "YESMEM_JOB_DIR"); ok {
		t.Errorf("env = %v, strict cannot reach the job dir", strict.Env)
	}
}

// An unknown stored profile must not run the job without a sandbox.
func TestJobSandboxProfile_UnknownFallsBackToStandard(t *testing.T) {
	if got := jobSandboxProfile("yolo"); got != ProfileStandard {
		t.Errorf("unknown profile resolved to %s, want standard", got)
	}
	if got := jobSandboxProfile(""); got != ProfileStandard {
		t.Errorf("an empty column resolved to %s, want standard (an absent profile inherits, ParseSandboxProfile does not read it as none)", got)
	}
	if got := jobSandboxProfile("none"); got != ProfileNone {
		t.Errorf("an explicit none must survive, got %s", got)
	}
	if got := jobSandboxProfile("strict"); got != ProfileStrict {
		t.Errorf("strict must survive, got %s", got)
	}
}

// ai-jail parses --rw-map as SOURCE:DEST, so a ':' anywhere in the path makes the
// mount vanish without a word. Only the data dir can carry one.
func TestEnsureJobDir_RefusesAColonInTheDataDir(t *testing.T) {
	h, _ := mustHandler(t)
	h.dataDir = filepath.Join(t.TempDir(), "co:lon")

	if _, err := h.ensureJobDir(&ScheduledJob{ID: "sched-x-1", Name: "x"}); err == nil {
		t.Fatal("a data dir with ':' must be refused, the mount would be dropped silently")
	}
}

// A duplicate ID must be refused before the row is written: SaveScheduledJob
// replaces, and the rollback could not tell a fresh row from an overwritten one.
func TestPersistJob_RefusesADuplicateIDWithoutWriting(t *testing.T) {
	h, store := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	job := ScheduledJob{ID: "dup-1", Name: "dup", Cron: "* * * * *", Prompt: "echo", Enabled: true, Recurring: true, Mode: "bash"}
	if err := h.scheduler.AddJob(job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	row := storage.ScheduledJobRow{ID: job.ID, Name: job.Name, Cron: job.Cron, Prompt: job.Prompt, Enabled: true, Recurring: true, Mode: "bash"}
	if err := h.persistJob(job, row); err == nil {
		t.Fatal("a duplicate job ID must be refused")
	}

	jobs, err := store.ListScheduledJobs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused create must not leave a row behind, got %+v", jobs)
	}
}

// Deleting a job mid-run keeps the dir the running sandbox still writes into.
func TestScheduleDelete_KeepsTheDirOfARunningJob(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	job := ScheduledJob{ID: "sched-busy-9", Name: "busy", Cron: "* * * * *", Prompt: "echo", Enabled: true, Recurring: true, Mode: "bash"}
	if err := h.scheduler.AddJob(job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	dir, err := h.ensureJobDir(&job)
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	h.scheduler.running[job.ID] = true

	resp := h.Handle(Request{Method: "schedule", Params: map[string]any{"action": "delete", "id": job.ID}})
	if resp.Error != "" {
		t.Fatalf("schedule delete: %s", resp.Error)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("the dir of a running job was removed: %v", statErr)
	}
}

// A duplicate key in the appended environment must win over the inherited one.
func TestMergeEnv_OverridesInheritedKeys(t *testing.T) {
	t.Setenv("YESMEM_JOB_DIR", "/inherited")
	merged := mergeEnv([]string{"YESMEM_JOB_DIR=/job-dir"})
	var hits []string
	for _, kv := range merged {
		if strings.HasPrefix(kv, "YESMEM_JOB_DIR=") {
			hits = append(hits, kv)
		}
	}
	if len(hits) != 1 || hits[0] != "YESMEM_JOB_DIR=/job-dir" {
		t.Fatalf("merged env has %v, want exactly the override", hits)
	}
}

// A timed-out headless run must not leave a section that reads like a finished one.
func TestExecuteHeadless_TimeoutIsMarkedInTheSection(t *testing.T) {
	previous := headlessJobTimeout
	headlessJobTimeout = 200 * time.Millisecond
	t.Cleanup(func() { headlessJobTimeout = previous })

	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	// executeHeadless builds a sandbox, and that materialises mask placeholders in
	// the home (YM-291). A fake home keeps the test off the developer's own.
	t.Setenv("HOME", t.TempDir())

	h, _ := mustHandler(t)
	job := ScheduledJob{ID: "red-timeout", Name: "timeout-job", Prompt: "hello", Mode: "headless"}
	h.executeHeadless(job)

	// The result is reported under the address the run belongs to: the job's
	// project, or its run work dir when it has none (YM-295).
	resp := h.handleScratchpadRead(map[string]any{"project": h.jobWorkDir(&job), "section": "sched-timeout-job"})
	if resp.Error != "" {
		t.Fatalf("read section: %s", resp.Error)
	}
	body := string(resp.Result)
	if !strings.Contains(body, "TIMEOUT") {
		t.Fatalf("section does not mark the aborted run: %s", body)
	}
	if !strings.Contains(body, "timeout") {
		t.Errorf("section header does not carry the status: %s", body)
	}
}

// Deleting a job also drops its log directory; nothing else ever cleans it up.
func TestScheduleDelete_RemovesTheJobDir(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	job := ScheduledJob{ID: "sched-gone-777", Name: "gone", Cron: "* * * * *", Prompt: "echo", Enabled: true, Recurring: true, Mode: "bash"}
	if err := h.scheduler.AddJob(job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	dir, err := h.ensureJobDir(&job)
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}

	resp := h.Handle(Request{Method: "schedule", Params: map[string]any{"action": "delete", "id": job.ID}})
	if resp.Error != "" {
		t.Fatalf("schedule delete: %s", resp.Error)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("job dir %q survived the delete (stat err %v)", dir, statErr)
	}
}

// ai-jail 2.x does not inherit the environment, so the job dir has to be named
// explicitly; 0.9.x inherits and has no --env flag.
func TestSandboxFlags_EnvIsForwardedOnTwoX(t *testing.T) {
	dir := t.TempDir()
	for _, major := range []int{0, 2} {
		stubAiJailVersion(t, major)
		s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
			RWMapPaths: []string{dir},
			Env:        []string{"YESMEM_JOB_DIR=" + dir},
		}}
		_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
		if err != nil {
			t.Fatalf("major=%d: wrap: %v", major, err)
		}
		hasEnv := containsString(args, "--env")
		if major == 2 && !hasEnv {
			t.Errorf("2.x must forward the variable by name, got %v", args)
		}
		if major == 2 && !containsString(args, "YESMEM_JOB_DIR") {
			t.Errorf("2.x forwards the name only, got %v", args)
		}
		if major == 2 && containsString(args, "YESMEM_JOB_DIR="+dir) {
			t.Errorf("the value must not appear in ai-jail's argv, got %v", args)
		}
		if major == 0 && hasEnv {
			t.Errorf("0.9.x has no --env and inherits, got %v", args)
		}
	}
}

// The sanitizer collapses different keys onto the same name, so the directory needs
// a hash part: two jobs must not share a state file.
func TestJobDirName_DistinctKeysStayApart(t *testing.T) {
	if jobDirName("sched-a/b-1") == jobDirName("sched-a_b-1") {
		t.Fatal("two different keys collapsed onto one directory name")
	}
	if jobDirName("sched-a/b-1") != jobDirName("sched-a/b-1") {
		t.Fatal("the name is not stable")
	}
	for _, key := range []string{"../../etc/passwd", "..", ".", "", "a/b"} {
		name := jobDirName(key)
		if strings.ContainsRune(name, filepath.Separator) || name == "." || name == ".." {
			t.Errorf("key %q produced an unsafe element %q", key, name)
		}
	}
}

func TestJobWorkDir_ResolvesFromTheJob(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	project := t.TempDir()

	job := &ScheduledJob{ID: "sched-workdir-proj", Name: "workdir-proj", Project: project}
	if got := h.jobWorkDir(job); got != project {
		t.Errorf("jobWorkDir(job) = %q, want the job's project %q", got, project)
	}
	// Without a project the run gets its own empty directory, never the runtime
	// dir itself and never a checkout the daemon invented.
	got := h.jobWorkDir(&ScheduledJob{ID: "sched-workdir-none"})
	if !strings.HasPrefix(got, RuntimeDir(h.dataDir)+string(filepath.Separator)) {
		t.Errorf("jobWorkDir(without project) = %q, want a directory inside %q", got, RuntimeDir(h.dataDir))
	}
}

func TestHandlerScheduler_NoHardcodedDevPath(t *testing.T) {
	src, err := os.ReadFile("handler_scheduler.go")
	if err != nil {
		t.Fatalf("read handler_scheduler.go: %v", err)
	}
	if strings.Contains(string(src), "/home/user/projects/yesmem") {
		t.Error("handler_scheduler.go contains hardcoded /home/user/projects/yesmem path — use h.jobWorkDir() / resolveAgentWorkDir instead")
	}
}

// TestScheduleCreate_IntervalSecondsWithoutCron verifies that schedule create
// accepts an empty cron when interval_seconds > 0. The scheduler at fire time
// uses interval_seconds when set, so cron is unused and should not be parsed.
// Without the fix, parseCron("") returns "need 5 fields, got 0".
func TestScheduleCreate_IntervalSecondsWithoutCron(t *testing.T) {
	h, _ := mustHandler(t)
	seedCapWithSandbox(t, h, "telegram", "telegram_poll", "echo poll", "")

	resp := h.Handle(Request{
		Method: "schedule",
		Params: map[string]any{
			"action":           "create",
			"name":             "telegram-poll",
			"interval_seconds": float64(15),
			"mode":             "bash",
			"cap_name":         "telegram",
			"script_name":      "telegram_poll",
		},
	})
	if resp.Error != "" {
		t.Fatalf("schedule create with empty cron + interval_seconds=15 should succeed, got error: %s", resp.Error)
	}
}

// TestScheduleCreate_EmptyCronAndZeroInterval ensures the existing validation
// still rejects jobs with neither cron nor interval_seconds set.
func TestScheduleCreate_EmptyCronAndZeroInterval(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "schedule",
		Params: map[string]any{
			"action": "create",
			"name":   "no-schedule",
			"mode":   "bash",
			"prompt": "echo hi",
		},
	})
	if resp.Error == "" {
		t.Fatal("expected error for missing cron and interval_seconds, got success")
	}
}

func TestStoreBashRun_SkipsIdleRuns(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{store: s}
	job := &ScheduledJob{ID: "test-idle", CapName: "test"}

	h.storeBashRun(job, "echo hi", "", nil, 0)

	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs for idle run, got %d", len(runs))
	}
}

func TestStoreBashRun_StillStoresErrors(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{store: s}
	job := &ScheduledJob{ID: "test-err", CapName: "test"}

	h.storeBashRun(job, "failing cmd", "", errors.New("boom"), 1)

	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("expected error run to be stored, got 0")
	}
	if runs[0].Status != "error" {
		t.Fatalf("expected status=error, got %q", runs[0].Status)
	}
}

func TestStoreBashRun_StillStoresRealOutput(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{store: s}
	job := &ScheduledJob{ID: "test-real", CapName: "test"}

	h.storeBashRun(job, "echo done", "Combined fire: polls=5 stored=2 replies=1 fails=0", nil, 0)

	runs, err := s.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("expected real-output run to be stored, got 0")
	}
}
