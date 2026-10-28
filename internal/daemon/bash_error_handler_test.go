package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/sanitize"
	"github.com/carsteneu/yesmem/internal/storage"
)

// disableSandbox forces NewSandbox(...).Available() to return false by removing
// ai-jail from PATH and short-circuiting the network-download fallback. Used
// to assert that auto-correct fails closed when the sandbox isn't available.
func disableSandbox(t *testing.T) {
	t.Helper()
	isolateHomeAt(t, t.TempDir())
	t.Setenv("YESMEM_DISABLE_AIJAIL_DOWNLOAD", "1")
}

// When a bash error has no matching scheduled job (or AutoCorrect=false) and
// CapName is empty, processBashJobErrors falls into diagnoseBashError. When the
// model call fails, the run must still be marked processed: retrying the same
// failing call every five seconds forever was the defect (YM-167), not a retry
// contract. The failure is broadcast instead.
func TestProcessBashErrors_DiagnoseFailureMarksProcessed(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{ID: "diag-only", Name: "broken-script", Project: "/srv/los32d/diag", IntervalSeconds: 300}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	h.SetAutoCorrectClient(&autoCorrectStubClient{err: errAutoCorrectStub})

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "diag-only",
		JobName:  "broken-script",
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		Output:   "boom",
		ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("a diagnosis that could not run must not be retried forever; got %d unprocessed", len(runs))
	}

	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/diag")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "broken-script") && strings.Contains(m.Content, "exit status 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failed diagnosis must still be reported; got %d broadcasts", len(msgs))
	}
}

// Without a wired client the pass must not drain its rows: the client arrives
// from the daemon's async startup goroutine, and a run that failed during startup
// would otherwise be marked processed without ever being looked at. The window is
// time-bound (bashErrorNoClientGrace), so the test pins the start time instead of
// relying on the wall clock.
func TestProcessBashErrors_WithoutClientLeavesRunsQueued(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.startedAt = time.Now()

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "queued",
		JobName:  "queued-script",
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("a run must stay queued while there is no client to diagnose it; got %d unprocessed", len(runs))
	}
}

// When a scheduled job has AutoCorrect=true and a CapName, processBashJobErrors
// takes the auto-correct branch which always marks the run processed — even if the
// cap can't be resolved or the LLM call fails. The user-visible contract: a row
// only re-appears in GetUnprocessedBashErrors if the diagnose-only fallback ran.
func TestProcessBashErrors_AutoCorrectMarksProcessedEvenWhenCapMissing(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: "diagnosis text"})

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "ac-job",
		Name:        "auto-correct-test",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "never-existed",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "ac-job",
		JobName:  "auto-correct-test",
		CapName:  "never-existed",
		Command:  "exit 7",
		Status:   "error",
		ExitCode: 7,
		Output:   "fail",
		ErrorMsg: "exit status 7",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("auto-correct branch should always mark processed; got %d unprocessed", len(runs))
	}
}

// Empty queue must be a no-op, not a panic, since the heartbeat calls this every
// tick whether or not there is work.
func TestProcessBashErrors_NoErrorsIsNoop(t *testing.T) {
	h, _ := mustHandler(t)
	h.processBashJobErrors(context.Background())
}

// findScheduledJob is called from processBashJobErrors before any scheduler is
// guaranteed to be attached (daemon bootstrap order). It must return nil rather
// than panic when h.scheduler is nil.
func TestFindScheduledJob_NilScheduler(t *testing.T) {
	h, _ := mustHandler(t)
	if got := h.findScheduledJob("anything"); got != nil {
		t.Errorf("expected nil from findScheduledJob with no scheduler, got %+v", got)
	}
}

// findScheduledJob returns the matching job by ID and nil for unknown IDs. This
// is the lookup that drives whether auto-correct fires for a given run.
func TestFindScheduledJob_Match(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "find-me",
		Name:        "x",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	got := h.findScheduledJob("find-me")
	if got == nil || got.ID != "find-me" || !got.AutoCorrect {
		t.Errorf("findScheduledJob(\"find-me\") = %+v", got)
	}
	if h.findScheduledJob("missing") != nil {
		t.Error("missing job should be nil")
	}
}

