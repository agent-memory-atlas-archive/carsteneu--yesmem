package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// CacheKeepaliveConfig holds configuration for the keepalive timer.
type CacheKeepaliveConfig struct {
	Target           string            // API endpoint URL (default upstream)
	ProviderTargets  map[string]string // per-provider upstream URLs, e.g. {"deepseek": "https://api.deepseek.com"}
	Mode             string            // "auto", "5m", "1h"
	Pings5m          int               // max pings per phase when TTL=5min
	Pings1h          int               // max pings per phase when TTL=1h
	MinMessages      int               // skip keepalive when request body has fewer messages (0 = always)
	Detector         *CacheTTLDetector // for auto mode
	IntervalOverride time.Duration     // testing only: override computed interval
	TimeoutOverride  time.Duration     // testing only: override the derived ping timeout
	Logger           *log.Logger
	OnPing           func(threadID string, cacheRead, cacheWrite, ephemeral1h int) // optional callback, successful pings only
}

// threadState stores per-thread request body, timer, and ping state.
type threadState struct {
	body           []byte
	apiKey         string
	anthropicBeta  string // original anthropic-beta header; beta-gated body fields need it
	lastUsed       time.Time
	timer          *time.Timer
	pingsRemaining int
	generation     uint64
}

// CacheKeepalive sends bounded ping requests to keep the prompt cache warm.
// Each thread gets its own independent timer so active sessions don't starve quiet ones.
type CacheKeepalive struct {
	cfg     CacheKeepaliveConfig
	mu      sync.Mutex
	threads map[string]*threadState // threadID → state
	client  *http.Client
}

const (
	threadEvictionAge = 2 * time.Hour
	// keepaliveMaxThreads bounds the tracked threads. Every entry keeps a full
	// prompt body, so the age rule alone leaves the memory bound to whatever a
	// caller invents as thread IDs within two hours.
	keepaliveMaxThreads = 256
	// keepaliveMaxResponseBytes bounds the ping response body. A ping asks for up
	// to 8000 output tokens and the body is upstream-controlled content, so an
	// unbounded read is an unbounded allocation on every ping.
	keepaliveMaxResponseBytes = 1 << 20

	// A ping is a cache read of the whole prompt, so the time it needs grows
	// with the context. The fixed 20s client timeout failed every ping above
	// roughly 750k tokens (live 2026-10-18, YM-347) while real requests kept
	// working: the window belongs to the request size, not to a constant.
	keepalivePingTimeoutFloor = 30 * time.Second
	// keepalivePingTimeoutCeil is the hard bound of that window and at the same
	// time the client timeout. A hung upstream must not pin the ping slot until
	// the thread is evicted two hours later, so the bound stays below the
	// shortest ping interval (270s in 5m mode) and a ping cannot swallow the slot
	// of the next one. The cycle is interval plus ping duration either way.
	keepalivePingTimeoutCeil = 4 * time.Minute
	// keepaliveBytesPerToken estimates the prompt size from the stored JSON
	// body. This is a scale estimate for the budget, not billing arithmetic.
	keepaliveBytesPerToken = 4
	// keepaliveTokensPerSecond is the cache-read rate the budget assumes. It is
	// deliberately far below the measured throughput: a ping must survive a slow
	// day, and its latency costs nobody anything because it runs off the request
	// path.
	keepaliveTokensPerSecond = 10000
)

// keepalivePingTimeout is the deadline for one ping of a body of that size.
// The step is a full second: a packet of a few hundred bytes must land exactly
// on the floor, and sub-second additions buy nothing.
func keepalivePingTimeout(bodyLen int) time.Duration {
	if bodyLen <= 0 {
		return keepalivePingTimeoutFloor
	}
	seconds := bodyLen / keepaliveBytesPerToken / keepaliveTokensPerSecond
	// Clamp before the multiplication: a body big enough to overflow a duration
	// is past the ceiling anyway.
	if seconds > int(keepalivePingTimeoutCeil/time.Second) {
		return keepalivePingTimeoutCeil
	}
	d := keepalivePingTimeoutFloor + time.Duration(seconds)*time.Second
	if d > keepalivePingTimeoutCeil {
		return keepalivePingTimeoutCeil
	}
	return d
}

