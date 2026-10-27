package daemon

import (
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/sanitize"
)

// llmClientOpts describes one daemon LLM construction site. Zero-valued fields
// fall back to a sane default: provider to ac.LLM.Provider, baseURL to
// ac.ResolvedOpenAIBaseURL(), tracker/gate to "no such layer".
type llmClientOpts struct {
	provider     string // empty → ac.LLM.Provider
	apiKey       string
	model        string
	claudeBinary string
	baseURL      string // empty → ac.ResolvedOpenAIBaseURL()

	tracker    *extraction.BudgetTracker // nil → no budget metering
	gate       *extraction.APIGate       // nil → no api gate
	gateBucket string                    // "extract" | "quality"
	throttle   func() bool

	maxPerCall float64 // 0 → no per-call cap
}

// Decoration inventory of the daemon's LLM paths. Every path carries the
// redactor (when secrets_sanitization.enabled) and reaches its provider through
// newDaemonLLMClient; the budget/gate/cap layers are NOT uniform:
//
//	path                        | tracker | gate | per-call cap | built by
//	extract   (client)          | extract | yes  | yes          | meteredOpts
//	quality   (qualityClient)   | quality | yes  | yes          | meteredOpts
//	llm_complete override       | extract | yes  | yes          | meteredOpts
//	bash-job auto-correct       | extract | yes  | yes          | meteredOpts
//	two-pass summarize/extract  | extract |  –   | –            | literal
//	summarize (Haiku, base)     | –       | –    | –            | literal
//	briefing refinement         | –       | yes  | –            | literal
//	gap review                  | extract | –    | –            | literal
//
// The columns show the configured case: without a daily extraction budget the
// extract tracker is nil and those rows meter nothing, and without
// max_budget_per_call_usd the cap column is zero. The quality handle carries the
// *extraction* tracker unless a separate narrative model is configured.
//
// The lower rows end up with the same layer set as before the factory existed;
// adding a gate or a cap there is a behaviour change independent of the
// redaction switch, so it is deliberately not part of this change and needs its
// own ticket and its own red test. Package main builds its own clients in
// cmd_maintenance.go and the other cmd_*.go files, outside both this inventory
// and the guard below; it applies the redactor itself through redacted(), and
// redactor_coverage_los06c_test.go requires that wrap for every construction
// site. internal/setup does the same through redactedForDataDir.
//
// llmClientEnv carries the daemon-wide handles the fully decorated paths share.
type llmClientEnv struct {
	apiKey       string
	claudeBinary string
	baseURL      string
	tracker      *extraction.BudgetTracker
	gate         *extraction.APIGate
	throttle     func() bool
	maxPerCall   float64
}

// daemonLLMEnv gathers the daemon-wide handles. Listing them in one function
// instead of an inline literal at each site keeps the shared tracker, the gate,
// the throttle and the cap from being dropped by omission.
func daemonLLMEnv(apiKey, claudeBinary, baseURL string, tracker *extraction.BudgetTracker, gate *extraction.APIGate, throttle func() bool, maxPerCall float64) llmClientEnv {
	return llmClientEnv{
		apiKey:       apiKey,
		claudeBinary: claudeBinary,
		baseURL:      baseURL,
		tracker:      tracker,
		gate:         gate,
		throttle:     throttle,
		maxPerCall:   maxPerCall,
	}
}

// meteredOpts builds the opts for a fully decorated daemon path. The layers are
// filled here rather than at each call site, so a site cannot drop the tracker,
// the gate or the cap by omission — the paths that deliberately run without a
// layer build a literal instead (see the inventory above).
func meteredOpts(env llmClientEnv, model, bucket string) llmClientOpts {
	return llmClientOpts{
		apiKey:       env.apiKey,
		claudeBinary: env.claudeBinary,
		baseURL:      env.baseURL,
		model:        model,
		tracker:      env.tracker,
		gate:         env.gate,
		gateBucket:   bucket,
		throttle:     env.throttle,
		maxPerCall:   env.maxPerCall,
	}
}

// newDaemonLLMClient builds the LLM handle for one daemon path and returns it
// fully decorated.
//
// It is the only place in this package that constructs an extraction.LLMClient,
// enforced by TestDaemonLLMConstructionSitesAreCentralised. Centralising the
// construction is what keeps every daemon LLM path on the same decoration: a
// path that builds its own client silently loses the redaction wrapper, the
// budget metering, the api gate or the per-call cap.
func newDaemonLLMClient(ac *config.Config, o llmClientOpts) (extraction.LLMClient, error) {
	provider := o.provider
	if provider == "" {
		provider = ac.LLM.Provider
	}
	baseURL := o.baseURL
	if baseURL == "" {
		baseURL = ac.ResolvedOpenAIBaseURL()
	}

	raw, err := extraction.NewLLMClient(provider, o.apiKey, o.model, o.claudeBinary, baseURL)
	if err != nil || raw == nil {
		return raw, err
	}
	return decorateDaemonLLMClient(raw, ac, o), nil
}

// decorateDaemonLLMClient applies the standard daemon decoration to a raw
// client:
//
//	raw  ->  BudgetClient (extract|quality)  ->  GatedClient  ->  SanitizingClient
//
// The redactor stays the OUTERMOST layer per the contract in
// internal/extraction/sanitizing_client.go, so prompts are redacted before they
// reach any metering or transport code and completions are redacted before any
// caller logs or persists them. Redaction is off only when
// secrets_sanitization.enabled is false, in which case the client is returned
// with budget and gate only — the same shape it had before the wrapper existed.
func decorateDaemonLLMClient(raw extraction.LLMClient, ac *config.Config, o llmClientOpts) extraction.LLMClient {
	c := raw
	if c == nil {
		return nil
	}
	if o.tracker != nil {
		bc := extraction.NewBudgetClient(c, o.tracker)
		bc.ThrottleFn = o.throttle
		// Meter at the configured rates: the builtin table in extraction does not
		// know every provider model and would fall back to sonnet prices.
		if ac != nil {
			inPerM, outPerM := ac.PricingForModel(c.Model())
			bc.SetPricing(inPerM, outPerM)
		}
		c = bc
	}
	if o.gate != nil {
		c = extraction.NewGatedClient(c, o.gate, o.gateBucket)
	}
	if o.maxPerCall > 0 {
		// Before the sanitization wrap: the cap is a CLI flag, and reaching the
		// CLI client through the decorator chain is what SetMaxBudgetPerCall does.
		extraction.SetMaxBudgetPerCall(c, o.maxPerCall)
	}
	if ac == nil || !ac.SecretsSanitization.Enabled {
		return c
	}
	return wrapWithSanitizationIfEnabled(c, ac, sanitize.NewSecretRedactor(ac.SecretsSanitization.AllowedExceptions))
}