// minimalOriginal/minimalFixed differ by one word, so ClassifyCapDiff returns
// DiffMinimal and the fix reaches the verification path.
const (
	minimalOriginal = "set -eu; echo processing one; exit 0"
	minimalFixed    = "set -eu; echo processed one; exit 0"
)

// When the sandbox is unavailable (ai-jail missing AND download disabled), the
// auto-correct loop must NOT persist the LLM-proposed body. The test-run is
// supposed to verify the fix inside ai-jail; falling back to direct shell
// execution would run unreviewed LLM output on the host. This test fails
// against today's code (FallbackUnsandboxed=true) and is the regression guard
// for T0 of the auto-correct hardening plan.
func TestAutoCorrect_DoesNotPersistWhenSandboxUnavailable(t *testing.T) {
	disableSandbox(t)
	neutralizeClaude(t)

	h, s := mustHandler(t)
	// A minimal diff, so the run reaches the sandbox check instead of being
	// staged as a proposal.
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("echo processing one", "echo processed one")})
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j1",
		Name:        "broken",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "broken_cap",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	original := minimalOriginal
	saveResp := h.Handle(Request{
		Method: "save_cap",
		Params: map[string]any{
			"name":         "broken_cap",
			"description":  "intentionally failing cap for T0 regression",
			"handler_bash": original,
		},
	})
	if saveResp.Error != "" {
		t.Fatalf("save_cap: %v", saveResp.Error)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "j1",
		JobName:  "broken",
		CapName:  "broken_cap",
		Command:  original,
		Status:   "error",
		ExitCode: 1,
		ErrorMsg: "exit 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	if len(caps) == 0 {
		t.Fatalf("no caps in store after save_cap")
	}
	var found bool
	for _, l := range caps {
		if !strings.Contains(l.Context, "broken_cap") {
			continue
		}
		found = true
		if !strings.Contains(l.Context, original) {
			t.Errorf("active cap lost original body %q; context:\n%s", original, l.Context)
		}
		if strings.Contains(l.Context, "echo processed") {
			t.Errorf("active cap was overwritten with LLM-proposed body despite sandbox unavailable; context:\n%s", l.Context)
		}
	}
	if !found {
		t.Fatalf("cap %q not found in active learnings", "broken_cap")
	}
}

// TestAutoCorrect_MinimalDiffPersistsDirectly asserts that a small fix
// (≤5 new lines, ≥0.85 token similarity) is persisted in place via save_cap
// with source='auto_correct_accepted', and that the stored cap is a new version
// rather than a second row.
func TestAutoCorrect_MinimalDiffPersistsDirectly(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	fixed := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processed $f\ndone\nexit 0\n"
	if got := ClassifyCapDiff(body, fixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-min",
		Name:        "min-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "minimal_cap",
		ScriptName:  "run",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	scripts := `[{"name":"run","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(body) + `}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "minimal_cap", "description": "minimal", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-min", JobName: "min-job", CapName: "minimal_cap", ScriptName: "run",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	caps, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	var found bool
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "minimal_cap" {
			continue
		}
		found = true
		if l.Source != "auto_correct_accepted" {
			t.Errorf("the in-place fix must be recorded as an accepted auto-correct, source=%q", l.Source)
		}
		if m.Version < 2 {
			t.Errorf("the fix must supersede the cap as a new version, got version %d", m.Version)
		}
		if !strings.Contains(m.Scripts[0].Body, "echo processed") {
			t.Errorf("the fixed body was not stored: %q", m.Scripts[0].Body)
		}
	}
	if !found {
		t.Fatal("the cap disappeared from its project")
	}
}

