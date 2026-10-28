package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// DecayStage represents how much a stub has faded.
const (
	DecayStage0 = 0 // Fresh: full stub with annotation
	DecayStage1 = 1 // Middle: short form
	DecayStage2 = 2 // Old: minimal
	DecayStage3 = 3 // Compacted: content lives in the archive block, stub is empty
)

// DecayTracker tracks when messages were first stubbed and applies progressive decay.
//
// One tracker belongs to exactly one thread. State keyed only by message index
// was shared by every thread: a message at index 5 inherited the stub age of
// another thread's message 5, and each thread persisted the whole tracker, so
// the stored blobs were byte-identical (YM-128). Server.decayFor owns the
// per-thread instances.
type DecayTracker struct {
	mu sync.RWMutex
	// messageKey → request index when first stubbed
	stubbedAt map[string]int
	// messageKey → emotional intensity at stub time
	intensity map[string]float64
	// messageKey → file path referenced in this message's stub
	filePaths map[string]string
	// paths from narrative's active phases — stubs referencing these decay slower
	pinnedPaths map[string]bool
	persistFn   PersistFunc
	loadFn      LoadFunc
	loaded      bool
	// clock is the highest request index observed while serving this thread.
	// The index is this thread's own request count (see Tick), not the
	// proxy-wide counter: a thread that pauses must not age its stubs with
	// every other thread's traffic (YM-275). Persist stores ages relative to
	// this clock, so a restart (whose counter starts at 1) still restores the
	// real age instead of a negative one.
	clock int
}

// Tick advances this thread's own request clock and returns its new value. The
// proxy calls it once per request the thread makes; ages are then measured in
// the thread's own turns.
func (d *DecayTracker) Tick() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clock++
	return d.clock
}

// Now returns this thread's request clock without advancing it.
func (d *DecayTracker) Now() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.clock
}

// decayPersisted is the JSON-serializable form of decay state for a thread.
// V is the format version: 3 stores Ages in this thread's own turns (the unit
// Tick counts). Version 2 stored them relative to the proxy-wide request
// counter, which is a different unit for the same number, so a v2 blob is
// ignored rather than reinterpreted; version 0/1 held absolute indices of a dead
// process clock and is ignored for the same reason.
type decayPersisted struct {
	V         int                `json:"v"`
	Ages      map[string]int     `json:"ages"`
	Intensity map[string]float64 `json:"intensity"`
	FilePaths map[string]string  `json:"file_paths"`
}

const decayPersistVersion = 3

// maxDecayAge rejects implausible ages from a persisted blob: a stub that is
// more than a million requests old is either corruption or a tampered value.
const maxDecayAge = 1_000_000

// NewDecayTracker creates a new decay tracker for a single thread.
func NewDecayTracker() *DecayTracker {
	return &DecayTracker{
		stubbedAt:   make(map[string]int),
		intensity:   make(map[string]float64),
		filePaths:   make(map[string]string),
		pinnedPaths: make(map[string]bool),
	}
}

// MarkStubbed records that a message was stubbed at the given request index
// with the current emotional intensity. Only the first stub is recorded.
func (d *DecayTracker) MarkStubbed(msgIndex, requestIdx int, emotionalIntensity float64) {
	key := fmt.Sprintf("msg_%d", msgIndex)
	d.mu.Lock()
	defer d.mu.Unlock()
	if requestIdx > d.clock {
		d.clock = requestIdx
	}
	if _, exists := d.stubbedAt[key]; !exists {
		d.stubbedAt[key] = requestIdx
		d.intensity[key] = emotionalIntensity
	}
}

// SetFilePath records which file path a stubbed message references.
func (d *DecayTracker) SetFilePath(msgIndex int, path string) {
	if path == "" {
		return
	}
	key := fmt.Sprintf("msg_%d", msgIndex)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filePaths[key] = path
}

// SetPinnedPaths updates the set of paths that should decay slower.
// Called before each Stubify pass with the narrative's active paths.
func (d *DecayTracker) SetPinnedPaths(paths []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pinnedPaths = make(map[string]bool, len(paths))
	for _, p := range paths {
		d.pinnedPaths[p] = true
	}
}

// decayBoundaries returns adaptive stage boundaries based on thread length and token pressure.
// pressure = totalTokens / threshold. Low pressure → stretch boundaries (decay slower).
// pressure=0 is valid and used for conservative evaluation (e.g. compaction — stretch clamped to 4.0).
// Returns (s0end, s1end, s2end) — the age thresholds where each stage ends.
func decayBoundaries(threadLen int, pressure float64) (s0end, s1end, s2end int) {
	var base0, base1, base2 int
	switch {
	case threadLen < 500:
		base0, base1, base2 = 5, 15, 50
	case threadLen < 2000:
		base0, base1, base2 = 5, 12, 40
	default:
		base0, base1, base2 = 4, 10, 30
	}

	// Scale boundaries by pressure:
	// pressure 1.0-1.5 → stretch 3x-2x (keep more content when barely over threshold)
	// pressure 1.5-2.5 → stretch 2x-1x (gradual transition)
	// pressure 2.5+    → 1x (full decay, like before)
	stretch := 1.0
	if pressure < 2.5 {
		stretch = 1.0 + (2.5-pressure)/0.75 // 1.0→3.0, 1.75→2.0, 2.5→1.0
		if stretch > 4.0 {
			stretch = 4.0
		}
		if stretch < 1.0 {
			stretch = 1.0
		}
	}

	return int(float64(base0) * stretch),
		int(float64(base1) * stretch),
		int(float64(base2) * stretch)
}

