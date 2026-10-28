package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/setup"
	"gopkg.in/yaml.v3"
)

// suggestCooldownTTL is the minimum time between identical SUGGESTs for the
// same skill. Repeats within this window are silenced to keep system-reminder
// noise tolerable across long sessions.
const suggestCooldownTTL = 10 * time.Minute

// canBlockTools is the set of tool names where a BLOCK decision is honoured
// unconditionally: a file write is judged on its content alone.
var canBlockTools = map[string]bool{
	"Write": true,
	"Edit":  true,
}

// shellTools are the tools that carry a command. Their BLOCK is not honoured
// unconditionally: the guard has no conversation context and cannot tell whether
// the user authorised a Bash/REPL action this turn, so only a citation the
// Decision Policy classifies as objectively detectable is enforced (policyBlockRules).
var shellTools = map[string]bool{
	"Bash": true,
	"REPL": true,
}

// policyBlockRules are the rule numbers the Decision Policy in RULES.md allows
// the guard to BLOCK on from the tool input alone: LLM signature in commit
// message text (1), secret in the call (2), destructive command (25), PR merge
// via CLI or API (32). Everything else is a SUGGEST there.
//
// Rule 1 carries a sub-case this set cannot separate: the policy puts
// "auto-commit detection" in the SUGGEST bullet, because only the conversation
// says whether the user asked for the commit. The gate is the rule number, so an
// auto-commit BLOCK that cites rule 1 is enforced; the prompt steers the model
// to cite rule 1 for the signature case only. TestLos68PolicySetMatchesRulesFile
// fails when RULES.md moves a rule into or out of the BLOCK bullet.
var policyBlockRules = map[int]bool{
	1:  true,
	2:  true,
	25: true,
	32: true,
}

// ruleCitation matches the word that introduces a rule citation, with the
// singular/plural form kept as a submatch.
var ruleCitation = regexp.MustCompile(`(?i)\b(rules?)\b`)

// citationHead captures the number list that follows a rule word: "Rule 25",
// "Rule #32", "Rule No. 25", "Rule-25", "Rules 2, 4 and 25". Case-insensitive
// for the qualifier too: the model writes "Rule No. 25" as readily as "Rule no.
// 25", and a lost citation is a lost block. RE2 has no backtracking, so the list
// pattern cannot blow up the way a PCRE one would.
var citationHead = regexp.MustCompile(`(?i)^[\s#]*(?:number|no\.?|nr\.?)?[\s\-]*(#?\d+(?:(?:\s*,\s*|\s+and\s+|\s*&\s*)#?\d+)*)`)

var citationDigits = regexp.MustCompile(`\d+`)

// citedRuleNumbers returns the rule numbers a violation cites in the positions
// that make them citations. Digits that merely sit in the reason text are not
// citations: "Rule 14: the file mentions line 25" cites only 14.
func citedRuleNumbers(violation string) []int {
	var numbers []int
	matches := ruleCitation.FindAllStringSubmatchIndex(violation, -1)
	for _, match := range matches {
		head := citationHead.FindStringSubmatch(violation[match[1]:])
		if head == nil {
			continue
		}
		list := head[1]
		// A comma list belongs to the plural form ("Rules 2, 4 and 25"). Behind
		// a singular citation the comma is prose ("Rule 14, 25 characters exceed
		// the limit"), and reading the number after it would cite a policy rule
		// the model never named.
		if !strings.EqualFold(violation[match[2]:match[3]], "rules") {
			if comma := strings.Index(list, ","); comma >= 0 {
				list = list[:comma]
			}
		}
		for _, digits := range citationDigits.FindAllString(list, -1) {
			if n, err := strconv.Atoi(digits); err == nil {
				numbers = append(numbers, n)
			}
		}
	}
	return numbers
}

