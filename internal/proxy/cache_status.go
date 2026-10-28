package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tokenizerDriftFactor compensates for qhenkart BPE tokenizer undercounting
// ~13% vs Claude 4's actual tokenizer, plus structural tokens not captured
// by countMessageTokens (message boundaries, role markers).
// Calibrated: countMessageTokens+overhead=210k vs CC's actual 249k → 1.19.
const tokenizerDriftFactor = 1.19

// StatusPlanItem is one step of a plan as the opencode sidebar reads it. It
// mirrors the daemon's PlanItem on the wire: status reuses the todowrite
// vocabulary, depth carries nesting, reason is the field todowrite lacks.
type StatusPlanItem struct {
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Depth   int    `json:"depth,omitempty"`
}

// StatusPlan is the plan snapshot a thread's status file carries. A nil pointer
// means "no active plan" and keeps the JSON free of the key, which is what a
// content-only plan has always looked like.
type StatusPlan struct {
	Items []StatusPlanItem `json:"items"`
}

// CacheStatus is the JSON structure written for the statusline script.
type CacheStatus struct {
	TTL                   string      `json:"ttl"`
	TTLSeconds            int         `json:"ttl_seconds"`
	RemainingS            int         `json:"remaining_s"`
	CacheState            string      `json:"cache_state"`
	CostPerReq            float64     `json:"cost_per_req"`
	LastRequestTS         int64       `json:"last_request_ts"`
	TotalTokens           int         `json:"total_tokens"`
	RawTokenEstimate      int         `json:"raw_token_estimate,omitempty"`
	CacheReadTokens       int         `json:"cache_read_tokens"`
	CacheWriteTokens      int         `json:"cache_write_tokens"`
	ThreadID              string      `json:"thread_id,omitempty"`
	TokenThreshold        int         `json:"token_threshold,omitempty"`
	TokenMinimumThreshold int         `json:"token_minimum_threshold,omitempty"`
	DetectedTTL           string      `json:"detected_ttl"`              // "unknown", "5min", "1h"
	KeepaliveMode         string      `json:"keepalive_mode"`            // "auto", "5m", "1h"
	PingIntervalS         int         `json:"ping_interval_s,omitempty"` // seconds between pings
	PingsRemaining        int         `json:"pings_remaining"`           // pings left in current phase
	ActiveThreads         int         `json:"active_threads,omitempty"`  // tracked keepalive threads
	Plan                  *StatusPlan `json:"plan,omitempty"`            // active plan outline for the sidebar
}

// CacheStatusWriter writes cache status to disk every second.
type CacheStatusWriter struct {
	mu                    sync.Mutex
	dataDir               string
	ttlConfig             string
	tokenMinimumThreshold int
	detector              *CacheTTLDetector
	keepalive             *CacheKeepalive
	threads               map[string]*statusThreadState

	// done ends the ticker loop, stopped is closed once that loop has returned,
	// and stopOnce makes Stop idempotent. Without them the constructor's
	// goroutine outlived every test that built a writer (YM-396).
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// writeStatusHold is nil in production. A test installs a function to keep
	// one status write open, which is how Stop's synchronous guarantee becomes
	// observable: Stop must not return while the write runs. A hook must not
	// call Stop: that waits for the hook's own write to finish and would
	// deadlock the loop goroutine.
	writeStatusHold func()
}

// liveCacheStatusWriters counts the writers whose ticker loop is still running.
// The package-final leak guard asserts it stays at the instances the known
// Run-harness tests leave behind (YM-396); a counter survives method renames and
// a truncated stack dump, unlike scanning goroutine stacks for a name.
var liveCacheStatusWriters atomic.Int64

// statusThreadState holds per-session cache status data.
type statusThreadState struct {
	lastRequest      time.Time
	totalTokens      int
	rawTokenEstimate int
	cacheRead        int
	cacheWrite       int
	tokenThreshold   int
	// plan is replaced wholesale on every publish and never mutated in place,
	// so the snapshot copy in writeStatus may share the pointer.
	plan *StatusPlan
}

// NewCacheStatusWriter creates and starts a background writer. Stop ends it;
// every caller owns that call (the proxy shutdown, and tests through t.Cleanup).
func NewCacheStatusWriter(dataDir string, ttlConfig string, defaultThreshold int, defaultMinThreshold int) *CacheStatusWriter {
	w := &CacheStatusWriter{
		dataDir:               dataDir,
		ttlConfig:             ttlConfig,
		tokenMinimumThreshold: defaultMinThreshold,
		threads:               make(map[string]*statusThreadState),
		done:                  make(chan struct{}),
		stopped:               make(chan struct{}),
	}
	liveCacheStatusWriters.Add(1)
	go w.tickLoop()
	return w
}

