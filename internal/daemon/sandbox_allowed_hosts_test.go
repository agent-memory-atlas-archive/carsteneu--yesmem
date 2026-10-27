package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// seedCapWithHosts stores a cap whose bash script declares an egress allowlist.
func seedCapWithHosts(t *testing.T, h *Handler, capName, scriptName, body string, hosts []string) {
	t.Helper()
	meta := CapMeta{
		Name: capName,
		Scripts: []ScriptMeta{{
			Name:         scriptName,
			Kind:         "handler",
			Runtime:      "bash",
			Body:         body,
			Sandbox:      "standard",
			AllowedHosts: hosts,
		}},
		Version: 1,
		Tested:  true,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category: "cap",
		Content:  capName,
		Project:  "",
		Context:  ctx,
		Source:   "user_stated",
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}

// D13b: the egress allowlist is per script, so a self-hosted service (the
// rocketchat cap) does not have to be compiled into the daemon.
func TestResolveBashCommand_ReturnsScriptAllowedHosts(t *testing.T) {
	h, _ := mustHandler(t)
	seedCapWithHosts(t, h, "rocketchat", "poll", "echo ok", []string{"chat.example.org"})

	job := &ScheduledJob{CapName: "rocketchat", ScriptName: "poll"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hosts := resolved.hosts; len(hosts) != 1 || hosts[0] != "chat.example.org" {
		t.Errorf("hosts = %v, want [chat.example.org]", hosts)
	}
}

// A job that runs its own prompt has no cap script to take hosts from.
func TestResolveBashCommand_PromptJobHasNoHosts(t *testing.T) {
	h, _ := mustHandler(t)

	job := &ScheduledJob{Prompt: "echo ok"}
	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hosts := resolved.hosts; len(hosts) != 0 {
		t.Errorf("hosts = %v, want none", hosts)
	}
}

// The schedule surface has to carry the field end to end: create stores it, the
// list reports it, and the job in memory has it for the next run.
func TestScheduleCreate_StoresAllowedHosts(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)

	resp := h.handleSchedule(map[string]any{
		"action":        "create",
		"name":          "rc_poll",
		"cron":          "*/5 * * * *",
		"mode":          "bash",
		"prompt":        "echo poll",
		"allowed_hosts": "chat.example.org, api.telegram.org",
	})
	if resp.Error != "" {
		t.Fatalf("create: %s", resp.Error)
	}

	jobs := h.scheduler.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs))
	}
	if got := strings.Join(jobs[0].AllowedHosts, ","); got != "chat.example.org,api.telegram.org" {
		t.Errorf("job AllowedHosts = %q", got)
	}

	list := h.handleSchedule(map[string]any{"action": "list"})
	var payload struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(list.Result, &payload); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(payload.Jobs) != 1 {
		t.Fatalf("expected one job in the list, got %d", len(payload.Jobs))
	}
	if got, _ := payload.Jobs[0]["allowed_hosts"].(string); got != "chat.example.org, api.telegram.org" {
		t.Errorf("scheduleList allowed_hosts = %q", got)
	}
}

// A URL is not a host; the create call is the place to say so.
func TestScheduleCreate_RejectsInvalidAllowedHost(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)

	resp := h.handleSchedule(map[string]any{
		"action":        "create",
		"name":          "bad_host",
		"cron":          "*/5 * * * *",
		"mode":          "bash",
		"prompt":        "echo poll",
		"allowed_hosts": "https://chat.example.org",
	})
	if resp.Error == "" {
		t.Fatal("expected a rejection of a URL in allowed_hosts")
	}
	if !strings.Contains(resp.Error, "allowed host") {
		t.Errorf("error should name the allowed host, got %s", resp.Error)
	}
}

// An empty list means no egress: the standard profile must not invent a default.
func TestSandboxFlagsV2_EmptyAllowedHostsMeansNoEgress(t *testing.T) {
	stubAiJailVersion(t, 2)
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail"}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--no-network") {
		t.Errorf("standard must deny by default, got %v", args)
	}
	if strings.Contains(joined, "--allow-host") {
		t.Errorf("no host was configured, so none may be allowed, got %v", args)
	}
}