// citesPolicyBlockRule reports whether a violation cites at least one rule the
// policy lets the guard judge alone. One is enough: a violation naming a policy
// rule among advisory ones still describes an act the policy calls blockable,
// and downgrading it would let exactly that act through.
func citesPolicyBlockRule(violations []string) bool {
	for _, violation := range violations {
		for _, number := range citedRuleNumbers(violation) {
			if policyBlockRules[number] {
				return true
			}
		}
	}
	return false
}

// downgradeUnauthorizedBlock converts a BLOCK decision to a mandatory-check
// SUGGEST when the tool may not block on the cited rules. Write/Edit always may;
// shell tools only on a policyBlockRules citation. The original violations are
// preserved in the suggestion text so the user sees what triggered the check.
func downgradeUnauthorizedBlock(d GuardDecision, toolName string) GuardDecision {
	if d.Decision != "BLOCK" {
		return d
	}
	if canBlockTools[toolName] || (shellTools[toolName] && citesPolicyBlockRule(d.Violations)) {
		return d
	}
	reason := strings.Join(d.Violations, "; ")
	if reason == "" {
		reason = "review against RULES.md before proceeding"
	}
	return GuardDecision{
		Decision:   "SUGGEST",
		Suggestion: "yesmem-remember: mandatory check — " + reason,
	}
}

// GuardDecision is the JSON output from the guard.
type GuardDecision struct {
	Decision   string   `json:"decision"`             // BLOCK, SUGGEST, PASS
	Violations []string `json:"violations,omitempty"` // rule violations (BLOCK)
	Suggestion string   `json:"suggestion,omitempty"` // skill name (SUGGEST)
}

// formatGuardOutput converts a GuardDecision into the Claude Code PreToolUse
// hook output schema so SUGGEST/BLOCK reach the model as system-reminders.
// PASS and ill-formed decisions return "" (silent — no stdout, exit 0),
// which Claude Code treats as "no decision; normal permission flow applies".
func formatGuardOutput(d GuardDecision) string {
	switch d.Decision {
	case "SUGGEST":
		if d.Suggestion == "" {
			return ""
		}
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PreToolUse",
				"additionalContext": d.Suggestion,
			},
		}
		b, _ := json.Marshal(out)
		return string(b)
	case "BLOCK":
		reason := strings.Join(d.Violations, "; ")
		if reason == "" {
			reason = "blocked by RULES.md"
		}
		out := map[string]any{
			// Legacy decision field retained for older Claude Code versions;
			// modern versions prefer hookSpecificOutput.permissionDecision.
			"decision": "block",
			"reason":   reason,
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": reason,
				"additionalContext":        reason,
			},
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	return ""
}

// guardConfig holds the resolved configuration for the API call.
type guardConfig struct {
	Model    string
	APIURL   string
	APIKey   string
	APIType  string // "opencode" or "anthropic"
}

// ConfigLite is a minimal config struct for field access.
type ConfigLite struct {
	Extraction struct {
		Model string `yaml:"model"`
	} `yaml:"extraction"`
}

// authEntry represents a provider entry in auth.json.
type authEntry struct {
	Key string `json:"key"`
}

// FIRST_PARTY_DEFAULTS maps provider IDs to their default API endpoints.
var firstPartyDefaults = map[string]string{
	"openai":    "https://api.openai.com/v1",
	"deepseek":  "https://api.deepseek.com",
	"mistral":   "https://api.mistral.ai/v1",
	"anthropic": "https://api.anthropic.com",
	"google":    "https://generativelanguage.googleapis.com",
}

// guardCacheEntry holds a cached guard result.
type guardCacheEntry struct {
	Decision GuardDecision
	TS       time.Time
}

var (
	guardCacheMu sync.RWMutex
	guardCache   = make(map[string]guardCacheEntry)
	guardTTL     = 5 * time.Second
)