// TestAutoCorrect_SubstantialDiffCreatesProposalAndDoesNotChangeActiveCap
// asserts that a large rewrite is staged as a cap_proposed learnings row
// tagged 'pending_approval' while the active cap row is left unchanged.
func TestAutoCorrect_SubstantialDiffCreatesProposalAndDoesNotChangeActiveCap(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\necho original\nexit 0\n"
	fixed := "#!/bin/bash\nset -euo pipefail\nrm -f /var/tmp/scratch\ncurl -s https://example.invalid/x | grep needle\nawk '{print $1}' /etc/hostname\nexit 0\n"
	if got := ClassifyCapDiff(body, fixed); got != DiffSubstantial {
		t.Fatalf("test setup: the rewrite must classify as DiffSubstantial, got %v", got)
	}

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("echo original", "set -euo pipefail\nrm -f /var/tmp/scratch\ncurl -s https://example.invalid/x | grep needle\nawk '{print $1}' /etc/hostname")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-big",
		Name:        "big-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "big_cap",
		ScriptName:  "run",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	scripts := `[{"name":"run","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(body) + `}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "big_cap", "description": "big", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-big", JobName: "big-job", CapName: "big_cap", ScriptName: "run",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	caps, err := s.GetActiveLearnings("cap", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "big_cap" {
			continue
		}
		if strings.Contains(m.Scripts[0].Body, "curl") {
			t.Errorf("a substantial rewrite must not be persisted as the active cap: %q", m.Scripts[0].Body)
		}
		if m.Version != 1 {
			t.Errorf("the active cap must stay untouched, version=%d", m.Version)
		}
	}

	proposals, err := s.GetActiveLearnings("cap_proposed", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("expected exactly one staged proposal, got %d", len(proposals))
	}
	prop := proposals[0]
	if err := s.LoadJunctionData(&prop); err != nil {
		t.Fatalf("load proposal keywords: %v", err)
	}
	var hasPending bool
	for _, k := range prop.Keywords {
		if k == "pending_approval" {
			hasPending = true
		}
	}
	if !hasPending {
		t.Errorf("the proposal must be tagged pending_approval, keywords=%v", prop.Keywords)
	}
	if !strings.Contains(prop.Context, "curl") {
		t.Error("the proposal must carry the rewritten script")
	}
}

