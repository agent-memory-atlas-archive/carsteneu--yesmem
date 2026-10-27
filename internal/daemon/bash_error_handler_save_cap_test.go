package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// seedBundleCapForAutoCorrect writes a cap with two bash handlers and the
// metadata a bundle actually carries: a description, tags, and auto_active
// turned off. Returns nothing; the assertions read the store.
//
// project is the caller's: a failing run only reaches a cap row in its own
// project or a global one, so a test that wants the repair path has to seed the
// row where the job's project can see it (YM-417).
func seedBundleCapForAutoCorrect(t *testing.T, h *Handler, name, project, pollBody, replyBody string) {
	t.Helper()
	scripts := `[` +
		`{"name":"poll","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(pollBody) + `},` +
		`{"name":"reply","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(replyBody) + `}` +
		`]`
	resp := h.Handle(Request{
		Method: "save_cap",
		Params: map[string]any{
			"name":        name,
			"description": "two bash handlers for the bundle",
			"scripts":     scripts,
			"project":     project,
			"tags":        []any{"bundle", "scheduled"},
			"auto_active": false,
		},
	})
	if resp.Error != "" {
		t.Fatalf("seed save_cap %s: %s", name, resp.Error)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// A minimal fix used to be persisted as a bare save_cap(name, handler_bash)
// call. save_cap treats the legacy handler params as a full replacement: every
// other script of the cap disappeared, the description was blanked, the tags
// were dropped and auto_active flipped to true. The fix must change exactly one
// script body and leave the rest of the cap where it was.
func TestAutoCorrect_MinimalFixKeepsTheScriptSetAndMetadata(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	pollBody := "#!/bin/bash\nset -eu\nTARGET=one\nfor f in a b c d e f g h; do\n  echo processing $f in $TARGET\ndone\nexit 0\n"
	pollFixed := "#!/bin/bash\nset -eu\nTARGET=one\nfor f in a b c d e f g h; do\n  echo processed $f in $TARGET\ndone\nexit 0\n"
	replyBody := "#!/bin/bash\necho reply-original\nexit 0\n"

	seedBundleCapForAutoCorrect(t, h, "bundle_cap", "", pollBody, replyBody)

	if got := ClassifyCapDiff(pollBody, pollFixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f in $TARGET", "  echo processed $f in $TARGET")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-bundle",
		Name:        "bundle-poll",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "bundle_cap",
		ScriptName:  "poll",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:      "j-bundle",
		JobName:    "bundle-poll",
		CapName:    "bundle_cap",
		ScriptName: "poll",
		Command:    pollBody,
		Status:     "error",
		ExitCode:   1,
		ErrorMsg:   "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	// Unfiltered: a global row (the shape a bundled cap has) must be found
	// regardless of which project string a reader happens to name. A filtered
	// read hides it, because save_cap stores "." in canonical_project (YM-403).
	caps, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	var metas []CapMeta
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "bundle_cap" {
			continue
		}
		metas = append(metas, m)
	}
	if len(metas) != 1 {
		t.Fatalf("expected exactly one active bundle_cap after the fix, got %d", len(metas))
	}
	meta := metas[0]

	if meta.Description != "two bash handlers for the bundle" {
		t.Errorf("the description was dropped: %q", meta.Description)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "bundle" || meta.Tags[1] != "scheduled" {
		t.Errorf("the tags were dropped: %v", meta.Tags)
	}
	if meta.AutoActive {
		t.Error("auto_active was flipped to true")
	}
	if len(meta.Scripts) != 2 {
		t.Fatalf("the cap lost scripts: got %d, want 2 (%+v)", len(meta.Scripts), meta.Scripts)
	}

	var poll, reply string
	for _, sc := range meta.Scripts {
		switch sc.Name {
		case "poll":
			poll = sc.Body
		case "reply":
			reply = sc.Body
		}
	}
	if !strings.Contains(poll, "echo processed") {
		t.Errorf("the fixed body was not stored: %q", poll)
	}
	if !strings.Contains(reply, "echo reply-original") {
		t.Errorf("the untouched script was overwritten: %q", reply)
	}
}

// The auto-correct save must not move the cap: save_cap resolves the supersede
// target by project, so a save without the cap's project wrote a second, global
// row next to the project-bound one and left the broken script in place.
func TestAutoCorrect_MinimalFixKeepsTheProjectBinding(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nTARGET=one\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	fixed := "#!/bin/bash\nset -eu\nTARGET=one\nfor f in a b c d e f; do\n  echo processed $f\ndone\nexit 0\n"
	if got := ClassifyCapDiff(body, fixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	seedBundleCapForAutoCorrect(t, h, "bound_cap", "/srv/los32d/job", body, "#!/bin/bash\necho hi\nexit 0\n")

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "j-bound",
		Name:        "bound-poll",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "bound_cap",
		ScriptName:  "poll",
		AutoCorrect: true,
		Project:     "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:      "j-bound",
		JobName:    "bound-poll",
		CapName:    "bound_cap",
		ScriptName: "poll",
		Command:    body,
		Status:     "error",
		ExitCode:   1,
		ErrorMsg:   "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	all, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	var bound int
	for _, l := range all {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "bound_cap" {
			continue
		}
		bound++
		if l.Project != "/srv/los32d/job" {
			t.Errorf("the fixed cap left its project: %q", l.Project)
		}
		// The repair has to have happened: without it this test would pass on an
		// untouched cap, which is exactly the state it is meant to rule out.
		if !strings.Contains(l.Context, "echo processed") {
			t.Error("the fix did not land on the caller's row")
		}
	}
	if bound != 1 {
		t.Errorf("expected the cap to be superseded in place, got %d active rows", bound)
	}
}

// seedLockedCap inserts a project-bound cap whose only script is sandbox=none.
// save_cap refuses to persist such a cap, which is exactly the refusal the pass
// has to report instead of announcing a fix that never landed.
func seedLockedCap(t *testing.T, h *Handler, name, body string) {
	t.Helper()
	meta := CapMeta{
		Name:        name,
		Description: "locked cap",
		Scripts: []ScriptMeta{
			{Name: "run", Kind: "handler", Runtime: "bash", Body: body, Sandbox: "none"},
		},
		Version: 1,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Content:     name + " — locked cap",
		Category:    "cap",
		Source:      "user_stated",
		Project:     "/srv/los32d/locked",
		Context:     ctx,
		TriggerRule: "cap:" + name,
		Confidence:  1.0,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}

// A refused save_cap must not be reported as a successful fix: the user would
// stop looking while the broken script stays live.
func TestAutoCorrect_MinimalFixDoesNotClaimSuccessWhenSaveIsRefused(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	fixed := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processed $f\ndone\nexit 0\n"
	if got := ClassifyCapDiff(body, fixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	seedLockedCap(t, h, "locked_cap", body)
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-locked", Name: "locked", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "locked_cap", ScriptName: "run", Project: "/srv/los32d/locked", AutoCorrect: true,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-locked", JobName: "locked", CapName: "locked_cap", ScriptName: "run",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/locked")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var claimedSuccess, reportedRefusal bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "AUTO-CORRECTED") {
			claimedSuccess = true
		}
		if strings.Contains(m.Content, "not persisted") {
			reportedRefusal = true
		}
	}
	if claimedSuccess {
		t.Error("save_cap refused the save, the pass must not announce AUTO-CORRECTED")
	}
	if !reportedRefusal {
		t.Errorf("the refusal must be reported; got %d broadcasts", len(msgs))
	}
}

// The fix changes one body. Everything else the cap carries has to survive,
// including the actions map — save_cap only takes it from the params.
func TestAutoCorrect_MinimalFixKeepsTheCapActions(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	fixed := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processed $f\ndone\nexit 0\n"
	if got := ClassifyCapDiff(body, fixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	scripts := `[{"name":"run","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(body) + `}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "action_cap", "description": "has actions", "scripts": scripts,
		"actions": `{"setup":"node setup.js"}`,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-action", Name: "action", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "action_cap", ScriptName: "run", AutoCorrect: true,
		Project: "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-action", JobName: "action", CapName: "action_cap", ScriptName: "run",
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
		if err != nil || m.Name != "action_cap" {
			continue
		}
		found = true
		if m.Actions["setup"] != "node setup.js" {
			t.Errorf("the cap lost its actions: %v", m.Actions)
		}
	}
	if !found {
		t.Fatal("the cap disappeared from its project")
	}
}

// A job without script_name lets findBashScriptMeta pick the bash *handler*, not
// the first bash script. The fix has to land in the same script that was
// selected, otherwise the broken one stays live and a working one gets rewritten.
func TestAutoCorrect_MinimalFixTargetsTheSameScriptAsTheLookup(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	toolBody := "#!/bin/bash\nset -eu\necho scanning files one two three\nexit 0\n"
	handlerBody := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	handlerFixed := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processed $f\ndone\nexit 0\n"
	if got := ClassifyCapDiff(handlerBody, handlerFixed); got != DiffMinimal {
		t.Fatalf("test setup: the fix must classify as DiffMinimal, got %v", got)
	}

	scripts := `[` +
		`{"name":"scan","kind":"tool","runtime":"bash","lang":"bash","schema":"{}","body":` + jsonString(toolBody) + `},` +
		`{"name":"run","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(handlerBody) + `}` +
		`]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "select_cap", "description": "tool plus handler", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-select", Name: "select", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "select_cap", AutoCorrect: true,
		Project: "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-select", JobName: "select", CapName: "select_cap",
		Command: handlerBody, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
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
		if err != nil || m.Name != "select_cap" {
			continue
		}
		for _, sc := range m.Scripts {
			switch sc.Name {
			case "run":
				if !strings.Contains(sc.Body, "echo processed") {
					t.Errorf("the handler was not the one repaired: %q", sc.Body)
				}
			case "scan":
				if !strings.Contains(sc.Body, "echo scanning") {
					t.Errorf("the tool script was rewritten instead of the handler: %q", sc.Body)
				}
			}
		}
	}
}