// RunGuard reads PreToolUse JSON from stdin, evaluates against RULES.md
// via DeepSeek, and outputs a GuardDecision on stdout.
// Always exits 0: BLOCK is signaled via hookSpecificOutput.permissionDecision
// ("deny"), SUGGEST via additionalContext, PASS via silence (no stdout).
// An exit 2 would make Claude Code ignore the JSON entirely.
func RunGuard(dataDir string) {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var hook HookInput
	if json.Unmarshal(input, &hook) != nil {
		return
	}

	// Only evaluate Bash, REPL, Edit, Write
	switch hook.ToolName {
	case "Bash", "REPL", "Edit", "Write":
		// continue
	default:
		return
	}

	// Parse tool input to get a human-readable description
	toolDesc := describeToolCall(&hook)

	// Resolve guard config
	cfg, err := resolveGuardConfig(dataDir)
	if err != nil || cfg.APIKey == "" {
		return
	}

	// Load RULES.md
	rulesPath := filepath.Join(dataDir, "..", "..", "memory", "yesmem", "RULES.md")
	rules := loadRulesFile(rulesPath, hook.CWD)
	if rules == "" {
		return
	}

	// Filter out catalog entries for skills that aren't loadable on this
	// machine (ghost dirs without SKILL.md). Prevents false-positive
	// SUGGESTs naming skills the agent cannot activate. No-op if the
	// filesystem scan fails — pass through conservatively.
	rules = setup.FilterStaleSkillRules(rules, setup.LoadAgentLoadableSkillNames())

	// Hardcoded pre-check for destructive bash/REPL patterns: bypass DeepSeek
	// entirely and BLOCK before any model roundtrip. These patterns describe
	// commands no plausible workflow needs (rm -rf /, force-push to main,
	// DROP TABLE, etc.) so a false positive is cheaper than a missed block.
	// Blocking happens via the deny JSON on exit 0 — an exit 2 would cause
	// Claude Code to ignore the JSON entirely.
	if hook.ToolName == "Bash" || hook.ToolName == "REPL" {
		if patName := matchDestructivePattern(toolDesc); patName != "" {
			decision := GuardDecision{
				Decision:   "BLOCK",
				Violations: []string{"destructive pattern: " + patName},
			}
			if out := formatGuardOutput(decision); out != "" {
				fmt.Println(out)
			}
			return
		}
	}

	// Evaluate via DeepSeek (with retry)
	decision := evaluateGuard(cfg, rules, toolDesc, hook.ToolName)

	// Enforce the block set at the Go level: Write/Edit may block on any rule,
	// shell tools only on a Decision Policy rule (1, 2, 25, 32). A BLOCK naming
	// any other rule is downgraded to a mandatory-check SUGGEST, so shell calls
	// reach the user instead of dying silently.
	decision = downgradeUnauthorizedBlock(decision, hook.ToolName)

	cooldownPath := filepath.Join(dataDir, "guard_state.db")

	// Drop suggestions for skills that aren't loadable on this machine
	// (ghost dirs without SKILL.md). Runs before the cooldown check so an
	// unavailable suggestion doesn't get recorded as fired. Conservative:
	// empty available set (filesystem scan failed) = pass through.
	decision = maybeSuppressUnavailableSkill(decision, setup.LoadAgentLoadableSkillNames())

	if maybeSuppressSuggestion(decision, cooldownPath, suggestCooldownTTL, time.Now()) {
		return
	}

	if hookOut := formatGuardOutput(decision); hookOut != "" {
		fmt.Println(hookOut)
	}
}

// describeToolCall returns a human-readable description of the proposed tool call.
func describeToolCall(hook *HookInput) string {
	switch hook.ToolName {
	case "Bash":
		var b BashInput
		if json.Unmarshal(hook.ToolInput, &b) == nil {
			return "Bash: " + b.Command
		}
	case "REPL":
		var r REPLInput
		if json.Unmarshal(hook.ToolInput, &r) == nil {
			return "REPL: " + r.Code
		}
	case "Edit", "Write":
		var f FileInput
		if json.Unmarshal(hook.ToolInput, &f) == nil {
			return hook.ToolName + ": " + f.FilePath
		}
	}
	return hook.ToolName
}

