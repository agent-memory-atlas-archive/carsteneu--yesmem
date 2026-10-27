package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Auto-correct rate limiting (T4): bash-job auto-correct is rate-limited so
// a cap with a persistent bug can't burn through the LLM budget. Two gates,
// checked in priority order:
//
//  1. per-cap cooldown — once an attempt has fired for a cap, further attempts
//     on that same cap are skipped until cooldown expires.
//  2. per-tick budget — at most autoCorrectMaxPerTick attempts per call to
//     processBashJobErrors, even across different caps.
//
// Both gates emit a broadcast containing the word "cooldown" so external
// listeners can react uniformly.
const (
	autoCorrectCooldownDuration     = 5 * time.Minute
	autoCorrectMaxPerTick           = 1
	autoCorrectMaxGenerationsPerDay = 3
	autoCorrectGenerationWindow     = 24 * time.Hour
	autoCorrectDefaultTestTimeout   = 10
)

// bashErrorNoClientGrace is how long a fresh daemon leaves its failed runs queued
// while the model client is still being wired. The wiring happens in an async
// startup goroutine, so a run that failed during startup would otherwise be
// drained without ever being looked at. Once the window is over, the pass reports
// undiagnosed instead of keeping the queue blocked — which also gives up the
// repair for that backlog, so the window is generous.
const bashErrorNoClientGrace = 5 * time.Minute

// autoCorrectTestRunTimeout returns the timeout (in seconds) used for the
// auto-correct test-run as a function of the job's IntervalSeconds. Sub-minute
// intervals get a tight timeout matching the interval so a fast-poll job is
// never tested longer than its own cadence; everything else (zero, negative,
// or >= 60) falls back to the default so a long-cron job does not produce a
// minute-long test-run.
func autoCorrectTestRunTimeout(intervalSec int) int {
	if intervalSec > 0 && intervalSec < 60 {
		return intervalSec
	}
	return autoCorrectDefaultTestTimeout
}

// runBashErrorPass is the heartbeat entry point for the failed bash-job pass.
// The pass asks a model and may run a corrected script, so it must not hold the
// one-second ticker: relay delivery, crash recovery and the agent guards share
// it. One pass at a time — a tick that arrives while the previous one is still
// in flight is a no-op, not a second pass over the same rows.
func (h *Handler) runBashErrorPass(ctx context.Context) {
	h.bashErrorsMu.Lock()
	if h.bashErrorsRunning {
		h.bashErrorsMu.Unlock()
		return
	}
	h.bashErrorsRunning = true
	h.bashErrorsMu.Unlock()

	go func() {
		defer func() {
			h.bashErrorsMu.Lock()
			h.bashErrorsRunning = false
			h.bashErrorsMu.Unlock()
		}()
		h.processBashJobErrors(ctx)
	}()
}

// autoCorrectVerifyTimeout picks the timeout for the verification run. It mirrors
// fireJobBash: a script that declares its own runtime gets it here too. Using
// only the interval heuristic rejected exactly the long-running scripts the
// declaration exists for, so their fixes were never persisted.
func autoCorrectVerifyTimeout(intervalSec int, declaredTimeoutSec int64) int {
	if declaredTimeoutSec > 0 {
		return int(declaredTimeoutSec)
	}
	return autoCorrectTestRunTimeout(intervalSec)
}

