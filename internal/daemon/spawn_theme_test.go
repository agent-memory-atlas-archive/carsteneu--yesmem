package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

func TestAgentThemeKey(t *testing.T) {
	tests := map[string]string{
		"yesloop-agent-themes":      "yesloop",
		"yesloop-metamemory-refine": "yesloop",
		"yesresearch-fusion-energy": "yesresearch",
		"subresearch-materials":     "subresearch",
		"orchestrator":              "orchestrator",
		"":                          "",
		"-leading-dash":             "",
		"yesmem-dark":               "yesmem",
	}
	for section, want := range tests {
		if got := agentThemeKey(section); got != want {
			t.Errorf("agentThemeKey(%q) = %q, want %q", section, got, want)
		}
	}
}

func TestMergeAgentThemes(t *testing.T) {
	merged := mergeAgentThemes(map[string]string{"yesloop": "custom-loop", "yesresearch": ""})

	if merged["yesloop"] != "custom-loop" {
		t.Errorf("configured override lost: yesloop = %q", merged["yesloop"])
	}
	if value, ok := merged["yesresearch"]; !ok || value != "" {
		t.Errorf("an explicitly empty entry must disable the default, got %q (present=%v)", value, ok)
	}
	if merged["orchestrator"] != "yesmem-dark" {
		t.Errorf("untouched default lost: orchestrator = %q", merged["orchestrator"])
	}
	if defaultAgentThemes["yesloop"] != "yesmem-loop" {
		t.Error("mergeAgentThemes mutated the package default map")
	}
}

func TestResolveAgentTheme(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		section  string
		themes   map[string]string
		want     string
	}{
		{name: "explicit wins", explicit: "dracula", section: "yesloop-x", want: "dracula"},
		{name: "explicit wins over a configured default", explicit: "dracula", section: "yesloop-x",
			themes: map[string]string{"yesloop": "yesmem-loop"}, want: "dracula"},
		{name: "yesloop section default", section: "yesloop-agent-themes", want: "yesmem-loop"},
		{name: "yesresearch section default", section: "yesresearch-fusion", want: "yesmem-research"},
		{name: "subresearch section default", section: "subresearch-materials", want: "yesmem-subresearch"},
		{name: "orchestrator section default", section: "orchestrator", want: "yesmem-dark"},
		{name: "unknown type gets no theme", section: "something-else"},
		{name: "empty section gets no theme"},
		{name: "configured map replaces the default for that type", section: "yesloop-x",
			themes: map[string]string{"yesloop": "gruvbox"}, want: "gruvbox"},
		{name: "configured empty value disables the type", section: "yesloop-x",
			themes: map[string]string{"yesloop": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{agentThemes: tt.themes}
			if got := h.resolveAgentTheme(tt.explicit, tt.section); got != tt.want {
				t.Errorf("resolveAgentTheme(%q, %q) = %q, want %q", tt.explicit, tt.section, got, tt.want)
			}
		})
	}
}

// TestThemeSpawnOverride covers the spawn seam: which backends get a theme, how
// it is applied, and the fallback when the binary does not know --theme.
func TestThemeSpawnOverride(t *testing.T) {
	helpWithFlag := "FLAGS\n  --theme string          Theme to use for this session only\n"
	helpWithoutFlag := "FLAGS\n  --standalone            Run with a private server\n"

	t.Run("non-opencode backend is untouched", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		h := &Handler{}
		flags, env := h.themeSpawnOverride("claude", "/usr/bin/claude", "dracula", "yesloop-x", nil)
		if len(flags) != 0 || len(env) != 0 {
			t.Fatalf("claude spawn got flags=%v env=%v, want none", flags, env)
		}
	})

	t.Run("no theme resolved means no override", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "unknown-type", nil)
		if len(flags) != 0 || len(env) != 0 {
			t.Fatalf("spawn got flags=%v env=%v, want none", flags, env)
		}
	})

	t.Run("capable binary gets the flag", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", nil)
		if strings.Join(flags, " ") != "--theme yesmem-loop" {
			t.Fatalf("flags = %v, want [--theme yesmem-loop]", flags)
		}
		if len(env) != 0 {
			t.Fatalf("env = %v, want none when the flag is available", env)
		}
	})

	t.Run("incapable binary gets the env route", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", nil)
		if len(flags) != 0 {
			t.Fatalf("flags = %v, want none for a binary without --theme", flags)
		}
		if len(env) != 1 || !strings.HasPrefix(env[0], opencodeThemeEnvKey+"=") {
			t.Fatalf("env = %v, want one %s entry", env, opencodeThemeEnvKey)
		}
		content := strings.TrimPrefix(env[0], opencodeThemeEnvKey+"=")
		var parsed struct {
			Theme struct {
				Name string `json:"name"`
			} `json:"theme"`
		}
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			t.Fatalf("env content %q is not JSON: %v", content, err)
		}
		if parsed.Theme.Name != "yesmem-loop" {
			t.Errorf("env theme name = %q, want yesmem-loop", parsed.Theme.Name)
		}
	})

	t.Run("unreadable binary falls back to the env route", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubBinaryIdentity(t, func(string) (string, bool) { return "", false })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", nil)
		if len(flags) != 0 || len(env) != 1 {
			t.Fatalf("flags=%v env=%v, want the env route when the binary cannot be probed", flags, env)
		}
	})

	t.Run("an operator-set config content wins", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		environ := []string{opencodeThemeEnvKey + `={"theme":{"name":"mine"},"diffs":{"wrap":"word"}}`}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", environ)
		if len(flags) != 0 || len(env) != 0 {
			t.Fatalf("flags=%v env=%v, want none: an explicit %s must not be overwritten", flags, env, opencodeThemeEnvKey)
		}
	})

	t.Run("explicit param reaches the flag", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		flags, _ := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "tokyonight", "yesloop-x", nil)
		if strings.Join(flags, " ") != "--theme tokyonight" {
			t.Fatalf("flags = %v, want [--theme tokyonight]", flags)
		}
	})

	// A name that reaches argv as another flag would abort the start of the
	// TUI ("Unrecognized flag"), so a bad config or MCP value must be dropped.
	t.Run("a value that could be parsed as a flag is dropped", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		for _, bad := range []string{"--model", "-x", "yesmem-loop --model x", "yesmem-loop\n--model x", strings.Repeat("a", 65)} {
			if bad == "" {
				continue
			}
			flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", bad, "yesloop-x", nil)
			if len(flags) != 0 || len(env) != 0 {
				t.Errorf("theme %q produced flags=%v env=%v, want none", bad, flags, env)
			}
		}
		if flags, _ := h.themeSpawnOverride("opencode", "/usr/bin/opencode", strings.Repeat("a", 64), "yesloop-x", nil); len(flags) != 2 {
			t.Errorf("a 64-character name must still be accepted, got flags=%v", flags)
		}
	})

	// opencode silently falls back to its built-in theme for an unknown name,
	// so a missing install has to leave a trace in the daemon log. It is not a
	// hard stop: a project-local .opencode/themes/ copy resolves it too.
	t.Run("a missing yesmem theme is still applied and logged", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t) // nothing installed
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", nil)
		if len(flags) != 0 || len(env) != 1 {
			t.Fatalf("flags=%v env=%v, want the env route with a warning", flags, env)
		}
	})

	// A daemon started inside an agent window inherits the value it exported
	// itself; reading that as an operator decision would disable theming for
	// good, so our own shape is replaced.
	t.Run("our own inherited config content is replaced", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		environ := []string{opencodeThemeEnvKey + `={"theme":{"name":"yesmem-loop"}}`}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "", "yesloop-x", environ)
		if len(flags) != 0 || len(env) != 1 {
			t.Fatalf("flags=%v env=%v, want our own value replaced", flags, env)
		}
	})

	// Built-in opencode themes have no file of ours; opencode resolves them.
	t.Run("a built-in theme name needs no installed file", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t)
		h := &Handler{}
		flags, env := h.themeSpawnOverride("opencode", "/usr/bin/opencode", "tokyonight", "yesloop-x", nil)
		if len(flags) != 0 || len(env) != 1 {
			t.Fatalf("flags=%v env=%v, want the env route for a built-in theme", flags, env)
		}
	})
}