// TestPersistProposalForReview_FindsBashByRuntime drives persistProposalForReview
// directly against a real cap seeded through save_cap. After Cap-Spec v1.1 the
// bash body lives on a ScriptMeta with Kind=='tool'|'handler' and Runtime=='bash';
// scanning Scripts[i].Kind=='bash' silently misses every script and the proposal
// path collapses with "no bash script to replace". Asserts that the proposal row
// is created with the swapped body, that the active cap is left untouched, and
// that project metadata is propagated onto the proposal.
func TestPersistProposalForReview_FindsBashByRuntime(t *testing.T) {
	h, _ := mustHandler(t)
	saveResp := h.Handle(Request{
		Method: "save_cap",
		Params: map[string]any{
			"name":         "demo_persist",
			"description":  "demo cap for persistProposalForReview",
			"handler_bash": "echo old\nexit 0\n",
			"project":      "yesmem",
			"tags":         []any{"demo"},
			"_from_disk":   true,
		},
	})
	if saveResp.Error != "" {
		t.Fatalf("seed save_cap: %s", saveResp.Error)
	}

	run := storage.BashJobRun{
		ID:       42,
		JobID:    "j1",
		JobName:  "broken",
		CapName:  "demo_persist",
		ErrorMsg: "exit 1",
	}
	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	var active *models.Learning
	for i := range caps {
		if caps[i].TriggerRule == "cap:demo_persist" {
			active = &caps[i]
			break
		}
	}
	if active == nil {
		t.Fatalf("seeded cap demo_persist not found among active learnings")
	}
	propID, err := h.persistProposalForReview(run, active, "echo fixed\nexit 0\n")
	if err != nil {
		t.Fatalf("persistProposalForReview: %v", err)
	}
	if propID <= 0 {
		t.Fatalf("expected proposal ID > 0, got %d", propID)
	}

	prop, err := h.store.GetLearning(propID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if prop.Category != "cap_proposed" {
		t.Errorf("category=%q, want cap_proposed", prop.Category)
	}
	if prop.TriggerRule != "cap_proposed:demo_persist" {
		t.Errorf("trigger=%q, want cap_proposed:demo_persist", prop.TriggerRule)
	}
	if prop.Project != "yesmem" {
		t.Errorf("project=%q, want yesmem (inherited from active)", prop.Project)
	}

	meta, err := ParseCapMeta(prop.Context)
	if err != nil {
		t.Fatalf("parse proposal CapMeta: %v", err)
	}
	var newBody string
	for _, sc := range meta.Scripts {
		if sc.Runtime == "bash" {
			newBody = sc.Body
			break
		}
	}
	if !strings.Contains(newBody, "echo fixed") {
		t.Errorf("proposal bash body not swapped to fixed version: %q", newBody)
	}

	// The active cap must still hold the original body — proposals do not mutate the live row.
	caps, err = h.store.GetActiveLearnings("cap", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read active caps: %v", err)
	}
	var activeBody string
	for i := range caps {
		m, err := ParseCapMeta(caps[i].Context)
		if err != nil || m.Name != "demo_persist" {
			continue
		}
		for _, sc := range m.Scripts {
			if sc.Runtime == "bash" {
				activeBody = sc.Body
				break
			}
		}
		break
	}
	if !strings.Contains(activeBody, "echo old") {
		t.Errorf("active cap body wrongly mutated: %q", activeBody)
	}
}

// TestAutoCorrect_CooldownSkipsRapidRetries verifies that when several failed
// runs of the same cap pile up in a single processBashJobErrors tick, only the
// first triggers an auto-correct attempt; the remaining are skipped (still
// marked processed) so the LLM is not hammered while a cap has a persistent
// bug. With the sandbox disabled, the first attempt broadcasts the SKIPPED
// (sandbox unavailable) message; the remaining two MUST broadcast a
// "cooldown" skip — that is the contract this test pins.
func TestAutoCorrect_CooldownSkipsRapidRetries(t *testing.T) {
	neutralizeClaude(t)
	disableSandbox(t)

	h, s := mustHandler(t)
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("exit 1", "exit 0")})
	seedCap(t, h, "flaky", "", "exit 1\n", []string{"flaky"})

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j1",
		Name:        "flaky-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "flaky",
		Project:     "/srv/los32d/cooldown",
		AutoCorrect: true,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.SaveBashJobRun(storage.BashJobRun{
			JobID:    "j1",
			JobName:  "flaky-job",
			CapName:  "flaky",
			Command:  "exit 1",
			Status:   "error",
			ExitCode: 1,
			Output:   "",
			ErrorMsg: "exit status 1",
		}); err != nil {
			t.Fatalf("save run %d: %v", i, err)
		}
	}

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("all 3 runs must be marked processed; got %d unprocessed", len(runs))
	}

	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/cooldown")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	cooldownCount := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, "cooldown") {
			cooldownCount++
		}
	}
	if cooldownCount != 2 {
		t.Errorf("expected 2 cooldown-skip broadcasts (runs 2 and 3); got %d (broadcasts total: %d)", cooldownCount, len(msgs))
	}
}

// TestAutoCorrect_GenerationLimitBlocksAfter3PerDay verifies that once a cap
// has accumulated autoCorrectMaxGenerationsPerDay auto-correct outcomes
// (applied caps via Source='auto_correct_accepted' OR staged proposals via
// Source='auto_correct_proposal') within the last 24h, the next failed run
// for that cap is skipped with a "limit"-tagged broadcast — preventing a
// broken cap from burning unbounded LLM tokens. The gate runs after T4's
// per-cap cooldown but before the per-tick budget and the in-flight
// semaphore.
func TestAutoCorrect_GenerationLimitBlocksAfter3PerDay(t *testing.T) {
	neutralizeClaude(t)
	disableSandbox(t)

	h, s := mustHandler(t)
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("exit 1", "exit 0")})
	seedCap(t, h, "buggy", "", "exit 1\n", []string{"buggy"})

	for i := 0; i < 3; i++ {
		_, err := s.InsertLearning(&models.Learning{
			Content:     fmt.Sprintf("auto-correct apply #%d", i),
			Category:    "cap",
			Source:      "auto_correct_accepted",
			TriggerRule: "cap:buggy",
			Project:     "yesmem",
			Confidence:  1.0,
			CreatedAt:   time.Now().Add(-time.Duration(i+1) * time.Hour),
		})
		if err != nil {
			t.Fatalf("seed prior accept #%d: %v", i, err)
		}
	}

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j1",
		Name:        "buggy-job",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "buggy",
		Project:     "/srv/los32d/limit",
		AutoCorrect: true,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}

	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    "j1",
		JobName:  "buggy-job",
		CapName:  "buggy",
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	runs, err := s.GetUnprocessedBashErrors(10)
	if err != nil {
		t.Fatalf("get unprocessed: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("fresh run must be marked processed; got %d unprocessed", len(runs))
	}

	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/limit")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	limitCount := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, "limit") {
			limitCount++
		}
	}
	if limitCount != 1 {
		t.Errorf("expected 1 generation-limit broadcast; got %d (broadcasts total: %d)", limitCount, len(msgs))
	}
}

