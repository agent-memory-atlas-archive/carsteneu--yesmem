package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/storage"
)

func (h *Handler) handleSchedule(params map[string]any) Response {
	action := stringOr(params, "action", "")
	switch action {
	case "create":
		return h.scheduleCreate(params)
	case "list":
		return h.scheduleList(params)
	case "delete":
		return h.scheduleDelete(params)
	case "run":
		return h.scheduleRun(params)
	default:
		return errorResponse(fmt.Sprintf("schedule: unknown action %q (create|list|delete|run)", action))
	}
}

func (h *Handler) scheduleCreate(params map[string]any) Response {
	name := stringOr(params, "name", "")
	cron := stringOr(params, "cron", "")
	prompt := stringOr(params, "prompt", "")
	mode := stringOr(params, "mode", "agent")
	capName := stringOr(params, "cap_name", "")
	scriptName := stringOr(params, "script_name", "")
	allowedPorts := stringOr(params, "allowed_ports", "80,443")
	allowedHosts := stringOr(params, "allowed_hosts", "")
	for _, host := range capfile.SplitHostList(allowedHosts) {
		if err := capfile.ValidateHost(host); err != nil {
			return errorResponse(fmt.Sprintf("schedule create: %v", err))
		}
	}
	autoCorrect := true
	if v, ok := params["auto_correct"].(bool); ok {
		autoCorrect = v
	}
	enabled := true
	if v, ok := params["enabled"].(bool); ok {
		enabled = v
	}
	recurring := true
	if v, ok := params["recurring"].(bool); ok {
		recurring = v
	}
	intervalSeconds := intOr(params, "interval_seconds", 0)
	model := stringOr(params, "model", "")
	backend := stringOr(params, "backend", "")
	if backend != "" && !allowedAgentBackends[backend] {
		return errorResponse(fmt.Sprintf("schedule create: backend %q is not supported (claude|codex|opencode)", backend))
	}

	if mode != "agent" && mode != "headless" && mode != "bash" {
		return errorResponse(fmt.Sprintf("schedule create: mode must be 'agent', 'headless', or 'bash', got %q", mode))
	}

	if mode == "bash" && capName == "" && prompt == "" {
		return errorResponse("schedule create: bash mode requires 'cap_name' or 'prompt'")
	}

	if name == "" || (cron == "" && intervalSeconds <= 0) {
		return errorResponse("schedule create: name and (cron or interval_seconds) are required")
	}
	if mode != "bash" && prompt == "" {
		return errorResponse("schedule create: prompt is required for agent/headless mode")
	}

	sandboxStr := stringOr(params, "sandbox", "")
	// An explicit default_sandbox_profile: none is a decision, not an absent
	// setting. ProfileNone is also the zero value, so the flag is what tells the
	// two apart; without it a configured none silently became standard (YM-173).
	defaultProfile := ProfileStandard
	if h.sandboxDefaultSet {
		defaultProfile = h.defaultSandboxProfile
	}
	sandbox, err := resolveJobSandbox(sandboxStr, defaultProfile)
	if err != nil {
		return errorResponse(fmt.Sprintf("schedule create: %v", err))
	}

	id := fmt.Sprintf("sched-%s-%d", jobNameToken(name), time.Now().UnixMilli()%100000)

	// A job belongs to the project it was created from. Its cap may have to name
	// a project to the daemon, and a bare basename is refused once two checkouts
	// share it (YM-281), so the caller's project is recorded here. The MCP layer
	// always sends the working directory; an explicit project wins over it.
	// A relative value is not stored: it would reach the daemon as a short name
	// and fail exactly like the bare basename this replaces.
	//
	// The project is read before the sandbox lock below: a cap is resolved in the
	// project the job will run in, so the lock has to ask with the same value the
	// run will use (YM-417).
	project := stringOr(params, "project", "")
	if project == "" {
		if cwd, _ := params["_cwd"].(string); cwd != "" {
			project = filepath.Clean(cwd)
		}
	}
	if project != "" && !filepath.IsAbs(project) {
		log.Printf("[scheduler] job %s: ignoring non-absolute project %q", id, project)
		project = ""
	}

	// The save lock keeps a project-bound cap off the unsandboxed path, but a job
	// resolves its profile on its own (script, else job, else default) — so the
	// lock has to sit on the profile the run will really use, not only on
	// save_cap (YM-346). Only a bash job runs a cap script at all; an agent or
	// headless job may carry a cap_name without ever executing it. A cap this call
	// cannot resolve is left to fireJobBash: it stops on the same resolution error
	// before running anything.
	if mode == "bash" && capName != "" {
		if resolved, rerr := h.los35cResolveBashCap(&ScheduledJob{CapName: capName, ScriptName: scriptName, Project: project}); rerr == nil {
			if err := h.los35cRefuseUnsandboxedCapRun(capName, los35cEffectiveJobProfile(sandbox, resolved.scriptSandbox), resolved.projectBound); err != nil {
				return errorResponse(fmt.Sprintf("schedule create: %v", err))
			}
		}
	}

	if cron != "" {
		if _, err := parseCron(cron); err != nil {
			return errorResponse(fmt.Sprintf("schedule create: invalid cron %q: %v", cron, err))
		}
	}

	job := ScheduledJob{
		ID: id, Name: name, Cron: cron, Prompt: prompt, Enabled: enabled, Recurring: recurring, Mode: mode,
		CapName: capName, ScriptName: scriptName, Project: project, AutoCorrect: autoCorrect, AllowedPorts: allowedPorts,
		AllowedHosts: capfile.SplitHostList(allowedHosts),
		Sandbox:      sandbox, IntervalSeconds: intervalSeconds, Model: model, Backend: backend,
	}

	row := storage.ScheduledJobRow{
		ID: id, Name: name, Cron: cron, Prompt: prompt, Enabled: enabled, Recurring: recurring, Mode: mode,
		CapName: capName, ScriptName: scriptName, Project: project, AutoCorrect: autoCorrect, AllowedPorts: allowedPorts,
		AllowedHosts: allowedHosts,
		Sandbox:      sandbox.String(), IntervalSeconds: intervalSeconds, Model: model, Backend: backend,
		CreatedAt: time.Now(),
	}
	// The row is written before the job enters the scheduler: a one-shot that fires
	// in between would be deleted while it is not persisted yet, and the insert
	// would bring it back for the next restart (YM-169).
	if err := h.persistJob(job, row); err != nil {
		return errorResponse(fmt.Sprintf("schedule create: %v", err))
	}

	return jsonResponse(map[string]any{"id": id, "name": name, "cron": cron, "mode": mode, "cap_name": capName, "script_name": scriptName, "project": project, "auto_correct": autoCorrect, "allowed_ports": allowedPorts, "allowed_hosts": allowedHosts, "status": "created"})
}

