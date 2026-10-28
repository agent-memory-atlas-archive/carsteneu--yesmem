package daemon

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/sanitize"
	"github.com/carsteneu/yesmem/internal/storage"
)

// patchReply renders the reply the fix prompt asks for: the block to replace and
// its replacement. The flow tests stub the model with this, because the prompt no
// longer carries the script body and the model therefore cannot answer with a
// whole body.
func patchReply(oldBlock, newBlock string) string {
	return "REPLACE\n<<<<\n" + oldBlock + "\n====\n" + newBlock + "\n>>>>\n"
}

func promptFixture(t *testing.T, body string, run storage.BashJobRun) string {
	t.Helper()
	h, _ := mustHandler(t)
	h.redactor = sanitize.NewSecretRedactor(nil)
	sc := ScriptMeta{Name: "run", Kind: "handler", Runtime: "bash", Body: body}
	meta := &CapMeta{
		Name:        "backup",
		Description: "nightly backup of the bucket",
		Scripts: []ScriptMeta{
			sc,
			{Name: "poll", Kind: "handler", Runtime: "bash", Body: "#!/bin/bash\necho poll\n"},
		},
	}
	if run.CapName == "" {
		run.CapName = "backup"
	}
	if run.ScriptName == "" {
		run.ScriptName = "run"
	}
	return h.autoCorrectPrompt(run, sc, meta)
}

// The fix prompt must not carry the script body: the body is a live cap and can
// hold literals the model has no business reading. What it carries is the failing
// command, the error, the output excerpt and the cap metadata.
func TestAutoCorrectPrompt_DoesNotCarryTheCapBody(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	body := "#!/bin/bash\nset -eu\nupload_aws " + secret + "\nrun_backup --all\nexit 0\n"

	prompt := promptFixture(t, body, storage.BashJobRun{
		ExitCode: 1,
		ErrorMsg: "run_backup: command not found",
		Output:   "starting\nboom",
	})
	if strings.Contains(prompt, secret) {
		t.Errorf("the cap body's literal reached the model: %q", prompt)
	}
	// The cap's own metadata is operator-written, not operator-vetted: it goes
	// through the redactor too.
	meta := &CapMeta{
		Name:        "backup",
		Description: "nightly backup with " + secret,
		Scripts:     []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: body}},
	}
	h, _ := mustHandler(t)
	h.redactor = sanitize.NewSecretRedactor(nil)
	metaPrompt := h.autoCorrectPrompt(
		storage.BashJobRun{CapName: "backup", ScriptName: "run", ExitCode: 1, ErrorMsg: "run_backup: command not found"},
		meta.Scripts[0], meta,
	)
	if strings.Contains(metaPrompt, secret) {
		t.Errorf("the cap description's literal reached the model: %q", metaPrompt)
	}
	for _, withheld := range []string{"set -eu", "upload_aws", "exit 0"} {
		if strings.Contains(prompt, withheld) {
			t.Errorf("the prompt still carries the body line %q: %q", withheld, prompt)
		}
	}
	// The excerpt itself is the point of the pass and has to be in the prompt.
	for _, want := range []string{"run_backup --all", "run_backup: command not found", "boom"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the excerpt lacks %q: %q", want, prompt)
		}
	}
	// Enough structure to propose a fix against the live script.
	for _, want := range []string{"backup", "nightly backup", "handler", "poll"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks the cap metadata %q: %q", want, prompt)
		}
	}
}

// A failing line that carries a secret is withheld instead of being sent
// redacted: a redacted copy would come back as the model's "old block" and match
// nothing in the live script, so sending it invites a useless patch.
func TestAutoCorrectPrompt_WithholdsAFailingLineWithASecret(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	body := "#!/bin/bash\nupload_aws " + secret + "\nexit 0\n"

	prompt := promptFixture(t, body, storage.BashJobRun{
		ExitCode: 1,
		ErrorMsg: "upload_aws: command not found",
	})

	if strings.Contains(prompt, secret) {
		t.Errorf("the failing line's literal reached the model: %q", prompt)
	}
	if strings.Contains(prompt, "[REDACTED:") {
		t.Errorf("a redacted line was sent as if it were script text: %q", prompt)
	}
	if !strings.Contains(prompt, "withheld") {
		t.Errorf("the prompt must say the failing line was withheld: %q", prompt)
	}
}

