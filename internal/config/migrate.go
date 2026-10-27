package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type configMigration struct {
	key     string
	snippet string
}

var proxyMigrations = []configMigration{
	{
		key: "skill_eval_inject",
		snippet: `
  # Skill evaluation injection mode.
  # "true" = forced visible evaluation every turn (verbose)
  # "silent" = evaluate internally, output only on skill match (default)
  # "false" = disable skill-eval injection entirely
  skill_eval_inject: "silent"
`,
	},
	{
		key: "effort_floor",
		snippet: `
  # Minimum effort level for model responses.
  # Options: "" (off), "low", "medium", "high", "max"
  # effort_floor: ""
`,
	},
	{
		key: "auto_configure_providers",
		snippet: `
  # Automatically discover and configure provider routing from opencode config.
  auto_configure_providers: true
`,
	},
}

const opencodeDBKey = "opencode_db"

const opencodeDBSnippet = `
  # Path to opencode's SQLite database for session indexing.
  # Default: ~/.local/share/opencode/opencode.db
  opencode_db: ~/.local/share/opencode/opencode.db
`

const modelFeaturesBlock = `
  # --- Per-Model Feature Gates ---
  # Control which yesmem behavioral features are active per model/provider.
  # Keys are model name prefixes matched case-insensitively (longest wins).
  # Models not listed fall back to feature_defaults.
  #
  # Gate reference:
  #   skill_eval               = Inject [skill-eval] block — checks which skills apply
  #   briefing                 = Inject yesmem briefing at session start
  #   rules_reminder           = Periodic reminder of project rules/guidelines
  #   plan_checkpoint          = Inject plan checkpoint reminders
  #   think_reminder           = Inject hybrid_search() hint (check memory before assuming)
  #   think_reminder_min_chars = Min user text length for the reminder; 0 = inherit from feature_defaults (0 there = always)
  #   timestamps               = Inject [HH:MM:SS] [msg:N] [+Δ] markers
  #   assoc_context            = Inject [assoc-context] from hybrid_search (frozen per msg:N)
  #   loop_warning             = Inject [loop-warning] when loop detection fires
  model_features:
    claude:
      assoc_context: true
      briefing: true
      loop_warning: true
      plan_checkpoint: true
      rules_reminder: true
      skill_eval: true
      think_reminder: true
      think_reminder_min_chars: 0
      timestamps: false
    deepseek:
      assoc_context: true
      briefing: true
      loop_warning: true
      plan_checkpoint: false
      rules_reminder: true
      skill_eval: true
      think_reminder: true
      think_reminder_min_chars: 0
      timestamps: true
    glm:
      assoc_context: true
      briefing: true
      loop_warning: true
      plan_checkpoint: false
      rules_reminder: true
      skill_eval: true
      think_reminder: true
      think_reminder_min_chars: 0
      timestamps: true
    gpt:
      assoc_context: true
      briefing: true
      loop_warning: true
      plan_checkpoint: false
      rules_reminder: true
      skill_eval: true
      think_reminder: false
      think_reminder_min_chars: 0
      timestamps: false
    openai:
      assoc_context: true
      briefing: true
      loop_warning: true
      plan_checkpoint: false
      rules_reminder: true
      skill_eval: true
      think_reminder: false
      think_reminder_min_chars: 0
      timestamps: false

  feature_defaults:
    # Fallback for models not listed above.
    # Most features on; assoc_context stays on so an unlisted model does not lose
    # the associative context that every listed model has.
    assoc_context: true
    briefing: true
    loop_warning: true
    plan_checkpoint: true
    rules_reminder: true
    skill_eval: true
    think_reminder: true
    think_reminder_min_chars: 0
    timestamps: true
`

const deepseekPricingSnippet = `
  deepseek-v4-flash: { input: 0.14, output: 0.56 }
  deepseek-v4-pro:   { input: 0.28, output: 1.12 }
`

