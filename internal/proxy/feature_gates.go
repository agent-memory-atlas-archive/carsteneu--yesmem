package proxy

import (
	"strings"

	"github.com/carsteneu/yesmem/internal/config"
)

// isFeatureEnabled checks whether a behavioral feature is active for a given model.
// Resolution order, per key:
//  1. the longest matching model prefix in ModelFeatures, for the keys it names
//  2. FeatureDefaults, for a key the matching block does not name (and for a
//     model no block matches at all)
//  3. false (zero value)
func isFeatureEnabled(cfg *Config, model, feature string) bool {
	if cfg == nil {
		return false
	}

	gate := resolveFeatureGate(cfg, model)
	if gate == nil {
		return false
	}
	if gate.Names(feature) {
		return gateForFeature(gate, feature)
	}
	// A matching block that does not name the key has no opinion on it, so the
	// default speaks, exactly as it does for a model no block matches (YM-455).
	return gateForFeature(cfg.FeatureDefaults, feature)
}

// resolveFeatureGate finds the best-matching FeatureGates for a model.
// Returns FeatureDefaults if no model-specific config matches.
func resolveFeatureGate(cfg *Config, model string) *config.FeatureGates {
	if cfg.ModelFeatures != nil {
		modelLower := strings.ToLower(model)
		var bestMatch *config.FeatureGates
		bestLen := 0
		for key, gates := range cfg.ModelFeatures {
			if gates == nil {
				continue
			}
			keyLower := strings.ToLower(key)
			if strings.HasPrefix(modelLower, keyLower) || strings.HasPrefix(modelLower, keyLower+"-") {
				if len(keyLower) > bestLen {
					bestMatch = gates
					bestLen = len(keyLower)
				}
			}
		}
		if bestMatch != nil {
			return bestMatch
		}
	}
	return cfg.FeatureDefaults
}

// gateForFeature extracts a boolean from FeatureGates by feature name.
func gateForFeature(g *config.FeatureGates, feature string) bool {
	if g == nil {
		return false
	}
	switch feature {
	case "skill_eval":
		return g.SkillEval
	case "briefing":
		return g.Briefing
	case "rules_reminder":
		return g.RulesReminder
	case "plan_checkpoint":
		return g.PlanCheckpoint
	case "think_reminder":
		return g.ThinkReminder
	case "timestamps":
		return g.Timestamps
	case "assoc_context":
		return g.AssocContext
	case "loop_warning":
		return g.LoopWarning
	default:
		return false
	}
}

// resolveThinkReminderMinChars returns the minimum user text length required to trigger
// the think_reminder. Resolution: per-model FeatureGates → FeatureDefaults → 0 (always).
// A per-model block that leaves the minimum unset (0) must not erase a default:
// the block only states the values it names, and 0 is "no opinion" here, exactly
// like an absent key.
// The named key set stays out of this one on purpose: an int has no written-false
// problem, and the per-key migration writes 0 into blocks, so an explicit 0 must
// keep meaning "inherit" rather than "always".
func resolveThinkReminderMinChars(cfg *Config, model string) int {
	if cfg == nil {
		return 0
	}
	if gate := resolveFeatureGate(cfg, model); gate != nil && gate.ThinkReminderMinChars > 0 {
		return gate.ThinkReminderMinChars
	}
	if cfg.FeatureDefaults != nil && cfg.FeatureDefaults.ThinkReminderMinChars > 0 {
		return cfg.FeatureDefaults.ThinkReminderMinChars
	}
	return 0
}