// GetStage returns the decay stage for a message based on its age, thread length, and token pressure.
// pressure = totalTokens / threshold. Low pressure stretches decay boundaries.
// Uses the emotional intensity stored at stub time to slow decay for intense moments.
// Messages referencing pinned paths (active plans, etc.) get an extra boost.
func (d *DecayTracker) GetStage(msgIndex, currentRequestIdx, threadLen int, pressure float64) int {
	key := fmt.Sprintf("msg_%d", msgIndex)
	d.mu.Lock()
	stubbedAt, exists := d.stubbedAt[key]
	emotionalIntensity := d.intensity[key]
	filePath := d.filePaths[key]
	isPinned := d.isPinnedPath(filePath)
	if currentRequestIdx > d.clock {
		d.clock = currentRequestIdx
	}
	d.mu.Unlock()

	if !exists {
		return DecayStage0
	}

	age := currentRequestIdx - stubbedAt
	if age < 0 {
		// Only reachable if a caller hands us an index older than the stub
		// (the load path rebases, so the normal restart case is age >= 0). The
		// clamp keeps the age arithmetic below from going negative; the stage
		// itself is stage 0 either way, because a negative age is also below
		// every boundary.
		age = 0
	}
	boost := int(emotionalIntensity * 20) // 0-20 extra requests before decay

	// Pinned paths (active plans, etc.) get +30 extra requests before decay
	if isPinned {
		boost += 30
	}

	s0end, s1end, s2end := decayBoundaries(threadLen, pressure)

	if age < s0end+boost {
		return DecayStage0
	}
	if age < s1end+boost {
		return DecayStage1
	}
	if age < s2end+boost {
		return DecayStage2
	}
	return DecayStage3
}

// ApplyDecay takes a stage-0 stub and compresses it based on decay stage.
func ApplyDecay(stub string, stage int, role string) string {
	switch stage {
	case DecayStage0:
		return stub
	case DecayStage1:
		return decayToStage1(stub, role)
	case DecayStage2:
		return decayToStage2(stub, role)
	case DecayStage3:
		return "" // content lives in the archive block
	default:
		return stub
	}
}

// decayToStage1 keeps tool + path + keyword, removes annotation detail.
func decayToStage1(stub string, role string) string {
	if role == "user" || role == "assistant" {
		// Text stubs: truncate further
		r := []rune(stub)
		limit := 120
		if role == "user" {
			limit = 200
		}
		if len(r) > limit {
			return string(r[:limit]) + "..."
		}
		return stub
	}
	return stub
}

// decayToStage2 keeps only the minimal skeleton.
func decayToStage2(stub string, role string) string {
	if role == "user" || role == "assistant" {
		r := []rune(stub)
		limit := 50
		if role == "user" {
			limit = 80
		}
		if len(r) > limit {
			return string(r[:limit]) + "..."
		}
		return stub
	}
	return stub
}

// ApplyDecayToToolStub decays a tool stub based on stage.
func ApplyDecayToToolStub(stub string, stage int) string {
	switch stage {
	case DecayStage0:
		return stub
	case DecayStage1:
		// Remove annotation (everything after " — ")
		if idx := findAnnotationSep(stub); idx >= 0 {
			// Keep first keyword from annotation
			ann := stub[idx+len(" — "):]
			words := firstNWords(ann, 3)
			if words != "" {
				return stub[:idx] + " — " + words
			}
			return stub[:idx]
		}
		return stub
	case DecayStage2:
		// Remove everything after first space-dash-space
		if idx := findAnnotationSep(stub); idx >= 0 {
			return stub[:idx]
		}
		return stub
	case DecayStage3:
		return "" // content lives in the archive block
	default:
		return stub
	}
}

func findAnnotationSep(s string) int {
	return strings.Index(s, " — ")
}

// isPinnedPath checks if a file path matches any pinned path.
// Must be called while holding at least a read lock.
func (d *DecayTracker) isPinnedPath(filePath string) bool {
	if filePath == "" || len(d.pinnedPaths) == 0 {
		return false
	}
	// Direct match
	if d.pinnedPaths[filePath] {
		return true
	}
	// Suffix match: pinnedPaths may be short ("proxy/proxy.go")
	// while filePath is full ("/home/user/.../proxy/proxy.go")
	for pp := range d.pinnedPaths {
		if strings.HasSuffix(filePath, pp) || strings.HasSuffix(pp, filePath) {
			return true
		}
	}
	return false
}