// persistJob writes the row and then registers the job. The order is deliberate: a
// one-shot that fires before its row exists is deleted from a row that does not
// exist yet and would be inserted afterwards (YM-169). An ID that is already
// scheduled is refused before the write, because SaveScheduledJob replaces and the
// rollback could not tell a fresh row from an overwritten one.
func (h *Handler) persistJob(job ScheduledJob, row storage.ScheduledJobRow) error {
	if h.scheduler != nil {
		for _, existing := range h.scheduler.ListJobs() {
			if existing.ID == job.ID {
				return fmt.Errorf("job %q already exists", job.ID)
			}
		}
	}
	if err := h.store.SaveScheduledJob(row); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	if h.scheduler != nil {
		if err := h.scheduler.AddJob(job); err != nil {
			if dbErr := h.store.DeleteScheduledJob(job.ID); dbErr != nil {
				log.Printf("[scheduler] roll back job %s: %v", job.ID, dbErr)
			}
			return err
		}
	}
	return nil
}

func (h *Handler) scheduleList(params map[string]any) Response {
	var jobs []ScheduledJob
	if h.scheduler != nil {
		jobs = h.scheduler.ListJobs()
	}
	result := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		entry := map[string]any{
			"id":               j.ID,
			"name":             j.Name,
			"cron":             j.Cron,
			"prompt":           j.Prompt,
			"enabled":          j.Enabled,
			"recurring":        j.Recurring,
			"mode":             j.Mode,
			"cap_name":         j.CapName,
			"script_name":      j.ScriptName,
			"project":          j.Project,
			"auto_correct":     j.AutoCorrect,
			"allowed_ports":    j.AllowedPorts,
			"allowed_hosts":    strings.Join(j.AllowedHosts, ", "),
			"sandbox":          j.Sandbox.String(),
			"interval_seconds": j.IntervalSeconds,
			"backend":          j.Backend,
			"model":            j.Model,
		}
		if !j.LastRun.IsZero() {
			entry["last_run"] = j.LastRun.Format(time.RFC3339)
		}
		result = append(result, entry)
	}
	return jsonResponse(map[string]any{"jobs": result, "count": len(result)})
}

func (h *Handler) scheduleDelete(params map[string]any) Response {
	id := stringOr(params, "id", "")
	if id == "" {
		return errorResponse("schedule delete: id is required")
	}

	// Captured before RemoveJob, which drops the running flag: a run that is still
	// writing into its job dir keeps it until it is done.
	running := h.scheduler != nil && h.scheduler.IsRunning(id)

	if h.scheduler != nil {
		if err := h.scheduler.RemoveJob(id); err != nil {
			return errorResponse(fmt.Sprintf("schedule delete: %v", err))
		}
	}

	if err := h.store.DeleteScheduledJob(id); err != nil && err != storage.ErrNotFound {
		return errorResponse(fmt.Sprintf("schedule delete: db: %v", err))
	}

	h.headlessSessionsMu.Lock()
	delete(h.headlessSessions, id)
	h.headlessSessionsMu.Unlock()

	if !running {
		h.removeJobDir(id)
	}

	return jsonResponse(map[string]any{"id": id, "status": "deleted"})
}

func (h *Handler) scheduleRun(params map[string]any) Response {
	id := stringOr(params, "id", "")
	prompt := stringOr(params, "prompt", "")

	if id == "" && prompt == "" {
		return errorResponse("schedule run: id or prompt required")
	}

	if id != "" && h.scheduler != nil {
		for _, j := range h.scheduler.ListJobs() {
			if j.ID == id {
				go h.executeScheduledPrompt(j)
				return jsonResponse(map[string]any{"status": "running", "id": id, "mode": j.Mode})
			}
		}
		return errorResponse(fmt.Sprintf("schedule run: job %q not found", id))
	}

	go h.executeScheduledPrompt(ScheduledJob{
		ID: id, Name: "manual-run", Prompt: prompt, Mode: stringOr(params, "mode", "agent"),
	})

	return jsonResponse(map[string]any{"status": "running", "prompt": prompt})
}