// A one-line body is its own failing line. Quoting the line would quote the whole
// script, so the prompt must narrow it to the statement that failed, and quote
// nothing at all if even that is the whole body.
func TestAutoCorrectPrompt_NeverQuotesAOneLineBody(t *testing.T) {
	oneLiner := `run_backup --all && curl -s "$FEED" | jq -r .items > /tmp/feed.txt && exit 0`

	prompt := promptFixture(t, oneLiner, storage.BashJobRun{
		ExitCode: 127,
		ErrorMsg: "jq: command not found",
	})
	if strings.Contains(prompt, oneLiner) {
		t.Errorf("the whole one-line body was quoted as the failing command: %q", prompt)
	}
	// The narrowing has to name something: the statement that failed.
	if !strings.Contains(prompt, "/tmp/feed.txt") {
		t.Errorf("the failing statement was not narrowed out of the body: %q", prompt)
	}
	if strings.Contains(prompt, "run_backup") {
		t.Errorf("a statement the error does not name was quoted: %q", prompt)
	}

	// The same body with an error that names a longer command: the line is the
	// whole body again, so only the named statement may leave.
	named := promptFixture(t, `run_backup --all && echo done`, storage.BashJobRun{
		ExitCode: 127,
		ErrorMsg: "run_backup: command not found",
	})
	if !strings.Contains(named, "Failing command (line 1): run_backup --all") {
		t.Errorf("the named statement was not narrowed out of the one-line body: %q", named)
	}

	// Nothing narrower than the body exists: the prompt says so instead of
	// sending it. The error line still names the command, which is the failure
	// text, not the script.
	single := promptFixture(t, "justoneword", storage.BashJobRun{
		ExitCode: 127,
		ErrorMsg: "justoneword: command not found",
	})
	if strings.Contains(single, "Failing command (line") {
		t.Errorf("a body with no statement to narrow was quoted as the failing command: %q", single)
	}
	if !strings.Contains(single, "withheld") {
		t.Errorf("the prompt must say why the command is missing: %q", single)
	}
}

// A "line N" that a program prints into the output points into another file. Only
// the shell's own form may steer the excerpt.
func TestFailingLineExcerpt_IgnoresForeignLineNumbers(t *testing.T) {
	body := "#!/bin/bash\nset -eu\njq -r .items /tmp/feed\nexit 0\n"

	line, text, ok := failingLineExcerpt(body, "exit status 1", "File \"/opt/report.py\", line 3, in <module>")
	if ok {
		t.Errorf("a python traceback steered the excerpt to (%d, %q)", line, text)
	}

	line, text, ok = failingLineExcerpt(body, "exit status 1", "bash: line 3: jq: command not found")
	if !ok || line != 3 || text != "jq -r .items /tmp/feed" {
		t.Errorf("the shell's own line reference was ignored: (%d, %q, %v)", line, text, ok)
	}
}

