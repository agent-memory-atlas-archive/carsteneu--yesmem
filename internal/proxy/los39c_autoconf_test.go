package proxy

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const los39cModelsJSON = `{
  "deepseek": {"id":"deepseek","npm":"@ai-sdk/openai-compatible","api":"https://api.deepseek.com",
    "models":{"deepseek-chat":{"id":"deepseek-chat"}}},
  "zai": {"id":"zai","npm":"@ai-sdk/openai-compatible","api":"https://api.z.ai/api/coding/paas/v4",
    "models":{"glm-4.6":{"id":"glm-4.6"}}}
}`

const los39cConfigJSON = `{
  "provider": {
    "deepseek": {
      "options": {
        "timeout": 600000
      }
    },
    "zai": {
      "options": {
        "timeout": 600000
      }
    }
  },
  "model": "deepseek/deepseek-chat"
}`

const los39cAuthDeepseekOnly = `{"deepseek":{"type":"api","key":"sk-ds"}}`
const los39cAuthWithZai = `{"deepseek":{"type":"api","key":"sk-ds"},"zai":{"type":"api","key":"sk-zai"}}`

// los39cBaseURLs reads the patched baseURL per provider out of the opencode config.
func los39cBaseURLs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, data)
	}
	out := make(map[string]string, len(cfg.Provider))
	for id, block := range cfg.Provider {
		out[id] = block.Options.BaseURL
	}
	return out
}

// YM-326 Befund 4: der baseURL-Patch lief nur im NewServer. Ein Provider, der
// erst spaeter dazukommt (opencode auth login), bekam nie eine baseURL und damit
// auch nie Traffic ueber den Proxy, solange der Prozess lebte. Der Refresh patcht
// bei Dateiaenderung nach; opencode selbst muss fuer die neue baseURL neu starten.
func TestLos39c_LateProviderGetsBaseURLPatch(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}

	// The state right after start: only deepseek is logged in.
	if !s.refreshProviderAutoconf() {
		t.Fatal("first refresh patched nothing, want deepseek")
	}
	urls := los39cBaseURLs(t, cfgPath)
	if urls["deepseek"] != "http://localhost:9099/v1" {
		t.Fatalf("deepseek baseURL = %q, want the proxy", urls["deepseek"])
	}
	if urls["zai"] != "" {
		t.Fatalf("zai baseURL = %q although it has no credentials yet", urls["zai"])
	}

	// "opencode auth login" for zai: credentials appear, provider becomes active.
	if err := os.WriteFile(authPath, []byte(los39cAuthWithZai), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	if !s.refreshProviderAutoconf() {
		t.Fatal("a provider logged in after start must get its baseURL patched without a proxy restart")
	}
	if got := los39cBaseURLs(t, cfgPath)["zai"]; got != "http://localhost:9099/v1" {
		t.Errorf("zai baseURL = %q, want the proxy", got)
	}
	if got := s.resolveOpenAITarget("glm-4.6"); got != "https://api.z.ai/api/coding/paas/v4" {
		t.Errorf("route for the new provider = %q, want its upstream", got)
	}

	// Its own patch changes opencode.json, so the next refresh must not run again.
	if s.refreshProviderAutoconf() {
		t.Error("refresh ran again although no provider file changed")
	}
}

// A refresh that cannot read the provider files keeps the routes it had: the
// same fail-closed rule the request path follows (YM-262).
func TestLos39c_ProviderRefreshKeepsRoutesOnReadError(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	modelsPath := filepath.Join(home, ".cache", "opencode", "models.json")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	if !s.refreshProviderAutoconf() {
		t.Fatal("first refresh patched nothing, want deepseek")
	}
	before := s.resolveOpenAITarget("deepseek-chat")

	if err := os.WriteFile(modelsPath, []byte(`{"deepseek":{"id":"deepseek",`), 0o644); err != nil {
		t.Fatalf("truncate models.json: %v", err)
	}
	if s.refreshProviderAutoconf() {
		t.Error("a refresh with unreadable provider files must not report a patch")
	}
	if got := s.resolveOpenAITarget("deepseek-chat"); got != before {
		t.Errorf("route changed after a read error: %q -> %q", before, got)
	}
}