// Stop ends the background writer and returns after its loop has finished,
// including a status write that was in flight. Idempotent, and a no-op on a
// writer that was never started (a zero value has no channels to close).
func (w *CacheStatusWriter) Stop() {
	if w == nil || w.done == nil || w.stopped == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.done) })
	<-w.stopped
}

// SetDetector sets the TTL detector for dynamic TTL display.
func (w *CacheStatusWriter) SetDetector(d *CacheTTLDetector) {
	w.mu.Lock()
	w.detector = d
	w.mu.Unlock()
}

// SetKeepalive sets the keepalive for ping status display.
func (w *CacheStatusWriter) SetKeepalive(ka *CacheKeepalive) {
	w.mu.Lock()
	w.keepalive = ka
	w.mu.Unlock()
}

// Update records new values from an API response.
func (w *CacheStatusWriter) Update(lastRequest time.Time, totalTokens, cacheRead, cacheWrite int, threadID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ts := w.threads[threadID]
	if ts == nil {
		ts = &statusThreadState{}
		w.threads[threadID] = ts
	}
	ts.lastRequest = lastRequest
	ts.totalTokens = totalTokens
	ts.cacheRead = cacheRead
	ts.cacheWrite = cacheWrite
}

// UpdateThresholdForThread sets the token threshold for a specific thread.
func (w *CacheStatusWriter) UpdateThresholdForThread(threadID string, threshold int) {
	w.mu.Lock()
	if ts := w.threads[threadID]; ts != nil {
		ts.tokenThreshold = threshold
	}
	w.mu.Unlock()
}

// UpdateRawForThread sets the raw token estimate for a specific thread.
func (w *CacheStatusWriter) UpdateRawForThread(threadID string, raw int) {
	w.mu.Lock()
	if ts := w.threads[threadID]; ts != nil {
		ts.rawTokenEstimate = raw
	}
	w.mu.Unlock()
}

// SetPlan records the active plan a thread's status file should carry, so the
// opencode sidebar can draw it. A nil plan clears the snapshot. The value is
// replaced wholesale, never mutated, so readers may share the pointer.
//
// Only a thread the writer has already seen gets a snapshot: creating an entry
// here would write a zero last-request timestamp and a cold-cache status for a
// thread that never made a request. It reports whether the value landed, so the
// caller can retry instead of assuming it did.
func (w *CacheStatusWriter) SetPlan(threadID string, plan *StatusPlan) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	ts := w.threads[threadID]
	if ts == nil {
		return false
	}
	ts.plan = plan
	return true
}

func (w *CacheStatusWriter) tickLoop() {
	// Closing stopped is the exit signal Stop waits for: it is deferred, so it
	// fires after a writeStatus that is still running has returned. The counter
	// decrement is declared last and therefore runs first, so a writer counts as
	// gone by the time Stop returns.
	defer close(w.stopped)
	defer liveCacheStatusWriters.Add(-1)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.writeStatus()
		}
	}
}

