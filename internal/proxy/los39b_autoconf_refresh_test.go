package proxy

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

// YM-262 Befund 5: runAutoDiscovery lief einmal im NewServer, danach war das
// Ergebnis fuer die Prozesslebensdauer eingefroren. Wer sich nachtraeglich mit
// "opencode auth login" anmeldet, bekam bis zum Daemon-Neustart keine Route.
func TestAutoDiscovery_PicksUpNewCredentialsWithoutRestart(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{
  "provider": {
    "deepseek": { "options": { "timeout": 600000 } }
  }
}`, "")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}

	if got := s.resolveOpenAITarget("deepseek-chat"); got != "" {
		t.Fatalf("without credentials the model must stay unrouted, got %q", got)
	}

	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(authPath, []byte(`{"deepseek":{"type":"api","key":"sk-ds"}}`), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.deepseek.com" {
		t.Fatalf("new credentials must route without a restart, got %q", got)
	}
}
