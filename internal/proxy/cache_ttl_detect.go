package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CacheTTLDetector determines whether the API honors 1h cache TTL.
//
// Two signals:
//   - Negative: substantial write with ephemeral_1h=0 → immediate 5min
//   - Positive: after >5:01 gap, cache_read/(cache_read+cache_write) > 0.8
//     means the cache survived past 5min → confirmed 1h
type CacheTTLDetector struct {
	mu             sync.Mutex
	supported      *bool                // nil = unknown, true/false = detected
	testedAt       time.Time            // when supported was last determined
	threadRequests map[string]time.Time // per-thread last response time for gap measurement
	lastEvict      time.Time            // last sweep of threadRequests
	dataDir        string               // for persisting state across restarts
}

const (
	// ttlThreadEvictionAge bounds how long a gap timeline is worth keeping. The
	// evidence lives between 5 minutes and the 1h lifetime; an older timeline
	// can no longer prove anything and only costs memory.
	ttlThreadEvictionAge = 2 * time.Hour
	// ttlMaxTrackedThreads is the hard cap on tracked timelines. The thread ID
	// comes from a client header, so the age rule alone leaves the map open to
	// unbounded growth by a caller that invents one ID per request.
	ttlMaxTrackedThreads = 1024
	ttlEvictInterval     = 10 * time.Minute
)

func NewCacheTTLDetector() *CacheTTLDetector {
	return &CacheTTLDetector{
		threadRequests: make(map[string]time.Time),
	}
}

// NewCacheTTLDetectorWithPersist creates a detector that persists state to disk.
func NewCacheTTLDetectorWithPersist(dataDir string) *CacheTTLDetector {
	d := &CacheTTLDetector{
		threadRequests: make(map[string]time.Time),
		dataDir:        dataDir,
	}
	d.loadState()
	return d
}

// RecordResponse checks one thread's response fields to determine 1h TTL support.
// The thread ID is passed explicitly: a single "current" thread attributed each
// response to whichever request started last, so interleaved threads measured
// the wrong timeline.
func (d *CacheTTLDetector) RecordResponse(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
	d.RecordResponseAndReportChange(threadID, cacheRead, cacheWrite, ephemeral1h)
}

// RecordResponseAndReportChange records a response and reports the resulting
// detection state plus whether it differs from the previous one. Both happen
// under one lock: two concurrent responses must not both apply the same
// transition (double propagation, double keepalive retrigger).
func (d *CacheTTLDetector) RecordResponseAndReportChange(threadID string, cacheRead, cacheWrite, ephemeral1h int) (*bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	total := cacheRead + cacheWrite
	if total == 0 {
		return d.supported, false
	}

	prevSupported := d.supported

	// Gap measurement: time since THIS thread's previous response
	now := time.Now()
	prevTime, hasPrev := d.threadRequests[threadID]
	gap := now.Sub(prevTime)
	hasGap := hasPrev && gap > 301*time.Second

	// Update this thread's timestamp after measuring gap
	d.threadRequests[threadID] = now
	d.evictThreadRequestsLocked(now, threadID)

	// Positive: after >5:01 gap, cache still warm (>80% read) → 1h confirmed
	if hasGap && total > 0 && cacheRead*100/total > 80 {
		t := true
		d.supported = &t
		d.testedAt = time.Now()
		d.saveStateIfChanged(prevSupported)
		return d.supported, !boolPtrEqual(prevSupported, d.supported)
	}

	// Negative: substantial write (not delta) with ephemeral_1h=0 → 5min
	isSubstantialWrite := cacheWrite > 0 && (cacheRead == 0 || cacheWrite*5 >= cacheRead)
	if isSubstantialWrite && ephemeral1h == 0 {
		f := false
		d.supported = &f
		d.testedAt = time.Now()
		d.saveStateIfChanged(prevSupported)
	}
	return d.supported, !boolPtrEqual(prevSupported, d.supported)
}

// boolPtrEqual compares two optional detection results; nil never equals a value.
func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// evictThreadRequestsLocked bounds the per-thread gap timelines. Two rules: drop
// expired timelines, then enforce a hard cap oldest first. keep is the thread of
// the response being recorded and is never dropped. Caller holds mu. The sweep
// runs at most once per ttlEvictInterval unless the map is over the cap.
func (d *CacheTTLDetector) evictThreadRequestsLocked(now time.Time, keep string) {
	if len(d.threadRequests) > ttlMaxTrackedThreads || now.Sub(d.lastEvict) >= ttlEvictInterval {
		cutoff := now.Add(-ttlThreadEvictionAge)
		for tid, seen := range d.threadRequests {
			if tid != keep && seen.Before(cutoff) {
				delete(d.threadRequests, tid)
			}
		}
		d.lastEvict = now
	}
	if len(d.threadRequests) <= ttlMaxTrackedThreads {
		return
	}
	type entry struct {
		tid  string
		seen time.Time
	}
	entries := make([]entry, 0, len(d.threadRequests))
	for tid, seen := range d.threadRequests {
		entries = append(entries, entry{tid, seen})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seen.Before(entries[j].seen) })
	// Oldest first: a timeline that is closer to the age limit is the least
	// likely to still serve as gap evidence for a live session.
	for _, e := range entries[:len(entries)-ttlMaxTrackedThreads] {
		if e.tid != keep {
			delete(d.threadRequests, e.tid)
		}
	}
}

// Is1hSupported returns nil (unknown), true (confirmed), or false (denied).
func (d *CacheTTLDetector) Is1hSupported() *bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.supported
}

// EffectiveTTLSeconds returns 3600 if 1h is confirmed, 300 otherwise.
func (d *CacheTTLDetector) EffectiveTTLSeconds() int {
	sup := d.Is1hSupported()
	if sup != nil && *sup {
		return 3600
	}
	return 300
}

type ttlDetectState struct {
	Supported bool      `json:"supported"`
	TestedAt  time.Time `json:"tested_at"`
}

func (d *CacheTTLDetector) statePath() string {
	if d.dataDir == "" {
		return ""
	}
	return filepath.Join(d.dataDir, "cache_ttl_state.json")
}

// saveStateIfChanged writes state to disk when detection result changes. Caller holds mu.
func (d *CacheTTLDetector) saveStateIfChanged(prev *bool) {
	if d.supported == nil || d.statePath() == "" {
		return
	}
	if prev != nil && *prev == *d.supported {
		return
	}
	data, _ := json.Marshal(ttlDetectState{Supported: *d.supported, TestedAt: d.testedAt})
	os.WriteFile(d.statePath(), data, 0644)
}

// loadState reads persisted detection state. Only used at startup.
func (d *CacheTTLDetector) loadState() {
	p := d.statePath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var st ttlDetectState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	// Only use if tested recently (within 24h)
	if time.Since(st.TestedAt) > 24*time.Hour {
		return
	}
	d.supported = &st.Supported
	d.testedAt = st.TestedAt
}