func (w *CacheStatusWriter) writeStatus() {
	w.mu.Lock()
	hold := w.writeStatusHold
	w.mu.Unlock()
	if hold != nil {
		hold()
	}

	w.mu.Lock()
	if len(w.threads) == 0 {
		w.mu.Unlock()
		return
	}
	ttlConfig := w.ttlConfig
	minThreshold := w.tokenMinimumThreshold
	det := w.detector
	ka := w.keepalive

	// Snapshot all thread states under lock
	type snapshot struct {
		id string
		ts statusThreadState
	}
	snaps := make([]snapshot, 0, len(w.threads))
	for id, ts := range w.threads {
		snaps = append(snaps, snapshot{id: id, ts: *ts})
	}
	w.mu.Unlock()

	// Resolve TTL (shared across threads)
	ttlSec := 300
	detectedTTL := "unknown"
	if ttlConfig == "" || ttlConfig == "ephemeral" {
		ttlSec = 300
		detectedTTL = "5min"
	} else if ttlConfig == "1h" {
		if det != nil {
			ttlSec = det.EffectiveTTLSeconds()
			sup := det.Is1hSupported()
			if sup != nil {
				if *sup {
					detectedTTL = "1h"
				} else {
					detectedTTL = "5min"
				}
			}
		} else {
			ttlSec = 3600
			detectedTTL = "1h"
		}
	}

	// Keepalive status (shared across threads)
	var kaMode string
	var pingIntervalS, pingsRemaining, totalPings, activeThreads int
	if ka != nil {
		ks := ka.Status()
		kaMode = ks.Mode
		pingIntervalS = ks.IntervalS
		totalPings = ks.TotalPings
		pingsRemaining = ks.PingsRemaining
		activeThreads = ks.ActiveThreads
	}

	dir := filepath.Join(w.dataDir, "cache-status")
	os.MkdirAll(dir, 0755)

	for _, snap := range snaps {
		ts := snap.ts

		remaining := ttlSec - int(time.Since(ts.lastRequest).Seconds())
		if remaining < 0 {
			remaining = 0
		}

		state := "warm"
		if remaining == 0 {
			if totalPings > 0 && pingIntervalS > 0 {
				elapsed := int(time.Since(ts.lastRequest).Seconds())
				totalCoverage := totalPings*pingIntervalS + ttlSec
				remaining = totalCoverage - elapsed
				if remaining < 0 {
					remaining = 0
				}
			}
			if remaining > 0 {
				state = "warm"
			} else {
				state = "cold"
			}
		} else if remaining < 60 {
			state = "cooling"
		}

		tokensK := float64(ts.totalTokens) / 1000.0
		var costPerReq float64
		switch state {
		case "warm", "cooling":
			costPerReq = (tokensK-5)*0.0005 + 5*0.005
		case "cold":
			writeRate := 0.00625
			if ttlConfig == "1h" {
				writeRate = 0.01
			}
			costPerReq = tokensK * writeRate
		}

		threshold := ts.tokenThreshold

		status := CacheStatus{
			TTL:                   ttlConfig,
			TTLSeconds:            ttlSec,
			RemainingS:            remaining,
			CacheState:            state,
			CostPerReq:            costPerReq,
			LastRequestTS:         ts.lastRequest.Unix(),
			TotalTokens:           ts.totalTokens,
			RawTokenEstimate:      ts.rawTokenEstimate,
			CacheReadTokens:       ts.cacheRead,
			CacheWriteTokens:      ts.cacheWrite,
			ThreadID:              snap.id,
			TokenThreshold:        threshold,
			TokenMinimumThreshold: minThreshold,
			DetectedTTL:           detectedTTL,
			KeepaliveMode:         kaMode,
			PingIntervalS:         pingIntervalS,
			PingsRemaining:        pingsRemaining,
			ActiveThreads:         activeThreads,
			Plan:                  ts.plan,
		}

		data, err := json.Marshal(status)
		if err != nil {
			continue
		}

		tmp := CacheStatusPath(w.dataDir, snap.id) + ".tmp"
		target := CacheStatusPath(w.dataDir, snap.id)
		if err := os.WriteFile(tmp, data, 0644); err != nil {
			continue
		}
		os.Rename(tmp, target)
	}
}

// CacheStatusPath returns the path to the per-thread cache status JSON file.
func CacheStatusPath(dataDir string, threadID string) string {
	return filepath.Join(dataDir, "cache-status", "status-"+cacheStatusFileID(threadID)+".json")
}

// statusHashedPrefix marks a file name that is a hash rather than a thread id.
// A valid id is never given this prefix, which keeps the mapping one-to-one:
// two different ids cannot end up on one status file (YM-398).
const statusHashedPrefix = "hashed-"

// statusMaxThreadIDLen bounds the id that may become part of a file name. A
// longer id would exceed the file-system limit, so it is hashed like an
// unsafe one instead of silently producing no status file.
const statusMaxThreadIDLen = 128

// cacheStatusFileID maps a thread id onto the file-name segment of its status
// file. The id comes from a client session header, so it is not a path: only
// the characters a session id actually uses pass through unchanged, anything
// else becomes a hash of the whole id. Traversal cannot leave the directory and
// a legitimate session keeps the file name it had before (YM-398).
func cacheStatusFileID(threadID string) string {
	if isStatusFileIDSafe(threadID) && !strings.HasPrefix(threadID, statusHashedPrefix) {
		return threadID
	}
	sum := sha256.Sum256([]byte(threadID))
	return statusHashedPrefix + hex.EncodeToString(sum[:16])
}

// isStatusFileIDSafe reports whether the id can be used as a file-name segment
// verbatim. "/" and "\\" are separators, a NUL ends a path in the OS, and
// control characters end a journal line; all of them are rejected, and so is a
// length past the file-system limit.
func isStatusFileIDSafe(s string) bool {
	if s == "" || len(s) > statusMaxThreadIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return false
		}
	}
	return true
}

// FormatCacheStatus returns a short status string for the terminal.
func FormatCacheStatus(s CacheStatus) string {
	switch s.CacheState {
	case "warm":
		min := s.RemainingS / 60
		sec := s.RemainingS % 60
		return fmt.Sprintf("Cache: %d:%02d %s $%.2f/req", min, sec, s.TTL, s.CostPerReq)
	case "cooling":
		return fmt.Sprintf("Cache: %ds! %s $%.2f/req", s.RemainingS, s.TTL, s.CostPerReq)
	default:
		return fmt.Sprintf("Cache: COLD $%.2f/req", s.CostPerReq)
	}
}