func (h *Handler) executeScheduledPrompt(job ScheduledJob) {
	if job.Mode == "bash" {
		h.fireJobBash(&job)
		return
	}
	if job.Mode == "headless" {
		h.executeHeadless(job)
		return
	}
	h.executeAgent(job)
}

func headlessArgs(sessionID, model string) []string {
	// No human is there to approve a tool call, and the run used to get its
	// allowlist from an allow-all .claude/settings.local.json written into the
	// job's project: that polluted a real repository, and a symlinked .claude left
	// the run without any allowlist until its deadline (YM-295). The bypass
	// belongs on the command line.
	args := []string{"-p", "--output-format", "json", "--verbose", "--max-turns", "3",
		"--permission-mode", "bypassPermissions"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return args
}

func parseHeadlessOutput(output []byte) (text string, sessionID string) {
	var obj struct {
		Type      string `json:"type"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(output, &obj); err == nil && obj.Type == "result" {
		return obj.Result, obj.SessionID
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if err := json.Unmarshal([]byte(lines[i]), &obj); err == nil && obj.Type == "result" {
			return obj.Result, obj.SessionID
		}
	}

	return strings.TrimSpace(string(output)), ""
}

// headlessJobTimeout bounds one headless run. Without a bound a hanging claude
// keeps the job marked running forever, so it never fires again (YM-171). The
// value matches the claude allowance in fireJobBash.
var headlessJobTimeout = 10 * time.Minute

// errHeadlessTimeout marks a headless run that was killed by its deadline, so the
// caller can log the deadline instead of a bare signal error.
var errHeadlessTimeout = errors.New("headless run timed out")

// runHeadlessCommand runs the wrapped command with a deadline. The prompt arrives
// on stdin, which is how claude -p reads it. env is the run's sandbox environment:
// the MCP server claude starts inside dials the daemon through YESMEM_SOCK, and
// the sandbox only mounts the restricted socket (YM-267). The deadline takes down
// the whole process group: killing the direct child alone left the children it
// started running. WaitDelay bounds the wait for the pipes after the kill, so a
// grandchild cannot keep the job goroutine blocked.
func runHeadlessCommand(bin string, args []string, prompt, dir string, env []string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), headlessJobTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = mergeEnv(env)
	}
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	killProcessGroupOnCancel(cmd)

	err := cmd.Run()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w after %v", errHeadlessTimeout, headlessJobTimeout)
	}
	return err
}

func (h *Handler) executeHeadless(job ScheduledJob) {
	section := jobSectionName(job.Name)
	// Same registration as a bash run: the directory is keyed on the job ID, and a
	// deleted row must not cost a live run its cwd (YM-295).
	h.runStarting(jobKey(job.ID, job.Name))
	defer h.runFinished(jobKey(job.ID, job.Name))
	wrappedPrompt := fmt.Sprintf("You are a scheduled task agent. Job: %s (ID: %s). Your ONLY job is:\n\n%s\n\nExecute ONLY this task. No orientation, no health checks, no file modifications. When done, say DONE and stop.", job.Name, job.ID, job.Prompt)

	h.headlessSessionsMu.Lock()
	sessionID := h.headlessSessions[job.ID]
	h.headlessSessionsMu.Unlock()
	args := headlessArgs(sessionID, job.Model)

	// Fail closed: a headless job that asks for a sandbox profile must not run
	// unsandboxed when ai-jail is missing. Unlike fireJobBash this path has no run
	// record, so the skip shows up only in this log line.
	sb := h.newSandbox(SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		AllowedPorts:        parsePorts(job.AllowedPorts),
		AllowedHosts:        job.AllowedHosts,
		ForwardPorts:        h.agentForwardPorts(),
		// The dir the run really starts in (runHeadlessCommand below): keeping the
		// two in step means a later mount/env addition cannot pick the wrong one.
		WorkDir: h.jobWorkDir(&job),
	})
	bin, wrappedArgs, wrapErr := sb.WrapExecArgs("claude", args, job.Sandbox)
	if wrapErr != nil {
		log.Printf("[scheduler] headless job %s: %v", job.ID, wrapErr)
		return
	}

	log.Printf("[scheduler] headless run for job %s (resume=%v, sandbox=%s)", job.ID, sessionID != "", job.Sandbox)
	var stdout, stderr bytes.Buffer
	workDir := h.jobWorkDir(&job)
	if workDir == "" {
		log.Printf("[scheduler] headless job %s skipped: no working directory", job.ID)
		return
	}
	// No permissions file here: the run carries the bypass on its command line
	// (headlessArgs). ensureAgentPermissions used to write an allow-all
	// .claude/settings.local.json into the job's project, which is a real
	// repository, and it refused to do so through a symlinked .claude, leaving the
	// run without any allowlist until the deadline (YM-295).
	err := runHeadlessCommand(bin, wrappedArgs, wrappedPrompt, workDir, sb.cfg.Env, &stdout, &stderr)
	timedOut := errors.Is(err, errHeadlessTimeout)
	if err != nil {
		if timedOut {
			log.Printf("[scheduler] headless job %s timed out after %v", job.ID, headlessJobTimeout)
		} else {
			log.Printf("[scheduler] headless job %s failed: %v (stderr: %s)", job.ID, err, h.redact(truncatStr(stderr.String(), 500)))
		}
	}

	text, newSessionID := parseHeadlessOutput(stdout.Bytes())
	h.headlessSessionsMu.Lock()
	if newSessionID != "" {
		h.headlessSessions[job.ID] = newSessionID
	}
	sessionForLog := h.headlessSessions[job.ID]
	h.headlessSessionsMu.Unlock()

	result := h.redact(truncatStr(text, 10000))
	status := "done"
	if timedOut {
		// The section is the only thing that outlives the run, so an aborted run
		// must not read like a finished one.
		status = "timeout"
		result = fmt.Sprintf("TIMEOUT after %v, partial output:\n\n%s", headlessJobTimeout, result)
	}
	// The result is reported under the address the run belongs to: the job's
	// project when it has one, its job-owned run dir otherwise. Until YM-295 every
	// scheduler write used the literal "yesmem", the daemon's own checkout, so a
	// job of any other project reported into a foreign one.
	h.handleScratchpadWrite(map[string]any{
		"project": workDir,
		"section": section,
		"content": fmt.Sprintf("## Headless Result [%s] (%s)\n\n%s", job.Name, status, result),
	})
	log.Printf("[scheduler] headless job %s %s (%d bytes, session=%s, section=%s under %q)", job.ID, status, stdout.Len(), sessionForLog, section, workDir)
}

func (h *Handler) executeAgent(job ScheduledJob) {
	section := jobSectionName(job.Name)
	// One address for the agent row, the task section and every control call: the
	// job's project when it has one, the job-owned run work dir otherwise. Until
	// YM-295 all of them used the literal "yesmem", the daemon's own checkout, so
	// get/relay/stop could not find the agent of any other project and the run
	// reported into a foreign one.
	project := h.jobWorkDir(&job)
	if project == "" {
		// No address and no cwd: every call below would fail one by one and the
		// run would end as a silent no-op (executeHeadless fails closed here too).
		log.Printf("[scheduler] agent job %s skipped: no working directory", job.ID)
		return
	}

	h.handleStopAgent(map[string]any{
		"to":      section,
		"project": project,
	})

	taskContent := fmt.Sprintf("## SCHEDULED TASK [%s]\nJob-ID: %s\n\n", job.Name, job.ID) +
		"You are a scheduled task agent. Your ONLY job is:\n\n" +
		job.Prompt + "\n\n" +
		"Execute ONLY this task. No orientation, no health checks, no file modifications.\n" +
		"When done: write your result to this scratchpad section and stop.\n" +
		"If no task is found: write DONE and stop immediately."
	h.handleScratchpadWrite(map[string]any{
		"project": project,
		"section": section,
		"content": taskContent,
	})

	resp := h.handleSpawnAgent(map[string]any{
		"project":  project,
		"section":  section,
		"work_dir": project,
		"backend":  job.Backend,
		"model":    job.Model,
	})
	if resp.Error != "" {
		// A refused spawn used to end as a log line only. The section is the one
		// place a run's outcome is read from, so the failure belongs there too — a
		// project path the spawn guard refuses (quotes, control characters) must
		// not look like a job that never fired.
		log.Printf("[scheduler] spawn failed: %s", resp.Error)
		h.handleScratchpadWrite(map[string]any{
			"project": project,
			"section": section,
			"content": fmt.Sprintf("## Scheduled Agent Not Started [%s]\nJob-ID: %s\n\n%s", job.Name, job.ID, h.redact(resp.Error)),
		})
		return
	}
	log.Printf("[scheduler] agent spawned for job %s (section=%s under %q)", job.ID, section, project)
	wrappedPrompt := fmt.Sprintf("You are a scheduled task agent. Job: %s (ID: %s). Your ONLY job is:\n\n%s\n\nExecute ONLY this task. No orientation, no health checks, no file modifications. When done, say DONE and stop.", job.Name, job.ID, job.Prompt)
	h.handleRelayAgent(map[string]any{
		"to":      section,
		"content": wrappedPrompt,
		"project": project,
	})
	h.handleRelayAgent(map[string]any{
		"to":      section,
		"content": "",
		"project": project,
	})

	go h.watchScheduledAgent(section, project)
}

func (h *Handler) watchScheduledAgent(section, project string) {
	const idleTimeout = 10 * time.Minute
	const pollInterval = 15 * time.Second

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		resp := h.handleGetAgent(map[string]any{
			"to":      section,
			"project": project,
		})
		if resp.Error != "" {
			log.Printf("[scheduler] agent %s gone: %s", section, resp.Error)
			return
		}
		var data map[string]any
		if err := json.Unmarshal(resp.Result, &data); err != nil {
			continue
		}
		status, _ := data["status"].(string)
		if status == "stopped" || status == "exited" {
			log.Printf("[scheduler] agent %s finished (status=%s)", section, status)
			return
		}
		lastStr, _ := data["last_activity_at"].(string)
		if lastStr == "" {
			continue
		}
		last, err := time.Parse("2006-01-02 15:04:05", lastStr)
		if err != nil {
			continue
		}
		idle := time.Since(last)
		if idle > idleTimeout {
			log.Printf("[scheduler] agent %s idle for %v (status=%s), stopping", section, idle.Round(time.Second), status)
			h.handleStopAgent(map[string]any{
				"to":      section,
				"project": project,
			})
			return
		}
	}
}

func prepareBashCommand(command string) string {
	if !strings.Contains(command, "store ") && !strings.Contains(command, "store'") && !strings.Contains(command, "llm ") {
		return command
	}
	return capfile.GenerateAdapterBash() + command
}

func (h *Handler) fireJobBash(job *ScheduledJob) {
	log.Printf("[scheduler] bash run for job %s (cap=%s)", job.ID, job.CapName)
	// The run's cwd is keyed on the job ID; registering it keeps the directory
	// sweep off a live process after its job row was deleted (YM-295).
	h.runStarting(jobKey(job.ID, job.Name))
	defer h.runFinished(jobKey(job.ID, job.Name))

	resolved, err := h.los35cResolveBashCap(job)
	if err != nil {
		h.storeBashRun(job, "", "", err, -1)
		return
	}
	command, scriptSandbox, scriptHosts, scriptTimeout := resolved.body, resolved.scriptSandbox, resolved.hosts, resolved.timeout

	command = prepareBashCommand(command)

	// Egress allowlist: a non-empty script list wins over the job's, mirroring how
	// the script's sandbox profile wins below. A script without a list inherits the
	// job's, so an empty script list cannot express "no egress at all" for a job
	// that declares hosts.
	allowedHosts := job.AllowedHosts
	if len(scriptHosts) > 0 {
		allowedHosts = scriptHosts
	}

	// The effective profile decides whether a job dir is reachable at all, so it is
	// resolved before the sandbox config is built: lockdown drops every mount.
	// Order (YM-230): the script's own profile, else the job's. A job row created
	// through schedule create already carries the configured default; a legacy row
	// with an empty column resolved to standard when it was loaded
	// (jobSandboxProfile), so an omitted script profile is never a decision to run
	// unsandboxed.
	effectiveProfile := los35cEffectiveJobProfile(job.Sandbox, scriptSandbox)
	if effectiveProfile != job.Sandbox {
		log.Printf("[scheduler] cap=%s script=%s sandbox override: job=%s -> script=%s",
			job.CapName, job.ScriptName, job.Sandbox.String(), effectiveProfile.String())
	}
	if scriptSandbox != "" {
		if _, perr := ParseSandboxProfile(scriptSandbox); perr != nil {
			log.Printf("[scheduler] cap=%s script=%s invalid sandbox %q, using job profile: %v",
				job.CapName, job.ScriptName, scriptSandbox, perr)
		}
	}

	// A job row that predates the create-side check keeps its profile in the
	// database, so the same lock holds here: a project-bound cap must never run
	// unsandboxed because a job said none (YM-346).
	if err := h.los35cRefuseUnsandboxedCapRun(job.CapName, effectiveProfile, resolved.projectBound); err != nil {
		h.storeBashRun(job, "", "", err, -1)
		return
	}

	// Fail closed: a job that asks for a sandbox profile must not silently run
	// unsandboxed when ai-jail is missing (the run lands as status=error).
	cfg := h.bashSandboxConfig(job, allowedHosts, effectiveProfile)
	if cfg.WorkDir == "" {
		// Without a work dir a 2.x run would inherit the daemon's cwd, the user's
		// home, and bind it over the sandbox's private home. Fail closed with a run
		// record, like a missing sandbox (YM-286).
		h.storeBashRun(job, "", "", fmt.Errorf("no working directory for job %s", job.ID), -1)
		return
	}
	sb := h.newSandbox(cfg)
	timeout := bashJobTimeout(command, scriptTimeout)

	output, exitCode, runErr := sb.RunWithProfile(command, timeout, effectiveProfile)

	// A run that never started (the requested profile needs ai-jail, which is
	// missing, a mask could not be applied, the work dir covers the home, or the work
	// dir is missing) is not a script failure: recording it without the command keeps
	// the auto-correct pass from rewriting a live cap over an infrastructure error.
	recordedCommand := command
	if runNeverStarted(runErr) {
		recordedCommand = ""
	}

	h.storeBashRun(job, recordedCommand, output, runErr, exitCode)

	// A delete that arrived during the run could not remove the dir while the job
	// was still running; the run is over now.
	if h.scheduler != nil && !h.scheduler.HasJob(job.ID) {
		h.removeJobDir(jobKey(job.ID, job.Name))
	}

	if runErr != nil {
		log.Printf("[scheduler] bash job %s FAILED (exit=%d): %v", job.ID, exitCode, runErr)
	} else {
		log.Printf("[scheduler] bash job %s OK (%d bytes)", job.ID, len(output))
	}
}

// bashSandboxConfig assembles the sandbox a bash job runs in. /tmp inside the
// sandbox is its own tmpfs, so cap logs written there are gone when the run ends;
// the job dir is mapped in and named in the environment instead (YM-218). Under
// strict nothing can be mounted into the sandbox, so a YESMEM_JOB_DIR there would
// point at a path the cap cannot create: its log writes would silently go nowhere,
// which is worse than the documented /tmp fallback.
func (h *Handler) bashSandboxConfig(job *ScheduledJob, allowedHosts []string, profile SandboxProfile) SandboxConfig {
	cfg := SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		AllowedPorts:        parsePorts(job.AllowedPorts),
		AllowedHosts:        allowedHosts,
		// A bash job may itself start an agent (`claude -p`), which needs the
		// proxy relay on 2.x just like a headless job. Caps that only use
		// store()/llm() reach the daemon over the mapped socket and ignore it.
		ForwardPorts: h.agentForwardPorts(),
		// Job-owned working directory: the run must not inherit the daemon's cwd,
		// where a project .ai-jail could change the sandbox.
		WorkDir: h.jobWorkDir(job),
	}
	// A cap that talks to the daemon has to name a project, and a bare basename
	// is refused as soon as two checkouts share it (live: "yesmem" matched
	// /home/tester/memory/yesmem and /home/tester/projects/yesmem, and the
	// broadcast was rejected). The job carries the project it was created from;
	// without one the variable is emptied rather than omitted, so an inherited
	// value cannot leak in through mergeEnv and the cap fails loudly instead of
	// broadcasting into a project the daemon guessed. Env only, no mount, so the
	// entry is also set for the strict profile: ai-jail 2.x forwards the name via
	// --env, while 0.9.x inherits the environment (under --lockdown it scrubs it,
	// but a lockdown run cannot reach the daemon socket anyway).
	project := ""
	if filepath.IsAbs(job.Project) {
		project = job.Project
	}
	cfg.Env = append(cfg.Env, "YESMEM_PROJECT="+project)
	if profile == ProfileStrict {
		return cfg
	}
	jobDir, err := h.ensureJobDir(job)
	if err != nil {
		// The run is still worth doing; only the persistent log dir is lost.
		log.Printf("[scheduler] job %s: no job dir, /tmp inside the sandbox stays volatile: %v", job.ID, err)
		return cfg
	}
	cfg.RWMapPaths = jobDirMounts(jobDir)
	cfg.Env = append(cfg.Env, jobDirEnv(jobDir)...)
	return cfg
}

// los35cBashCap is one resolved bash script plus the binding of the row it came
// from. The binding belongs to the resolution and not to a second lookup: a cap
// name can exist in more than one project, and the row the run takes is the row
// the lock has to judge.
type los35cBashCap struct {
	body          string
	scriptSandbox string
	hosts         []string
	timeout       int64
	projectBound  bool
}

// los35cResolveBashCap returns the command a bash job runs, the sandbox profile
// the cap script declares, its egress allowlist, its declared runtime in seconds
// (0 = none declared) and whether the row that supplied it is project-bound.
//
// The row is resolved the way the catalog and execute_cap resolve a name (own
// project first, then global, a foreign project never). Walking the active rows
// and taking the first name match was an execution path for another project's
// code: a job in project A ran project B's body whenever B's row happened to
// come first, or whatever row shadowed a global cap of the same name (YM-417).
// A job without a project reaches the global rows only, which is the same
// fail-closed rule execute_cap applies to a caller that names no project.
func (h *Handler) los35cResolveBashCap(job *ScheduledJob) (los35cBashCap, error) {
	if job.CapName != "" {
		row, meta, err := h.capRowFor(job.CapName, job.Project)
		if err != nil {
			return los35cBashCap{}, fmt.Errorf("cap %q not found for project %q (a cap saved in another project is not runnable from here): %w", job.CapName, job.Project, err)
		}
		sc, ok := findBashScriptMeta(&meta, job.ScriptName)
		if !ok {
			if job.ScriptName != "" {
				return los35cBashCap{}, fmt.Errorf("cap %q has no bash script named %q", job.CapName, job.ScriptName)
			}
			return los35cBashCap{}, fmt.Errorf("cap %q has no bash script", job.CapName)
		}
		return los35cBashCap{
			body:          sc.Body,
			scriptSandbox: sc.Sandbox,
			hosts:         sc.AllowedHosts,
			timeout:       sc.Timeout,
			projectBound:  capIsProjectBound(row.Project, meta),
		}, nil
	}
	if job.Prompt != "" {
		return los35cBashCap{body: job.Prompt}, nil
	}
	return los35cBashCap{}, fmt.Errorf("bash job has neither cap_name nor prompt")
}

// los35cEffectiveJobProfile is the chain fireJobBash resolves: a script that
// names a profile wins over the job's, an unparsable one leaves the job profile
// standing (the same fallback the run path takes when it logs the override).
func los35cEffectiveJobProfile(jobProfile SandboxProfile, scriptSandbox string) SandboxProfile {
	if scriptSandbox == "" {
		return jobProfile
	}
	profile, err := ParseSandboxProfile(scriptSandbox)
	if err != nil {
		return jobProfile
	}
	return profile
}

// los35cRefuseUnsandboxedCapRun is the save lock on the job level. The lock in
// validateScriptSandboxForScope only guards save_cap; a job resolves script, job
// and default on its own and reached the unsandboxed path without any save.
//
// The caller passes the EFFECTIVE profile, because that is the decision the run
// takes, and the binding of the row that supplied the script. Only a
// project-bound cap is refused: a user-scoped cap may run unsandboxed, which is
// exactly what save_cap allows.
func (h *Handler) los35cRefuseUnsandboxedCapRun(capName string, profile SandboxProfile, projectBound bool) error {
	if capName == "" || profile != ProfileNone || !projectBound {
		return nil
	}
	return fmt.Errorf("cap %q is project-bound and would run unsandboxed (sandbox=none with no script profile); declare a profile for the script or use a user-scoped cap", capName)
}

// bashJobTimeout picks the timeout for one bash run. A script that declares its
// own runtime wins: the 20-second default killed a job that scans an hour of
// journal output on every run, and the "mentions claude/llm" heuristic only
// covers agent work.
func bashJobTimeout(command string, scriptTimeoutSec int64) int {
	if scriptTimeoutSec > 0 {
		return int(scriptTimeoutSec)
	}
	if strings.Contains(command, "claude ") || strings.Contains(command, "llm ") {
		return 600
	}
	return 20
}

func (h *Handler) storeBashRun(job *ScheduledJob, command, output string, err error, exitCode int) {
	status := "ok"
	errMsg := ""
	if err != nil {
		status = "error"
		// The run record is readable by whoever looks at the job, so a mask refusal
		// loses its host paths here (Befund 5, YM-334); the daemon log keeps them.
		errMsg = maskRefusalForCaller(err)
	}
	if status == "ok" && len(output) == 0 {
		return
	}
	if dbErr := h.store.SaveBashJobRun(storage.BashJobRun{
		JobID:      job.ID,
		JobName:    job.Name,
		CapName:    job.CapName,
		ScriptName: job.ScriptName,
		Command:    h.redact(command),
		Status:     status,
		ExitCode:   exitCode,
		Output:     h.redact(truncatStr(output, 10000)),
		ErrorMsg:   h.redact(errMsg),
	}); dbErr != nil {
		log.Printf("[scheduler] failed to save bash run for job %s: %v", job.ID, dbErr)
	}
}

// validatedHosts parses a stored allowlist and drops entries that are not bare
// hostnames. The create and cap paths validate on the way in; a row that predates
// them (or was edited in SQLite) is filtered here instead of reaching ai-jail.
func validatedHosts(stored string) []string {
	hosts := capfile.SplitHostList(stored)
	kept := hosts[:0]
	for _, host := range hosts {
		if err := capfile.ValidateHost(host); err != nil {
			log.Printf("[scheduler] dropping invalid allowed host %q: %v", host, err)
			continue
		}
		kept = append(kept, host)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func truncatStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	truncated := s[:max]
	for len(truncated) > 0 && !utf8.Valid([]byte(truncated)) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "..."
}

func resolveJobSandbox(param string, defaultProfile SandboxProfile) (SandboxProfile, error) {
	if param == "" {
		return defaultProfile, nil
	}
	return ParseSandboxProfile(param)
}

// jobSandboxProfile parses a profile stored in the database. A row carrying an
// unknown value must not quietly run without a sandbox, so it falls back to
// standard, the profile the column default already prescribes.
func jobSandboxProfile(stored string) SandboxProfile {
	if stored == "" {
		return ProfileStandard
	}
	profile, err := ParseSandboxProfile(stored)
	if err != nil {
		log.Printf("[scheduler] unknown sandbox profile %q, using standard: %v", stored, err)
		return ProfileStandard
	}
	return profile
}

// agentForwardPorts is the loopback relay a sandboxed agent run needs: the
// yesmem proxy the host already points claude/codex at. Empty when no port was
// resolved, which leaves the run without the relay (and without the LLM).
func (h *Handler) agentForwardPorts() []int {
	if h.proxyForwardPort <= 0 {
		return nil
	}
	return []int{h.proxyForwardPort}
}

func parsePorts(s string) []int {
	if s == "" {
		return []int{80, 443}
	}
	var ports []int
	for _, part := range strings.Split(s, ",") {
		p, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && p > 0 && p <= 65535 {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return []int{80, 443}
	}
	return ports
}

// isHomeOrRoot reports whether dir is the user's home or the filesystem root.
// Neither may become an agent's or a run's working directory: ai-jail 2.x mounts
// the cwd read-write and replaces its private home with a write bind of the real
// one when they coincide, and an allow-all permissions file written there would
// apply to every session of the user. Symlinks are resolved on both sides — a
// home that is itself reached through a link is still the home (YM-286).
func isHomeOrRoot(dir string) bool {
	if dir == "" {
		return true
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}
	if canonical == "/" {
		return true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Without knowing the home there is no way to tell whether dir is it.
		return true
	}
	if resolved, err := filepath.EvalSymlinks(homeDir); err == nil {
		homeDir = resolved
	}
	return canonical == homeDir
}

// jobProjectUsable reports whether dir can serve as a job's working directory:
// an existing directory that is neither the root nor the user's home.
func jobProjectUsable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return !isHomeOrRoot(dir)
}

// jobKey is the identity a job's directories are keyed on: its ID, or its name for
// a row that carries none. Creation (ensureJobDir), the run work dir and the
// sweep's live set all have to derive the same element from a job, or the sweep
// treats a live job's directory as an orphan and removes it (YM-300). A job with
// neither has no key; the callers decide what that means for them.
func jobKey(id, name string) string {
	if id != "" {
		return id
	}
	return name
}

// jobWorkDir is the working directory a scheduled job's runs get: the project the
// job was created from (YM-281), which schedule create only stores when it is
// absolute. A job without a usable project gets an empty directory of its own
// inside the runtime dir.
//
// Naming the daemon's own checkout "$HOME/projects/<its basename>" invented a
// second yesmem beside the real one, created it, and ran every such job there
// (YM-286).
func (h *Handler) jobWorkDir(job *ScheduledJob) string {
	if job != nil && filepath.IsAbs(job.Project) {
		dir := filepath.Clean(job.Project)
		if jobProjectUsable(dir) {
			return h.resolveAgentWorkDir(dir, "")
		}
		log.Printf("[scheduler] job %s: project %q is no usable working directory, using the run work dir", job.ID, job.Project)
	}
	key := ""
	if job != nil {
		key = jobKey(job.ID, job.Name)
	}
	dir, err := EnsureRunWorkDir(h.dataDir, key)
	if err != nil {
		log.Printf("[scheduler] no working directory for job %q: %v", key, err)
		return ""
	}
	return dir
}

// ensureJobDir creates the job's persistent log directory. Caps write their logs
// there (via YESMEM_JOB_DIR) instead of /tmp, which the sandbox replaces with a
// throwaway tmpfs (YM-218). The directory is keyed on the job ID, not the name, so
// two jobs sharing a name do not overwrite each other's state file by accident.
// Same-uid runs are not isolated from each other here: a sandboxed job can read
// the other job dirs. It sits in the runtime dir beside the data dir, because the
// sandbox masks the data dir as a whole (YM-276): a mounted path below the mask
// would put that part of the data dir straight back into the sandbox.
func (h *Handler) ensureJobDir(job *ScheduledJob) (string, error) {
	runtimeDir, err := EnsureRuntimeDir(h.dataDir)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(runtimeDir, ':') {
		// ai-jail reads --rw-map as SOURCE:DEST and drops the mount without a word.
		// The job key cannot introduce a ':' (the sanitizer replaces it), so the
		// runtime dir is the only source.
		return "", fmt.Errorf("runtime dir %q contains ':', ai-jail would parse --rw-map as SOURCE:DEST", runtimeDir)
	}
	key := jobKey(job.ID, job.Name)
	dir := filepath.Join(runtimeDir, "jobs", jobDirName(key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// jobNameToken maps a job name onto a single safe token. The name is user input
// and ends up in a scratchpad section key, in log lines and in the job ID, so it
// must not carry a line break (forges log lines), a separator, or another agent's
// section id (lets a job write into foreign state).
//
// The mapping is lossy ("a b", "a/b" and "a_b" all read as "a_b"), so a name the
// mapper had to change gets a hash of the original appended — the same rule
// jobDirName applies, and for the same reason: two jobs sharing a section would
// overwrite each other's result, and executeAgent stops the agent it finds under
// that section, which would then be the other job's. Names that already are
// tokens map to themselves unchanged, so existing jobs keep their section and no
// registered agent is orphaned by the upgrade.
func jobNameToken(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, name)
	safe = strings.Trim(safe, ".")
	if len(safe) > 64 {
		safe = safe[:64]
	}
	if safe == "" {
		safe = "unnamed"
	}
	if safe != name {
		sum := sha256.Sum256([]byte(name))
		return fmt.Sprintf("%s-%x", safe, sum[:4])
	}
	return safe
}

// jobSectionName is the scratchpad section a run of this job writes to.
func jobSectionName(name string) string {
	return "sched-" + jobNameToken(name)
}

// jobDirName turns a job key into a single, non-hidden path element. The key is
// user input (the job ID carries the name), so it must not carry a separator or a
// parent reference into the path.
func jobDirName(key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, key)
	safe = strings.Trim(safe, ".")
	if len(safe) > 64 {
		// A path element is bounded at 255 bytes; the hash below adds nine more.
		safe = safe[:64]
	}
	if safe == "" {
		safe = "unnamed"
	}
	// The sanitizer is not injective ("sched-a/b" and "sched-a_b" collapse onto the
	// same name), so a hash of the raw key keeps the directories apart.
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s-%x", safe, sum[:4])
}

// removeJobDir drops a job's log directory. A deleted job and a finished one-shot
// have no use for it, and nothing else cleans the directory up.
//
// The run work dir is deliberately NOT removed here: a job started by hand
// (`schedule run`) never enters the scheduler's running set, so a delete racing it
// would RemoveAll the cwd of a live process. An empty 0700 directory inside the
// runtime dir is the cheaper price (YM-286).
func (h *Handler) removeJobDir(jobID string) {
	if h.dataDir == "" || jobID == "" {
		return
	}
	dir := filepath.Join(RuntimeDir(h.dataDir), "jobs", jobDirName(jobID))
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("[scheduler] remove job dir %s: %v", dir, err)
	}
}

func jobDirMounts(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{dir}
}

func jobDirEnv(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{"YESMEM_JOB_DIR=" + dir}
}