func (h *Handler) processBashJobErrors(ctx context.Context) {
	// The client is wired by the daemon's async startup goroutine, after the
	// heartbeat is already ticking, so a run that failed during startup gets one
	// grace window to be looked at properly. Past that window the client is not
	// coming (no key configured, provider unavailable) and holding the rows back
	// is worse than an undiagnosed report: the batch takes the five OLDEST
	// failures, so five undrainable rows hid every newer failure for good.
	client := h.autoCorrectLLM()
	if client == nil && time.Since(h.startedAt) < bashErrorNoClientGrace {
		return
	}

	const bashErrorBatchSize = 5

	errors, err := h.store.GetUnprocessedBashErrors(bashErrorBatchSize)
	if err != nil || len(errors) == 0 {
		return
	}

	perTickCount := 0

	for _, run := range errors {
		if ctx.Err() != nil {
			// The daemon is shutting down: a fix run or a model call started now
			// would outlive it. Leaving the row queued is correct, the next start
			// picks it up. This is the whole reach of the binding — the LLMClient
			// interface takes no context, so a call already in flight runs to its
			// own timeout; the pass only refuses to START further work.
			return
		}
		job := h.findScheduledJob(run.JobID)
		autoCorrect := job != nil && job.AutoCorrect
		project := bashErrorProject(job)
		ports := "80,443"
		if job != nil && job.AllowedPorts != "" {
			ports = job.AllowedPorts
		}

		switch {
		case project == "":
			// No address: a broadcast would reach every checkout sharing the
			// basename, and an auto-correct would rewrite a live cap body with no
			// user-facing trace at all. Neither is acceptable, so the run is left
			// unnotified and unmodified; this log line is the notification (YM-286).
			log.Printf("[bash-errors] job %s (%s): no project recorded, run left without notification", run.JobID, run.JobName)
		case run.Command == "":
			// No command was recorded, so no script ran: the run failed before it
			// started (a vanished work dir, an unresolvable cap, a missing
			// sandbox). Asking a model to repair a script that never executed would
			// rewrite a live cap body over an infrastructure error, but the failure
			// still has to reach the operator.
			h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED before it ran: %s", run.JobName, run.ErrorMsg))
		case client == nil:
			// No model to ask (none configured, or the grace window is over). The
			// failure still reaches the operator, undiagnosed, and the row leaves
			// the queue — re-reading it every five seconds forever would hide every
			// newer failure behind the same five rows.
			h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED (exit %d): %s\nNo model client: the failure was not diagnosed.", run.JobName, run.ExitCode, strings.ReplaceAll(run.ErrorMsg, "\n", " ")))
		case autoCorrect && run.CapName != "":
			if proceed, reason := h.tryStartAutoCorrect(run.CapName, perTickCount); proceed {
				perTickCount++
				h.autoCorrectBashCap(run, job, ports)
				h.finishAutoCorrect()
			} else {
				h.broadcastBashError(project, fmt.Sprintf(
					"[bash-job] %q FAILED, auto-correct skipped (%s)\nError: %s",
					run.JobName, reason, run.ErrorMsg))
			}
		default:
			h.diagnoseBashError(run, project)
		}

		// Every branch marks the run processed. A pass that could not reach a
		// model has already broadcast its outcome; leaving the row in the queue
		// only re-ran the same failing call every five seconds, forever.
		if err := h.store.MarkBashJobRunProcessed(run.ID); err != nil {
			log.Printf("[bash-errors] failed to mark run %d processed: %v", run.ID, err)
		}
	}
}

// tryStartAutoCorrect atomically checks rate-limit gates and reserves the
// auto-correct slot for the named cap. On (true, "") the caller MUST run
// autoCorrectBashCap and then call finishAutoCorrect. On (false, reason)
// the caller MUST broadcast a "cooldown" skip message; the run is still
// marked processed by the surrounding loop.
func (h *Handler) tryStartAutoCorrect(capName string, perTickCount int) (proceed bool, reason string) {
	h.autoCorrectMu.Lock()
	defer h.autoCorrectMu.Unlock()

	now := time.Now()
	if until, ok := h.autoCorrectCooldown[capName]; ok && until.After(now) {
		return false, fmt.Sprintf("cooldown until %s", until.Format(time.RFC3339))
	}
	if gens, err := h.store.CountAutoCorrectGenerations(capName, now.Add(-autoCorrectGenerationWindow)); err == nil && gens >= autoCorrectMaxGenerationsPerDay {
		return false, fmt.Sprintf("cooldown: generation limit %d/24h reached", autoCorrectMaxGenerationsPerDay)
	}
	if perTickCount >= autoCorrectMaxPerTick {
		return false, fmt.Sprintf("cooldown: per-tick budget %d exhausted", autoCorrectMaxPerTick)
	}
	if h.autoCorrectRunning {
		return false, "cooldown: another auto-correct in flight"
	}
	h.autoCorrectRunning = true
	h.autoCorrectCooldown[capName] = now.Add(autoCorrectCooldownDuration)
	return true, ""
}

// finishAutoCorrect releases the cross-tick semaphore reserved by
// tryStartAutoCorrect. The per-cap cooldown timestamp set during reservation
// is intentionally NOT cleared here — that is what rate-limits the next tick.
func (h *Handler) finishAutoCorrect() {
	h.autoCorrectMu.Lock()
	h.autoCorrectRunning = false
	h.autoCorrectMu.Unlock()
}

