package proxy

import "testing"

// YM-152: bare model names resolved to the wrong provider. opencode Zen
// ("opencode") carries a credential-free copy of many models and sorts before
// every paid provider, so "glm-5.2" resolved to opencode/glm-5.2 while the user
// pays for zai-coding-plan/glm-5.2. A provider the user has credentials for is
// the intended target; the free tier is a fallback, never the winner.
func TestBuildModelProviderMap_PrefersCredentialedProvider(t *testing.T) {
	providers := []autoDiscoveredProvider{
		{ModelID: "glm-5.2", ProviderID: "opencode", UpstreamURL: "https://opencode.ai/inference/v1", IsOpenAICompat: true, HasCredentials: false},
		{ModelID: "glm-5.2", ProviderID: "zai-coding-plan", UpstreamURL: "https://api.z.ai", IsOpenAICompat: true, HasCredentials: true},
	}
	m := buildModelProviderMap(providers)
	if m["glm-5.2"] != "zai-coding-plan" {
		t.Fatalf("bare glm-5.2 = %q, want zai-coding-plan (credentialed provider beats the free tier)", m["glm-5.2"])
	}

	// Input order must not matter.
	reversed := []autoDiscoveredProvider{providers[1], providers[0]}
	if m := buildModelProviderMap(reversed); m["glm-5.2"] != "zai-coding-plan" {
		t.Fatalf("bare glm-5.2 = %q with reversed input, want zai-coding-plan", m["glm-5.2"])
	}
}

// YM-152: two credentialed providers carrying the same model stay deterministic
// (alphabetical), because nothing but the user can resolve that ambiguity.
func TestBuildModelProviderMap_AlphabeticalAmongCredentialed(t *testing.T) {
	providers := []autoDiscoveredProvider{
		{ModelID: "glm-5.2", ProviderID: "zai", UpstreamURL: "https://api.z.ai", HasCredentials: true},
		{ModelID: "glm-5.2", ProviderID: "openrouter", UpstreamURL: "https://openrouter.ai", HasCredentials: true},
	}
	if m := buildModelProviderMap(providers); m["glm-5.2"] != "openrouter" {
		t.Fatalf("bare glm-5.2 = %q, want openrouter (alphabetically first among credentialed)", m["glm-5.2"])
	}
}

// YM-152: the routing map (which upstream a bare model is sent to) must use the
// same preference as the spawn map, or model and endpoint disagree.
func TestBuildAutoProviderTargets_PrefersCredentialedProvider(t *testing.T) {
	providers := []autoDiscoveredProvider{
		{ModelID: "glm-5.2", ProviderID: "opencode", UpstreamURL: "https://opencode.ai/inference/v1", IsOpenAICompat: true, HasCredentials: false},
		{ModelID: "glm-5.2", ProviderID: "zai-coding-plan", UpstreamURL: "https://api.z.ai", IsOpenAICompat: true, HasCredentials: true},
	}
	m := buildAutoProviderTargets(providers, nil)
	if m["glm-5.2"] != "https://api.z.ai" {
		t.Fatalf("bare glm-5.2 routes to %q, want https://api.z.ai", m["glm-5.2"])
	}
	// The qualified key keeps pointing at its own provider.
	if m["opencode/glm-5.2"] != "https://opencode.ai/inference/v1" {
		t.Fatalf("opencode/glm-5.2 = %q, want the Zen upstream", m["opencode/glm-5.2"])
	}
}

// YM-152 testimony: discovery must record whether a provider is credential-backed.
// opencode Zen is included without credentials by design (free tier).
func TestDiscoverOpenAICompatibleProviders_RecordsCredentials(t *testing.T) {
	models := map[string]modelsJSONEntry{
		"opencode": {ID: "opencode", NPM: "@ai-sdk/openai-compatible", API: "https://opencode.ai/inference/v1",
			Models: map[string]modelsJSONModel{"glm-5.2": {ID: "glm-5.2"}}},
		"zai-coding-plan": {ID: "zai-coding-plan", NPM: "@ai-sdk/openai-compatible", API: "https://api.z.ai",
			Models: map[string]modelsJSONModel{"glm-5.2": {ID: "glm-5.2"}}},
	}
	opencodeCfg := map[string]opencodeProviderBlock{
		"opencode":        {},
		"zai-coding-plan": {},
	}
	auth := map[string]string{"zai-coding-plan": "sk-zai"}

	active, _ := discoverOpenAICompatibleProviders(models, opencodeCfg, auth)
	got := map[string]bool{}
	for _, p := range active {
		got[p.ProviderID] = p.HasCredentials
	}
	if !got["zai-coding-plan"] {
		t.Errorf("zai-coding-plan has an auth.json entry but HasCredentials = false")
	}
	if got["opencode"] {
		t.Errorf("opencode has no credential and must not be marked HasCredentials")
	}
}