// The excerpt has to name the line it wants replaced, otherwise the model cannot
// produce a patch that matches the live script.
func TestFailingLineExcerpt(t *testing.T) {
	plain := "#!/bin/bash\nset -eu\nrun_backup --all\nexit 0\n"
	withAdapter := "#!/bin/bash\nset -eu\nstore '{\"capability\":\"x\",\"action\":\"upsert\"}'\nrun_backup --all\nexit 0\n"

	cases := []struct {
		name     string
		body     string
		errMsg   string
		output   string
		wantLine int
		wantText string
		wantOK   bool
	}{
		{
			name:     "the error names the command",
			body:     plain,
			errMsg:   "bash: run_backup: command not found",
			wantLine: 3,
			wantText: "run_backup --all",
			wantOK:   true,
		},
		{
			name:     "the output quotes the whole statement",
			body:     plain,
			output:   "+ run_backup --all\n",
			errMsg:   "exit status 1",
			wantLine: 3,
			wantText: "run_backup --all",
			wantOK:   true,
		},
		{
			name:     "a line number in the error wins over a guessed token",
			body:     plain,
			errMsg:   "bash: line 2: set: bad substitution",
			wantLine: 2,
			wantText: "set -eu",
			wantOK:   true,
		},
		{
			name:     "nothing to go on",
			body:     plain,
			errMsg:   "exit status 1",
			wantLine: 0,
			wantOK:   false,
		},
		{
			name:   "a bare exit code names no line",
			body:   plain,
			output: "",
			errMsg: "exit status 137",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, text, ok := failingLineExcerpt(tc.body, tc.errMsg, tc.output)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if line != tc.wantLine || text != tc.wantText {
				t.Errorf("failingLineExcerpt = (%d, %q), want (%d, %q)", line, text, tc.wantLine, tc.wantText)
			}
		})
	}

	// The run executes prepareBashCommand(body), so a line number in the error
	// counts from above the script. Without the shift the excerpt points at the
	// wrong statement, and a patch built from it matches nothing.
	shifted := "bash: line " + strconv.Itoa(4+adapterPreludeLines(withAdapter)) + ": run_backup: command not found"
	if line, text, ok := failingLineExcerpt(withAdapter, shifted, ""); !ok || line != 4 || text != "run_backup --all" {
		t.Errorf("the adapter prelude was not subtracted: got (%d, %q, %v), want (4, \"run_backup --all\", true)", line, text, ok)
	}
}

func TestParseFixPatch(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantOld string
		wantNew string
		wantNo  string
		wantErr bool
	}{
		{
			name:    "a replace block",
			reply:   patchReply("run_backup --all", "run_backup --all --retry 3"),
			wantOld: "run_backup --all",
			wantNew: "run_backup --all --retry 3",
		},
		{
			name:    "a multi-line replacement",
			reply:   patchReply("echo a", "echo a\necho b"),
			wantOld: "echo a",
			wantNew: "echo a\necho b",
		},
		{
			name:   "a decline",
			reply:  "UNFIXABLE: the excerpt does not show which file is missing",
			wantNo: "the excerpt does not show which file is missing",
		},
		{
			name:    "a fenced reply is still a replace block",
			reply:   "```\n" + patchReply("echo a", "echo b") + "```\n",
			wantOld: "echo a",
			wantNew: "echo b",
		},
		{
			name:    "empty reply",
			reply:   "   \n",
			wantErr: true,
		},
		{
			name:    "a body without the protocol",
			reply:   "#!/bin/bash\necho a\nexit 0\n",
			wantErr: true,
		},
		{
			name:    "unterminated block",
			reply:   "REPLACE\n<<<<\necho a\n====\necho b\n",
			wantErr: true,
		},
		{
			name:    "an empty old block",
			reply:   patchReply("", "echo b"),
			wantErr: true,
		},
		// The client redacts the completion. A marker in the replacement would be
		// written into the live cap as if it were the fix.
		{
			name:    "a replacement carrying a redaction marker",
			reply:   patchReply("echo a", "echo [REDACTED:generic_api_key]"),
			wantErr: true,
		},
		// Two blocks would be half-applied: the first fix lands, the operator is
		// told the script is repaired, and the second problem is silently gone.
		{
			name:    "two replace blocks",
			reply:   patchReply("echo a", "echo b") + patchReply("echo c", "echo d"),
			wantErr: true,
		},
		{
			name:   "a refusal with a prefix keeps its reason",
			reply:  "Answer: UNFIXABLE: the excerpt does not name the file",
			wantNo: "the excerpt does not name the file",
		},
		{
			name:   "a refusal without a reason still declines",
			reply:  "UNFIXABLE:",
			wantNo: "the model gave no reason",
		},
		{
			name:    "a CRLF reply still yields an applicable block",
			reply:   "REPLACE\r\n<<<<\r\nrun_backup\r\n====\r\nrun_backup --retry 3\r\n>>>>\r\n",
			wantOld: "run_backup",
			wantNew: "run_backup --retry 3",
		},
		{
			name:    "a multi-line CRLF reply yields a block that matches an LF body",
			reply:   "REPLACE\r\n<<<<\r\necho a\r\necho b\r\n====\r\necho a\r\necho c\r\n>>>>\r\n",
			wantOld: "echo a\necho b",
			wantNew: "echo a\necho c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseFixPatch(tc.reply)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFixPatch(%q) = %+v, want an error", tc.reply, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFixPatch(%q): %v", tc.reply, err)
			}
			if p.oldBlock != tc.wantOld || p.newBlock != tc.wantNew || p.unfixable != tc.wantNo {
				t.Errorf("parseFixPatch = %+v, want old=%q new=%q unfixable=%q", p, tc.wantOld, tc.wantNew, tc.wantNo)
			}
		})
	}
}

