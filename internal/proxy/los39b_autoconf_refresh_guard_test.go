package proxy

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

// A refresh must not rewrite the user's opencode config: the baseURL patch is a
// startup action, and the request path has to stay read-only (YM-262).
func TestDiscoveredTargets_DoesNotRewriteOpenCodeConfig(t *testing.T) {
	cfgJSON := `{
  "provider": {
    "deepseek": { "options": { "timeout": 600000 } }
  }
}`
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", cfgJSON, los39AuthJSON)

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.deepseek.com" {
		t.Fatalf("discovery must route the credentialed provider, got %q", got)
	}

	path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != cfgJSON {
		t.Errorf("discovery refresh rewrote the opencode config:\n%s", got)
	}
}

// Discovery stays off when the user turned it off, whatever the files say.
func TestDiscoveredTargets_DisabledConfigSkipsDiscovery(t *testing.T) {
	los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{"provider":{"deepseek":{"apiKey":"sk-ds"}}}`, los39AuthJSON)

	s := &Server{logger: log.New(io.Discard, "", 0)}
	if got := s.discoveredTargets(); got != nil {
		t.Errorf("auto_configure_providers=false must discover nothing, got %v", got)
	}
}

// A stamp that did not change must not trigger another discovery: resolveOpenAITarget
// runs on the request path.
func TestDiscoveredTargets_ReusesResultWhileFilesAreUnchanged(t *testing.T) {
	los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{"provider":{"deepseek":{"apiKey":"sk-ds"}}}`, los39AuthJSON)

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	first := s.discoveredTargets()
	if len(first) == 0 {
		t.Fatal("discovery found nothing")
	}
	s.autoTargetsMu.RLock()
	stampBefore := s.autoTargetsStamp
	s.autoTargetsMu.RUnlock()

	second := s.discoveredTargets()
	s.autoTargetsMu.RLock()
	stampAfter := s.autoTargetsStamp
	s.autoTargetsMu.RUnlock()

	if stampBefore != stampAfter {
		t.Errorf("stamp changed without a file change: %d -> %d", stampBefore, stampAfter)
	}
	if len(second) != len(first) {
		t.Errorf("second discovery returned %d targets, want %d", len(second), len(first))
	}
}

// YM-262: an unreadable credential file reads like a logout (every
// credential-backed provider disappears). It must keep the routes instead of
// clearing them. A truncated auth.json changes the stamp, so a refresh really runs.
func TestDiscoveredTargets_UnreadableAuthKeepsRoutes(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{"provider":{"deepseek":{"options":{"timeout":600000}}}}`, los39AuthJSON)

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.deepseek.com" {
		t.Fatalf("setup: expected a route, got %q", got)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"deepseek":{"type":"api",`), 0o644); err != nil {
		t.Fatalf("truncate auth.json: %v", err)
	}

	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.deepseek.com" {
		t.Fatalf("an unreadable auth.json must keep the route, got %q", got)
	}
}

// YM-262: a readable, empty credential file is a logout. The routes have to go,
// otherwise the proxy keeps sending traffic to a provider without credentials.
func TestDiscoveredTargets_LogoutClearsRoutes(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{"provider":{"deepseek":{"options":{"timeout":600000}}}}`, los39AuthJSON)

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.deepseek.com" {
		t.Fatalf("setup: expected a route, got %q", got)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.WriteFile(authPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("empty auth.json: %v", err)
	}

	if got := s.resolveOpenAITarget("deepseek-chat"); got != "" {
		t.Fatalf("after a logout the route must be gone, got %q", got)
	}
}

// YM-262: the daemon-side cache keeps its last map when the files cannot be read.
// Otherwise a half-written auth.json would make every bare model name stop
// resolving until the next file change.
func TestBuildModelProviderMapCached_KeepsMapOnReadError(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{"provider":{"deepseek":{"options":{"timeout":600000}}}}`, los39AuthJSON)

	first := BuildModelProviderMapCached()
	if first["deepseek-chat"] != "deepseek" {
		t.Fatalf("setup: expected the deepseek mapping, got %v", first)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"deepseek":{"type":"api",`), 0o644); err != nil {
		t.Fatalf("truncate auth.json: %v", err)
	}

	second := BuildModelProviderMapCached()
	if second["deepseek-chat"] != "deepseek" {
		t.Errorf("a read error must keep the previous map, got %v", second)
	}
}