// loadRulesFile tries to load RULES.md from the project CWD first,
// falling back to the given path.
func loadRulesFile(fallbackPath, cwd string) string {
	paths := []string{}
	if cwd != "" {
		paths = append(paths, filepath.Join(cwd, "RULES.md"))
	}
	paths = append(paths, fallbackPath)
	// Also try relative to dataDir
	home := os.Getenv("HOME")
	if home != "" {
		paths = append(paths, filepath.Join(home, "memory", "yesmem", "RULES.md"))
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil && len(data) > 0 {
			return string(data)
		}
	}
	return ""
}

// resolveGuardConfig tries OpenCode config first, then falls back to Anthropic.
func resolveGuardConfig(dataDir string) (*guardConfig, error) {
	cfg, err := resolveOpenCodeConfig(dataDir)
	if err == nil && cfg.APIKey != "" {
		return cfg, nil
	}

	cfg, err = resolveAnthropicConfig()
	if err == nil && cfg.APIKey != "" {
		return cfg, nil
	}

	return nil, fmt.Errorf("no guard config found (tried OpenCode + Anthropic)")
}

// resolveOpenCodeConfig resolves model, apiUrl, and apiKey using the same
// provider-agnostic logic as the OpenCode plugin:
//  1. Read extraction.model from config.yaml (fallback: deepseek-v4-flash)
//  2. Find provider in models.json that hosts the model AND has a key in auth.json
//  3. Fall back to first match without key
func resolveOpenCodeConfig(dataDir string) (*guardConfig, error) {
	cfgPath := filepath.Join(dataDir, "config.yaml")
	cfg, err := loadConfigYAML(cfgPath)
	if err != nil {
		return nil, err
	}

	model := "deepseek-v4-flash"
	if cfg.Extraction.Model != "" {
		model = cfg.Extraction.Model
	}

	modelsPath := filepath.Join(os.Getenv("HOME"), ".cache", "opencode", "models.json")
	models, err := loadModelsJSON(modelsPath)
	if err != nil {
		return nil, err
	}

	authPath := filepath.Join(os.Getenv("HOME"), ".local", "share", "opencode", "auth.json")
	auth, err := loadAuthJSON(authPath)
	if err != nil {
		auth = make(map[string]authEntry)
	}

	apiURL := "https://api.deepseek.com"
	apiKey := ""
	var fallbackURL string

	for providerID, provider := range models {
		providerModels, ok := provider["models"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, hasModel := providerModels[model]; !hasModel {
			continue
		}
		url := ""
		if u, ok := provider["api"].(string); ok && u != "" {
			url = u
		} else if def, ok := firstPartyDefaults[providerID]; ok {
			url = def
		}
		key := ""
		if a, ok := auth[providerID]; ok {
			key = a.Key
		}
		if key != "" {
			apiURL = url
			apiKey = key
			break
		}
		if fallbackURL == "" {
			fallbackURL = url
		}
	}

	if apiKey == "" && fallbackURL != "" {
		apiURL = fallbackURL
		// Note: RunGuard gates on apiKey == "" and returns silently, so this
		// fallback URL is not currently used. It's preserved for future
		// scenarios where local/unauthenticated models are supported.
	}

	return &guardConfig{
		Model:   model,
		APIURL:  apiURL + "/v1/chat/completions",
		APIKey:  apiKey,
		APIType: "opencode",
	}, nil
}

func loadAuthJSON(path string) (map[string]authEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result map[string]authEntry
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func loadModelsJSON(path string) (map[string]map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result map[string]map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func loadConfigYAML(path string) (*ConfigLite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ConfigLite
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// claudeConfig matches ~/.claude/config.json primaryApiKey field.
type claudeConfig struct {
	PrimaryAPIKey string `json:"primaryApiKey"`
}

// resolveAnthropicConfig finds an Anthropic API key from:
//  1. ANTHROPIC_API_KEY env var
//  2. ~/.claude/config.json primaryApiKey
//  3. ~/.claude.json primaryApiKey
func resolveAnthropicConfig() (*guardConfig, error) {
	home, _ := os.UserHomeDir()

	// 1. Env var
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return &guardConfig{
			Model:   "claude-3-haiku-20240307",
			APIURL:  "https://api.anthropic.com/v1/messages",
			APIKey:  key,
			APIType: "anthropic",
		}, nil
	}

	// 2. ~/.claude/config.json
	cfgPath := filepath.Join(home, ".claude", "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		var cc claudeConfig
		if json.Unmarshal(data, &cc) == nil && cc.PrimaryAPIKey != "" {
			return &guardConfig{
				Model:   "claude-3-haiku-20240307",
				APIURL:  "https://api.anthropic.com/v1/messages",
				APIKey:  cc.PrimaryAPIKey,
				APIType: "anthropic",
			}, nil
		}
	}

	// 3. ~/.claude.json
	claudeJSONPath := filepath.Join(home, ".claude.json")
	if data, err := os.ReadFile(claudeJSONPath); err == nil {
		var cc claudeConfig
		if json.Unmarshal(data, &cc) == nil && cc.PrimaryAPIKey != "" {
			return &guardConfig{
				Model:   "claude-3-haiku-20240307",
				APIURL:  "https://api.anthropic.com/v1/messages",
				APIKey:  cc.PrimaryAPIKey,
				APIType: "anthropic",
			}, nil
		}
	}

	return nil, fmt.Errorf("no Anthropic API key found")
}

// evaluateGuard sends the rules + tool description to the configured LLM and returns a decision.
func evaluateGuard(cfg *guardConfig, rules, toolDesc, toolName string) GuardDecision {
	if cfg.APIType == "anthropic" {
		return callAnthropicGuard(cfg, rules, toolDesc, toolName)
	}
	return callOpenCodeGuard(cfg, rules, toolDesc, toolName)
}

// callOpenCodeGuard sends the prompt to an OpenAI-compatible API (DeepSeek, etc.).
func callOpenCodeGuard(cfg *guardConfig, rules, toolDesc, toolName string) GuardDecision {
	// Cache key: hash of rules + tool + context
	cacheKey := hashStrings(rules, toolDesc, toolName)

	guardCacheMu.RLock()
	if entry, ok := guardCache[cacheKey]; ok && time.Since(entry.TS) < guardTTL {
		guardCacheMu.RUnlock()
		return entry.Decision
	}
	guardCacheMu.RUnlock()

	// Build prompt
	systemPrompt := "You analyze a proposed tool call against rules. Output ONLY valid JSON. No explanation."
	userPrompt := buildGuardPrompt(rules, toolDesc, toolName)

	reqBody := map[string]interface{}{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature":     0,
		"max_tokens":      4096,
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
	}

	bodyBytes, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 15 * time.Second}

	// Try up to 2 times
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest("POST", cfg.APIURL, bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := client.Do(req)
		if err != nil {
			if attempt == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			break
		}

		if resp.StatusCode != 200 {
			// Drain body to return connection to keep-alive pool
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if attempt == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			break
		}

		var apiResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.NewDecoder(resp.Body).Decode(&apiResp) == nil && len(apiResp.Choices) > 0 {
			content := apiResp.Choices[0].Message.Content
			// Strip markdown code fences if present
			content = stripCodeFences(content)

			var decision GuardDecision
			if json.Unmarshal([]byte(content), &decision) == nil {
				resp.Body.Close()
				guardCacheMu.Lock()
				guardCache[cacheKey] = guardCacheEntry{Decision: decision, TS: time.Now()}
				guardCacheMu.Unlock()
				return decision
			}
		}
		resp.Body.Close()
		// Valid JSON response on first success — no retry needed
		break
	}

	return GuardDecision{Decision: "PASS"}
}

// callAnthropicGuard sends the prompt to Anthropic API (v1/messages).
func callAnthropicGuard(cfg *guardConfig, rules, toolDesc, toolName string) GuardDecision {
	cacheKey := hashStrings(rules, toolDesc, toolName)

	guardCacheMu.RLock()
	if entry, ok := guardCache[cacheKey]; ok && time.Since(entry.TS) < guardTTL {
		guardCacheMu.RUnlock()
		return entry.Decision
	}
	guardCacheMu.RUnlock()

	systemPrompt := "You analyze a proposed tool call against rules. Output ONLY valid JSON. No explanation."
	userPrompt := buildGuardPrompt(rules, toolDesc, toolName)

	// Anthropic messages format (no separate system key — use system field)
	reqBody := map[string]interface{}{
		"model":       cfg.Model,
		"max_tokens":  4096,
		"temperature": 0,
		"system":      systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	bodyBytes, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: 15 * time.Second}

	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest("POST", cfg.APIURL, bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := client.Do(req)
		if err != nil {
			if attempt == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			break
		}

		if resp.StatusCode != 200 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if attempt == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			break
		}

		var apiResp struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.NewDecoder(resp.Body).Decode(&apiResp) == nil && len(apiResp.Content) > 0 {
			content := stripCodeFences(apiResp.Content[0].Text)

			var decision GuardDecision
			if json.Unmarshal([]byte(content), &decision) == nil {
				resp.Body.Close()
				guardCacheMu.Lock()
				guardCache[cacheKey] = guardCacheEntry{Decision: decision, TS: time.Now()}
				guardCacheMu.Unlock()
				return decision
			}
		}
		resp.Body.Close()
		break
	}

	return GuardDecision{Decision: "PASS"}
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// stripCodeFences removes markdown code fence markers from LLM output.
func stripCodeFences(content string) string {
	re := regexp.MustCompile("(?s)^```(?:json)?\\s*\n?(.*?)\n?```\\s*$")
	if matches := re.FindStringSubmatch(content); matches != nil {
		return strings.TrimSpace(matches[1])
	}
	return strings.TrimSpace(content)
}

// blockOption is the BLOCK line the prompt offers for this tool. For shell tools
// it is narrowed to the rules the Go level enforces (policyBlockRules): offering
// them the unrestricted option would invite BLOCKs the guard has to downgrade
// again, and the model cannot know where the line runs unless the prompt says so.
func blockOption(toolName string) string {
	switch {
	case canBlockTools[toolName]:
		return `- BLOCK: call violates a rule → {"decision":"BLOCK","violations":["Rule X: reason"]}` + "\n"
	case shellTools[toolName]:
		return `- BLOCK: the command itself violates rule 1 (LLM signature in a commit message), rule 2 (secret on the command line), rule 25 (destructive command) or rule 32 (PR merge via CLI or API) → {"decision":"BLOCK","violations":["Rule X: reason"]}` + "\n" +
			`  Every other rule is a SUGGEST, never a BLOCK: the guard sees no conversation and cannot judge authorization.` + "\n"
	}
	return ""
}

func buildGuardPrompt(rules, toolDesc, toolName string) string {
	return fmt.Sprintf(`You analyze a proposed tool call against rules. Output ONLY valid JSON.

RULES:
%s

PROPOSED TOOL CALL: %s

Response options:
%s- SUGGEST: a skill or best practice would help → {"decision":"SUGGEST","suggestion":"SkillName: short reason max 60 chars"}
- PASS: no issue → {"decision":"PASS"}

For SUGGEST, the suggestion field MUST contain the exact skill name (from the rules) followed by ': ' and a short reason. Evaluate ALL Skill Catalog rules against the task context. Check triggers for keyword matches.`, rules, toolDesc, blockOption(toolName))
}
