package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// sawtoothTTLForCacheTTL returns how long frozen stubs survive in RAM.
// Must exceed the Anthropic cache TTL to avoid a dead zone where RAM is empty
// but the API cache still holds the old prefix (causing prefix mismatch).
func sawtoothTTLForCacheTTL(cacheTTL string) time.Duration {
	switch cacheTTL {
	case "1h":
		return 65 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// FrozenStubs stores per-thread frozen stub prefixes for cache optimization.
// Between stub-cycles, the frozen prefix is reused byte-identically so the API
// cache can hit on the prefix portion of the messages array.
type FrozenStubs struct {
	mu           sync.RWMutex
	writeLocks   [persistLockSlots]sync.Mutex // hash-slot persistence locks (see persistLock)
	ttl          time.Duration                // eviction TTL — derived from CacheTTL config
	messages     map[string][]any             // threadID → deep-copied stub messages
	cutoff       map[string]int               // threadID → original message count at stub time
	boundaryHash map[string]string            // threadID → hash of messages[cutoff-1]
	prefixHash   map[string]string            // threadID → hash of marshaled frozen prefix
	stubTime     map[string]time.Time
	tokens       map[string]int // threadID → token estimate of frozen stubs
	rawTokens    map[string]int // threadID → raw token estimate at stub time (pre-compression)
	lastAccess   map[string]time.Time
	persistFn    PersistFunc     // optional: persist frozen state to DB
	loadFn       LoadFunc        // optional: load frozen state from DB on cold start
	loadedFromDB map[string]bool // threadID → already attempted DB load
}

// frozenPersisted is the JSON-serializable form of frozen stub state.
type frozenPersisted struct {
	Messages     []any  `json:"messages"`
	Cutoff       int    `json:"cutoff"`
	BoundaryHash string `json:"boundary_hash"`
	PrefixHash   string `json:"prefix_hash"`
	Tokens       int    `json:"tokens"`
	RawTokens    int    `json:"raw_tokens,omitempty"`
}

// NewFrozenStubs creates a new FrozenStubs store with the default 30-minute TTL.
func NewFrozenStubs() *FrozenStubs {
	return NewFrozenStubsWithTTL(30 * time.Minute)
}

// NewFrozenStubsWithTTL creates a FrozenStubs store with a custom eviction TTL.
func NewFrozenStubsWithTTL(ttl time.Duration) *FrozenStubs {
	return &FrozenStubs{
		ttl:          ttl,
		messages:     make(map[string][]any),
		cutoff:       make(map[string]int),
		boundaryHash: make(map[string]string),
		prefixHash:   make(map[string]string),
		stubTime:     make(map[string]time.Time),
		tokens:       make(map[string]int),
		rawTokens:    make(map[string]int),
		lastAccess:   make(map[string]time.Time),
		loadedFromDB: make(map[string]bool),
	}
}

// persistLockSlots bounds the write-lock table. The thread id comes from the
// client, so a per-thread mutex map would grow without limit; a hash slot costs
// a short wait on a collision and never leaks. The number only has to be large
// enough that unrelated busy threads practically never share one; 1024 slots
// also keep a same-UID caller from parking foreign threads on a colliding slot.
const persistLockSlots = 1024

// persistLock returns the persistence lock for threadID. Every path that writes
// a prefix takes it BEFORE it captures state, so a payload captured before an
// invalidation can never be written after the tombstone. Lock order is always
// this lock before f.mu; no path holds f.mu while taking it.
func (f *FrozenStubs) persistLock(threadID string) *sync.Mutex {
	// FNV-1a, inlined: one import fewer for a hash that only has to spread.
	h := uint32(2166136261)
	for i := 0; i < len(threadID); i++ {
		h ^= uint32(threadID[i])
		h *= 16777619
	}
	return &f.writeLocks[h%persistLockSlots]
}

// Store freezes stubbed messages for a thread. Deep-copies messages and computes
// boundary/prefix hashes for validation.
// cutoff is the original message count (the index of the first unstubbed message).
// boundaryMsg is originalMessages[cutoff-1] used for boundary validation.
func (f *FrozenStubs) Store(threadID string, stubbed []any, cutoff int, boundaryMsg any, tokenEstimate int, rawTokenEstimate int) {
	// The lock covers the state capture AND the write, not just the callback: a
	// payload captured just before an Invalidate would otherwise land in
	// proxy_state after the tombstone and survive a restart.
	lk := f.persistLock(threadID)
	lk.Lock()
	defer lk.Unlock()

	frozen := deepCopyMessages(stubbed)

	frozenJSON, _ := json.Marshal(frozen)
	pHash := sha256hex(frozenJSON)

	bHash := stableBoundaryHash(boundaryMsg)

	now := time.Now()

	f.mu.Lock()

	f.messages[threadID] = frozen
	f.cutoff[threadID] = cutoff
	f.boundaryHash[threadID] = bHash
	f.prefixHash[threadID] = pHash
	f.stubTime[threadID] = now
	f.tokens[threadID] = tokenEstimate
	f.rawTokens[threadID] = rawTokenEstimate
	f.lastAccess[threadID] = now
	f.loadedFromDB[threadID] = true // in-memory is authoritative now
	f.mu.Unlock()

	// Persist to DB for cross-restart survival. writeMu is already held.
	if f.persistFn != nil {
		fp := frozenPersisted{
			Messages:     frozen,
			Cutoff:       cutoff,
			BoundaryHash: bHash,
			PrefixHash:   pHash,
			Tokens:       tokenEstimate,
			RawTokens:    rawTokenEstimate,
		}
		if data, err := json.Marshal(fp); err == nil {
			f.persistFn("frozen:"+threadID, string(data))
		}
	}
}

// FrozenResult holds a valid frozen prefix and its metadata.
type FrozenResult struct {
	Messages  []any // frozen stub messages (do not mutate!)
	Cutoff    int   // original message count at stub time
	Tokens    int   // token estimate
	RawTokens int   // raw token estimate at stub time (pre-compression)
}

// Get returns the frozen stubs for a thread if valid.
// Validates that: (1) enough messages exist (len >= cutoff),
// (2) frozen prefix hasn't been mutated in memory.
// CC may inject system-reminders into older messages, so we skip
// boundary hash validation (it's too fragile).
func (f *FrozenStubs) Get(threadID string, currentMessages []any) *FrozenResult {
	f.mu.RLock()
	msgs, ok := f.messages[threadID]
	loaded := f.loadedFromDB[threadID]
	f.mu.RUnlock()

	// Lazy-load from DB on cold start
	if !ok && !loaded {
		f.loadFrozenFromDB(threadID)
		f.mu.RLock()
		msgs, ok = f.messages[threadID]
		f.mu.RUnlock()
	}

	if !ok {
		return nil
	}

	f.mu.RLock()
	cutoff := f.cutoff[threadID]
	pHash := f.prefixHash[threadID]
	tokens := f.tokens[threadID]
	rawTokens := f.rawTokens[threadID]
	f.mu.RUnlock()

	// Validate: enough messages in current request
	if len(currentMessages) < cutoff {
		f.Invalidate(threadID)
		return nil
	}

	// Verify prefix hash (detect unexpected mutation of frozen data)
	frozenJSON, _ := json.Marshal(msgs)
	if sha256hex(frozenJSON) != pHash {
		f.Invalidate(threadID)
		return nil
	}

	// Update last access
	f.mu.Lock()
	f.lastAccess[threadID] = time.Now()
	f.mu.Unlock()

	// Deep-copy to prevent mutation by downstream operations
	// (UpgradeCacheTTL, InjectCacheBreakpoints add cache_control to maps in-place)
	copied := deepCopyMessages(msgs)
	if copied == nil {
		f.Invalidate(threadID)
		return nil
	}

	return &FrozenResult{
		Messages:  copied,
		Cutoff:    cutoff,
		Tokens:    tokens,
		RawTokens: rawTokens,
	}
}

// LengthFor returns the number of frozen messages stored for threadID, or 0 if
// no frozen entry exists. Used by the post-pipeline snapshot path to know how
// many leading messages of req["messages"] should be re-snapshotted.
func (f *FrozenStubs) LengthFor(threadID string) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if msgs, ok := f.messages[threadID]; ok {
		return len(msgs)
	}
	return 0
}

// UpdateMessages overwrites the stored frozen prefix with newMsgs and refreshes
// the prefix hash, so a later Get() call returns post-pipeline bytes (matching
// the bytes that were sent on the wire on the FREEZE turn). Length must equal
// the existing entry to guard against a second sawtooth firing inside the same
// pipeline. Returns true only when the stored prefix actually changed: the
// snapshot runs on every request, and the prefix (several MB) must not be
// rewritten into the daemon while its bytes are identical.
//
// The comparison is structural, not a hash of the marshalled prefix: the equal
// case is the normal case and has to be answered without marshalling megabytes
// per request. A payload the walk cannot decide is compared by its bytes below.
func (f *FrozenStubs) UpdateMessages(threadID string, newMsgs []any) bool {
	lk := f.persistLock(threadID)
	lk.Lock()
	defer lk.Unlock()

	f.mu.RLock()
	existing, ok := f.messages[threadID]
	currentHash := f.prefixHash[threadID]
	f.mu.RUnlock()
	if !ok {
		return false
	}
	if len(newMsgs) != len(existing) {
		return false
	}

	if los26eFrozenPrefixEqual(existing, newMsgs) {
		f.mu.Lock()
		f.lastAccess[threadID] = time.Now()
		f.mu.Unlock()
		return false
	}

	newJSON, err := json.Marshal(newMsgs)
	if err != nil {
		return false
	}
	pHash := sha256hex(newJSON)

	if pHash == currentHash {
		f.mu.Lock()
		f.lastAccess[threadID] = time.Now()
		f.mu.Unlock()
		return false
	}

	var fresh []any
	// The nil check is not redundant: json.Unmarshal("null") succeeds and leaves
	// fresh nil, so only the error check would let a null payload through.
	if json.Unmarshal(newJSON, &fresh) != nil || fresh == nil {
		return false
	}

	f.mu.Lock()
	f.messages[threadID] = fresh
	f.prefixHash[threadID] = pHash
	f.lastAccess[threadID] = time.Now()
	cutoff := f.cutoff[threadID]
	bHash := f.boundaryHash[threadID]
	tokens := f.tokens[threadID]
	rawTokens := f.rawTokens[threadID]
	f.mu.Unlock()

	if f.persistFn != nil {
		fp := frozenPersisted{
			Messages:     fresh,
			Cutoff:       cutoff,
			BoundaryHash: bHash,
			PrefixHash:   pHash,
			Tokens:       tokens,
			RawTokens:    rawTokens,
		}
		if data, err := json.Marshal(fp); err == nil {
			f.persistFn("frozen:"+threadID, string(data))
		}
	}
	return true
}

// Invalidate removes frozen stubs for a thread and drops the persisted prefix.
// The persisted row is overwritten with an empty value instead of being left in
// place, because the row can carry several megabytes and an invalidated prefix
// must not be offered to another process through the lazy DB load.
//
// loadedFromDB deliberately stays set: it is the in-memory tombstone that keeps
// this process from resurrecting the prefix. The DB write is best effort (the
// persist callback logs its error and returns nothing), so the guarantee that an
// invalidated prefix stays gone must not depend on it.
//
// The tombstone waits for an in-flight persist of the same thread (persistLock):
// the persist callback runs a daemon round trip, so without that order a payload
// captured just before this call could reach proxy_state after the empty value
// and leave a loadable prefix behind. The next freeze overwrites the row again;
// until then the in-memory tombstone holds.
func (f *FrozenStubs) Invalidate(threadID string) {
	// The persistence lock first: it has to cover the state drop as well, so a
	// payload captured before this call cannot be written after the tombstone.
	lk := f.persistLock(threadID)
	lk.Lock()
	defer lk.Unlock()

	f.mu.Lock()
	_, hadMessages := f.messages[threadID]
	fn := f.persistFn
	delete(f.messages, threadID)
	delete(f.cutoff, threadID)
	delete(f.boundaryHash, threadID)
	delete(f.prefixHash, threadID)
	delete(f.stubTime, threadID)
	delete(f.tokens, threadID)
	delete(f.rawTokens, threadID)
	delete(f.lastAccess, threadID)
	f.mu.Unlock()

	if hadMessages && fn != nil {
		fn("frozen:"+threadID, "")
	}
}

// UpdateTTL changes the eviction TTL at runtime (e.g. after 1h cache detection).
func (f *FrozenStubs) UpdateTTL(ttl time.Duration) {
	f.mu.Lock()
	f.ttl = ttl
	f.mu.Unlock()
}

// Touch refreshes lastAccess for a thread without changing content.
// Called by keepalive pings to prevent stubs from expiring while API cache is warm.
func (f *FrozenStubs) Touch(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.lastAccess[threadID]; ok {
		f.lastAccess[threadID] = time.Now()
	}
}

// Evict removes entries that haven't been accessed within the TTL. It is pure
// in-memory work on purpose: it runs on the request path (pruneIdleThreadState at
// the entry of every request family), so rewriting the
// persisted row here would put one daemon round trip per evicted thread on the
// request path and could clobber a snapshot written by a concurrent request.
// The trade-off is that the row survives the eviction: a thread that comes back
// after the TTL reloads a prefix that is no longer cache-warm. Reclaiming those
// rows needs a path other than the request path (only --reset-cache does it
// today), which is a pre-existing gap of the persistence layer.
func (f *FrozenStubs) Evict() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cutoff := time.Now().Add(-f.ttl)
	evicted := 0
	for tid, t := range f.lastAccess {
		if t.Before(cutoff) {
			delete(f.messages, tid)
			delete(f.cutoff, tid)
			delete(f.boundaryHash, tid)
			delete(f.prefixHash, tid)
			delete(f.stubTime, tid)
			delete(f.tokens, tid)
			delete(f.rawTokens, tid)
			delete(f.lastAccess, tid)
			delete(f.loadedFromDB, tid)
			evicted++
		}
	}
	return evicted
}