// TestInheritedThemeContent separates yesmem's own leftover value (replace it)
// from an operator's richer config content (leave it alone).
func TestInheritedThemeContent(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "our own shape", value: `{"theme":{"name":"yesmem-loop"}}`, want: true},
		{name: "theme plus another setting", value: `{"theme":{"name":"yesmem-loop"},"diffs":{"wrap":"word"}}`},
		{name: "theme plus mode", value: `{"theme":{"name":"yesmem-loop","mode":"dark"}}`},
		{name: "other section only", value: `{"diffs":{"wrap":"word"}}`},
		{name: "theme name is not a string", value: `{"theme":{"name":42}}`},
		{name: "not an object", value: `[]`},
		{name: "invalid json", value: `{`},
		{name: "empty", value: ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inheritedThemeContent(tt.value); got != tt.want {
				t.Errorf("inheritedThemeContent(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestOpencodeSpawnPlan_ThemeReachesArgsAndEnv covers the seam between the
// resolver and the spawned process: the theme must land in argv (capable
// binary) or in the environment (older binary), together with the resume and
// model flags.
func TestOpencodeSpawnPlan_ThemeReachesArgsAndEnv(t *testing.T) {
	helpWithFlag := "FLAGS\n  --theme string          Theme to use for this session only\n"
	helpWithoutFlag := "FLAGS\n  --standalone            Run with a private server\n"

	t.Run("new session with a capable binary", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		args, env := h.opencodeSpawnPlan("agent-1", "sess-uuid", false, "zai/glm-5.2", "", "yesloop-x", "/usr/bin/opencode", nil)
		if strings.Join(args, " ") != "--model zai/glm-5.2 --theme yesmem-loop" {
			t.Fatalf("args = %v, want the model and theme flags", args)
		}
		if len(env) != 0 {
			t.Fatalf("env = %v, want none alongside the flag", env)
		}
	})

	t.Run("new session with an older binary", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithoutFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h := &Handler{}
		args, env := h.opencodeSpawnPlan("agent-1", "sess-uuid", false, "", "", "yesloop-x", "/usr/bin/opencode", nil)
		if len(args) != 0 {
			t.Fatalf("args = %v, want none", args)
		}
		if len(env) != 1 || !strings.HasPrefix(env[0], opencodeThemeEnvKey+"=") {
			t.Fatalf("env = %v, want one %s entry", env, opencodeThemeEnvKey)
		}
	})

	t.Run("resume keeps the session flag", func(t *testing.T) {
		stubHelpExec(t, func(string) (string, error) { return helpWithFlag, nil })
		stubUserHome(t, "yesmem-loop")
		h, s := mustHandler(t)
		if err := s.AgentCreate(storage.Agent{ID: "agent-9", Project: "proj", Section: "yesloop-x",
			SessionID: "daemon-uuid", OpencodeSessionID: "ses_stored"}); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
		args, _ := h.opencodeSpawnPlan("agent-9", "daemon-uuid", true, "", "", "yesloop-x", "/usr/bin/opencode", nil)
		if strings.Join(args, " ") != "-s ses_stored --theme yesmem-loop" {
			t.Fatalf("args = %v, want the stored session id plus the theme", args)
		}
	})
}
