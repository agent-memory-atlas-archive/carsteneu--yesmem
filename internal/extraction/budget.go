package extraction

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// SpendPersister persists daily spend to durable storage.
type SpendPersister interface {
	TrackSpend(day, bucket string, costUSD float64) error
	GetDailySpend(day, bucket string) (float64, int, error)
}

// BudgetTracker tracks daily LLM spend across multiple clients.
// Thread-safe — shared between extraction and quality clients.
type BudgetTracker struct {
	limitUSD float64
	bucket   string // "extract" or "quality"

	mu       sync.Mutex
	day      string // "2006-01-02"
	spentUSD float64
	calls    int

	persist SpendPersister // optional: persist to DB
}

// NewBudgetTracker creates a shared daily budget tracker.
// limitUSD=0 means unlimited.
func NewBudgetTracker(limitUSD float64) *BudgetTracker {
	return &BudgetTracker{
		limitUSD: limitUSD,
		bucket:   "extract",
		day:      time.Now().Format("2006-01-02"),
	}
}

// NewPersistentBudgetTracker creates a tracker that persists spend to DB.
// Loads existing spend for today on creation.
func NewPersistentBudgetTracker(limitUSD float64, bucket string, persist SpendPersister) *BudgetTracker {
	t := &BudgetTracker{
		limitUSD: limitUSD,
		bucket:   bucket,
		day:      time.Now().Format("2006-01-02"),
		persist:  persist,
	}
	// Restore today's spend from DB
	if persist != nil {
		spent, calls, err := persist.GetDailySpend(t.day, bucket)
		if err == nil && spent > 0 {
			t.spentUSD = spent
			t.calls = calls
			log.Printf("Budget '%s' restored: $%.2f spent (%d calls today)", bucket, spent, calls)
		}
	}
	return t
}

// Check returns an error if the daily budget is exceeded.
func (t *BudgetTracker) Check() error {
	if t.limitUSD <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetIfNewDay()
	if t.spentUSD >= t.limitUSD {
		return fmt.Errorf("daily LLM budget exceeded: $%.2f spent of $%.2f limit (%d calls today)", t.spentUSD, t.limitUSD, t.calls)
	}
	return nil
}

// Track records estimated cost for a completed call. The caller decides whether
// to estimate: BudgetClient calls it only when the transport reported no
// measured usage for that call.
func (t *BudgetTracker) Track(inputPerM, outputPerM float64, system, userMsg, result string) {
	inputTokens := (len(system) + len(userMsg)) / 4
	outputTokens := len(result) / 4

	cost := float64(inputTokens)/1_000_000*inputPerM + float64(outputTokens)/1_000_000*outputPerM

	t.mu.Lock()
	t.spentUSD += cost
	t.calls++
	spent := t.spentUSD
	calls := t.calls
	limit := t.limitUSD
	bucket := t.bucket
	day := t.day
	t.mu.Unlock()

	// Persist to DB (fire-and-forget)
	if t.persist != nil {
		go t.persist.TrackSpend(day, bucket, cost)
	}

	if limit > 0 && spent >= limit*0.8 && spent-cost < limit*0.8 {
		log.Printf("⚠ Daily LLM budget '%s' 80%% reached: $%.2f of $%.2f (%d calls)", bucket, spent, limit, calls)
	}
}

// SpentToday returns current daily spend and call count.
func (t *BudgetTracker) SpentToday() (float64, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetIfNewDay()
	return t.spentUSD, t.calls
}

// Bucket returns the budget bucket name.
func (t *BudgetTracker) Bucket() string {
	return t.bucket
}

func (t *BudgetTracker) resetIfNewDay() {
	today := time.Now().Format("2006-01-02")
	if today != t.day {
		if t.calls > 0 {
			log.Printf("Daily LLM budget '%s' reset: yesterday $%.2f spent (%d calls)", t.bucket, t.spentUSD, t.calls)
		}
		t.day = today
		t.spentUSD = 0
		t.calls = 0
	}
}

// unwrapMetering peels the decorators that can sit in front of the metering
// chain. SanitizingClient is documented as the OUTERMOST decorator
// (sanitizing_client.go), so it is the one a caller meets first, and without
// looking through it the assertions below fall through to "unlimited".
func unwrapMetering(client LLMClient) LLMClient {
	for {
		switch c := client.(type) {
		case *SanitizingClient:
			client = c.inner
		case *GatedClient:
			client = c.Unwrap()
		default:
			return client
		}
	}
}

// CheckBudget tests if the client has remaining budget.
// Returns nil if unlimited or budget available, error if exceeded.
// Works on any LLMClient — returns nil for non-budget clients.
func CheckBudget(client LLMClient) error {
	if bc, ok := unwrapMetering(client).(*BudgetClient); ok {
		return bc.tracker.Check()
	}
	return nil
}

// HasBudget returns true if the client has remaining budget (with $0.50 reserve).
// Returns true for non-budget clients (unlimited).
func HasBudget(client LLMClient) bool {
	if bc, ok := unwrapMetering(client).(*BudgetClient); ok {
		if bc.ThrottleFn != nil && bc.ThrottleFn() {
			return false
		}
		if bc.tracker.limitUSD <= 0 {
			return true
		}
		bc.tracker.mu.Lock()
		bc.tracker.resetIfNewDay()
		remaining := bc.tracker.limitUSD - bc.tracker.spentUSD
		bc.tracker.mu.Unlock()
		return remaining > 0.50
	}
	return true
}

// BudgetClient wraps an LLMClient and enforces a shared daily budget.
type BudgetClient struct {
	inner      LLMClient
	tracker    *BudgetTracker
	inputPerM  float64
	outputPerM float64

	// ThrottleFn is called before each LLM call. If it returns true,
	// the call is skipped with an error. Used to gate on API utilization.
	ThrottleFn func() bool
}