// Ein gescheiterter Schreibvorgang darf nicht endgueltig verschwinden: die Datei
// traegt danach denselben Stempel wie vorher, deshalb haelt providerPatchRetry
// den Auftrag offen, bis er durchgeht.
func TestLos39c_FailedPatchIsRetried(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}

	if err := os.Chmod(cfgPath, 0o400); err != nil {
		t.Fatalf("make config read-only: %v", err)
	}
	if s.refreshProviderAutoconf() {
		t.Fatal("a read-only config must not report a patch")
	}
	if got := los39cBaseURLs(t, cfgPath)["deepseek"]; got != "" {
		t.Fatalf("deepseek baseURL = %q after a failed patch, want none", got)
	}
	if s.refreshProviderAutoconf() {
		t.Fatal("still read-only, no patch expected")
	}

	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatalf("make config writable: %v", err)
	}
	// The file state did not change (chmod leaves mtime and size alone), so only
	// the pending flag can make this retry.
	if !s.refreshProviderAutoconf() {
		t.Fatal("a failed patch must be retried once the file is writable again")
	}
	if got := los39cBaseURLs(t, cfgPath)["deepseek"]; got != "http://localhost:9099/v1" {
		t.Errorf("deepseek baseURL = %q after the retry, want the proxy", got)
	}
}

// Ein bewusst schreibgeschuetztes Config-Signal darf nicht jede Minute erneut
// angerannt werden: nach dem Versuchsbudget ruht der Patch, bis sich die
// Dateien aendern.
func TestLos39c_PatchRetryGivesUpUntilFilesChange(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	if err := os.Chmod(cfgPath, 0o400); err != nil {
		t.Fatalf("make config read-only: %v", err)
	}
	for i := 0; i < providerPatchMaxRetries; i++ {
		if s.refreshProviderAutoconf() {
			t.Fatalf("attempt %d reported a patch on a read-only config", i+1)
		}
	}
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatalf("make config writable: %v", err)
	}
	if s.refreshProviderAutoconf() {
		t.Error("the patch budget is spent for this file state, no further attempt expected")
	}

	// A new file state reopens the budget.
	if err := os.WriteFile(authPath, []byte(los39cAuthWithZai), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	if !s.refreshProviderAutoconf() {
		t.Fatal("a changed provider file must reopen the patch attempts")
	}
	if got := los39cBaseURLs(t, cfgPath)["zai"]; got != "http://localhost:9099/v1" {
		t.Errorf("zai baseURL = %q, want the proxy", got)
	}
}

// Der Re-Check vor dem Schreiben muss eine zwischenzeitliche Aenderung erkennen,
// sonst ueberschreibt der Watcher die Bearbeitung des Nutzers.
func TestLos39c_PatchRefusesStaleWrite(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")

	before, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	edited := los39cConfigJSON + "\n"
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("edit config: %v", err)
	}
	if !providerConfigChangedSince(cfgPath, before) {
		t.Error("a user edit after the read must be detected before the write")
	}

	after, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if providerConfigChangedSince(cfgPath, after) {
		t.Error("an unchanged file must not look changed")
	}
}

// Der Watcher ist die einzige Stelle, die den Patch nach dem Start nachzieht; er
// muss bei Dateiaenderung zuschlagen und auf dem Stopp-Kanal enden.
func TestLos39c_WatchProviderSourcesPatchesLateLoginAndStops(t *testing.T) {
	home := los39WriteHome(t, los39cModelsJSON, "opencode.jsonc", los39cConfigJSON, los39cAuthDeepseekOnly)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")

	s := &Server{
		cfg:    Config{AutoConfigureProviders: true},
		logger: log.New(io.Discard, "", 0),
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		s.watchProviderSources(stop, 10*time.Millisecond)
		close(stopped)
	}()

	if err := os.WriteFile(authPath, []byte(los39cAuthWithZai), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for los39cBaseURLs(t, cfgPath)["zai"] != "http://localhost:9099/v1" {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not patch the provider logged in after start")
		}
		time.Sleep(20 * time.Millisecond)
	}

	close(stop)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher ignores its stop channel")
	}
}