// MigrateConfig reads an existing config.yaml and inserts any missing
// proxy-section fields, paths fields, and model_features section.
// Returns the number of fields/sections added.
func MigrateConfig(path string) (int, error) {
	// The read-modify-write runs under the write lock, like the other
	// incremental writers: an update racing a "config set" would otherwise drop
	// one of the two changes (YM-392). The installer's full rewrite does not take
	// it; it replaces the document on purpose.
	unlock := lockConfigWrite(path)
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read config: %w", err)
	}

	content := string(data)
	added := 0

	// ━━ Proxy-section migrations ━━
	if strings.Contains(content, "proxy:") {
		var toAdd []string
		for _, m := range proxyMigrations {
			if strings.Contains(content, m.key) {
				continue
			}
			toAdd = append(toAdd, m.snippet)
		}
		if len(toAdd) > 0 {
			content = insertAtEndOfSection(content, "proxy:", strings.Join(toAdd, ""))
			added += len(toAdd)
		}
	}

	// ━━ Paths-section: opencode_db ━━
	if !strings.Contains(content, opencodeDBKey) {
		if strings.Contains(content, "paths:") {
			content = insertAtEndOfSection(content, "paths:", opencodeDBSnippet)
		} else {
			content = appendToEnd(content, "\npaths:"+opencodeDBSnippet)
		}
		added++
	}

	// ━━ model_features section (inside proxy:) ━━
	if !strings.Contains(content, "model_features:") {
		if strings.Contains(content, "proxy:") {
			content = insertAtEndOfSection(content, "proxy:", modelFeaturesBlock)
		} else {
			content = appendToEnd(content, "\nproxy:\n  enabled: true"+modelFeaturesBlock)
		}
		added++
	}

	// ━━ pricing section: deepseek entries ━━
	if !strings.Contains(content, "deepseek-v4-flash") {
		if strings.Contains(content, "pricing:") {
			content = insertAtEndOfSection(content, "pricing:", deepseekPricingSnippet)
			added++
		}
	}

	// ━━ agents section: default_backend ━━
	if !strings.Contains(content, "default_backend:") && strings.Contains(content, "agents:") {
		backend := "claude"
		if strings.Contains(content, "provider: openai_compatible") || strings.Contains(content, "provider: opencode") {
			backend = "opencode"
		}
		snippet := fmt.Sprintf(`
  # Default backend for spawned agents: claude or opencode
  default_backend: %s
`, backend)
		content = insertAtEndOfSection(content, "agents:", snippet)
		added++
	}

	// ━━ exclude_projects (top-level) ━━
	if !strings.Contains(content, "exclude_projects:") {
		user := os.Getenv("USER")
		if user == "" {
			user = os.Getenv("USERNAME") // Windows fallback
		}
		if user == "" {
			user = "user"
		}
		snippet := fmt.Sprintf(`
# --- Indexer ---
# Directories excluded from session indexing.
# Prevents home/tmp directories from accumulating internal sessions.
exclude_projects:
  - /home/%s
  - /tmp
`, user)
		content = appendToEnd(content, snippet)
		added++
	}

	// ━━ Missing top-level sections ━━
	if !strings.Contains(content, "caps_dir:") {
		content = appendToEnd(content, `
# --- Caps Directory ---
# Custom directory for capability files (CAP.md). Empty = use ~/.claude/caps/.
caps_dir: ""
`)
		added++
	}
	if !strings.Contains(content, "default_sandbox_profile:") {
		content = appendToEnd(content, `
# --- Sandbox ---
# Default sandbox profile for spawned agents.
default_sandbox_profile: ""
`)
		added++
	}
	if !strings.Contains(content, "secrets_sanitization:") {
		content = appendToEnd(content, `
# --- Secrets Sanitization ---
# Redact secrets from extraction content.
secrets_sanitization:
  enabled: false
  allowed_exceptions: []
`)
		added++
	}
	if !strings.Contains(content, "http:") {
		content = appendToEnd(content, `
# --- HTTP Server (optional) ---
http:
  enabled: false
  listen: "127.0.0.1:9377"
  auth_token: ""
`)
		added++
	}

	// ━━ Missing forked_agents fields ━━
	if strings.Contains(content, "forked_agents:") {
		var faAdds []string
		if !strings.Contains(content, "max_forks_per_session:") {
			faAdds = append(faAdds, "  max_forks_per_session: 50")
		}
		if !strings.Contains(content, "max_cost_per_session:") {
			faAdds = append(faAdds, "  max_cost_per_session: 5")
		}
		if len(faAdds) > 0 {
			content = insertAtEndOfSection(content, "forked_agents:", strings.Join(faAdds, "\n"))
			added += len(faAdds)
		}
	}

	// ━━ Missing proxy fields ━━
	if strings.Contains(content, "proxy:") {
		var pxAdds []string
		if !strings.Contains(content, "openai_target:") {
			pxAdds = append(pxAdds, "  openai_target: \"https://api.openai.com\"")
		}
		if !strings.Contains(content, "reset_cache:") {
			pxAdds = append(pxAdds, "  reset_cache: false")
		}
		if !strings.Contains(content, "cache_keepalive_min_messages:") {
			pxAdds = append(pxAdds, "  cache_keepalive_min_messages: 10")
		}
		if len(pxAdds) > 0 {
			content = insertAtEndOfSection(content, "proxy:", strings.Join(pxAdds, "\n"))
			added += len(pxAdds)
		}
	}

	// ━━ token_thresholds: deepseek/glm-5.2 ━━
	if strings.Contains(content, "token_thresholds:") {
		if !strings.Contains(content, "deepseek:") {
			content = insertAtEndOfSection(content, "token_thresholds:", "    deepseek: 600000")
			added++
		}
		if !strings.Contains(content, "glm-5.2:") {
			content = insertAtEndOfSection(content, "token_thresholds:", "    glm-5.2: 500000")
			added++
		}
	}

	// ━━ think_reminder_min_chars — inject into deepseek model_features ━━
	if strings.Contains(content, "model_features:") && !strings.Contains(content, "think_reminder_min_chars") {
		// Insert after the last "think_reminder: true" line inside a deepseek section
		content = injectThinkReminderMinChars(content, "deepseek:", "10")
		if !strings.Contains(content, "think_reminder_min_chars") {
			// fallback: also try at feature_defaults level with value 0
			content = injectThinkReminderMinChars(content, "feature_defaults:", "0")
		}
		if strings.Contains(content, "think_reminder_min_chars") {
			added++
		}
	}

	// ━━ Per-key gate migration: fill missing gate keys in existing model_features entries and feature_defaults ━━
	content, gatesAdded := migrateModelFeaturesGates(content)
	added += gatesAdded

	if added == 0 {
		return 0, nil
	}

	if err := backupFile(path); err != nil {
		return 0, fmt.Errorf("backup config: %w", err)
	}
	// Same writer as every other config path: an atomic replace with mode 0600.
	// os.WriteFile wrote through a symlink and kept a world-readable mode on a
	// file that carries the API keys (YM-392).
	if err := writeConfigFile(path, []byte(content)); err != nil {
		return 0, fmt.Errorf("write config: %w", err)
	}

	return added, nil
}

