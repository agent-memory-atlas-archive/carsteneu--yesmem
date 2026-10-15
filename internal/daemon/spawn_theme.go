package daemon

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// opencodeThemeEnvKey is the environment variable the v2 opencode CLI reads as
// additional configuration content; entries there are merged over
// ~/.config/opencode/cli.json, which is where a TUI reads its theme name.
const opencodeThemeEnvKey = "OPENCODE_CLI_CONFIG_CONTENT"

// defaultAgentThemes maps the agent type (the section prefix) to the theme that
// identifies it: one shared dark base, one accent per type. Overridable per
// type through agents.themes in config.yaml; an explicitly empty value disables
// the theme for that type.
//
// "subresearch" is a mapping, not a claim: it applies to sections named
// subresearch-*, which the yesresearch skill does not create today (it names
// its area agents yesresearch-<topic>-<cluster>, so they get the research
// accent). A caller can pass theme= explicitly for anything else.
var defaultAgentThemes = map[string]string{
	"yesloop":      "yesmem-loop",
	"yesresearch":  "yesmem-research",
	"subresearch":  "yesmem-subresearch",
	"orchestrator": "yesmem-dark",
}

// agentThemePrefix marks the themes yesmem itself installs. Only those can be
// checked for presence on disk; every other name is a built-in opencode theme
// that opencode resolves on its own.
const agentThemePrefix = "yesmem-"

// themeNamePattern is the shape a theme name must have to be safe on the
// command line. A value starting with "-" would be parsed as another flag and
// abort the start of the TUI, so an unchecked config or MCP value must never
// reach argv. The length cap keeps an oversized value out of argv (E2BIG) and
// out of the log line.
var themeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// userHomeDir is a seam for the install check (tests).
var userHomeDir = os.UserHomeDir

// warnConfigContentOnce keeps the "themes are disabled" notice to one line per
// daemon instead of one per spawn.
var warnConfigContentOnce sync.Once

// agentThemeKey derives the type key from a section name: the part before the
// first dash ("yesloop-agent-themes" → "yesloop"). Sections without a dash are
// their own type ("orchestrator").
func agentThemeKey(section string) string {
	if i := strings.IndexByte(section, '-'); i > 0 {
		return section[:i]
	}
	if strings.HasPrefix(section, "-") {
		return ""
	}
	return section
}

// mergeAgentThemes layers the configured map over the built-in defaults. A
// configured empty value disables a default instead of resurrecting it.
func mergeAgentThemes(configured map[string]string) map[string]string {
	merged := make(map[string]string, len(defaultAgentThemes)+len(configured))
	for key, value := range defaultAgentThemes {
		merged[key] = value
	}
	for key, value := range configured {
		merged[key] = value
	}
	return merged
}

// resolveAgentTheme applies the spawn precedence: an explicit theme parameter
// wins, otherwise the configured (or built-in) theme of the agent's type.
func (h *Handler) resolveAgentTheme(explicit, section string) string {
	if explicit != "" {
		return explicit
	}
	if section == "" {
		return ""
	}
	themes := h.agentThemes
	if themes == nil {
		themes = defaultAgentThemes
	}
	return themes[agentThemeKey(section)]
}

// themeSpawnOverride resolves the theme for one spawn and returns how to apply
// it: `--theme <name>` when the opencode binary knows the flag (session-only,
// never written back to the config), otherwise the config-content environment
// variable, which older binaries honour and unknown ones ignore. Only the
// opencode backend has themes.
func (h *Handler) themeSpawnOverride(backend, bin, explicit, section string, environ []string) (flags, env []string) {
	if backend != "opencode" {
		if explicit != "" {
			log.Printf("[agents] theme %q ignored: the %q backend has no opencode themes", explicit, backend)
		}
		return nil, nil
	}
	theme := h.resolveAgentTheme(explicit, section)
	if theme == "" {
		return nil, nil
	}
	if !themeNamePattern.MatchString(theme) {
		log.Printf("[agents] ignoring invalid theme name %q for section %q (must match %s)", theme, section, themeNamePattern)
		return nil, nil
	}
	if home, err := userHomeDir(); err == nil && !agentThemeInstalled(home, theme) {
		// opencode silently falls back to its built-in theme for an unknown
		// name, so a missing install is otherwise invisible. Not a hard stop:
		// a project-local .opencode/themes/ copy resolves it just as well.
		log.Printf("[agents] theme %q is not installed under ~/.config/opencode/themes — run `yesmem update` if the accent does not appear", theme)
	}
	if opencodeSupportsThemeFlag(bin) {
		return []string{"--theme", theme}, nil
	}
	content, err := themeEnvContent(theme)
	if err != nil {
		log.Printf("[agents] theme %q not applied: %v", theme, err)
		return nil, nil
	}
	entry := opencodeThemeEnvKey + "=" + content
	for _, existing := range environ {
		value, found := strings.CutPrefix(existing, opencodeThemeEnvKey+"=")
		if !found {
			continue
		}
		if inheritedThemeContent(value) {
			// Our own value, inherited from a daemon that was started inside an
			// agent window: it carries no operator decision, so it is replaced.
			continue
		}
		// Anything richer is an explicit operator setting; a theme must not
		// silently overwrite it.
		warnConfigContentOnce.Do(func() {
			log.Printf("[agents] %s is already set — agent themes stay disabled until it is unset", opencodeThemeEnvKey)
		})
		return nil, nil
	}
	return nil, []string{entry}
}

// inheritedThemeContent reports whether an OPENCODE_CLI_CONFIG_CONTENT value is
// one yesmem wrote itself: exactly a theme.name override, nothing else. A
// daemon started from an agent shell inherits that value, and without this check
// it would read its own leftovers as an operator decision and disable theming
// for good.
func inheritedThemeContent(value string) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &root); err != nil || len(root) != 1 {
		return false
	}
	theme, ok := root["theme"]
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(theme, &fields); err != nil || len(fields) != 1 {
		return false
	}
	name, ok := fields["name"]
	return ok && json.Unmarshal(name, new(string)) == nil
}

// agentThemeInstalled reports whether the theme can be resolved by opencode.
// Names outside the yesmem family are built-in opencode themes, which opencode
// resolves itself; only our own family can be checked on disk.
func agentThemeInstalled(home, theme string) bool {
	if !strings.HasPrefix(theme, agentThemePrefix) {
		return true
	}
	_, err := os.Stat(filepath.Join(home, ".config", "opencode", "themes", theme+".json"))
	return err == nil
}

// themeEnvContent builds the config-content JSON that selects theme without
// touching cli.json.
func themeEnvContent(theme string) (string, error) {
	data, err := json.Marshal(map[string]any{"theme": map[string]string{"name": theme}})
	if err != nil {
		return "", err
	}
	return string(data), nil
}
