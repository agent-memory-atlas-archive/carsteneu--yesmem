package daemon

import (
	"path/filepath"
	"testing"
)

func TestResolveProjectDir_UsesCWDWhenProvided(t *testing.T) {
	h, _ := mustHandler(t)

	// A resolved base keeps the expectation platform-neutral: resolveProjectDir
	// answers with the path as the OS reports it, and macOS resolves /tmp.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	cwd := filepath.Join(base, "worktree-path")
	params := map[string]any{
		"_cwd": cwd,
	}

	got := h.resolveProjectDir("yesmem", params)
	if got != cwd {
		t.Errorf("expected %s, got %q", cwd, got)
	}
}

func TestResolveProjectDir_PrefersProjectDirOverCWD(t *testing.T) {
	h, _ := mustHandler(t)

	params := map[string]any{
		"_cwd":        "/tmp/cwd-path",
		"project_dir": "/tmp/explicit-path",
	}

	got := h.resolveProjectDir("yesmem", params)
	if got != "/tmp/explicit-path" {
		t.Errorf("expected /tmp/explicit-path, got %q", got)
	}
}

func TestResolveProjectDir_FallsBackToStoreWhenNoParams(t *testing.T) {
	h, _ := mustHandler(t)

	got := h.resolveProjectDir("nonexistent-project")
	if got != "" {
		t.Errorf("expected empty for unregistered project, got %q", got)
	}
}

func TestResolveProjectDir_IgnoresEmptyCWD(t *testing.T) {
	h, _ := mustHandler(t)

	params := map[string]any{
		"_cwd": "",
	}

	got := h.resolveProjectDir("nonexistent-project", params)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestResolveProjectDir_IgnoresNonStringCWD(t *testing.T) {
	h, _ := mustHandler(t)

	params := map[string]any{
		"_cwd": 42,
	}

	got := h.resolveProjectDir("nonexistent-project", params)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