// insertAtEndOfSection inserts snippet at the end of a YAML section (before the next top-level key).
// Newline-safe: snippet boundaries are normalized so injected content never glues
// onto the preceding or following line (previously caused "10embedding:"-style
// corruption when snippets lacked leading/trailing newlines).
func insertAtEndOfSection(content, sectionKey, snippet string) string {
	lines := strings.Split(content, "\n")
	insertIdx := -1
	inSection := false
	for i, line := range lines {
		if strings.HasPrefix(line, sectionKey) {
			inSection = true
			continue
		}
		if inSection && len(line) > 0 && line[0] != ' ' && line[0] != '#' && line[0] != '\t' {
			insertIdx = i
			break
		}
	}

	snippet = strings.Trim(snippet, "\n")
	if insertIdx >= 0 {
		before := strings.Join(lines[:insertIdx], "\n")
		after := strings.Join(lines[insertIdx:], "\n")
		return before + "\n" + snippet + "\n" + after
	}
	return strings.TrimRight(content, "\n") + "\n" + snippet
}

// appendToEnd appends snippet to the end of the content.
func appendToEnd(content, snippet string) string {
	content = strings.TrimRight(content, "\n")
	return content + "\n" + snippet
}

// injectThinkReminderMinChars inserts think_reminder_min_chars: <value> after the
// last think_reminder line inside a model_features sub-section (e.g., "deepseek:").
// Returns unchanged content if the field already exists or the target section is not found.
func injectThinkReminderMinChars(content, targetSection, value string) string {
	lines := strings.Split(content, "\n")
	inSection := false
	sectionIndent := -1
	lastThinkLine := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == targetSection {
			inSection = true
			sectionIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			continue
		}
		if inSection {
			indent := 0
			if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
				indent = len(line) - len(strings.TrimLeft(line, " \t"))
			} else if len(strings.TrimSpace(line)) > 0 && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				// new top-level or peer key — left the section
				break
			}
			if indent <= sectionIndent && len(strings.TrimSpace(line)) > 0 && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				break
			}
			if strings.HasPrefix(trimmed, "think_reminder:") {
				lastThinkLine = i
			}
			if strings.HasPrefix(trimmed, "think_reminder_min_chars:") {
				return content // already exists
			}
		}
	}

	if lastThinkLine < 0 {
		return content
	}

	// Use the same indentation as the think_reminder line
	thinkIndent := ""
	thinkLine := lines[lastThinkLine]
	for _, ch := range thinkLine {
		if ch == ' ' || ch == '\t' {
			thinkIndent += string(ch)
		} else {
			break
		}
	}

	newLine := thinkIndent + "think_reminder_min_chars: " + value
	lines = append(lines[:lastThinkLine+1], append([]string{newLine}, lines[lastThinkLine+1:]...)...)
	return strings.Join(lines, "\n")
}

