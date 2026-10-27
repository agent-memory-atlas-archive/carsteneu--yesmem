package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLos68PolicyRuleBlockOnShellSurvives covers YM-469: the Decision Policy in
// RULES.md permits a BLOCK on objectively detectable input alone for rules 1
// (LLM signature), 2 (secret), 25 (destructive command) and 32 (PR merge via
// CLI/API). Those four fire on shell calls, so the Go guard has to honour them
// on Bash/REPL instead of downgrading every one of them to a SUGGEST.
func TestLos68PolicyRuleBlockOnShellSurvives(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		violation string
	}{
		{"rule 25 destructive command", "Bash", "Rule 25: rm -rf / destroys the filesystem"},
		{"rule 32 PR merge via CLI", "Bash", "Rule 32: gh pr merge 1 merges a pull request"},
		{"rule 2 secret on the command line", "Bash", "Rule 2: the command line carries an API key"},
		{"rule 1 LLM signature in the commit message", "Bash", "Rule 1: the commit message carries a Claude signature"},
		{"a citation list naming a policy rule", "Bash", "Rules 2 and 4: secret plus push to main"},
		{"a hash-prefixed rule number", "Bash", "Rule #32: merge via API"},
		{"REPL is shell work as well", "REPL", "Rule 25: rm -rf /"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := GuardDecision{Decision: "BLOCK", Violations: []string{tc.violation}}
			got := downgradeUnauthorizedBlock(d, tc.tool)
			if got.Decision != "BLOCK" {
				t.Errorf("%s BLOCK citing a policy rule must survive, got %s (%q)", tc.tool, got.Decision, got.Suggestion)
			}
		})
	}
}

// TestLos68AdvisoryRuleBlockOnShellDowngrades keeps the narrowing honest: a
// BLOCK that rests only on rules outside the policy set stays a mandatory-check
// SUGGEST, exactly as before YM-469.
func TestLos68AdvisoryRuleBlockOnShellDowngrades(t *testing.T) {
	cases := []struct {
		name       string
		violations []string
	}{
		{"an advisory rule", []string{"Rule 14: verify before overwriting"}},
		{"two advisory rules", []string{"Rule 15: check the schema first", "Rule 3: run the tests"}},
		{"no citation at all", []string{"review against RULES.md before proceeding"}},
		{"no violations", nil},
		{"a policy number inside the reason text", []string{"Rule 14: the file mentions line 25"}},
		{"a policy number as a separate token", []string{"Rule 15: see item 32 of the checklist"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := GuardDecision{Decision: "BLOCK", Violations: tc.violations}
			got := downgradeUnauthorizedBlock(d, "Bash")
			if got.Decision != "SUGGEST" {
				t.Fatalf("expected SUGGEST, got %s", got.Decision)
			}
			if !strings.Contains(got.Suggestion, "yesmem-remember") {
				t.Errorf("expected the mandatory-check prefix, got %q", got.Suggestion)
			}
			for _, v := range tc.violations {
				if !strings.Contains(got.Suggestion, v) {
					t.Errorf("violation %q should be preserved in %q", v, got.Suggestion)
				}
			}
		})
	}
}

// TestLos68WriteEditBlockUnaffected pins acceptance 6: the citation gate is a
// shell gate. A file write keeps blocking on any rule the model cites.
func TestLos68WriteEditBlockUnaffected(t *testing.T) {
	for _, tool := range []string{"Write", "Edit"} {
		d := GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 14: verify before overwriting"}}
		if got := downgradeUnauthorizedBlock(d, tool); got.Decision != "BLOCK" {
			t.Errorf("%s BLOCK must stay honoured, got %s", tool, got.Decision)
		}
	}
}