// loadFrozenFromDB attempts to restore frozen stubs from DB for a thread.
// Called once per thread on cold start (lazy load from Get).
func (f *FrozenStubs) loadFrozenFromDB(threadID string) {
	f.mu.Lock()
	if f.loadedFromDB[threadID] {
		f.mu.Unlock()
		return
	}
	f.loadedFromDB[threadID] = true
	loadFn := f.loadFn
	f.mu.Unlock()

	if loadFn == nil {
		return
	}

	raw, ok := loadFn("frozen:" + threadID)
	if !ok || raw == "" {
		return
	}

	var fp frozenPersisted
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		return
	}
	if len(fp.Messages) == 0 || fp.PrefixHash == "" {
		return
	}

	// Verify prefix hash matches stored messages
	frozenJSON, _ := json.Marshal(fp.Messages)
	if sha256hex(frozenJSON) != fp.PrefixHash {
		return
	}

	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	// Only populate if still empty (Store() may have been called concurrently)
	if _, exists := f.messages[threadID]; exists {
		return
	}
	f.messages[threadID] = fp.Messages
	f.cutoff[threadID] = fp.Cutoff
	f.boundaryHash[threadID] = fp.BoundaryHash
	f.prefixHash[threadID] = fp.PrefixHash
	f.tokens[threadID] = fp.Tokens
	f.rawTokens[threadID] = fp.RawTokens
	f.stubTime[threadID] = now
	f.lastAccess[threadID] = now
}