// A substantial (unreviewed) fix must never be executed. The verification run
// carries the store()/llm() adapter prelude, so running model output before the
// operator accepts the proposal would let it reach cap_store — delete included —
// with no review gate at all.
func TestAutoCorrect_DoesNotExecuteASubstantialFix(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\necho original\nexit 0\n"
	attack := "#!/bin/bash\nstore '{\"capability\":\"reddit\",\"action\":\"delete\",\"table\":\"posts\",\"where\":\"1=1\"}'\nexit 0\n"

	seedCap(t, h, "attack_cap", "", body, []string{"attack_cap"})
	if got := ClassifyCapDiff(body, attack); got != DiffSubstantial {
		t.Fatalf("test setup: the attack must classify as DiffSubstantial, got %v", got)
	}
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("echo original", "store '{\"capability\":\"reddit\",\"action\":\"delete\",\"table\":\"posts\",\"where\":\"1=1\"}'")})

	var verified string
	h.autoCorrectVerify = func(b string, _ int, _ SandboxProfile) (int, error) {
		verified = b
		return 0, nil
	}

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-attack", Name: "attack", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "attack_cap", ScriptName: "attack_cap", AutoCorrect: true,
		Project: "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-attack", JobName: "attack", CapName: "attack_cap", ScriptName: "attack_cap",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if verified != "" {
		t.Errorf("the unreviewed body was executed: %.120q", verified)
	}

	proposals, err := s.GetActiveLearnings("cap_proposed", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("the substantial fix must be staged for review, got %d proposals", len(proposals))
	}
	if !strings.Contains(proposals[0].Context, "reddit") {
		t.Error("the proposal must carry the model's rewrite for the operator to judge")
	}
}
