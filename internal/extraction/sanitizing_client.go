package extraction

import (
	"errors"

	"github.com/carsteneu/yesmem/internal/sanitize"
)

var _ LLMClient = (*SanitizingClient)(nil)

// SanitizingClient wraps an LLMClient and runs all string inputs through
// a Sanitizer before sending, and all string outputs through the Sanitizer
// after receiving. Schemas (for CompleteJSON) are structural and are passed
// through unchanged. Name() and Model() are pass-throughs.
type SanitizingClient struct {
	inner LLMClient
	san   sanitize.Sanitizer
}

// NewSanitizingClient wraps inner with a Sanitizer that runs before forwarding
// each prompt and after receiving each completion. san must not be nil; wrapping
// is explicit at the call site.
//
// Decorator order convention. SanitizingClient must be the OUTERMOST decorator
// in the LLMClient stack. The full chain reads inner-to-outer:
//
//	raw client  ->  BudgetClient  ->  GatedClient  ->  SanitizingClient
//
// Reasons to keep SanitizingClient outermost:
//
//  1. SetMaxBudgetPerCall in internal/extraction/cli_client.go recurses through
//     SanitizingClient, GatedClient and BudgetClient to reach CLIClient, so the
//     per-call cap stays reachable no matter which decorator the caller holds.
//     Construction sites still call SetMaxBudgetPerCall on the inner handle
//     BEFORE applying the sanitization wrap, keeping the recursion path short.
//  2. CheckBudget and HasBudget in internal/extraction/budget.go peel the
//     decorators that can sit in front of the metering chain, SanitizingClient
//     included (unwrapMetering), so they keep finding the BudgetClient no matter
//     which decorator the caller holds.
//  3. GatedClient and BudgetClient sit inside the redactor, so they meter the
//     text the provider actually receives. Redaction usually shrinks a payload,
//     so the estimate then runs slightly low (a longer marker can also push it
//     up); it is an accepted trade-off, since the provider is billed for the
//     redacted text. Real token usage from the response, when the provider
//     reports it, is unaffected.
//  4. Outputs from the inner stack pass through the Sanitizer before any
//     caller-side logging or persistence; placing SanitizingClient inside
//     another wrapper would let raw secrets escape into instrumentation.
//
// Construction sites apply this convention through newDaemonLLMClient in
// internal/daemon/llm_client_factory.go, which is the only place in the daemon
// package that builds a client and returns it unchanged when
// secrets_sanitization.enabled is false. Direct calls to NewSanitizingClient
// exist where no factory is in reach: the client/qualityClient wires in
// internal/daemon/quickstart.go and the llm_complete boundary in
// internal/daemon/handler_redact.go.
func NewSanitizingClient(inner LLMClient, san sanitize.Sanitizer) *SanitizingClient {
	return &SanitizingClient{inner: inner, san: san}
}

func (c *SanitizingClient) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	system = c.san.Sanitize(system)
	userMsg = c.san.Sanitize(userMsg)
	out, err := c.inner.Complete(system, userMsg, opts...)
	return c.san.Sanitize(out), c.redactErr(err)
}

func (c *SanitizingClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	system = c.san.Sanitize(system)
	userMsg = c.san.Sanitize(userMsg)
	out, err := c.inner.CompleteJSON(system, userMsg, schema, opts...)
	return c.san.Sanitize(out), c.redactErr(err)
}

// redactErr runs the error text through the Sanitizer. A CLI client reports
// provider and process output verbatim and callers log the error, so the text
// leaks what the output path already redacts.
func (c *SanitizingClient) redactErr(err error) error {
	if err == nil {
		return nil
	}
	redacted := c.san.Sanitize(err.Error())
	if redacted == err.Error() {
		return err
	}
	return &redactedError{msg: redacted, err: err}
}

// redactedError reports a redacted message and keeps the chain reachable for
// errors.Is/As — ErrBudgetExceeded travels out through this decorator. It
// deliberately does not implement Unwrap: that would hand the raw text, the leak
// this type exists to close, to any caller that walks the chain.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string        { return e.msg }
func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }
func (e *redactedError) As(target any) bool   { return errors.As(e.err, target) }

func (c *SanitizingClient) Name() string  { return c.inner.Name() }
func (c *SanitizingClient) Model() string { return c.inner.Model() }