// TestAutoCorrectTestRunTimeout pins the timeout selected for the auto-correct
// test-run as a function of the job's interval. Sub-minute intervals get a
// tight timeout matching the interval, anything else falls back to the default
// so a long-cron job does not produce a minute-long test-run.
func TestAutoCorrectTestRunTimeout(t *testing.T) {
	cases := []struct {
		name        string
		intervalSec int
		want        int
	}{
		{"zero defaults to 10", 0, 10},
		{"negative defaults to 10", -5, 10},
		{"sub-minute uses interval", 15, 15},
		{"just-under-60 uses interval", 59, 59},
		{"60 falls back to default", 60, 10},
		{"minutes fall back to default", 120, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoCorrectTestRunTimeout(tc.intervalSec); got != tc.want {
				t.Errorf("intervalSec=%d: got %d, want %d", tc.intervalSec, got, tc.want)
			}
		})
	}
}

// TestPersistProposalForReview_MultiBashTargetsScriptByName seeds a bundle cap
// with two bash handlers (poll + reply) and verifies that persistProposalForReview
// patches only the script named in run.ScriptName. The other bash script must
// keep its original body, and the proposal's Keywords must carry "script:reply"
// so handleCapProposalDecide can route the accept back to the same script.
func TestPersistProposalForReview_MultiBashTargetsScriptByName(t *testing.T) {
	h, _ := mustHandler(t)

	scriptsJSON := `[` +
		`{"name":"poll","kind":"handler","runtime":"bash","lang":"bash","body":"echo poll-original\nexit 0\n"},` +
		`{"name":"reply","kind":"handler","runtime":"bash","lang":"bash","body":"echo reply-original\nexit 0\n"}` +
		`]`
	saveResp := h.Handle(Request{
		Method: "save_cap",
		Params: map[string]any{
			"name":        "tg_bundle",
			"description": "two bash handlers",
			"scripts":     scriptsJSON,
			"project":     "yesmem",
		},
	})
	if saveResp.Error != "" {
		t.Fatalf("seed save_cap: %s", saveResp.Error)
	}

	caps, err := h.store.GetActiveLearnings("cap", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	var active *models.Learning
	for i := range caps {
		if caps[i].TriggerRule == "cap:tg_bundle" {
			active = &caps[i]
			break
		}
	}
	if active == nil {
		t.Fatalf("seeded cap not found")
	}

	run := storage.BashJobRun{
		ID:         99,
		JobID:      "j1",
		JobName:    "reply_tick",
		CapName:    "tg_bundle",
		ScriptName: "reply",
		ErrorMsg:   "timeout",
	}
	propID, err := h.persistProposalForReview(run, active, "echo reply-fixed\nexit 0\n")
	if err != nil {
		t.Fatalf("persistProposalForReview: %v", err)
	}

	prop, err := h.store.GetLearning(propID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if err := h.store.LoadJunctionData(prop); err != nil {
		t.Fatalf("LoadJunctionData: %v", err)
	}
	meta, err := ParseCapMeta(prop.Context)
	if err != nil {
		t.Fatalf("parse proposal CapMeta: %v", err)
	}
	var pollBody, replyBody string
	for _, sc := range meta.Scripts {
		switch sc.Name {
		case "poll":
			pollBody = sc.Body
		case "reply":
			replyBody = sc.Body
		}
	}
	if !strings.Contains(replyBody, "echo reply-fixed") {
		t.Errorf("reply body not patched, got %q", replyBody)
	}
	if !strings.Contains(pollBody, "echo poll-original") {
		t.Errorf("poll body wrongly mutated, got %q", pollBody)
	}

	scriptKeyword := ""
	for _, k := range prop.Keywords {
		if strings.HasPrefix(k, "script:") {
			scriptKeyword = k
			break
		}
	}
	if scriptKeyword != "script:reply" {
		t.Errorf("missing or wrong script keyword: got %q, want %q", scriptKeyword, "script:reply")
	}
}

// The fix prompt redacts its untrusted inputs itself: the output it sends is
// arbitrary command output and leaves only after the redactor saw it.
func TestAutoCorrectPrompt_RedactsTheJobOutput(t *testing.T) {
	h, _ := mustHandler(t)
	h.redactor = sanitize.NewSecretRedactor(nil)

	secret := "AKIAIOSFODNN7EXAMPLE"
	prompt := h.autoCorrectPrompt(
		storage.BashJobRun{CapName: "backup", ErrorMsg: "upload_aws: command not found", Output: "uploaded with key=" + secret},
		ScriptMeta{Name: "run", Kind: "handler", Runtime: "bash", Body: "#!/bin/bash\nupload_aws\nexit 0\n"},
		&CapMeta{Name: "backup"},
	)

	if strings.Contains(prompt, secret) {
		t.Errorf("the job output reached the model unredacted: %q", prompt)
	}
	if !strings.Contains(prompt, "upload_aws: command not found") {
		t.Errorf("the error excerpt is the point of the pass: %q", prompt)
	}
}

// A secret that straddles the truncation cut must not survive as a fragment: the
// redactor only matches the whole literal, so redaction has to run first.
func TestAutoCorrectPrompt_RedactsBeforeTruncating(t *testing.T) {
	h, _ := mustHandler(t)
	h.redactor = sanitize.NewSecretRedactor(nil)

	secret := "AKIAIOSFODNN7EXAMPLE"
	output := strings.Repeat("a", 1990) + " key=" + secret
	prompt := h.autoCorrectPrompt(
		storage.BashJobRun{CapName: "backup", Output: output},
		ScriptMeta{Name: "run", Kind: "handler", Runtime: "bash", Body: "#!/bin/bash\nexit 0\n"},
		&CapMeta{Name: "backup"},
	)

	if strings.Contains(prompt, "AKIA") {
		t.Errorf("a fragment of the secret survived the truncation: %q", prompt[len(prompt)-80:])
	}
}

// A model that fences its answer anyway must not make a one-line fix look like
// a rewrite: the fence lines would count as new command tokens.
func TestTrimCodeFence(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no fence", "echo hi\nexit 0", "echo hi\nexit 0"},
		{"bash fence", "```bash\necho hi\nexit 0\n```", "echo hi\nexit 0"},
		{"bare fence", "```\necho hi\n```", "echo hi"},
		{"opening fence only", "```bash", "```bash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimCodeFence(tc.in); got != tc.want {
				t.Errorf("trimCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The verification run has to use the runtime the script declares, exactly like
// fireJobBash. With only the interval heuristic, a script declaring 120s (which
// exists because it scans an hour of journal output) failed its own test run and
// the fix was never persisted.
func TestAutoCorrectVerifyTimeout(t *testing.T) {
	cases := []struct {
		name     string
		interval int
		declared int64
		want     int
	}{
		{"no declaration, no interval", 0, 0, 10},
		{"sub-minute interval, no declaration", 15, 0, 15},
		{"declaration wins", 0, 120, 120},
		{"declaration wins over the interval", 15, 120, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoCorrectVerifyTimeout(tc.interval, tc.declared); got != tc.want {
				t.Errorf("autoCorrectVerifyTimeout(%d, %d) = %d, want %d", tc.interval, tc.declared, got, tc.want)
			}
		})
	}
}