// Has returns true if frozen stubs exist for a thread (without validation).
func (f *FrozenStubs) Has(threadID string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.messages[threadID]
	return ok
}

// Persist writes current in-memory frozen state for a thread to DB.
// Used to re-persist after a failed Store() (e.g. daemon unreachable after deploy).
func (f *FrozenStubs) Persist(threadID string) {
	lk := f.persistLock(threadID)
	lk.Lock()
	defer lk.Unlock()

	f.mu.RLock()
	msgs, ok := f.messages[threadID]
	if !ok {
		f.mu.RUnlock()
		return
	}
	fn := f.persistFn
	cutoff := f.cutoff[threadID]
	bHash := f.boundaryHash[threadID]
	pHash := f.prefixHash[threadID]
	tokens := f.tokens[threadID]
	rawTokens := f.rawTokens[threadID]
	f.mu.RUnlock()

	if fn == nil {
		return
	}

	fp := frozenPersisted{
		Messages:     msgs,
		Cutoff:       cutoff,
		BoundaryHash: bHash,
		PrefixHash:   pHash,
		Tokens:       tokens,
		RawTokens:    rawTokens,
	}
	if data, err := json.Marshal(fp); err == nil {
		fn("frozen:"+threadID, string(data))
	}
}

// SetPersistFunc sets the callback for persisting frozen state to DB.
func (f *FrozenStubs) SetPersistFunc(fn PersistFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persistFn = fn
}