// gateDefaults defines the value injected for each missing gate key during
// per-key migration of existing model_features entries and feature_defaults.
// The values follow feature_defaults in config.Default() and the fresh template
// in insertFeatureDefaultsSection: a missing key means the user never stated an
// opinion, so the migration has to write what a fresh install gets. The earlier
// false values (briefing, plan_checkpoint, rules_reminder, think_reminder,
// timestamps) silently switched those blocks off on an upgrade while a fresh
// install had them on (YM-349). loop_warning and skill_eval are true for the
// same reason: both ran ungated before the gate existed.
var gateDefaults = map[string]string{
	"assoc_context":            "true",
	"briefing":                 "true",
	"loop_warning":             "true",
	"plan_checkpoint":          "true",
	"rules_reminder":           "true",
	"skill_eval":               "true",
	"think_reminder":           "true",
	"think_reminder_min_chars": "0",
	"timestamps":               "true",
}

// modelGateDefaults holds the value a gate receives in a NAMED model block that
// does not name it. It mirrors feature_defaults of config.Default() and the
// fresh template block by block: a block is a complete statement, so a missing
// key has to inherit what that model gets on a fresh install, not what the
// fallback says (YM-349, cold review MEDIUM-3). A block this map does not list
// (a custom model name) falls back to gateDefaults.
var modelGateDefaults = map[string]map[string]string{
	"claude":   {"timestamps": "false"},
	"deepseek": {"plan_checkpoint": "false"},
	"gpt":      {"plan_checkpoint": "false", "think_reminder": "false", "timestamps": "false"},
	"openai":   {"plan_checkpoint": "false", "think_reminder": "false", "timestamps": "false"},
	"glm":      {"plan_checkpoint": "false"},
}

// gateValueFor returns the value to inject for key inside the block named
// section (the header without its colon).
func gateValueFor(section, key string) string {
	if overrides, ok := modelGateDefaults[section]; ok {
		if v, ok := overrides[key]; ok {
			return v
		}
	}
	return gateDefaults[key]
}

// canonicalGateOrder is the insertion order for missing gate keys (alphabetical).
var canonicalGateOrder = []string{
	"assoc_context",
	"briefing",
	"loop_warning",
	"plan_checkpoint",
	"rules_reminder",
	"skill_eval",
	"think_reminder",
	"think_reminder_min_chars",
	"timestamps",
}