// los68HookOutput is the PreToolUse hook schema the guard prints.
type los68HookOutput struct {
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		AdditionalContext        string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// TestLos68BashPolicyBlockReachesDenyJSON proves the honoured BLOCK actually
// leaves the guard as a deny for the host: same composition RunGuard runs
// (downgrade, then formatGuardOutput).
func TestLos68BashPolicyBlockReachesDenyJSON(t *testing.T) {
	d := downgradeUnauthorizedBlock(
		GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 25: rm -rf / destroys the filesystem"}},
		"Bash",
	)
	raw := formatGuardOutput(d)
	if raw == "" {
		t.Fatal("expected hook output for an honoured BLOCK, got silence")
	}
	var out los68HookOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("hook output is not JSON: %v (%s)", err, raw)
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("expected hookEventName PreToolUse, got %q", out.HookSpecificOutput.HookEventName)
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected permissionDecision deny, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	if out.Decision != "block" {
		t.Errorf("expected the legacy decision field to stay block, got %q", out.Decision)
	}
	if !strings.Contains(out.Reason, "Rule 25") {
		t.Errorf("expected the citation in the reason, got %q", out.Reason)
	}

	// The downgraded case must stay a context notice, never a deny.
	d = downgradeUnauthorizedBlock(
		GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 14: verify before overwriting"}},
		"Bash",
	)
	raw = formatGuardOutput(d)
	out = los68HookOutput{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("SUGGEST hook output is not JSON: %v (%s)", err, raw)
	}
	if out.HookSpecificOutput.PermissionDecision != "" {
		t.Errorf("a downgraded BLOCK must not deny, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	if out.HookSpecificOutput.AdditionalContext == "" {
		t.Error("a downgraded BLOCK must reach the model as additionalContext")
	}
}

// TestLos68CitedRuleNumbers pins the citation parse. Only numbers directly
// behind "rule"/"rules" are citations; digits in the reason text are not.
func TestLos68CitedRuleNumbers(t *testing.T) {
	cases := []struct {
		name      string
		violation string
		want      []int
	}{
		{"single citation", "Rule 25: destructive", []int{25}},
		{"hash prefix", "Rule #32: merge", []int{32}},
		{"lower case", "rule 2: secret", []int{2}},
		{"hyphen instead of space", "Rule-25: destructive", []int{25}},
		{"abbreviated number", "Rule no. 25: destructive", []int{25}},
		{"capitalised abbreviation", "Rule No. 25: destructive", []int{25}},
		{"capitalised spelled-out number", "Rule NUMBER 25: destructive", []int{25}},
		{"spelled-out number", "Rule number 25: destructive", []int{25}},
		{"plural with and", "Rules 2 and 4: secret plus push", []int{2, 4}},
		{"plural comma list", "Rules 2, 4 and 25: several", []int{2, 4, 25}},
		{"ampersand list", "Rules 1 & 32: signature plus merge", []int{1, 32}},
		{"digits in the reason stay out", "Rule 14: the file mentions line 25", []int{14}},
		{"digits later in the reason stay out", "Rule 15: see item 32 of the checklist", []int{15}},
		{"a comma behind a singular citation is prose", "Rule 14, 25 characters exceed the limit", []int{14}},
		{"two separate citations", "Rule 25: destructive. Rule 2: secret", []int{25, 2}},
		// Documented limit: a second rule word inside the reason counts as a
		// citation. It over-blocks (never under-blocks), and separating "per rule
		// 25" from a real second citation needs semantics.
		{"a second rule word in the reason counts", "Rule 26: destructive per rule 25", []int{26, 25}},
		{"no citation", "review against RULES.md before proceeding", nil},
		{"the word without a number", "Rules apply here", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := citedRuleNumbers(tc.violation)
			if len(got) != len(tc.want) {
				t.Fatalf("citedRuleNumbers(%q) = %v, want %v", tc.violation, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("citedRuleNumbers(%q) = %v, want %v", tc.violation, got, tc.want)
				}
			}
		})
	}
}

// TestLos68BlockOptionPerTool pins what the prompt offers each tool. Offering
// shell the unrestricted BLOCK option would invite decisions the Go level has to
// downgrade again.
func TestLos68BlockOptionPerTool(t *testing.T) {
	for _, tool := range []string{"Write", "Edit"} {
		opt := blockOption(tool)
		if !strings.Contains(opt, "BLOCK") {
			t.Errorf("%s must be offered BLOCK, got %q", tool, opt)
		}
		if strings.Contains(opt, "rule 1") || strings.Contains(opt, "never a BLOCK") {
			t.Errorf("%s must keep the unrestricted option, got %q", tool, opt)
		}
	}
	for _, tool := range []string{"Bash", "REPL"} {
		opt := blockOption(tool)
		for _, want := range []string{"BLOCK", "rule 1", "rule 2", "rule 25", "rule 32", "never a BLOCK", "Rule X"} {
			if !strings.Contains(opt, want) {
				t.Errorf("%s option must name %q, got %q", tool, want, opt)
			}
		}
		// The example violation must not carry a real rule number: a model that
		// copies the shown shape would otherwise satisfy the Go gate for any
		// violation at all.
		for _, number := range []string{"Rule 25:", "Rule 1:", "Rule 2:", "Rule 32:"} {
			if strings.Contains(opt, number) {
				t.Errorf("%s option shows the literal citation %q, got %q", tool, number, opt)
			}
		}
	}
	if opt := blockOption("Grep"); opt != "" {
		t.Errorf("a tool the guard never blocks must not be offered BLOCK, got %q", opt)
	}
}

// TestLos68RuleOneAutoCommitBlockIsAccepted pins the one asymmetry the set
// knowingly carries. RULES.md puts "auto-commit detection (rule 1)" in the
// SUGGEST bullet and "LLM signature in commit message text (part of rule 1)" in
// the BLOCK bullet, so the same citation covers both. The gate is the rule
// number and cannot separate them; the orchestrator decision on YM-469 was to
// keep rule 1 in the set, because the signature case is the one that has to
// stop, and the prompt steers the model to cite rule 1 for it only. A pinned
// asymmetry beats an unpinned one: without this test the next refactor picks a
// side silently.
func TestLos68RuleOneAutoCommitBlockIsAccepted(t *testing.T) {
	d := GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 1: auto-commit without explicit user instruction"}}
	if got := downgradeUnauthorizedBlock(d, "Bash"); got.Decision != "BLOCK" {
		t.Fatalf("rule 1 is in the policy set and must be enforced, got %s", got.Decision)
	}
}

// TestLos68RunGuardShellPolicyBlockEmitsDeny drives the whole hook, not just the
// composition: RunGuard must turn a policy citation on Bash into the deny JSON
// the host reads. Without this, a post-processor placed between the gate and the
// output could swallow the deny while every unit test stayed green.
func TestLos68RunGuardShellPolicyBlockEmitsDeny(t *testing.T) {
	server := guardDecisionServer(t, `{"decision":"BLOCK","violations":["Rule 32: gh pr merge merges a pull request"]}`)
	defer server.Close()
	dataDir := guardEnv(t, server.URL)
	out := runGuardCapture(t, dataDir,
		`{"tool_name":"Bash","tool_input":{"command":"gh pr merge 1"}}`)
	var parsed los68HookOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid json: %v (out=%q)", err, out)
	}
	if parsed.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected permissionDecision deny, got %q (out=%q)", parsed.HookSpecificOutput.PermissionDecision, out)
	}
	if !strings.Contains(parsed.HookSpecificOutput.PermissionDecisionReason, "Rule 32") {
		t.Errorf("expected the citation in the deny reason, got %q", parsed.HookSpecificOutput.PermissionDecisionReason)
	}
}