// SetLoadFunc sets the callback for loading frozen state from DB on cold start.
func (f *FrozenStubs) SetLoadFunc(fn LoadFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadFn = fn
}

// deepCopyMessages creates a deep copy of a messages slice via JSON round-trip.
func deepCopyMessages(msgs []any) []any {
	data, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	var out []any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// stableBoundaryHash creates a hash from stable parts of a message:
// role + first 200 chars of the first text content block.
// This avoids invalidation from injected system-reminders and context tags.
func stableBoundaryHash(msg any) string {
	mm, ok := msg.(map[string]any)
	if !ok {
		data, _ := json.Marshal(msg)
		return sha256hex(data)
	}

	role, _ := mm["role"].(string)
	text := ""

	// Extract first text from content
	switch c := mm["content"].(type) {
	case string:
		text = c
	case []any:
		for _, block := range c {
			if bm, ok := block.(map[string]any); ok {
				if t, ok := bm["text"].(string); ok {
					text = t
					break
				}
			}
		}
	}

	// Truncate to first 200 chars for stability
	if len(text) > 200 {
		text = text[:200]
	}

	return sha256hex([]byte(role + ":" + text))
}

// sha256hex returns the hex-encoded SHA-256 hash of data.
func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

// --- Sawtooth Trigger ---

// TriggerReason indicates why a stub-cycle was triggered.
type TriggerReason string

const (
	TriggerNone      TriggerReason = ""
	TriggerTokens    TriggerReason = "tokens"    // lastTotalTokens > 180k
	TriggerPause     TriggerReason = "pause"     // pause > 330s && tokens > 100k
	TriggerEmergency TriggerReason = "emergency" // rawEstimate > 190k
)

// PersistFunc writes a key-value pair to persistent storage (e.g. proxy_state DB).
type PersistFunc func(key, value string)

// LoadFunc reads a key from persistent storage. Returns value and whether found.
type LoadFunc func(key string) (string, bool)

// SawtoothTrigger decides when to run a stub-cycle based on token usage and timing.
type SawtoothTrigger struct {
	mu               sync.RWMutex
	lastTotalTokens  map[string]int           // threadID → last API response input tokens
	lastMessageCount map[string]int           // threadID → message count at last response
	lastRequestTime  map[string]time.Time     // threadID → time of last API response
	loadedFromDB     map[string]bool          // threadID → already attempted DB load
	estimateFactor   map[string]float64       // threadID → measured actual/estimate ratio (1.0 = uncalibrated)
	pendingEstimate  map[string]int           // threadID → estimate of the payload about to be sent
	loading          map[string]chan struct{} // threadID → in-flight DB load, closed when it finishes
	lastTouch        map[string]time.Time     // threadID → last state activity (eviction baseline)
	pauseThreshold   time.Duration            // cache TTL - safety margin
	tokenThreshold   int                      // trigger stub-cycle above this (from config)
	tokenMinimum     int                      // stub down to this floor (from config)
	persistFn        PersistFunc              // optional: persist tokens to DB on update
	loadFn           LoadFunc                 // optional: load tokens from DB on cold start
}

// NewSawtoothTrigger creates a new trigger state tracker.
func NewSawtoothTrigger(pauseThreshold time.Duration, tokenThreshold, tokenMinimum int) *SawtoothTrigger {
	return &SawtoothTrigger{
		lastTotalTokens:  make(map[string]int),
		lastMessageCount: make(map[string]int),
		lastRequestTime:  make(map[string]time.Time),
		loadedFromDB:     make(map[string]bool),
		estimateFactor:   make(map[string]float64),
		pendingEstimate:  make(map[string]int),
		loading:          make(map[string]chan struct{}),
		lastTouch:        make(map[string]time.Time),
		pauseThreshold:   pauseThreshold,
		tokenThreshold:   tokenThreshold,
		tokenMinimum:     tokenMinimum,
	}
}

// ShouldTrigger checks if a stub-cycle should run for this thread.
// rawEstimate is the pre-API-call token estimate (for emergency brake).
// tokenThreshold is the effective threshold for the request's model; callers
// resolve it through effectiveTokenThreshold so model-specific overrides are
// honoured. A non-positive value falls back to the configured default.
func (st *SawtoothTrigger) ShouldTrigger(threadID string, rawEstimate, tokenThreshold int) TriggerReason {
	st.mu.RLock()
	defer st.mu.RUnlock()

	if tokenThreshold <= 0 {
		tokenThreshold = st.tokenThreshold
	}
	emergencyThreshold := tokenThreshold + 10_000 // 10k margin above threshold

	// Emergency brake — raw estimate too high, no usage data needed
	if rawEstimate > emergencyThreshold {
		return TriggerEmergency
	}

	// Token threshold based on raw estimate (works even without prior API data)
	if rawEstimate > tokenThreshold {
		return TriggerTokens
	}

	tokens, hasTokens := st.lastTotalTokens[threadID]
	lastTime, hasTime := st.lastRequestTime[threadID]

	// No prior data and below threshold → no stub needed
	if !hasTokens {
		return TriggerNone
	}

	// Token threshold based on last API response (more accurate than estimate)
	if tokens > tokenThreshold {
		return TriggerTokens
	}

	// Pause detection: cache is cold after TTL, worth re-stubbing if enough tokens
	if hasTime && tokens > st.tokenMinimum {
		if time.Since(lastTime) > st.pauseThreshold {
			return TriggerPause
		}
	}

	return TriggerNone
}

// SetTokenThreshold updates the trigger threshold dynamically (e.g. from runtime config overrides).
func (st *SawtoothTrigger) SetTokenThreshold(threshold int) {
	st.mu.Lock()
	st.tokenThreshold = threshold
	st.mu.Unlock()
}

// SetPauseThreshold updates the idle gap that counts as a cold cache. It follows
// the TTL detection result: a confirmed 1h cache must not be treated as cold
// after the 5 minute gap.
func (st *SawtoothTrigger) SetPauseThreshold(threshold time.Duration) {
	if threshold <= 0 {
		return
	}
	st.mu.Lock()
	st.pauseThreshold = threshold
	st.mu.Unlock()
}

// SetPersistFunc sets the callback for writing token state to persistent storage.
func (st *SawtoothTrigger) SetPersistFunc(fn PersistFunc) {
	st.persistFn = fn
}

// SetLoadFunc sets the callback for reading token state from persistent storage.
func (st *SawtoothTrigger) SetLoadFunc(fn LoadFunc) {
	st.loadFn = fn
}

// sawtoothStateTTL bounds how long per-thread trigger state survives without
// activity. It exceeds the longest prompt-cache TTL (1h) plus headroom, so a
// warm thread never loses its baseline.
const sawtoothStateTTL = 2 * time.Hour

// Prune drops the per-thread state of threads idle for longer than ttl and
// returns the number of evicted threads. The thread id comes from the client,
// so without this the maps grow for the lifetime of the process. Pure in-memory
// work, called off the request path next to FrozenStubs.Evict: an evicted
// thread that comes back has no baseline for one request and repopulates it
// from the next response.
func (st *SawtoothTrigger) Prune(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	evicted := 0
	for tid, touched := range st.lastTouch {
		if touched.After(cutoff) {
			continue
		}
		delete(st.lastTouch, tid)
		delete(st.lastTotalTokens, tid)
		delete(st.lastMessageCount, tid)
		delete(st.lastRequestTime, tid)
		delete(st.loadedFromDB, tid)
		delete(st.estimateFactor, tid)
		delete(st.pendingEstimate, tid)
		evicted++
	}
	return evicted
}

// persistedState is the JSON structure stored in proxy_state.
type persistedState struct {
	Tokens         int     `json:"tokens"`
	MsgCount       int     `json:"msg_count"`
	EstimateFactor float64 `json:"estimate_factor,omitempty"`
	// TS is the unix time of the last response. ShouldTrigger needs it for the
	// pause trigger, and without it an evicted or restarted proxy loses the
	// cold-cache signal for the first request of a resuming thread.
	TS int64 `json:"ts,omitempty"`
}

// UpdateAfterResponse records the actual token count, message count, and time after an API response.
// When a sent estimate is pending for the thread, the response also yields the
// measured actual/estimate ratio: the systematic gap between our count and what
// the API charges is what makes a hard context limit reachable while the
// estimate still looks harmless.
func (st *SawtoothTrigger) UpdateAfterResponse(threadID string, totalInputTokens, messageCount int) {
	st.mu.Lock()
	if st.estimateFactor == nil {
		st.estimateFactor = make(map[string]float64)
	}
	if st.pendingEstimate == nil {
		st.pendingEstimate = make(map[string]int)
	}
	if st.lastTouch == nil {
		st.lastTouch = make(map[string]time.Time)
	}
	st.lastTotalTokens[threadID] = totalInputTokens
	st.lastMessageCount[threadID] = messageCount
	now := time.Now()
	st.lastRequestTime[threadID] = now
	st.loadedFromDB[threadID] = true
	st.lastTouch[threadID] = now

	if sent, ok := st.pendingEstimate[threadID]; ok {
		delete(st.pendingEstimate, threadID)
		if sent > 0 && totalInputTokens > 0 {
			ratio := float64(totalInputTokens) / float64(sent)
			// A sample outside the band is not a measurement of our counting error
			// (foreign payload, cache-only usage, wrong unit), so it must not move
			// the factor at all.
			if ratio >= estimateFactorMin && ratio <= estimateFactorMax {
				if prev, has := st.estimateFactor[threadID]; has && prev > 0 {
					ratio = (prev + ratio) / 2
				}
				st.estimateFactor[threadID] = ratio
			}
		}
	}
	factor := st.estimateFactor[threadID]
	st.mu.Unlock()

	if st.persistFn != nil {
		data, _ := json.Marshal(persistedState{
			Tokens:         totalInputTokens,
			MsgCount:       messageCount,
			EstimateFactor: factor,
			TS:             now.Unix(),
		})
		st.persistFn("sawtooth:"+threadID, string(data))
	}
}

// TouchRequestTime refreshes lastRequestTime without changing token/message state.
// Called by keepalive pings to prevent false pause-triggered stub-cycles.
func (st *SawtoothTrigger) TouchRequestTime(threadID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.lastRequestTime[threadID]; ok {
		st.lastRequestTime[threadID] = time.Now()
	}
}

// GetLastTokens returns the last known token count for a thread (0 if unknown).
// On cold start (no in-memory value), attempts to load from DB via LoadFunc.
func (st *SawtoothTrigger) GetLastTokens(threadID string) int {
	st.mu.RLock()
	tokens := st.lastTotalTokens[threadID]
	loaded := st.loadedFromDB[threadID]
	st.mu.RUnlock()

	if tokens > 0 || loaded {
		return tokens
	}
	st.loadFromDB(threadID)

	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.lastTotalTokens[threadID]
}

// GetLastMessageCount returns the last known message count for a thread (0 if unknown).
// On cold start (no in-memory value), attempts to load from DB via LoadFunc.
func (st *SawtoothTrigger) GetLastMessageCount(threadID string) int {
	st.mu.RLock()
	count := st.lastMessageCount[threadID]
	loaded := st.loadedFromDB[threadID]
	st.mu.RUnlock()

	if count > 0 || loaded {
		return count
	}
	st.loadFromDB(threadID)

	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.lastMessageCount[threadID]
}

// loadFromDB loads persisted token state for a thread. Called once per thread on cold start.
// The daemon round trip runs WITHOUT the write lock: loadFn takes up to 15s on
// a cold start, and holding st.mu across it queued every other thread's
// ShouldTrigger/GetLastTokens behind that one call. Concurrent callers for the
// same thread wait on the in-flight load instead of issuing a second round trip.
func (st *SawtoothTrigger) loadFromDB(threadID string) {
	st.mu.Lock()
	if st.loadedFromDB[threadID] {
		st.mu.Unlock()
		return
	}
	if wait, inflight := st.loading[threadID]; inflight {
		st.mu.Unlock()
		<-wait
		return
	}
	loadFn := st.loadFn
	if loadFn == nil {
		st.loadedFromDB[threadID] = true
		if st.lastTouch == nil {
			st.lastTouch = make(map[string]time.Time)
		}
		st.lastTouch[threadID] = time.Now()
		st.mu.Unlock()
		return
	}
	if st.loading == nil {
		st.loading = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	st.loading[threadID] = done
	st.mu.Unlock()

	// The release runs as a deferred call: a panicking loader (net/http
	// recovers handler panics) would otherwise leave the thread registered
	// forever and park every later request for it on the wait channel.
	defer st.finishLoad(threadID, done)

	val, ok := loadFn("sawtooth:" + threadID)

	var state persistedState
	parsed := ok && val != ""
	if parsed {
		parsed = json.Unmarshal([]byte(val), &state) == nil
	}
	if !parsed {
		return
	}

	st.mu.Lock()
	// A response that landed during the load already wrote fresher state.
	if _, answered := st.lastTotalTokens[threadID]; !answered {
		st.lastTotalTokens[threadID] = state.Tokens
		st.lastMessageCount[threadID] = state.MsgCount
		if state.TS > 0 && state.TS <= time.Now().Unix() {
			// A future timestamp would make time.Since negative and suppress the
			// pause trigger for that thread forever; proxy_state is socket-writable.
			st.lastRequestTime[threadID] = time.Unix(state.TS, 0)
		}
		if state.EstimateFactor >= estimateFactorMin && state.EstimateFactor <= estimateFactorMax {
			if st.estimateFactor == nil {
				st.estimateFactor = make(map[string]float64)
			}
			st.estimateFactor[threadID] = state.EstimateFactor
		}
	}
	st.mu.Unlock()
}

// finishLoad releases the in-flight marker of a state load and registers the
// thread for eviction, so Prune reaches threads whose load found nothing.
func (st *SawtoothTrigger) finishLoad(threadID string, done chan struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.loading, threadID)
	close(done)
	st.loadedFromDB[threadID] = true
	if st.lastTouch == nil {
		st.lastTouch = make(map[string]time.Time)
	}
	st.lastTouch[threadID] = time.Now()
}

// shouldInvalidateFrozen decides whether existing frozen stubs must be
// re-created. Only the combined token count (frozen + fresh + overhead)
// matters — lastTokens from the previous API response reflects the
// PRE-collapse state and must not invalidate already-good stubs.
func shouldInvalidateFrozen(combinedTokens, threshold int) bool {
	return combinedTokens > threshold
}

// estimateFactorFor returns the measured actual/estimate ratio for a thread.
// 1.0 means no calibration data yet.
func (st *SawtoothTrigger) estimateFactorFor(threadID string) float64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if f, ok := st.estimateFactor[threadID]; ok && f > 0 {
		return f
	}
	return 1
}

