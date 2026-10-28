package daemon

import "github.com/carsteneu/yesmem/internal/extraction"

func (h *Handler) redact(s string) string {
	if h.redactor == nil {
		return s
	}
	return h.redactor.Sanitize(s)
}

// sanitizeClient wraps c with a SanitizingClient when a redactor is configured,
// and returns c unchanged otherwise. The SanitizingClient is the OUTERMOST
// decorator (see the contract in internal/extraction/sanitizing_client.go), so
// callers apply it after any budget or gate wrapping. Applied a second time to a
// client that already carries a redactor it is harmless — redaction is
// idempotent — which keeps a caller from having to know how its client was wired.
func (h *Handler) sanitizeClient(c extraction.LLMClient) extraction.LLMClient {
	if c == nil || h.redactor == nil {
		return c
	}
	return extraction.NewSanitizingClient(c, h.redactor)
}