// The protocol markers are their own lines. Matched as bare text, a block whose
// content contains "====" splits at the wrong place and the spliced body still
// has a unique match.
func TestParseFixPatch_MarkersMustBeTheirOwnLines(t *testing.T) {
	reply := "REPLACE\n<<<<\necho \"==== section ====\"\n====\necho \"==== done ====\"\n>>>>\n"
	p, err := parseFixPatch(reply)
	if err != nil {
		t.Fatalf("parseFixPatch: %v", err)
	}
	if p.oldBlock != `echo "==== section ===="` || p.newBlock != `echo "==== done ===="` {
		t.Errorf("the block was split at a marker inside its content: %+v", p)
	}

	// A block that itself needs a bare "====" line cannot be expressed. Aborting is
	// the only safe answer; splicing at the first one would corrupt the script.
	ambiguous := "REPLACE\n<<<<\necho a\n====\necho b\n====\necho c\n>>>>\n"
	if _, err := parseFixPatch(ambiguous); err == nil {
		t.Error("a block with an internal separator line must be refused")
	}
}

// A quoted block keeps the indentation the model copied. A trimmed copy matches
// no indented body line, so the pass would lose every indented statement.
func TestParseFixPatch_KeepsTheBlocksIndentation(t *testing.T) {
	reply := "REPLACE\n<<<<\n  echo processing $f\n====\n  echo processed $f\n>>>>\n"
	p, err := parseFixPatch(reply)
	if err != nil {
		t.Fatalf("parseFixPatch: %v", err)
	}

	body := "#!/bin/bash\nfor f in a b; do\n  echo processing $f\ndone\n"
	got, err := applyFixPatch(body, p)
	if err != nil {
		t.Fatalf("an indented block must match an indented body line: %v", err)
	}
	if want := "#!/bin/bash\nfor f in a b; do\n  echo processed $f\ndone\n"; got != want {
		t.Errorf("applyFixPatch = %q, want %q", got, want)
	}
}

func TestApplyFixPatch(t *testing.T) {
	const body = "#!/bin/bash\nset -eu\nrun_backup --all\nexit 0\n"
	cases := []struct {
		name    string
		body    string
		patch   fixPatch
		want    string
		wantErr bool
	}{
		{
			name:  "one unique block",
			patch: fixPatch{oldBlock: "run_backup --all", newBlock: "run_backup --all --retry 3"},
			want:  "#!/bin/bash\nset -eu\nrun_backup --all --retry 3\nexit 0\n",
		},
		{
			name:    "the block is not in the script",
			patch:   fixPatch{oldBlock: "run_restore --all", newBlock: "run_restore --all --retry 3"},
			wantErr: true,
		},
		{
			name:    "the block is ambiguous",
			body:    "exit 0\nrun_backup\nexit 0\n",
			patch:   fixPatch{oldBlock: "exit 0", newBlock: "exit 1"},
			wantErr: true,
		},
		{
			name:    "the replacement changes nothing",
			patch:   fixPatch{oldBlock: "run_backup --all", newBlock: "run_backup --all"},
			wantErr: true,
		},
		{
			name:    "the patch names no block",
			patch:   fixPatch{newBlock: "run_backup --all --retry 3"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject := tc.body
			if subject == "" {
				subject = body
			}
			got, err := applyFixPatch(subject, tc.patch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("applyFixPatch(%+v) = %q, want an error", tc.patch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyFixPatch: %v", err)
			}
			if got != tc.want {
				t.Errorf("applyFixPatch = %q, want %q", got, tc.want)
			}
		})
	}
}

// The patch is applied to the live body, so the fix that lands is the whole
// script with exactly one block swapped. The rest of the body never went to the
// model.
func TestAutoCorrect_AppliesThePatchToTheLiveBody(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nrun_backup --all\nexit 0\n"
	seedCap(t, h, "patch_cap", "", body, []string{"patch_cap"})

	stub := &autoCorrectStubClient{reply: patchReply("run_backup --all", "run_backup --all --retry 3")}
	h.SetAutoCorrectClient(stub)
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-patch", Name: "patch-job", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "patch_cap", ScriptName: "patch_cap", AutoCorrect: true,
		Project: "/srv/los32d/job",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-patch", JobName: "patch-job", CapName: "patch_cap", ScriptName: "patch_cap",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "run_backup: command not found",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if got := stub.callCount(); got != 1 {
		t.Fatalf("the fix pass must ask the model once, got %d calls", got)
	}
	if prompt := stub.lastPrompt(); strings.Contains(prompt, "set -eu") || strings.Contains(prompt, "exit 0") {
		t.Errorf("the prompt carried the script body: %q", prompt)
	}

	caps, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	var patched string
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "patch_cap" {
			continue
		}
		for _, sc := range m.Scripts {
			if sc.Runtime == "bash" {
				patched = sc.Body
			}
		}
	}
	if patched != "#!/bin/bash\nset -eu\nrun_backup --all --retry 3\nexit 0\n" {
		t.Errorf("the patched body is not the live body with one block swapped: %q", patched)
	}
}