// diagnoseSystemPrompt asks for the short diagnosis the pass broadcasts.
const diagnoseSystemPrompt = "You diagnose a failing scheduled bash job. Answer in at most three sentences."

// diagnoseBashError asks the model for a short diagnosis and broadcasts it. A
// failing call still broadcasts the run, so the failure is visible; the caller
// marks the run processed either way. Without a wired client this function is
// not reached — the pass leaves its rows queued (see processBashJobErrors).
//
// project is the address the outcome is announced to; see bashErrorProject.
func (h *Handler) diagnoseBashError(run storage.BashJobRun, project string) {
	client := h.autoCorrectLLM()
	if client == nil {
		log.Printf("[bash-errors] diagnosis for %s skipped: no LLM client", run.JobID)
		return
	}

	prompt := fmt.Sprintf(
		"A scheduled bash job %q failed.\n\nCap: %s\nCommand: %s\nExit: %d\nError: %s\nOutput:\n%s",
		run.JobName, run.CapName, h.redact(run.Command), run.ExitCode, h.redact(run.ErrorMsg), h.redactThenTruncate(run.Output, 2000))

	diagOut, err := client.Complete(diagnoseSystemPrompt, prompt)
	if err != nil {
		log.Printf("[bash-errors] diagnosis for %s failed: %v", run.JobID, err)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED (exit %d): %s", run.JobName, run.ExitCode, run.ErrorMsg))
		return
	}

	h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED (exit %d)\nDiagnosis: %s", run.JobName, run.ExitCode, truncatStr(strings.TrimSpace(diagOut), 1000)))
}

// redactThenTruncate redacts before cutting. The other order can cut a secret in
// half, and the fragment that survives no longer matches the redactor.
func (h *Handler) redactThenTruncate(s string, max int) string {
	return truncatStr(h.redact(s), max)
}

// autoCorrectSandboxConfig assembles the sandbox the verification run uses. It
// is deliberately the job path's own builder: the corrected script has to see the
// same job dir and the same YESMEM_JOB_DIR as the run it is meant to fix. Without
// them a fix that needs the dir fails verification, and one that only passes
// without it (writing its log into the sandbox's throwaway /tmp) verifies while
// still writing nowhere in production (YM-267).
func (h *Handler) autoCorrectSandboxConfig(run storage.BashJobRun, job *ScheduledJob, allowedPorts string, allowedHosts []string, profile SandboxProfile) SandboxConfig {
	if job == nil {
		// The job record may be gone by the time its failed run is corrected; the
		// dir is keyed on the ID, and the ID travels in the run.
		job = h.findScheduledJob(run.JobID)
	}
	if job == nil {
		job = &ScheduledJob{ID: run.JobID, Name: run.JobName, AllowedPorts: allowedPorts}
	}
	return h.bashSandboxConfig(job, allowedHosts, profile)
}