// injectMissingGates scans a YAML sub-section named by sectionHeader (e.g.,
// "claude:") and appends any missing gate keys at the section's key indent.
// Returns the modified content and the number of keys added. Idempotent.
//
// Key indent is auto-detected from the first existing key in the section;
// falls back to sectionIndent+2 for empty sections. Missing keys are appended
// at the section end (before any sibling that dedents to or past sectionIndent).
func injectMissingGates(content, sectionHeader string) (string, int) {
	lines := strings.Split(content, "\n")
	inSection := false
	sectionIndent := -1
	keyIndent := ""
	insertAt := -1
	existing := map[string]bool{}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inSection {
			// Section header match tolerates inline comments (e.g., "claude: # preset").
			// The YAML key is the token before the first colon; trailing comment is ignored.
			if key := strings.SplitN(trimmed, ":", 2); len(key) == 2 && key[0]+":" == sectionHeader {
				rest := strings.TrimSpace(key[1])
				if rest == "" || strings.HasPrefix(rest, "#") {
					inSection = true
					sectionIndent = len(line) - len(strings.TrimLeft(line, " \t"))
					insertAt = i + 1
				}
			}
			continue
		}
		if len(trimmed) == 0 || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := 0
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			indent = len(line) - len(strings.TrimLeft(line, " \t"))
		}
		if indent <= sectionIndent {
			break
		}
		existing[strings.SplitN(trimmed, ":", 2)[0]] = true
		if keyIndent == "" {
			keyIndent = line[:indent]
		}
		insertAt = i + 1
	}

	if !inSection {
		return content, 0
	}
	if keyIndent == "" {
		keyIndent = strings.Repeat(" ", sectionIndent+2)
	}

	section := strings.TrimSuffix(sectionHeader, ":")
	var toInsert []string
	for _, key := range canonicalGateOrder {
		if !existing[key] {
			toInsert = append(toInsert, keyIndent+key+": "+gateValueFor(section, key))
		}
	}
	if len(toInsert) == 0 {
		return content, 0
	}

	newLines := append(append([]string{}, lines[:insertAt]...), toInsert...)
	newLines = append(newLines, lines[insertAt:]...)
	return strings.Join(newLines, "\n"), len(toInsert)
}

// migrateModelFeaturesGates scans an existing model_features block plus the
// feature_defaults sibling and adds missing gate keys to each entry. Returns
// the modified content and the total number of keys added. Idempotent.
//
// Model entries are discovered dynamically (any sub-section header at indent
// == model_features+2 is processed), so custom model names in user configs
// also receive per-key migration. New model entries (e.g., glm) are NOT
// inserted — only existing entries are augmented; missing models fall back
// to feature_defaults at runtime.
func migrateModelFeaturesGates(content string) (string, int) {
	var headers []string

	lines := strings.Split(content, "\n")
	mfLine := -1
	mfIndent := -1
	for i, line := range lines {
		// Top-level header match tolerates inline comments (e.g., "model_features: # gates").
		// Inline values like "model_features: {}" are NOT section headers.
		trimmed := strings.TrimSpace(line)
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 && parts[0]+":" == "model_features:" {
			rest := strings.TrimSpace(parts[1])
			if rest == "" || strings.HasPrefix(rest, "#") {
				mfLine = i
				mfIndent = len(line) - len(strings.TrimLeft(line, " \t"))
				break
			}
		}
	}

	if mfLine >= 0 {
		subIndent := mfIndent + 2
		for i := mfLine + 1; i < len(lines); i++ {
			line := lines[i]
			trimmed := strings.TrimSpace(line)
			if len(trimmed) == 0 || strings.HasPrefix(trimmed, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent <= mfIndent {
				break
			}
			if indent == subIndent {
				// Sub-section header: "<name>:" optionally followed by inline comment.
				// Inline values like "deepseek: {}" are NOT section headers.
				parts := strings.SplitN(trimmed, ":", 2)
				if len(parts) == 2 {
					rest := strings.TrimSpace(parts[1])
					if rest == "" || strings.HasPrefix(rest, "#") {
						headers = append(headers, parts[0]+":")
					}
				}
			}
		}
	}

	// feature_defaults is a sibling of model_features, not nested.
	// If missing entirely, insert the full section with documented template
	// values before per-key migration runs. Rationale: a missing section
	// previously meant code-false fallback; a newly created visible section
	// should carry the documented defaults so ungated features (loop_warning)
	// don't regress. The template matches modelFeaturesBlock.feature_defaults.
	total := 0
	if !sectionExists(content, "feature_defaults:") {
		content = insertFeatureDefaultsSection(content)
		total += len(canonicalGateOrder)
	}

	headers = append(headers, "feature_defaults:")

	for _, h := range headers {
		var added int
		content, added = injectMissingGates(content, h)
		total += added
	}
	return content, total
}

// sectionExists reports whether a YAML sub-section header (e.g., "feature_defaults:")
// appears as a line, tolerant of inline comments ("feature_defaults: # fallback").
// Inline values ("feature_defaults: {}") are NOT treated as section headers.
func sectionExists(content, sectionHeader string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 || parts[0]+":" != sectionHeader {
			continue
		}
		rest := strings.TrimSpace(parts[1])
		if rest == "" || strings.HasPrefix(rest, "#") {
			return true
		}
	}
	return false
}

