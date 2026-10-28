package proxy

import (
	"testing"
)

// PRX-P45 (1): both resolvers ranged over provider_targets and returned the
// first prefix hit. With overlapping keys the winner depended on map order, so
// the same model could reach two upstreams between calls. Repeated because one
// resolution can pick the expected entry by luck.
const los39dIterations = 200

func los39dOverlappingTargets() map[string]string {
	return map[string]string{
		"deepseek":               "https://api.deepseek.com",
		"deepseek-v4-flash-free": "https://free.deepseek.example",
	}
}

func TestLos39dResolveOpenAITargetLongestPrefixWins(t *testing.T) {
	s := &Server{cfg: Config{
		ProviderTargets: los39dOverlappingTargets(),
		OpenAITargetURL: "https://api.openai.com",
	}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveOpenAITarget("deepseek-v4-flash-free"); got != "https://free.deepseek.example" {
			t.Fatalf("iteration %d: resolveOpenAITarget = %q, want the longer prefix route", i, got)
		}
	}
}

func TestLos39dResolveAnthropicTargetLongestPrefixWins(t *testing.T) {
	s := &Server{cfg: Config{
		ProviderTargets: los39dOverlappingTargets(),
		TargetURL:       "https://api.anthropic.com",
	}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveAnthropicTarget("deepseek-v4-flash-free"); got != "https://free.deepseek.example" {
			t.Fatalf("iteration %d: resolveAnthropicTarget = %q, want the longer prefix route", i, got)
		}
	}
}

// An entry without a URL cannot route anything, so it must not shadow the
// shorter prefix that does have one.
func TestLos39dResolveOpenAITargetLongestPrefixSkipsEmptyURL(t *testing.T) {
	s := &Server{cfg: Config{ProviderTargets: map[string]string{
		"deepseek":               "https://api.deepseek.com",
		"deepseek-v4-flash-free": "",
	}}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveOpenAITarget("deepseek-v4-flash-free"); got != "https://api.deepseek.com" {
			t.Fatalf("iteration %d: resolveOpenAITarget = %q, want the shorter entry with a URL", i, got)
		}
	}
}

// An exact model key keeps winning: it is the longest possible prefix.
func TestLos39dResolveAnthropicTargetExactKeyBeatsShorterPrefix(t *testing.T) {
	s := &Server{cfg: Config{ProviderTargets: map[string]string{
		"glm":     "https://api.z.ai/legacy",
		"glm-5.2": "https://api.z.ai/api/coding/paas/v4",
	}}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveAnthropicTarget("glm-5.2"); got != "https://api.z.ai/api/coding/paas/v4" {
			t.Fatalf("iteration %d: resolveAnthropicTarget = %q, want the exact model key", i, got)
		}
	}
}

// Keys that differ only in case collapse onto the same prefix and tie on length;
// the smaller URL keeps the answer independent of map order.
func TestLos39dResolveOpenAITargetCaseDuplicateKeysAreDeterministic(t *testing.T) {
	s := &Server{cfg: Config{ProviderTargets: map[string]string{
		"DeepSeek": "https://b.example",
		"deepseek": "https://a.example",
	}}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://a.example" {
			t.Fatalf("iteration %d: resolveOpenAITarget = %q, want a stable answer", i, got)
		}
	}
}

// A URL that only becomes empty through trimming cannot route anything either,
// and it must not shadow the shorter entry that does have a target.
func TestLos39dResolveOpenAITargetSkipsSlashOnlyURL(t *testing.T) {
	s := &Server{cfg: Config{
		ProviderTargets: map[string]string{
			"deepseek":               "https://api.deepseek.com",
			"deepseek-v4-flash-free": "/",
		},
		OpenAITargetURL: "https://api.openai.com",
	}}
	for i := 0; i < los39dIterations; i++ {
		if got := s.resolveOpenAITarget("deepseek-v4-flash-free"); got != "https://api.deepseek.com" {
			t.Fatalf("iteration %d: resolveOpenAITarget = %q, want the entry with a usable URL", i, got)
		}
	}
}

// The keepalive resolver carried its own copy of the first-hit rule, so a ping
// could go to a different upstream than the request it keeps warm.
func TestLos39dCacheKeepaliveResolveTargetLongestPrefixWins(t *testing.T) {
	ka := &CacheKeepalive{cfg: CacheKeepaliveConfig{
		ProviderTargets: los39dOverlappingTargets(),
		Target:          "https://api.anthropic.com",
	}}
	body := []byte(`{"model":"deepseek-v4-flash-free"}`)
	for i := 0; i < los39dIterations; i++ {
		if got := ka.resolveTarget(body); got != "https://free.deepseek.example" {
			t.Fatalf("iteration %d: resolveTarget = %q, want the longer prefix route", i, got)
		}
	}
}

// The autoconf coverage check answers "is any prefix route configured"; it has
// to use the same rule as the resolvers, including the empty-key case.
func TestLos39dCoveredByProviderTargetsAgreesWithResolver(t *testing.T) {
	targets := map[string]string{"": "https://catch-all.example"}
	if coveredByProviderTargets("deepseek-chat", targets) {
		t.Errorf("an empty key must not count as covering every model")
	}
	s := &Server{cfg: Config{ProviderTargets: targets, OpenAITargetURL: "https://api.openai.com"}}
	if got := s.resolveOpenAITarget("deepseek-chat"); got != "https://api.openai.com" {
		t.Errorf("resolveOpenAITarget = %q, want the fallback", got)
	}
}