func (h *Handler) autoCorrectBashCap(run storage.BashJobRun, job *ScheduledJob, allowedPorts string) {
	project := bashErrorProject(job)
	// The row to repair is the row the failed run took, so it is resolved the
	// same way the run resolves it: the job's project first, then a global row.
	// The old name-only scan repaired whichever row came first, which for a
	// shadowed name was another project's body — the model's output would then be
	// saved into a cap the failing job never ran (YM-417).
	row, meta, err := h.capRowFor(run.CapName, project)
	if err != nil {
		h.diagnoseBashError(run, project)
		return
	}
	sc, ok := findBashScriptMeta(&meta, run.ScriptName)
	if !ok {
		h.diagnoseBashError(run, project)
		return
	}

	currentHandler := sc.Body
	allowedHosts := sc.AllowedHosts
	activeCap := &row
	activeMeta := meta
	activeScriptTimeout := sc.Timeout

	// Cheapest refusal first: without a client there is no model to ask, and
	// building (or downloading) the sandbox for it would be wasted work.
	client := h.autoCorrectLLM()
	if client == nil {
		log.Printf("[bash-errors] auto-correct for %s skipped: no LLM client", run.CapName)
		return
	}

	req := h.autoCorrectPrompt(run, sc, &meta)

	fixOut, err := client.Complete(fixSystemPrompt, req)
	if err != nil {
		log.Printf("[bash-errors] auto-correct for %s failed: %v", run.CapName, err)
		h.diagnoseBashError(run, project)
		return
	}

	// The reply names a block of the script to replace, because the prompt never
	// carried the script. Anything else is an abort with its reason: writing a
	// body the model invented over a live cap is the failure mode this pass
	// exists to avoid.
	patch, patchErr := parseFixPatch(fixOut)
	if patchErr != nil {
		log.Printf("[bash-errors] auto-correct for %s: unusable answer: %v", run.CapName, patchErr)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct aborted: %v\nError: %s", run.JobName, patchErr, run.ErrorMsg))
		return
	}
	if patch.unfixable != "" {
		log.Printf("[bash-errors] auto-correct for %s declined: %q", run.CapName, truncatStr(patch.unfixable, 500))
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct not possible\nError: %s\nAnswer: %s", run.JobName, run.ErrorMsg, truncatStr(patch.unfixable, 500)))
		return
	}
	fixed, applyErr := applyFixPatch(currentHandler, patch)
	if applyErr != nil {
		log.Printf("[bash-errors] auto-correct for %s: patch does not fit the script: %v", run.CapName, applyErr)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct aborted: %v\nError: %s", run.JobName, applyErr, run.ErrorMsg))
		return
	}

	// The diff class decides whether the model's body may run at all. A
	// substantial change is staged for review and never executed here: the
	// verification run gets the store()/llm() adapter prelude, so running it
	// would execute unreviewed model code (cap_store.delete included) before the
	// operator ever sees the proposal.
	diff := ClassifyCapDiff(currentHandler, fixed)
	if diff != DiffMinimal {
		proposalID, pErr := h.persistProposalForReview(run, activeCap, fixed)
		if pErr != nil {
			log.Printf("[bash-errors] proposal-persist for %s failed: %v", run.CapName, pErr)
			h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q AUTO-CORRECT proposal save failed: %v", run.JobName, pErr))
			return
		}
		preview := truncatStr(unifiedDiffPreview(currentHandler, fixed), 1500)
		h.broadcastBashError(project, fmt.Sprintf(
			"[bash-job] %q AUTO-CORRECT PROPOSAL #%d for cap %q (substantial change). Active cap unchanged.\nOriginal error: %s\nDecide via cap_proposal_decide(id=%d, decision=accept|reject).\n\nDIFF:\n%s",
			run.JobName, proposalID, run.CapName, run.ErrorMsg, proposalID, preview,
		))
		return
	}

	// Production: verify the fix inside the job's sandbox, on the body the job
	// would actually run. prepareBashCommand adds the store()/llm() adapter
	// prelude, and the job runs from its own work dir — without both, a fix that
	// only works (or only fails) with the adapter passed verification either way.
	// Only a minimal diff reaches this point, and a minimal diff may not introduce
	// a command token (ClassifyCapDiff), so the run is bounded to the command set
	// the operator already accepted.
	// The profile comes first: it decides whether a job dir is reachable at all
	// (lockdown drops every mount), and the verification run has to see the same
	// dir the failing run saw.
	profile := ProfileStandard
	intervalSec := 0
	if job != nil {
		profile = job.Sandbox
		intervalSec = job.IntervalSeconds
	}

	verify := h.autoCorrectVerify
	if verify == nil {
		sb := h.newSandbox(h.autoCorrectSandboxConfig(run, job, allowedPorts, allowedHosts, profile))
		if !sb.Available() {
			h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct SKIPPED: sandbox unavailable (ai-jail missing)\nError: %s", run.JobName, run.ErrorMsg))
			return
		}
		verify = func(body string, timeoutSec int, profile SandboxProfile) (int, error) {
			_, exitCode, err := sb.RunWithProfile(body, timeoutSec, profile)
			return exitCode, err
		}
	}

	testExit, testErr := verify(prepareBashCommand(fixed), autoCorrectVerifyTimeout(intervalSec, activeScriptTimeout), profile)

	if runNeverStarted(testErr) {
		// The verification run never started: a missing sandbox, a mask that could
		// not be applied, a work dir that covers the home, or a work dir that is
		// missing is infrastructure, not the script's fault. Calling it "auto-correct
		// unsuccessful" would throw away a fix that was never tried.
		log.Printf("[bash-errors] %s auto-correct could not verify: %v", run.CapName, testErr)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct SKIPPED: the sandbox refused the verification run\nError: %s", run.JobName, maskRefusalForCaller(testErr)))
		return
	}

	if testErr != nil || testExit != 0 {
		log.Printf("[bash-errors] corrected script for %s also failed: %v (exit=%d)", run.CapName, testErr, testExit)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, auto-correct unsuccessful\nError: %s\nSuggested fix:\n%s", run.JobName, run.ErrorMsg, truncatStr(fixed, 500)))
		return
	}

	params, pErr := autoCorrectSaveParams(activeCap, activeMeta, run.ScriptName, fixed)
	if pErr != nil {
		log.Printf("[bash-errors] minimal-fix params for %s failed: %v", run.CapName, pErr)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q AUTO-CORRECT not persisted: %v", run.JobName, pErr))
		return
	}
	resp := h.Handle(Request{Method: "save_cap", Params: params})
	if resp.Error != "" {
		// save_cap refuses for real reasons (sandbox lock on a project-bound
		// cap, a supersede lookup that found nothing, a DB error). Claiming
		// success here left the broken script live with the user told otherwise.
		log.Printf("[bash-errors] minimal-fix save for %s failed: %s", run.CapName, resp.Error)
		h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q AUTO-CORRECT not persisted: %s", run.JobName, resp.Error))
		return
	}
	h.broadcastBashError(project, fmt.Sprintf("[bash-job] %q FAILED, AUTO-CORRECTED (minimal).\nOriginal error: %s\nCap %q handler_bash updated and tested.", run.JobName, run.ErrorMsg, run.CapName))
}