// applyEstimateFactor scales a raw token estimate by the measured
// actual/estimate ratio of the thread.
func (st *SawtoothTrigger) applyEstimateFactor(threadID string, raw int) int {
	factor := st.estimateFactorFor(threadID)
	if factor == 1 {
		return raw
	}
	return int(float64(raw) * factor)
}

// estimateFactorMin/Max define both the validity band for a single measured
// sample and the bounds of the resulting factor: a response outside the band
// does not describe our counting error, so it is discarded.
const (
	estimateFactorMin = 0.5
	estimateFactorMax = 2.0
)

// SetSentEstimate records the token estimate of the payload that is about to
// be sent. The next UpdateAfterResponse derives the actual/estimate ratio from
// it and discards it.
func (st *SawtoothTrigger) SetSentEstimate(threadID string, estimate int) {
	if estimate <= 0 {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pendingEstimate == nil {
		st.pendingEstimate = make(map[string]int)
	}
	if st.lastTouch == nil {
		st.lastTouch = make(map[string]time.Time)
	}
	st.pendingEstimate[threadID] = estimate
	st.lastTouch[threadID] = time.Now()
}

// ClearSentEstimate drops a sample left over from an earlier request. Called at
// request entry so a response is only ever paired with the estimate of the
// request that is actually in flight (a response without usage would otherwise
// leave a sample behind for the next response to consume).
// The slot is per thread, not per request: two requests overlapping on one
// thread can still pair one sample with the other response. Such a mispair is
// bounded by the sample band (0.5..2.0) and the EMA smooths it away over the
// next responses, so it is accepted rather than plumbed per request.
func (st *SawtoothTrigger) ClearSentEstimate(threadID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.pendingEstimate, threadID)
}
