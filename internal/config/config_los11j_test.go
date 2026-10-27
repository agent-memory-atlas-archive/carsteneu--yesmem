package config

import (
	"path/filepath"
	"testing"
)

// YM-354 (Los 11j): config.yaml's paths are consumed as filesystem paths, but
// nothing normalizes them. The setup template ships claude_projects as an
// unexpanded "~/.claude/projects", and an explicitly empty value overrides the
// default during unmarshal. Both would hand a directory scan a path relative to
// the process's working directory, which fails silently.
func TestClaudeProjectsDirResolvesTheConfiguredValue_Los11j(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeDefault := filepath.Join(home, ".claude", "projects")

	cases := []struct{ name, value, want string }{
		{"absolute value wins", "/mnt/data/claude-projects", "/mnt/data/claude-projects"},
		{"empty falls back to the home default", "", homeDefault},
		{"whitespace falls back to the home default", "   ", homeDefault},
		{"tilde is expanded", "~/claude-elsewhere", filepath.Join(home, "claude-elsewhere")},
		{"bare tilde is expanded", "~", home},
		{"relative value is refused", "claude/projects", homeDefault},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Paths.ClaudeProjects = tc.value
		if got := cfg.ClaudeProjectsDir(); got != tc.want {
			t.Errorf("%s: ClaudeProjectsDir() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// config.Default() itself has to survive the same resolver: it is what a
// missing config file yields, and it must not resolve to anything else.
func TestClaudeProjectsDirKeepsTheDefault_Los11j(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := Default().ClaudeProjectsDir(), filepath.Join(home, ".claude", "projects"); got != want {
		t.Errorf("ClaudeProjectsDir() on the default config = %q, want %q", got, want)
	}
}

// Without HOME there is no default, and the fallback must not produce a relative
// path: the daemon would scan and write below its working directory.
func TestClaudeProjectsDirWithoutHome_Los11j(t *testing.T) {
	t.Setenv("HOME", "")

	cfg := Default()
	cfg.Paths.ClaudeProjects = ""
	if got := cfg.ClaudeProjectsDir(); got != "" {
		t.Errorf("ClaudeProjectsDir() without HOME = %q, want an empty value", got)
	}

	cfg.Paths.ClaudeProjects = "~/claude-elsewhere"
	if got := cfg.ClaudeProjectsDir(); got != "" {
		t.Errorf("a tilde path without HOME = %q, want an empty value", got)
	}
}