// persistProposalForReview clones the active cap's CapMeta, swaps the bash
// script body to the proposed fix, and writes a NEW learnings row with
// category='cap_proposed' tagged 'pending_approval'. The active cap row
// is left untouched. Caller decides the final action via cap_proposal_decide.
//
// The caller passes the already-resolved active cap so we don't reload it
// from storage. If active is nil the call fails fast.
func (h *Handler) persistProposalForReview(run storage.BashJobRun, active *models.Learning, fixed string) (int64, error) {
	if active == nil {
		return 0, fmt.Errorf("no active cap %q to clone for proposal", run.CapName)
	}
	meta, err := ParseCapMeta(active.Context)
	if err != nil {
		return 0, fmt.Errorf("parse active CapMeta: %w", err)
	}
	swapped := false
	for i := range meta.Scripts {
		if meta.Scripts[i].Runtime != "bash" {
			continue
		}
		if run.ScriptName != "" && meta.Scripts[i].Name != run.ScriptName {
			continue
		}
		meta.Scripts[i].Body = fixed
		swapped = true
		break
	}
	if !swapped {
		return 0, fmt.Errorf("active cap %q has no bash script %q to replace", run.CapName, run.ScriptName)
	}
	ctxJSON, err := meta.ToJSON()
	if err != nil {
		return 0, fmt.Errorf("render proposal CapMeta: %w", err)
	}
	keywords := appendUniqueTag(active.Keywords, "pending_approval")
	if run.ScriptName != "" {
		keywords = appendUniqueTag(keywords, "script:"+run.ScriptName)
	}
	content := fmt.Sprintf("%s [auto-correct proposal from bash_job_runs.id=%d]", active.Content, run.ID)
	l := &models.Learning{
		Content:            content,
		Category:           "cap_proposed",
		Source:             "auto_correct_proposal",
		Project:            active.Project,
		Confidence:         1.0,
		Context:            ctxJSON,
		Domain:             active.Domain,
		TriggerRule:        "cap_proposed:" + run.CapName,
		Keywords:           keywords,
		AnticipatedQueries: []string{run.CapName, "proposal", "pending_approval", "cap_proposed"},
		CreatedAt:          time.Now(),
	}
	return h.store.InsertLearning(l)
}