// A patch that does not fit the live script must not be written anywhere: the
// pass cannot tell what it would overwrite, so it aborts with the reason.
func TestAutoCorrect_AbortsWhenThePatchDoesNotFitTheScript(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nrun_backup --all\nexit 0\n"
	seedCap(t, h, "nomatch_cap", "", body, []string{"nomatch_cap"})

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("run_restore --all", "run_restore --all --retry 3")})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-nomatch", Name: "nomatch-job", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "nomatch_cap", ScriptName: "nomatch_cap", AutoCorrect: true,
		Project: "/srv/los32d/nomatch",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-nomatch", JobName: "nomatch-job", CapName: "nomatch_cap", ScriptName: "nomatch_cap",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "run_backup: command not found",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	caps, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "nomatch_cap" {
			continue
		}
		if m.Version != 1 {
			t.Errorf("a patch that does not fit must not be saved, version=%d", m.Version)
		}
		if m.Scripts[0].Body != body {
			t.Errorf("the live body was touched: %q", m.Scripts[0].Body)
		}
	}
	proposals, err := s.GetActiveLearnings("cap_proposed", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	if len(proposals) != 0 {
		t.Errorf("an unappliable patch must not be staged for review, got %d proposals", len(proposals))
	}
	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/nomatch")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var reported bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "auto-correct aborted") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the abort must be reported with its reason; got %d broadcasts", len(msgs))
	}
}

// A model that declines, because the excerpt does not show enough, must leave the
// cap alone and say why.
func TestAutoCorrect_AbortsWhenTheModelDeclines(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nrun_backup --all\nexit 0\n"
	seedCap(t, h, "declined_cap", "", body, []string{"declined_cap"})

	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: "UNFIXABLE: the excerpt does not name the missing file"})
	h.autoCorrectVerify = autoCorrectPassVerifier(t)
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-declined", Name: "declined-job", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "declined_cap", ScriptName: "declined_cap", AutoCorrect: true,
		Project: "/srv/los32d/declined",
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-declined", JobName: "declined-job", CapName: "declined_cap", ScriptName: "declined_cap",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	caps, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	for _, l := range caps {
		m, err := ParseCapMeta(l.Context)
		if err != nil || m.Name != "declined_cap" {
			continue
		}
		if m.Version != 1 {
			t.Errorf("a declined fix must not be saved, version=%d", m.Version)
		}
	}
	msgs, err := s.GetUnreadBroadcasts("test-session", "/srv/los32d/declined")
	if err != nil {
		t.Fatalf("get broadcasts: %v", err)
	}
	var reported bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "does not name the missing file") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the decline and its reason must be broadcast; got %d broadcasts", len(msgs))
	}
}