// StatusLines holds the pre-formatted lines for the terminal display.
type StatusLines struct {
	CacheLine      string // "CacheLifeTime (1h) until 13:20 | Keepalive until 17:20 with 4 ping(s) every 54min"
	CollapsingLine string // "Lossless collapsing at 200k to 80k Token | actual usage: 98k / 200k Token"
	ExpiryCostLine string // "after Cache expiry $1.84 for refresh"
}

// FormatStatusLines builds structured terminal display lines from cache status.
func FormatStatusLines(s CacheStatus) StatusLines {
	var lines StatusLines

	ttlLabel := ttlDisplayLabel(s.DetectedTTL)
	tokK := s.TotalTokens / 1000
	coldCost := calcColdCost(s)

	if s.CacheState == "cold" {
		tokDisplay := formatTokenDisplay(tokK, s.TokenThreshold)
		lines.CacheLine = fmt.Sprintf("COLD (%s) | %s Token", ttlLabel, tokDisplay)
		lines.ExpiryCostLine = fmt.Sprintf("$%.2f for refresh", coldCost)
	} else {
		cacheExpiry := time.Unix(s.LastRequestTS+int64(s.TTLSeconds), 0)
		cacheExpiryStr := cacheExpiry.Format("15:04")

		cachePart := fmt.Sprintf("CacheLifeTime (%s) until %s", ttlLabel, cacheExpiryStr)

		if s.PingsRemaining > 0 && s.PingIntervalS > 0 {
			keepaliveEnd := cacheExpiry.Add(time.Duration(s.PingsRemaining) * time.Duration(s.PingIntervalS) * time.Second)
			keepaliveEndStr := keepaliveEnd.Format("15:04")
			lines.CacheLine = fmt.Sprintf("%s | Keepalive until %s with %d ping(s) every %s",
				cachePart, keepaliveEndStr, s.PingsRemaining, formatInterval(s.PingIntervalS))
		} else {
			lines.CacheLine = cachePart
		}

		lines.ExpiryCostLine = fmt.Sprintf("after Cache expiry $%.2f for refresh", coldCost)
	}

	if s.TokenThreshold > 0 && s.TokenMinimumThreshold > 0 {
		if s.RawTokenEstimate > 0 && s.RawTokenEstimate > s.TotalTokens {
			rawK := int(float64(s.RawTokenEstimate)*tokenizerDriftFactor) / 1000
			saved := 100 - tokK*100/rawK
			lines.CollapsingLine = fmt.Sprintf("Lossless collapsing %dk → %dk (%d%% saved) | threshold: %dk", rawK, tokK, saved, s.TokenThreshold/1000)
		} else {
			colPart := fmt.Sprintf("Lossless collapsing at %dk to %dk Token", s.TokenThreshold/1000, s.TokenMinimumThreshold/1000)
			usagePart := fmt.Sprintf("actual usage: %dk / %dk Token", tokK, s.TokenThreshold/1000)
			lines.CollapsingLine = fmt.Sprintf("%s | %s", colPart, usagePart)
		}
	}

	return lines
}

func ttlDisplayLabel(detected string) string {
	switch detected {
	case "1h":
		return "1h"
	case "5min":
		return "5min"
	default:
		return "detecting…"
	}
}

func formatTokenDisplay(tokK int, threshold int) string {
	if threshold > 0 {
		return fmt.Sprintf("%dk/%dk", tokK, threshold/1000)
	}
	return fmt.Sprintf("%dk", tokK)
}

func calcColdCost(s CacheStatus) float64 {
	tokK := float64(s.TotalTokens) / 1000.0
	// Opus 4.6: 5min cache write $6.25/MTok, 1h cache write $10.00/MTok
	writeRate := 0.00625
	if s.DetectedTTL == "1h" || s.TTL == "1h" {
		writeRate = 0.01
	}
	return tokK * writeRate
}

// formatInterval renders seconds as "Xm", "XmYs", or "Ys".
func formatInterval(totalS int) string {
	m := totalS / 60
	s := totalS % 60
	if m == 0 {
		return fmt.Sprintf("%ds", s)
	}
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

// pruneThreads bounds the per-thread status states. The status file only shows
// live threads; a dropped entry is rebuilt on that thread's next request.
func (w *CacheStatusWriter) pruneThreads(keep int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return pruneThreadKeyed(w.threads, keep)
}
