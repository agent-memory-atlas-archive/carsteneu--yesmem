package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// los39bWriteHome lays out a throwaway HOME with the files auto-discovery reads.
func los39bWriteHome(t *testing.T, models, opencodeConfig, auth string) string {
	t.Helper()
	home := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write(filepath.Join(".cache", "opencode", "models.json"), models)
	write(filepath.Join(".config", "opencode", "opencode.json"), opencodeConfig)
	if auth != "" {
		write(filepath.Join(".local", "share", "opencode", "auth.json"), auth)
	}
	isolateHomeAt(t, home)
	return home
}

// YM-262 Befund 5: resolvedAgentModel baute die Provider-Map per sync.Once und
// behielt sie fuer die gesamte Prozesslebensdauer. Neu angemeldete Provider
// wurden erst nach einem Daemon-Neustart aufgeloest.
func TestResolvedAgentModel_PicksUpNewCredentialsWithoutRestart(t *testing.T) {
	home := los39bWriteHome(t,
		`{"zai":{"id":"zai","npm":"@ai-sdk/openai-compatible","api":"https://api.z.ai","models":{"glm-9":{"id":"glm-9"}}}}`,
		`{"provider":{"zai":{"options":{"timeout":600000}}}}`,
		"")

	h := &Handler{}
	if got := h.resolvedAgentModel("glm-9", "opencode"); got != "glm-9" {
		t.Fatalf("without credentials the bare model must pass through, got %q", got)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(authPath, []byte(`{"zai":{"type":"api","key":"sk-zai"}}`), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	if got := h.resolvedAgentModel("glm-9", "opencode"); got != "zai/glm-9" {
		t.Fatalf("a new credential must resolve without a daemon restart, got %q", got)
	}
}