// NewBudgetClient wraps a client with a shared budget tracker.
func NewBudgetClient(inner LLMClient, tracker *BudgetTracker) *BudgetClient {
	inputPerM, outputPerM := PricingForModel(inner.Model())
	return &BudgetClient{
		inner:      inner,
		tracker:    tracker,
		inputPerM:  inputPerM,
		outputPerM: outputPerM,
	}
}

func (b *BudgetClient) Name() string  { return b.inner.Name() }
func (b *BudgetClient) Model() string { return b.inner.Model() }

// SetPricing overrides the per-million prices this client meters with. A caller
// holding a config-aware price table (config.PricingForModel knows provider
// entries and user overrides the builtin table does not) passes its prices here,
// so measured usage is billed at the same rate as the estimate and the daily
// spend stays comparable. Without it the builtin fallback would price an unknown
// model as sonnet.
func (b *BudgetClient) SetPricing(inputPerM, outputPerM float64) {
	b.inputPerM = inputPerM
	b.outputPerM = outputPerM
}

var errThrottled = fmt.Errorf("LLM call throttled: API utilization above threshold")

func (b *BudgetClient) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	if b.ThrottleFn != nil && b.ThrottleFn() {
		return "", errThrottled
	}
	if err := b.tracker.Check(); err != nil {
		return "", err
	}
	var (
		realIn, realOut int
		reported        bool
	)
	callOpts := appendMeteredSink(opts, func(in, out int) {
		// A transport may report several steps for one call (opencode emits one
		// step_finish per tool roundtrip), so the sink accumulates.
		realIn += in
		realOut += out
		reported = true
	})
	result, err := b.inner.Complete(system, userMsg, callOpts...)
	if err == nil || reported {
		// A failed call that already reported its token counts is still billed by
		// the provider, so it is still booked.
		b.book(reported, realIn, realOut, system, userMsg, result)
	}
	return result, err
}

func (b *BudgetClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	if b.ThrottleFn != nil && b.ThrottleFn() {
		return "", errThrottled
	}
	if err := b.tracker.Check(); err != nil {
		return "", err
	}
	var (
		realIn, realOut int
		reported        bool
	)
	callOpts := appendMeteredSink(opts, func(in, out int) {
		realIn += in
		realOut += out
		reported = true
	})
	result, err := b.inner.CompleteJSON(system, userMsg, schema, callOpts...)
	if err == nil || reported {
		b.book(reported, realIn, realOut, system, userMsg, result)
	}
	return result, err
}

// appendMeteredSink returns the caller's options plus this call's usage sink.
// The slice is copied: appending to the caller's variadic slice could write into
// the backing array a caller still holds.
func appendMeteredSink(opts []CallOption, sink usageSink) []CallOption {
	out := make([]CallOption, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, withUsageSink(sink))
	return out
}

// book records exactly one cost for this call: the measured token counts when
// the transport reported them, the char estimate otherwise. The decision lives in
// the call frame, so concurrent calls through one client cannot consume each
// other's signal — the old per-tracker realUsageTracked flag did exactly that,
// booking one call twice and the next one not at all.
func (b *BudgetClient) book(reported bool, inputTokens, outputTokens int, system, userMsg, result string) {
	if reported {
		b.tracker.TrackTokens(inputTokens, outputTokens, b.inputPerM, b.outputPerM)
		return
	}
	b.tracker.Track(b.inputPerM, b.outputPerM, system, userMsg, result)
}

// TrackTokens records actual token usage from the API response, which is more
// accurate than the char estimate in Track.
func (t *BudgetTracker) TrackTokens(inputTokens, outputTokens int, inputPerM, outputPerM float64) {
	cost := float64(inputTokens)/1_000_000*inputPerM + float64(outputTokens)/1_000_000*outputPerM

	t.mu.Lock()
	t.spentUSD += cost
	t.calls++
	spent := t.spentUSD
	calls := t.calls
	limit := t.limitUSD
	bucket := t.bucket
	day := t.day
	t.mu.Unlock()

	if t.persist != nil {
		go t.persist.TrackSpend(day, bucket, cost)
	}

	if limit > 0 && spent >= limit*0.8 && spent-cost < limit*0.8 {
		log.Printf("⚠ Daily LLM budget '%s' 80%% reached: $%.2f of $%.2f (%d calls)", bucket, spent, limit, calls)
	}
}

// Remaining returns remaining budget in USD. Returns math.MaxFloat64 if unlimited.
func (t *BudgetTracker) Remaining() float64 {
	if t.limitUSD <= 0 {
		return 1e18
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetIfNewDay()
	return t.limitUSD - t.spentUSD
}

// PricingForModel returns input/output per-million-token pricing using built-in defaults.
// Prefer config.Config.PricingForModel() when a config object is available.
func PricingForModel(model string) (inputPerM, outputPerM float64) {
	type mp struct{ in, out float64 }
	defaults := map[string]mp{
		"haiku":      {1.0, 5.0},
		"sonnet":     {3.0, 15.0},
		"opus":       {5.0, 25.0},
		"gpt-5-mini": {0.25, 2.0},
		"gpt-5.2":    {1.75, 14.0},
		"gpt-5.4":    {2.5, 15.0},
	}
	// Exact match first
	if p, ok := defaults[model]; ok {
		return p.in, p.out
	}
	// Substring match
	for key, p := range defaults {
		if strings.Contains(model, key) {
			return p.in, p.out
		}
	}
	return 3.0, 15.0
}
