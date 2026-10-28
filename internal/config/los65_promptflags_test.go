package config

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"gopkg.in/yaml.v3"
)

// YM-465: a prompt layer is an overlay, so a written false has to switch off an
// inherited flag. YAML 1.1 spells false as no/off/n as well, and yaml.v3 hands
// those out as strings in an untyped decode while still writing them into the
// bool field. Presence therefore has to be decided by the typed decoder.
func TestLos65YAML11FalseDisablesInheritedFlag(t *testing.T) {
	for _, spelling := range []string{"no", "off", "n", "false"} {
		t.Run(spelling, func(t *testing.T) {
			cfg := Default()
			cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_beweislast: true\n")
			cfg.Proxy.OpencodePrompt = los33PromptLayer(t, "prompt_beweislast: "+spelling+"\n")

			if cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode).Beweislast {
				t.Errorf("prompt_beweislast: %s must disable the inherited flag", spelling)
			}
		})
	}
}

// The true spellings have to keep counting as named as well: YAML 1.1 writes
// them as strings too, and a layer that names a true must still raise a flag the
// parent layer left off.
func TestLos65YAML11TrueSpellingsCountAsNamed(t *testing.T) {
	for _, spelling := range []string{"yes", "on", "y", "true"} {
		t.Run(spelling, func(t *testing.T) {
			// A bare ProxyConfig, so nothing is inherited that could mask the
			// layer's own statement.
			cfg := &ProxyConfig{OpencodePrompt: los33PromptLayer(t, "prompt_fable: "+spelling+"\n")}

			if !cfg.EffectivePromptFlags(models.ProfileOpencode).Fable {
				t.Errorf("prompt_fable: %s must count as a named true", spelling)
			}
		})
	}
}

// A key the layer does not spell out still inherits, and a named false next to
// it must not touch its neighbour.
func TestLos65NamedFalseLeavesUnnamedKeysInheriting(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_beweislast: true\nprompt_fable: true\n")
	cfg.Proxy.ClaudePrompt = los33PromptLayer(t, "prompt_beweislast: off\n")

	flags := cfg.Proxy.EffectivePromptFlags(models.ProfileClaude)
	if flags.Beweislast {
		t.Error("the named false must reach the profile, not just the shared layer")
	}
	if !flags.Fable {
		t.Error("a key the profile does not mention must keep inheriting")
	}
}

// A blank value states nothing and has to stay "no opinion", also after the
// presence check moved to the node.
func TestLos65BlankValueStillStatesNothing(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_rewrite: true\n")
	cfg.Proxy.OpencodePrompt = los33PromptLayer(t, "prompt_rewrite:\n")

	if !cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode).Rewrite {
		t.Error("a key without a value must not disable the inherited flag")
	}
}

// MarshalYAML has to write a named YAML-1.1 false as false, so the round trip
// keeps the overlay decision, and unknown keys still survive.
func TestLos65MarshalKeepsNamedFalse(t *testing.T) {
	layer := los33PromptLayer(t, "prompt_beweislast: no\nnote: free text\n")

	raw, err := yaml.Marshal(layer)
	if err != nil {
		t.Fatalf("marshal layer: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "prompt_beweislast: false") {
		t.Errorf("a named YAML-1.1 false must be written as false, got:\n%s", out)
	}
	if !strings.Contains(out, "note: free text") {
		t.Errorf("an unknown key must survive the rewrite, got:\n%s", out)
	}
	if strings.Contains(out, "prompt_fable") {
		t.Errorf("an unnamed key must not be written, got:\n%s", out)
	}

	var back PromptFlags
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-read layer: %v", err)
	}
	cfg := Default()
	cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_beweislast: true\n")
	cfg.Proxy.OpencodePrompt = &back
	if cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode).Beweislast {
		t.Error("the round trip lost the named false")
	}
}

// Merge keys carry the flags of the anchored block, and the struct decoder
// applies them, so they count as named exactly like spelled-out keys. Every
// merge form the decoder accepts has to reach the flags.
func TestLos65MergedBlockCountsAsNamed(t *testing.T) {
	docs := map[string]string{
		"alias":       "layer:\n  <<: *base\n",
		"alias list":  "layer:\n  <<: [*base]\n",
		"inline map":  "layer:\n  <<:\n    prompt_rewrite: no\n    prompt_fable: true\n",
		"alias value": "value: &v no\nlayer:\n  prompt_rewrite: *v\n  prompt_fable: true\n",
	}
	for name, tail := range docs {
		t.Run(name, func(t *testing.T) {
			doc := "base: &base\n  prompt_rewrite: no\n  prompt_fable: true\n" + tail
			var parsed struct {
				Layer *PromptFlags `yaml:"layer"`
			}
			if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
				t.Fatalf("decode merged layer: %v", err)
			}
			if parsed.Layer == nil {
				t.Fatal("layer missing after decode")
			}

			cfg := Default()
			cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_rewrite: true\n")
			cfg.Proxy.OpencodePrompt = parsed.Layer
			flags := cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode)
			if flags.Rewrite {
				t.Error("a false inherited through a merge key must count as named")
			}
			if !flags.Fable {
				t.Error("a true inherited through a merge key must count as named")
			}
		})
	}
}

