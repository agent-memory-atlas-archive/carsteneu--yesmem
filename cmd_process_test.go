package main

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
)

// YM-136: the profile layers must survive the mapping from config.yaml into the
// proxy's runtime config. They once did not: the resolution lived on a server
// field that no construction site assigned, so the whole feature was inert in
// the running binary and only the tests, which wired it by hand, saw it work.
func TestBuildProxyConfigCarriesPromptLayers(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.SharedPrompt = &config.PromptFlags{Beweislast: true}
	cfg.Proxy.OpencodePrompt = &config.PromptFlags{ClarifyFirst: true}

	pcfg := buildProxyConfig(cfg, t.TempDir())

	if pcfg.Prompt == nil {
		t.Fatal("proxy config does not carry the prompt layers")
	}
	opencode := pcfg.Prompt.EffectivePromptFlags(models.ProfileOpencode)
	if !opencode.ClarifyFirst {
		t.Error("opencode_prompt layer lost in the mapping")
	}
	if !opencode.Beweislast {
		t.Error("shared_prompt layer lost in the mapping")
	}
	if pcfg.Prompt.EffectivePromptFlags(models.ProfileCodex).ClarifyFirst {
		t.Error("opencode layer leaked into another profile")
	}
}
