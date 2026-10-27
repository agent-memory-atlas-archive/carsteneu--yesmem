package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-262 Befund 4: loadOpenCodeConfig dekodierte die ganze provider-Map in einem
// Zug. Ein einziger falsch typisierter Provider (hier apiKey als Zahl) brach
// damit die komplette Auto-Discovery ab, obwohl alle anderen Provider lesbar
// waren.
func TestLoadOpenCodeConfig_SkipsWrongTypedProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	cfgJSON := `{
  // the healthy provider
  "provider": {
    "deepseek": { "options": { "baseURL": "https://api.deepseek.com" } },
    "broken": { "apiKey": 42 }
  }
}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	providers, skipped, err := loadOpenCodeConfig(path)
	if err != nil {
		t.Fatalf("one wrongly typed provider must not abort discovery: %v", err)
	}
	block, ok := providers["deepseek"]
	if !ok {
		t.Fatalf("healthy provider missing, got %v", providers)
	}
	if block.Options.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseURL = %q", block.Options.BaseURL)
	}
	if _, ok := providers["broken"]; ok {
		t.Error("the wrongly typed provider must not appear with guessed zero values")
	}
	if len(skipped) != 1 || skipped[0] != "broken" {
		t.Errorf("skipped providers = %v, want [broken] (the caller logs them)", skipped)
	}
}