// Unknown keys keep the decoder's precedence on a rewrite: a key the layer
// spells out wins over one it inherits through a merge, whatever the order in
// the document.
func TestLos65UnknownKeyBeatsMergedUnknownKey(t *testing.T) {
	docs := map[string]string{
		"explicit before merge": "base: &base\n  note: from-base\nlayer:\n  note: from-layer\n  <<: *base\n",
		"explicit after merge":  "base: &base\n  note: from-base\nlayer:\n  <<: *base\n  note: from-layer\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			var parsed struct {
				Layer *PromptFlags `yaml:"layer"`
			}
			if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
				t.Fatalf("decode: %v", err)
			}
			raw, err := yaml.Marshal(parsed.Layer)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(raw), "note: from-layer") {
				t.Errorf("the spelled-out key must survive the rewrite, got:\n%s", raw)
			}
		})
	}
}

// The key walk must never run on a node graph the decoder rejects: a layer that
// merges itself is refused by the struct decode, and walking it first would
// recurse until the process dies of a stack overflow, which Load cannot recover
// from.
func TestLos65SelfReferentialMergeIsRejected(t *testing.T) {
	doc := "layer: &l\n  <<: *l\n  prompt_fable: yes\n"
	var parsed struct {
		Layer *PromptFlags `yaml:"layer"`
	}
	if err := yaml.Unmarshal([]byte(doc), &parsed); err == nil {
		t.Fatal("a self-referential merge must be rejected with an error, not walked")
	}
}

// The walk must not depend on the decoder for termination: a cyclic node graph
// built by hand (which no real document survives the decoder with) has to come
// back empty instead of recursing.
func TestLos65WalkTerminatesOnCyclicNodes(t *testing.T) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"},
		{Kind: yaml.AliasNode, Alias: root},
	}

	named := make(map[string]bool)
	if got := collectStatedFlags(root, (&PromptFlags{}).flagFields(), named, make(map[*yaml.Node]bool)); len(got) != 0 {
		t.Errorf("a cyclic merge graph states no unknown keys, got %v", got)
	}
	if len(named) != 0 {
		t.Errorf("a cyclic merge graph states no flags, got %v", named)
	}
}

// A quoted "<<" is an ordinary key to the decoder: it never expands the anchor,
// so the decoder's own cycle check never sees that value and skips it without
// looking. The walk has to gate on the same tag, otherwise it follows an edge the
// decoder refused and terminates only by overflowing the stack.
func TestLos65QuotedMergeKeyIsNoMerge(t *testing.T) {
	t.Run("self reference stays inert", func(t *testing.T) {
		doc := "layer: &l\n  \"<<\": *l\n  prompt_fable: yes\n"
		var parsed struct {
			Layer *PromptFlags `yaml:"layer"`
		}
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("a quoted merge key must decode, the decoder ignores it: %v", err)
		}
		if parsed.Layer == nil || !parsed.Layer.Fable {
			t.Fatal("the layer's own flag must still be read")
		}
	})

	// Writing "enable" must never come out as "disable": presence recorded from a
	// quoted merge would switch off what shared_prompt enabled.
	t.Run("inherited flags are not switched off", func(t *testing.T) {
		doc := "layer:\n  \"<<\":\n    prompt_beweislast: true\n    prompt_output_discipline: true\n"
		var parsed struct {
			Layer *PromptFlags `yaml:"layer"`
		}
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		cfg := Default()
		cfg.Proxy.SharedPrompt = los33PromptLayer(t, "prompt_beweislast: true\nprompt_output_discipline: true\n")
		cfg.Proxy.OpencodePrompt = parsed.Layer

		flags := cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode)
		if !flags.Beweislast || !flags.OutputDiscipline {
			t.Errorf("a quoted merge key must state nothing, got bew=%v outdisc=%v", flags.Beweislast, flags.OutputDiscipline)
		}
	})
}