func firstNWords(s string, n int) string {
	words := 0
	for i, ch := range s {
		if ch == ' ' {
			words++
			if words >= n {
				return s[:i]
			}
		}
	}
	return s
}

// SetPersistFunc sets the callback for persisting decay state to DB.
func (d *DecayTracker) SetPersistFunc(fn PersistFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.persistFn = fn
}

// SetLoadFunc sets the callback for loading decay state from DB on cold start.
func (d *DecayTracker) SetLoadFunc(fn LoadFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadFn = fn
}

// Persist saves the decay state of this thread to DB. Only this tracker's own
// entries are written, and ages are stored relative to the thread's request
// clock so a restart cannot turn them negative.
func (d *DecayTracker) Persist(threadID string) {
	d.mu.RLock()
	fn := d.persistFn
	if fn == nil {
		d.mu.RUnlock()
		return
	}
	clock := d.clock

	dp := decayPersisted{
		V:         decayPersistVersion,
		Ages:      make(map[string]int),
		Intensity: make(map[string]float64),
		FilePaths: make(map[string]string),
	}
	for k, v := range d.stubbedAt {
		age := clock - v
		if age < 0 {
			age = 0
		}
		dp.Ages[k] = age
	}
	for k, v := range d.intensity {
		dp.Intensity[k] = v
	}
	for k, v := range d.filePaths {
		dp.FilePaths[k] = v
	}
	d.mu.RUnlock()

	if data, err := json.Marshal(dp); err == nil {
		fn("decay:"+threadID, string(data))
	}
}

// LoadFromDB restores decay state for a thread from DB. Called once per thread
// with the thread's current request index: the persisted ages are rebased onto
// that clock, so a stub keeps its age across a proxy restart.
func (d *DecayTracker) LoadFromDB(threadID string, currentRequestIdx int) {
	d.mu.Lock()
	if d.loaded {
		d.mu.Unlock()
		return
	}
	d.loaded = true
	if currentRequestIdx > d.clock {
		d.clock = currentRequestIdx
	}
	loadFn := d.loadFn
	d.mu.Unlock()

	if loadFn == nil {
		return
	}

	raw, ok := loadFn("decay:" + threadID)
	if !ok || raw == "" {
		return
	}

	var dp decayPersisted
	if err := json.Unmarshal([]byte(raw), &dp); err != nil {
		return
	}
	if dp.V != decayPersistVersion {
		// A blob of a DIFFERENT version is written by another build: lower is a
		// legacy blob with absolute indices of a dead process clock, higher is a
		// format this build does not know (ages in another unit). Both would be
		// misread as this format, so neither is adopted; the entries start fresh
		// (stage 0 keeps the content).
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for k, age := range dp.Ages {
		// The blob lives in proxy_state, which anything with socket access can
		// write. Reject ages that would clear a stub permanently or pin it
		// forever instead of trusting the value.
		if age < 0 || age > maxDecayAge {
			continue
		}
		if _, exists := d.stubbedAt[k]; !exists {
			d.stubbedAt[k] = currentRequestIdx - age
		}
	}
	for k, v := range dp.Intensity {
		if _, exists := d.intensity[k]; !exists {
			d.intensity[k] = v
		}
	}
	for k, v := range dp.FilePaths {
		if _, exists := d.filePaths[k]; !exists {
			d.filePaths[k] = v
		}
	}
}

// maxDecayTrackers bounds the per-thread map. The thread id comes from a
// client header and is not validated, so an unbounded map would grow with every
// distinct id. Beyond the cap a thread keeps no decay state (stubs stay fresh)
// instead of growing the map forever.
const maxDecayTrackers = 256

// decayFor returns this thread's decay tracker, lazily creating it.
// The state must never be shared across threads (YM-128): one proxy-global
// tracker keyed by message index leaked each thread's stub ages into every
// other thread and persisted the same blob for all of them. Scope matches the
// other per-thread maps (narratives, loopStates, frozenStubs).
func (s *Server) decayFor(threadID string) *DecayTracker {
	s.decayMu.Lock()
	defer s.decayMu.Unlock()
	if d := s.decayTrackers[threadID]; d != nil {
		return d
	}
	if s.decayTrackers == nil {
		s.decayTrackers = make(map[string]*DecayTracker)
	}
	if len(s.decayTrackers) >= maxDecayTrackers {
		return NewDecayTracker() // stateless: no persist/load wired, decays nothing
	}
	d := NewDecayTracker()
	d.SetPersistFunc(func(key, value string) {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{"key": key, "value": value}); err != nil {
			// %q, not %s: the key carries the client-supplied thread id, and a
			// newline in it would forge a second log line (YM-275).
			s.logger.Printf("[decay] persist failed for %q: %v", key, err)
		}
	})
	d.SetLoadFunc(func(key string) (string, bool) {
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil || result == nil {
			return "", false
		}
		var resp struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &resp) != nil || resp.Value == "" {
			return "", false
		}
		return resp.Value, true
	})
	s.decayTrackers[threadID] = d
	return d
}