// pingTimeout applies the test override to the derived budget.
func (ka *CacheKeepalive) pingTimeout(bodyLen int) time.Duration {
	if ka.cfg.TimeoutOverride > 0 {
		return ka.cfg.TimeoutOverride
	}
	return keepalivePingTimeout(bodyLen)
}

// errPingBudget reports a ping that spent its own deadline. It is a separate
// value because the transport's timeout errors satisfy
// errors.Is(err, context.DeadlineExceeded) too: net's errTimeout matches that
// sentinel on purpose, and a dial timeout is an upstream failure, not a large
// context. Only the request's own context can witness which one fired.
var errPingBudget = errors.New("keepalive ping budget exceeded")

// isPingTimeout separates a spent local budget from an upstream answer, so
// proxy_health can count the first apart from the second.
func isPingTimeout(err error) bool {
	return errors.Is(err, errPingBudget)
}

// pingErrorFor labels a ping failure as a spent budget when the request's own
// deadline expired, and leaves it as an upstream failure otherwise.
func pingErrorFor(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: %v", errPingBudget, err)
	}
	return err
}

// NewCacheKeepalive creates a new keepalive timer (initially idle).
func NewCacheKeepalive(cfg CacheKeepaliveConfig) *CacheKeepalive {
	return &CacheKeepalive{
		cfg:     cfg,
		threads: make(map[string]*threadState),
		client: &http.Client{
			// The per-request deadline (derived from the body size) governs the
			// ping; this is only its hard upper bound.
			Timeout: keepalivePingTimeoutCeil,
			// The ping carries the session credential in x-api-key, and Go copies
			// that header to a redirect target. A redirect is no ping answer
			// either, so the client keeps it and the ping reports it as an error.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// effectiveInterval returns the ping interval based on mode and detected TTL.
func (ka *CacheKeepalive) effectiveInterval() time.Duration {
	if ka.cfg.IntervalOverride > 0 {
		return ka.cfg.IntervalOverride
	}
	switch ka.cfg.Mode {
	case "5m":
		return time.Duration(float64(300)*0.9) * time.Second // 270s
	case "1h":
		return time.Duration(float64(3600)*0.9) * time.Second // 3240s
	default: // "auto"
		if ka.cfg.Detector != nil {
			sup := ka.cfg.Detector.Is1hSupported()
			if sup != nil && *sup {
				return time.Duration(float64(3600)*0.9) * time.Second
			}
		}
		return time.Duration(float64(300)*0.9) * time.Second
	}
}

// effectivePings returns the ping count based on mode and detected TTL.
func (ka *CacheKeepalive) effectivePings() int {
	switch ka.cfg.Mode {
	case "5m":
		return ka.cfg.Pings5m
	case "1h":
		return ka.cfg.Pings1h
	default: // "auto"
		if ka.cfg.Detector != nil {
			sup := ka.cfg.Detector.Is1hSupported()
			if sup == nil {
				return 0 // detection pending: don't interfere
			}
			if *sup {
				return ka.cfg.Pings1h
			}
		}
		return ka.cfg.Pings5m
	}
}

// Reset stores the request body for a thread and starts/resets only that thread's timer.
// Skips internal/automated threads — only real user sessions get keepalive.
// anthropicBeta is the original request's anthropic-beta header; the ping must
// resend it, otherwise beta-gated body fields are rejected as extra inputs.
func (ka *CacheKeepalive) Reset(threadID string, requestBody []byte, apiKey, anthropicBeta string) {
	if !isRealUserSession(threadID) {
		return
	}
	if !isClaudeModel(requestBody) {
		return
	}
	ka.mu.Lock()
	defer ka.mu.Unlock()

	ts := ka.threads[threadID]
	if ts != nil && ts.timer != nil {
		ts.timer.Stop()
	}

	if ts == nil {
		ts = &threadState{}
		ka.threads[threadID] = ts
	}

	ts.body = make([]byte, len(requestBody))
	copy(ts.body, requestBody)
	ts.apiKey = apiKey
	ts.anthropicBeta = anthropicBeta
	ts.lastUsed = time.Now()

	ka.evictStaleLocked()

	pings := ka.effectivePings()
	if pings <= 0 {
		return
	}

	if ka.cfg.MinMessages > 0 {
		var tmp struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(requestBody, &tmp) == nil && len(tmp.Messages) < ka.cfg.MinMessages {
			return
		}
	}

	ts.pingsRemaining = pings
	ts.generation++
	gen := ts.generation
	ts.timer = time.AfterFunc(ka.effectiveInterval(), func() {
		ka.sendPingForThread(threadID, gen)
	})
}

// Retrigger restarts keepalive timers for all threads with recalculated interval
// and ping count. A detection change starts a new phase: the thread that spent
// its pings while discovering the change gets the new budget too.
func (ka *CacheKeepalive) Retrigger() {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	// Expired threads must not be resurrected by a new phase: their stored body
	// is a full prompt and their session stopped asking long ago.
	ka.evictStaleLocked()
	if len(ka.threads) == 0 {
		return
	}
	pings := ka.effectivePings()
	if pings <= 0 {
		return
	}
	for tid, ts := range ka.threads {
		if ts.timer != nil {
			ts.timer.Stop()
		}
		ts.pingsRemaining = pings
		ts.generation++
		gen := ts.generation
		capturedTID := tid
		ts.timer = time.AfterFunc(ka.effectiveInterval(), func() {
			ka.sendPingForThread(capturedTID, gen)
		})
	}
}

// Stop cancels all pending timers.
func (ka *CacheKeepalive) Stop() {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	for _, ts := range ka.threads {
		if ts.timer != nil {
			ts.timer.Stop()
		}
	}
}

// KeepaliveStatus holds current keepalive state for status display.
type KeepaliveStatus struct {
	Mode           string // "auto", "5m", "1h"
	IntervalS      int    // current ping interval in seconds
	TotalPings     int    // configured total pings (constant)
	PingsRemaining int    // max pings remaining across any single thread
	ActiveThreads  int    // number of tracked threads
}

// Status returns the current keepalive state for status display.
func (ka *CacheKeepalive) Status() KeepaliveStatus {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	maxRemaining := 0
	for _, ts := range ka.threads {
		if ts.pingsRemaining > maxRemaining {
			maxRemaining = ts.pingsRemaining
		}
	}
	return KeepaliveStatus{
		Mode:           ka.cfg.Mode,
		IntervalS:      int(ka.effectiveInterval().Seconds()),
		TotalPings:     ka.effectivePings(),
		PingsRemaining: maxRemaining,
		ActiveThreads:  len(ka.threads),
	}
}

func (ka *CacheKeepalive) sendPingForThread(threadID string, expectedGen uint64) {
	ka.mu.Lock()
	ts := ka.threads[threadID]
	if ts == nil || ts.generation != expectedGen || ts.pingsRemaining <= 0 {
		ka.mu.Unlock()
		return
	}
	ts.pingsRemaining--
	// The slot is spent before the request: a failing ping must not turn the
	// bounded phase into an unbounded retry loop.
	remaining := ts.pingsRemaining
	body := make([]byte, len(ts.body))
	copy(body, ts.body)
	apiKey := ts.apiKey
	anthropicBeta := ts.anthropicBeta
	ka.mu.Unlock()

	pingBody := buildPingBody(body)
	cacheRead, cacheWrite, outputTokens, ephemeral1h, pingErr := ka.doHTTPPing(pingBody, apiKey, anthropicBeta)

	if pingErr != nil {
		// A failed ping neither warms the cache nor proves it is warm, so the
		// OnPing consumers (sawtooth pause timestamp, frozen stubs, TTL detector)
		// must stay untouched.
		//
		// A spent local budget and an upstream failure are different diagnoses:
		// the first says the context outgrew its window (and names the size),
		// the second says the provider failed. proxy_health counts them apart.
		if ka.cfg.Logger != nil {
			ctxTokens := len(pingBody) / keepaliveBytesPerToken
			timeout := ka.pingTimeout(len(pingBody))
			if isPingTimeout(pingErr) {
				ka.cfg.Logger.Printf("[keepalive] ping timeout tid=%s ctx_tokens=%d body_bytes=%d timeout=%s remaining=%d mode=%s: %v",
					journalToken(threadID), ctxTokens, len(pingBody), timeout.Truncate(time.Millisecond), remaining, ka.cfg.Mode, pingErr)
			} else {
				ka.cfg.Logger.Printf("[keepalive] ping error tid=%s ctx_tokens=%d remaining=%d mode=%s: %v",
					journalToken(threadID), ctxTokens, remaining, ka.cfg.Mode, pingErr)
			}
		}
	} else {
		if ka.cfg.OnPing != nil {
			ka.cfg.OnPing(threadID, cacheRead, cacheWrite, ephemeral1h)
		}
		if ka.cfg.Logger != nil {
			ka.cfg.Logger.Printf("[keepalive] ping tid=%s cache_read=%d cache_write=%d out=%d remaining=%d mode=%s",
				journalToken(threadID), cacheRead, cacheWrite, outputTokens, remaining, ka.cfg.Mode)
		}
	}

	ka.mu.Lock()
	if ts.generation == expectedGen && ts.pingsRemaining > 0 {
		// An armed phase is activity: without this stamp a 1h phase with several
		// pings lives longer than threadEvictionAge and evicts itself.
		ts.lastUsed = time.Now()
		gen := ts.generation
		ts.timer = time.AfterFunc(ka.effectiveInterval(), func() {
			ka.sendPingForThread(threadID, gen)
		})
	}
	ka.mu.Unlock()
}

// evictStaleLocked drops expired threads and enforces the hard cap oldest
// first. Caller holds mu. The thread being reset has just been stamped with
// now, so it is never the first to go.
func (ka *CacheKeepalive) evictStaleLocked() {
	cutoff := time.Now().Add(-threadEvictionAge)
	for tid, ts := range ka.threads {
		if ts.lastUsed.Before(cutoff) {
			ka.dropThreadLocked(tid, ts)
		}
	}
	if len(ka.threads) <= keepaliveMaxThreads {
		return
	}
	type entry struct {
		tid  string
		used time.Time
	}
	entries := make([]entry, 0, len(ka.threads))
	for tid, ts := range ka.threads {
		entries = append(entries, entry{tid, ts.lastUsed})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for _, e := range entries[:len(entries)-keepaliveMaxThreads] {
		if ts, ok := ka.threads[e.tid]; ok {
			ka.dropThreadLocked(e.tid, ts)
		}
	}
}

func (ka *CacheKeepalive) dropThreadLocked(threadID string, ts *threadState) {
	if ts.timer != nil {
		ts.timer.Stop()
	}
	delete(ka.threads, threadID)
}

// setPingAuth applies the thread's credential in the form the upstream accepts:
// subscription clients (Claude Code OAuth) send Authorization: Bearer, API-key
// clients x-api-key. A Bearer value in x-api-key is rejected with 401.
func setPingAuth(req *http.Request, credential string) {
	if strings.HasPrefix(credential, "Bearer ") {
		req.Header.Set("Authorization", credential)
		return
	}
	req.Header.Set("x-api-key", credential)
}

// doHTTPPing sends one keepalive ping. A ping only counts as successful when
// the upstream answered with cache accounting: a 200 without cache_read and
// cache_write tokens proves nothing about the cache (live log: 33 of 124 pings
// reported exactly that with no error at all).
func (ka *CacheKeepalive) doHTTPPing(body []byte, apiKey, anthropicBeta string) (cacheRead, cacheWrite, outputTokens, ephemeral1h int, err error) {
	endpoint := ka.resolveTarget(body) + "/v1/messages"
	req, err := http.NewRequest("POST", endpoint, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	setPingAuth(req, apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	if anthropicBeta != "" {
		req.Header.Set("anthropic-beta", anthropicBeta)
	}
	req.Body = io.NopCloser(bytesReader(body))
	req.ContentLength = int64(len(body))
	// The budget is the request size, not a constant: a ping of a 750k-token
	// context is a large upload and a large cache read (YM-347).
	ctx, cancel := context.WithTimeout(context.Background(), ka.pingTimeout(len(body)))
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := ka.client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, pingErrorFor(ctx, err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, keepaliveMaxResponseBytes+1))

	if resp.StatusCode != http.StatusOK {
		// The status check comes first, like the size check below: an error answer
		// keeps its status in the message even when its body breaks off mid-read.
		// The body is foreign text on a journal line the health cap reads, so a
		// line break must not survive into the error (YM-380).
		return 0, 0, 0, 0, fmt.Errorf("HTTP %d body=%s", resp.StatusCode, oneLineForJournal(truncateBytes(respBody, 200)))
	}
	if readErr != nil {
		// The budget usually runs out here, not at the header: the upstream
		// writes the headers in milliseconds and the body is the expensive part.
		// Dropping this error turned a spent budget into "unparsable response",
		// which proxy_health then counted as an upstream failure (YM-347).
		return 0, 0, 0, 0, pingErrorFor(ctx, fmt.Errorf("reading response body: %w", readErr))
	}

	// The size check runs after the status check: an error answer keeps its
	// status in the message even when its body is larger than the limit.
	if len(respBody) > keepaliveMaxResponseBytes {
		return 0, 0, 0, 0, fmt.Errorf("response body exceeds %d bytes", keepaliveMaxResponseBytes)
	}

	var usage struct {
		Usage struct {
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreation            struct {
				Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &usage); err != nil {
		// The body of a 200 is upstream-controlled content (could be a full
		// completion); the status and the token counters are enough to diagnose.
		return 0, 0, 0, 0, fmt.Errorf("unparsable response: %v", err)
	}
	if usage.Usage.CacheReadInputTokens == 0 && usage.Usage.CacheCreationInputTokens == 0 {
		return 0, 0, 0, 0, fmt.Errorf("response without cache accounting (cache_read=0 cache_write=0 out=%d)", usage.Usage.OutputTokens)
	}
	return usage.Usage.CacheReadInputTokens, usage.Usage.CacheCreationInputTokens, usage.Usage.OutputTokens, usage.Usage.CacheCreation.Ephemeral1hInputTokens, nil
}

// buildPingBody sets max_tokens=8000 and stream=false on the request body.
// Keeps thinking intact (it's part of the cache key / request hash).
// max_tokens=8000 is above the implicit budget for adaptive thinking
// (avoids "max_tokens must be greater than thinking.budget_tokens").
// Cost: each ping can produce up to 8000 thinking tokens if the model
// decides to think — in practice these are near-zero because the input
// asks nothing new.
func buildPingBody(original []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(original, &m); err != nil {
		return original
	}
	maxTok := 8000
	if raw, ok := m["thinking"]; ok {
		var thinking struct {
			BudgetTokens int `json:"budget_tokens"`
		}
		if json.Unmarshal(raw, &thinking) == nil && thinking.BudgetTokens >= maxTok {
			maxTok = thinking.BudgetTokens + 1000
		}
	}
	m["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", maxTok))
	m["stream"] = json.RawMessage("false")
	delete(m, "metadata")
	delete(m, "context_management")
	out, err := json.Marshal(m)
	if err != nil {
		return original
	}
	return out
}

// bytesReader is a helper that wraps a byte slice as an io.Reader.
func bytesReader(b []byte) io.Reader {
	return &bytesReaderImpl{data: b}
}

type bytesReaderImpl struct {
	data []byte
	pos  int
}

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (ka *CacheKeepalive) resolveTarget(body []byte) string {
	var tmp struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &tmp)
	// Same rule as the request path, from the same helper: a ping must reach the
	// upstream the request it keeps warm is routed to.
	if url, ok := matchProviderTarget(tmp.Model, ka.cfg.ProviderTargets); ok {
		return url
	}
	return ka.cfg.Target
}

// isClaudeModel returns true when the request body contains a Claude (Anthropic) model.
// Non-Claude models (DeepSeek, GPT, etc.) use automatic caching and don't need keepalive.
func isClaudeModel(body []byte) bool {
	var tmp struct {
		Model string `json:"model"`
	}
	return json.Unmarshal(body, &tmp) == nil && strings.HasPrefix(strings.ToLower(tmp.Model), "claude")
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