// MoveMisplacedProxyKeys moves known proxy keys that sit on the top level of
// config.yaml into the proxy: section, where Load reads them, and returns the
// moved key names. It works on lines, so comments and the file's own
// indentation survive; a re-marshalled document would lose both. A missing file
// or an already clean file yields no moves.
func MoveMisplacedProxyKeys(path string) ([]string, error) {
	unlock := lockConfigWrite(path)
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	content, moved := moveMisplacedProxyKeys(string(data))
	if len(moved) == 0 {
		return nil, nil
	}

	// Never write a document this code cannot read back. A broken file is left
	// as it is; CheckKeys keeps reporting it.
	var check map[string]any
	if err := yaml.Unmarshal([]byte(content), &check); err != nil {
		return nil, fmt.Errorf("moved keys would leave config.yaml unparsable, leaving the file untouched: %w", err)
	}

	if err := backupFile(path); err != nil {
		return nil, fmt.Errorf("backup config: %w", err)
	}
	if err := writeConfigFile(path, []byte(content)); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	return moved, nil
}

func moveMisplacedProxyKeys(content string) (string, []string) {
	lines := strings.Split(content, "\n")

	hasProxySection := false
	for _, line := range lines {
		if key, ok := topLevelKeyName(line); ok && key == "proxy" {
			hasProxySection = true
			break
		}
	}
	proxyIdx := proxySectionLine(lines)
	if hasProxySection && proxyIdx < 0 {
		// Inline value ("proxy: {}"): indented children below it would be
		// invalid YAML, so the file is left alone.
		return content, nil
	}

	var blocks [][]string
	var moved []string
	removed := make([]bool, len(lines))
	proxyKeys := proxyChildKeys(lines, proxyIdx)
	for i := 0; i < len(lines); i++ {
		key, ok := topLevelKeyName(lines[i])
		if !ok || key == "proxy" || !isMisplacedProxyKey(key) {
			continue
		}
		// The proxy value is the one in effect. Moving the inert top-level value
		// over it would change live behavior during a maintenance command, and
		// keeping both would make the document invalid YAML; the key stays where
		// it is and CheckKeys keeps reporting it.
		if proxyKeys[key] {
			continue
		}
		end := blockEnd(lines, i)
		// Trailing blank lines and comments belong to whatever follows.
		for end > i+1 && isBlankOrComment(lines[end-1]) {
			end--
		}
		for j := i; j < end; j++ {
			removed[j] = true
		}
		blocks = append(blocks, lines[i:end])
		moved = append(moved, key)
		i = end - 1
	}
	if len(blocks) == 0 {
		return content, nil
	}

	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !removed[i] {
			kept = append(kept, line)
		}
	}

	if proxyIdx < 0 {
		// No proxy section at all: create one at the end, so the keys become
		// readable instead of staying inert.
		out := kept
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "", "proxy:")
		return strings.Join(append(out, indentBlock(blocks, 2)...), "\n"), moved
	}

	proxyIdx = proxySectionLine(kept)
	if proxyIdx < 0 {
		return content, nil
	}

	// Insert at the end of the proxy section, indented like its existing
	// children (yaml.Marshal writes four spaces, the setup template two).
	childIndent := 2
	indentSeen := false
	insertAt := proxyIdx + 1
	for i := proxyIdx + 1; i < len(kept); i++ {
		if _, ok := topLevelKeyName(kept[i]); ok {
			break
		}
		insertAt = i + 1
		if !indentSeen && !isBlankOrComment(kept[i]) && lineIndent(kept[i]) > 0 {
			childIndent = lineIndent(kept[i])
			indentSeen = true
		}
	}
	// Blank lines and a top-level banner comment below the section belong to
	// whatever follows, so the new keys go in front of them.
	for insertAt > proxyIdx+1 && isBlankOrTopLevelComment(kept[insertAt-1]) {
		insertAt--
	}

	out := make([]string, 0, len(kept)+len(blocks))
	out = append(out, kept[:insertAt]...)
	out = append(out, indentBlock(blocks, childIndent)...)
	out = append(out, kept[insertAt:]...)
	return strings.Join(out, "\n"), moved
}

func indentBlock(blocks [][]string, indent int) []string {
	prefix := strings.Repeat(" ", indent)
	var out []string
	for _, block := range blocks {
		for _, line := range block {
			if strings.TrimSpace(line) == "" {
				out = append(out, "")
				continue
			}
			out = append(out, prefix+line)
		}
	}
	return out
}

