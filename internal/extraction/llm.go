package extraction

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/models"
)

// LLMClient abstracts LLM calls across HTTP and CLI backends.
type LLMClient interface {
	Complete(system, userMsg string, opts ...CallOption) (string, error)
	CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error)
	Name() string  // backend/provider name for logging
	Model() string // full model ID
}

// CallOption configures a single LLM call.
type CallOption func(*callOpts)

// usageSink receives the measured token counts of the call it belongs to. A
// transport may report more than once per call (one step_finish per tool
// roundtrip on the opencode path), so a sink accumulates instead of replacing.
// A metering decorator installs one per call, so real usage is booked on the
// tracker of the path that made the call instead of a daemon-wide default.
type usageSink func(inputTokens, outputTokens int)

type callOpts struct {
	maxTokens int    // 0 = use default (8192)
	sessionID string // opencode session to resume
	tools     bool   // allow MCP tools in subprocess (sets YESMEM_ALLOW_CHILD_MCP=1)

	usageSink usageSink // set by the metering layer only
}

// withUsageSink attaches the real-usage sink for this call. Unexported: only
// BudgetClient sets it, so a caller cannot divert a client's usage accounting.
func withUsageSink(sink usageSink) CallOption {
	return func(o *callOpts) { o.usageSink = sink }
}

// reportMeasuredUsage reports a transport's own token counts, clamped and only
// when at least one of them is real. The counts come from an external process or
// a configurable endpoint: a negative value would credit the budget, an absurd
// one would block the rest of the day, and an all-zero usage block (sent by
// gateways that do not meter) would mark the call as MEASURED and book it at $0,
// so the whole call disappears from the budget instead of falling back to the
// char estimate.
//
// The counts go to this call's sink. Without one - clients built outside the
// metered paths (summarize, briefing refinement, gap review, package main) - they
// fall back to the package-wide OnUsage hook, which is where those paths have
// always booked.
func reportMeasuredUsage(sink usageSink, model string, inputTokens, outputTokens int) {
	in, out := clampTokens(inputTokens), clampTokens(outputTokens)
	if in+out == 0 {
		return
	}
	if sink != nil {
		sink(in, out)
		return
	}
	if OnUsage != nil {
		OnUsage(model, in, out)
	}
}

// WithMaxTokens sets the max output tokens for this call.
func WithMaxTokens(n int) CallOption {
	return func(o *callOpts) { o.maxTokens = n }
}

// WithSession sets the session ID to resume (opencode CLI).
func WithSession(id string) CallOption {
	return func(o *callOpts) { o.sessionID = id }
}

// WithTools enables MCP tool access in subprocess calls (opencode CLI).
// Sets YESMEM_ALLOW_CHILD_MCP=1 so the child yesmem mcp process is not killed.
func WithTools() CallOption {
	return func(o *callOpts) { o.tools = true }
}

func applyOpts(opts []CallOption) callOpts {
	o := callOpts{maxTokens: 8192}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// NewLLMClient creates the appropriate client based on config.
// "api" stays the Anthropic HTTP alias for backward compatibility.
func NewLLMClient(provider, apiKey, model, claudeBinary, baseURL string) (LLMClient, error) {
	switch provider {
	case "api":
		if apiKey == "" {
			return nil, fmt.Errorf("llm.provider=api but no API key available")
		}
		return NewClient(apiKey, model), nil

	case "openai":
		if apiKey == "" {
			return nil, fmt.Errorf("llm.provider=openai but no API key available")
		}
		return NewOpenAIClient(apiKey, model, "", "openai"), nil

	case "openai_compatible":
		if apiKey == "" {
			return nil, fmt.Errorf("llm.provider=openai_compatible but no API key available")
		}
		return NewOpenAIClient(apiKey, model, baseURL, "openai_compatible"), nil

	case "cli":
		bin := resolveClaudeBinary(claudeBinary)
		if bin == "" {
			return nil, fmt.Errorf("llm.provider=cli but claude binary not found in PATH")
		}
		return NewCLIClient(bin, model, models.SourceAgentClaude), nil

	case "opencode":
		bin := resolveBinary("opencode")
		if bin == "" {
			return nil, fmt.Errorf("llm.provider=opencode but opencode binary not found in PATH")
		}
		return NewCLIClient(bin, model, models.SourceAgentOpencode), nil

	case "codex":
		bin := resolveBinary("codex")
		if bin == "" {
			return nil, fmt.Errorf("llm.provider=codex but codex binary not found in PATH")
		}
		return NewCLIClient(bin, model, models.SourceAgentCodex), nil

	case "auto", "":
		// API key available → use API
		if apiKey != "" {
			return NewClient(apiKey, model), nil
		}
		// Try CLI fallback
		bin := resolveClaudeBinary(claudeBinary)
		if bin != "" {
			return NewCLIClient(bin, model, models.SourceAgentClaude), nil
		}
		// Neither available
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown llm.provider: %q (valid: auto, api, cli, opencode, codex, openai, openai_compatible)", provider)
	}
}

// resolveClaudeBinary finds the claude binary path.
func resolveClaudeBinary(configured string) string {
	if configured != "" {
		if _, err := exec.LookPath(configured); err == nil {
			return configured
		}
		return ""
	}
	return resolveBinary("claude")
}

func resolveBinary(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	fallbacks := map[string]string{
		"opencode": filepath.Join(home, ".opencode", "bin", "opencode"),
	}
	if fb, ok := fallbacks[name]; ok {
		if _, err := os.Stat(fb); err == nil {
			return fb
		}
	}
	return ""
}