// autoCorrectSaveParams builds the save_cap request for an in-place minimal fix.
//
// A bare save_cap(name, handler_bash) would replace the whole script set with a
// single script and blank the description and the tags on the way, because
// save_cap reads the legacy handler params as a full replacement. Without the
// cap's project the supersede lookup also misses the row, so the save writes a
// second, global cap and leaves the broken script in place. Sending the complete
// script array plus the cap's own metadata changes exactly one body.
func autoCorrectSaveParams(active *models.Learning, meta CapMeta, scriptName, fixed string) (map[string]any, error) {
	scripts := make([]ScriptMeta, len(meta.Scripts))
	copy(scripts, meta.Scripts)

	// Select the target the same way the caller did (findBashScriptMeta prefers a
	// handler when no name is given). Taking the first bash script instead could
	// write the fix into a different script than the one that failed.
	target, ok := findBashScriptMeta(&meta, scriptName)
	if !ok {
		return nil, fmt.Errorf("cap %q has no bash script %q to replace", meta.Name, scriptName)
	}
	swapped := false
	for i := range scripts {
		if scripts[i].Name == target.Name {
			scripts[i].Body = fixed
			swapped = true
			break
		}
	}
	if !swapped {
		return nil, fmt.Errorf("cap %q has no bash script %q to replace", meta.Name, scriptName)
	}
	scriptsJSON, err := json.Marshal(scripts)
	if err != nil {
		return nil, fmt.Errorf("render scripts: %w", err)
	}

	params := map[string]any{
		"name":      meta.Name,
		"scripts":   string(scriptsJSON),
		"tested":    true,
		"test_date": time.Now().Format(time.RFC3339),
		"source":    "auto_correct_accepted",
	}
	if active != nil {
		if active.Project != "" {
			params["project"] = active.Project
		}
	}
	if meta.Description != "" {
		params["description"] = meta.Description
	}
	if len(meta.Tags) > 0 {
		// parseTagsParam reads []any; a []string would be dropped silently.
		tags := make([]any, len(meta.Tags))
		for i, tag := range meta.Tags {
			tags[i] = tag
		}
		params["tags"] = tags
	}
	if len(meta.Actions) > 0 {
		// save_cap only takes actions from the params; omitting them emptied the
		// cap's actions on every auto-corrected fix.
		if actionsJSON, aErr := json.Marshal(meta.Actions); aErr == nil {
			params["actions"] = string(actionsJSON)
		}
	}
	if len(meta.Requires) > 0 {
		// Same class as actions: a declared list wins over detection, and omitting
		// it replaced the cap's declaration with whatever a scan of the repaired
		// body found.
		params["requires"] = strings.Join(meta.Requires, ",")
	}
	params["auto_active"] = meta.AutoActive
	if meta.Scope != "" {
		params["scope"] = meta.Scope
	}
	return params, nil
}

func appendUniqueTag(existing []string, tag string) []string {
	for _, t := range existing {
		if t == tag {
			return existing
		}
	}
	return append(existing, tag)
}

// unifiedDiffPreview renders a small unified-diff-style preview suitable
// for embedding in a broadcast notification. Lines that match exactly are
// shown as context, divergent lines as -old / +new pairs.
func unifiedDiffPreview(oldBody, newBody string) string {
	oldLines := strings.Split(strings.TrimRight(oldBody, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(newBody, "\n"), "\n")
	var b strings.Builder
	b.WriteString("--- before\n+++ after\n")
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}
	for i := 0; i < maxLen; i++ {
		var oldLine, newLine string
		if i < len(oldLines) {
			oldLine = oldLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}
		if i < len(oldLines) && i < len(newLines) && oldLine == newLine {
			fmt.Fprintf(&b, " %s\n", oldLine)
			continue
		}
		if i < len(oldLines) {
			fmt.Fprintf(&b, "-%s\n", oldLine)
		}
		if i < len(newLines) {
			fmt.Fprintf(&b, "+%s\n", newLine)
		}
	}
	return b.String()
}

// bashErrorProject is the address a failing run's outcome is announced to: the
// absolute project recorded on the job (YM-281). A bare basename never is — the
// broadcast reader matches the basename of every project too, so an address of
// "yesmem" reached every checkout of that name (YM-286).
//
// Empty when the job is gone or was created before jobs carried their project.
// The caller then has no address at all; see broadcastBashError.
func bashErrorProject(job *ScheduledJob) string {
	if job != nil && filepath.IsAbs(job.Project) {
		return job.Project
	}
	return ""
}

func (h *Handler) broadcastBashError(project, msg string) {
	log.Printf("[bash-errors] broadcast: %s", truncatStr(msg, 200))
	if project == "" {
		// A job without a recorded project has no address. Falling back to a
		// basename sends the message to the wrong projects, and an empty project
		// is unreadable for every session, so no row is written: the log line
		// above is the whole notification.
		log.Printf("[bash-errors] no project for this run, broadcast not addressable")
		return
	}
	_, _ = h.store.SendBroadcast("scheduler", project, msg)
}

func (h *Handler) findScheduledJob(jobID string) *ScheduledJob {
	if h.scheduler == nil {
		return nil
	}
	for _, j := range h.scheduler.ListJobs() {
		if j.ID == jobID {
			return &j
		}
	}
	return nil
}