// isBlankOrTopLevelComment reports lines that separate sections: blank lines and
// comments at indent 0 (banners like "# --- Signals ---").
func isBlankOrTopLevelComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	return lineIndent(line) == 0 && strings.HasPrefix(trimmed, "#")
}

// proxySectionLine returns the index of a block-form "proxy:" header. A header
// carrying an inline value ("proxy: {}") is not a block: appending indented
// children below it would produce invalid YAML, so nothing is moved.
func proxySectionLine(lines []string) int {
	for i, line := range lines {
		key, ok := topLevelKeyName(line)
		if !ok || key != "proxy" {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "proxy:"))
		if rest == "" || strings.HasPrefix(rest, "#") {
			return i
		}
		return -1
	}
	return -1
}

// proxyChildKeys collects the keys already defined inside the proxy section.
func proxyChildKeys(lines []string, proxyIdx int) map[string]bool {
	keys := map[string]bool{}
	for i := proxyIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if _, ok := topLevelKeyName(line); ok {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if colon := strings.Index(trimmed, ":"); colon > 0 {
			keys[trimmed[:colon]] = true
		}
	}
	return keys
}

// topLevelKeyName returns the key of a line that starts a top-level YAML key.
func topLevelKeyName(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
		return "", false
	}
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", false
	}
	key := line[:i]
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return "", false
		}
	}
	return key, true
}

// blockEnd returns the index one past the last line belonging to the top-level
// block that starts at start.
func blockEnd(lines []string, start int) int {
	for i := start + 1; i < len(lines); i++ {
		if _, ok := topLevelKeyName(lines[i]); ok {
			return i
		}
	}
	return len(lines)
}

func isBlankOrComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// isMisplacedProxyKey reports whether key is a proxy field that is not also a
// top-level Config field, so finding it at the top level means Load ignores it.
func isMisplacedProxyKey(key string) bool {
	if _, isSection := yamlField(configType, key); isSection {
		return false
	}
	_, isProxyKey := yamlField(proxyType, key)
	return isProxyKey
}

// insertFeatureDefaultsSection inserts a full feature_defaults block with
// documented template values immediately after the model_features section ends.
// Indent mirrors the model_features header. If model_features is absent, the
// block is appended at content end as a sibling of the proxy section.
//
// The migration only fills keys a block does not name. An existing explicit
// value stays untouched, including an assoc_context: false that an older
// template wrote: flipping a value the user has on disk would be a silent
// behavior change, so a config that predates the true default keeps it off until
// an operator changes it.
func insertFeatureDefaultsSection(content string) string {
	lines := strings.Split(content, "\n")
	mfLine := -1
	mfIndent := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 && parts[0]+":" == "model_features:" {
			rest := strings.TrimSpace(parts[1])
			if rest == "" || strings.HasPrefix(rest, "#") {
				mfLine = i
				mfIndent = len(line) - len(strings.TrimLeft(line, " \t"))
				break
			}
		}
	}

	block := strings.Repeat(" ", mfIndent) + "feature_defaults:\n" +
		strings.Repeat(" ", mfIndent+2) + "assoc_context: true\n" +
		strings.Repeat(" ", mfIndent+2) + "briefing: true\n" +
		strings.Repeat(" ", mfIndent+2) + "loop_warning: true\n" +
		strings.Repeat(" ", mfIndent+2) + "plan_checkpoint: true\n" +
		strings.Repeat(" ", mfIndent+2) + "rules_reminder: true\n" +
		strings.Repeat(" ", mfIndent+2) + "skill_eval: true\n" +
		strings.Repeat(" ", mfIndent+2) + "think_reminder: true\n" +
		strings.Repeat(" ", mfIndent+2) + "think_reminder_min_chars: 0\n" +
		strings.Repeat(" ", mfIndent+2) + "timestamps: true"

	if mfLine < 0 {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			return content + "\n" + block
		}
		return content + block
	}

	insertAt := len(lines)
	for i := mfLine + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= mfIndent {
			insertAt = i
			break
		}
	}

	newLines := append(append([]string{}, lines[:insertAt]...), block)
	newLines = append(newLines, lines[insertAt:]...)
	return strings.Join(newLines, "\n")
}