// TestLos68RunGuardShellAdvisoryBlockStaysSuggest is the same run for the
// downgraded half: an advisory citation must reach the model as a notice and
// must not deny.
func TestLos68RunGuardShellAdvisoryBlockStaysSuggest(t *testing.T) {
	server := guardDecisionServer(t, `{"decision":"BLOCK","violations":["Rule 14: verify before overwriting"]}`)
	defer server.Close()
	dataDir := guardEnv(t, server.URL)
	out := runGuardCapture(t, dataDir,
		`{"tool_name":"Bash","tool_input":{"command":"git status"}}`)
	if strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("an advisory citation must not deny, got %q", out)
	}
	var parsed los68HookOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid json: %v (out=%q)", err, out)
	}
	if !strings.Contains(parsed.HookSpecificOutput.AdditionalContext, "Rule 14") {
		t.Errorf("expected the citation in additionalContext, got %q", parsed.HookSpecificOutput.AdditionalContext)
	}
}

// policyRulesFromDecisionPolicy returns the rule numbers the Decision Policy's
// BLOCK bullet names, with continuation lines joined: a reflowed bullet must not
// hide a rule (the plugin treats wrapping as a real risk for the same reason,
// rule_guard.ts blockingRuleNumbers).
func policyRulesFromDecisionPolicy(t *testing.T, rules string) []int {
	t.Helper()
	start := strings.Index(rules, "## Decision Policy")
	if start < 0 {
		t.Fatal("no Decision Policy in the rules text")
	}
	section := rules[start:]
	if next := strings.Index(section[1:], "\n## "); next >= 0 {
		section = section[:next+1]
	}

	var numbers []int
	var bullet strings.Builder
	flush := func() {
		text := bullet.String()
		bullet.Reset()
		if text == "" || !strings.Contains(strings.ToUpper(text), "BLOCK") {
			return
		}
		numbers = append(numbers, citedRuleNumbers(text)...)
	}
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			flush()
			bullet.WriteString(trimmed)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if bullet.Len() > 0 {
			bullet.WriteString(" ")
			bullet.WriteString(trimmed)
		}
	}
	flush()
	return numbers
}

// TestLos68PolicyBulletParseJoinsWrappedLines pins the parser the drift pin
// relies on: a BLOCK bullet wrapped over several lines keeps every rule.
func TestLos68PolicyBulletParseJoinsWrappedLines(t *testing.T) {
	wrapped := `## Decision Policy

- **BLOCK** only on objectively detectable violations: LLM signature (rule 1),
  secrets in diff (rule 2),
  destructive bash patterns (rule 25),
  PR merge via CLI or API (rule 32).
- **SUGGEST** on rules where authorization matters: auto-commit (rule 3).

## Commits
`
	got := policyRulesFromDecisionPolicy(t, wrapped)
	want := []int{1, 2, 25, 32}
	if len(got) != len(want) {
		t.Fatalf("wrapped BLOCK bullet parsed to %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("wrapped BLOCK bullet parsed to %v, want %v", got, want)
		}
	}
}

// TestLos68PolicySetMatchesRulesFile keeps the Go set and the Decision Policy in
// step: the policy is repository content, and a rule moved into or out of its
// BLOCK bullet must force a change here. RULES.md is excluded from the public
// mirror (scripts/sync-public.sh), so an absent file skips instead of failing.
func TestLos68PolicySetMatchesRulesFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "RULES.md"))
	if err != nil {
		t.Skipf("RULES.md not available: %v", err)
	}
	policyRules := policyRulesFromDecisionPolicy(t, string(data))

	seen := map[int]bool{}
	for _, number := range policyRules {
		seen[number] = true
		if !policyBlockRules[number] {
			t.Errorf("the Decision Policy blocks on rule %d, policyBlockRules does not", number)
		}
	}
	for number := range policyBlockRules {
		if !seen[number] {
			t.Errorf("policyBlockRules holds rule %d, the Decision Policy BLOCK bullet no longer names it", number)
		}
	}
}
