package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TimestampStore persists timestamp/delta data per user message, keyed by msg:N
// (1-based position among user messages). msg:N is derived from counting, not stored here.
//
// Persistence uses the same SetPersistFunc/SetLoadFunc callback pattern as FrozenStubs.
type TimestampStore struct {
	mu      sync.RWMutex
	threads map[string]*threadTimestamps

	persistFunc PersistFunc
	loadFunc    LoadFunc
}

// TimestampMeta holds the temporal data for a single user message.
type TimestampMeta struct {
	Timestamp      string `json:"ts,omitempty"` // "Di 2026-04-14 21:32:03"
	Delta          string `json:"d,omitempty"`  // "36s" or "1m33s"
	ThinkReminder  string `json:"tr,omitempty"` // think-reminder text (stored once, replayed idempotent)
	SkillEval      string `json:"se,omitempty"` // skill-eval text (stored once, replayed idempotent)
	Rules          string `json:"ru,omitempty"` // rules reminder text (stored once, replayed idempotent)
	AssocContext   string `json:"ac,omitempty"` // associative context from hybrid_search (stored once, replayed idempotent)
	DocContext     string `json:"dc,omitempty"` // doc context from docs_search (stored once, replayed idempotent)
	LoopWarning    string `json:"lw,omitempty"` // loop detection warning (stored once, replayed idempotent)
	PlanCheckpoint string `json:"pc,omitempty"` // plan checkpoint reminder (stored once, replayed idempotent)
	PlanNudge      string `json:"pn,omitempty"` // plan auto-activation nudge, parity path counterpart of AppendSystemBlock (stored once, replayed idempotent)
	DocsHint       string `json:"dh,omitempty"` // docs hint reminder (stored once, replayed idempotent)
	Channel        string `json:"ch,omitempty"` // channel messages/broadcasts from check_channel (stored once, replayed idempotent)
}

// TimestampHint is prepended once as a system-prompt block explaining the
// timestamp format injected into every message. Keep it short.
const TimestampHint = "[HH:MM:SS] = message timestamp, [msg:N] = message number in conversation, [+Δ] = time since previous message"

type threadTimestamps struct {
	Entries map[int]*TimestampMeta `json:"entries"` // msg:N → timestamp data
	// lastUsed orders the thread map for CapState: the last write wins the race
	// for the cap, and a thread that only reads its annotations is not growing.
	// Not persisted: after a restart every restored thread starts equally old,
	// which only means the first sweep evicts in arbitrary order.
	lastUsed time.Time `json:"-"`
}

func NewTimestampStore() *TimestampStore {
	return &TimestampStore{
		threads: make(map[string]*threadTimestamps),
	}
}

func (ts *TimestampStore) SetPersistFunc(fn PersistFunc) { ts.persistFunc = fn }
func (ts *TimestampStore) SetLoadFunc(fn LoadFunc)       { ts.loadFunc = fn }

// Store saves timestamp data for a msg:N.
func (ts *TimestampStore) Store(threadID string, msgN int, meta *TimestampMeta) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tt := ts.getOrCreate(threadID)
	tt.lastUsed = time.Now()
	tt.Entries[msgN] = meta
}

// Get retrieves stored timestamp data for a msg:N.
func (ts *TimestampStore) Get(threadID string, msgN int) (*TimestampMeta, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	tt, ok := ts.threads[threadID]
	if !ok {
		return nil, false
	}
	meta, ok := tt.Entries[msgN]
	return meta, ok
}

// CapState bounds both dimensions of the store: the number of threads and the
// number of message entries per thread. The thread id comes from a client
// header, and the entries grow with every message, so both are caller-driven.
// Per thread the oldest msg:N go first — those are the messages a frozen
// annotation can no longer be needed for. Returns the number of dropped entries.
func (ts *TimestampStore) CapState(maxThreads, maxEntriesPerThread int) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	removed := 0
	for _, tt := range ts.threads {
		removed += capThreadEntries(tt, maxEntriesPerThread)
	}

	if maxThreads > 0 && len(ts.threads) > maxThreads {
		type aged struct {
			id   string
			used time.Time
		}
		all := make([]aged, 0, len(ts.threads))
		for id, tt := range ts.threads {
			all = append(all, aged{id: id, used: tt.lastUsed})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
		for _, a := range all[:len(all)-maxThreads] {
			removed += len(ts.threads[a.id].Entries)
			delete(ts.threads, a.id)
		}
	}
	return removed
}

// Persist serializes the thread's timestamp data via the persistFunc callback.
// The marshal runs under the read lock: the entry map is written by Store and
// scrubbed by ScrubPerSessionContent, and marshalling a live map outside the
// lock is a concurrent map read and write.
func (ts *TimestampStore) Persist(threadID string) {
	if ts.persistFunc == nil {
		return
	}
	ts.mu.RLock()
	tt, ok := ts.threads[threadID]
	var data []byte
	var err error
	if ok {
		data, err = json.Marshal(tt)
	}
	ts.mu.RUnlock()
	if !ok || err != nil {
		return
	}
	ts.persistFunc("timestamps:"+threadID, string(data))
}

// Load deserializes a thread's timestamp data via the loadFunc callback.
func (ts *TimestampStore) Load(threadID string) {
	ts.load(threadID, false)
}

// LoadScrubbed is Load for a key that does not name a session: the restored
// entries lose their session content in the same lock, so a concurrent Load of
// the same key cannot put the persisted blob back between the two steps.
func (ts *TimestampStore) LoadScrubbed(threadID string) {
	ts.load(threadID, true)
}

func (ts *TimestampStore) load(threadID string, scrub bool) {
	var restored *threadTimestamps
	if ts.loadFunc != nil {
		if raw, ok := ts.loadFunc("timestamps:" + threadID); ok && raw != "" {
			var tt threadTimestamps
			if json.Unmarshal([]byte(raw), &tt) == nil {
				if tt.Entries == nil {
					tt.Entries = make(map[int]*TimestampMeta)
				}
				// A restored entry set is trimmed like a live one: Load runs at the
				// start of every request, so without this the cap would be undone by
				// the next request of the same thread and the persisted blob would
				// keep growing.
				capThreadEntries(&tt, timestampEntriesPerThread)
				restored = &tt
			}
		}
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if restored != nil {
		if prev, ok := ts.threads[threadID]; ok && prev != nil {
			// Load runs on every request: keep the recency the live entry has, or
			// every thread looks equally old and CapState evicts at random.
			restored.lastUsed = prev.lastUsed
		}
		ts.threads[threadID] = restored
	}
	if scrub {
		// Scrub under the same lock as the swap: a concurrent Load of this key must
		// not be able to put the persisted content back between the two steps.
		if cur, ok := ts.threads[threadID]; ok && cur != nil {
			scrubThreadLocked(cur)
		}
	}
}

// capThreadEntries keeps the newest msg:N of one thread, in place. "Newest" is
// the largest number, which holds as long as the message offset stays. After a
// collapse renumbers the window, the largest numbers can be the stale ones; the
// cap only fires above 2000 entries, so this stays a rare corner.
func capThreadEntries(tt *threadTimestamps, keep int) int {
	if keep <= 0 || len(tt.Entries) <= keep {
		return 0
	}
	keys := make([]int, 0, len(tt.Entries))
	for k := range tt.Entries {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	removed := 0
	for _, k := range keys[keep:] {
		delete(tt.Entries, k)
		removed++
	}
	return removed
}

// ScrubPerSessionContent drops the per-session fields from every entry of a
// thread whose key is not a session.
//
// A derived thread key (DeriveThreadID) is the same string for every session of
// that client, so the store may already hold another session's frozen rules,
// associative context, plan checkpoint, docs hint or mailbox under it. Writing
// is gated on ThreadIsSession; this closes the read side, which would otherwise
// replay those entries into the next session that lands on the same key.
func (ts *TimestampStore) ScrubPerSessionContent(threadID string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tt, ok := ts.threads[threadID]
	if !ok {
		return 0
	}
	return scrubThreadLocked(tt)
}

// scrubThreadLocked drops the per-session fields of every entry. Must be called
// with ts.mu held.
func scrubThreadLocked(tt *threadTimestamps) int {
	// SkillEval stays: it is the static config text, not session content.
	scrubbed := 0
	for msgN, meta := range tt.Entries {
		if meta == nil {
			continue
		}
		if meta.ThinkReminder == "" && meta.Rules == "" && meta.AssocContext == "" &&
			meta.DocContext == "" && meta.LoopWarning == "" && meta.PlanCheckpoint == "" && meta.PlanNudge == "" &&
			meta.DocsHint == "" && meta.Channel == "" {
			continue
		}
		// Replace the entry instead of mutating it: Get hands the pointer to
		// readers that use it outside any lock, so clearing fields in place
		// would race with them.
		clean := *meta
		clean.ThinkReminder = ""
		clean.Rules = ""
		clean.AssocContext = ""
		clean.DocContext = ""
		clean.LoopWarning = ""
		clean.PlanCheckpoint = ""
		clean.PlanNudge = ""
		clean.DocsHint = ""
		clean.Channel = ""
		tt.Entries[msgN] = &clean
		scrubbed++
	}
	return scrubbed
}

func (ts *TimestampStore) getOrCreate(threadID string) *threadTimestamps {
	tt, ok := ts.threads[threadID]
	if !ok {
		tt = &threadTimestamps{Entries: make(map[int]*TimestampMeta)}
		ts.threads[threadID] = tt
	}
	return tt
}

// BuildMeta assembles the full annotation string from a msg:N and optional stored data.
// Returns the formatted metadata block. Additional stable injects (think-reminder, skill-eval,
// rules) are appended on separate lines when present. Lines are \n-separated.
// The TimestampHint is included on the first message (msg:1) only.
func BuildMeta(msgN int, meta *TimestampMeta) string {
	var parts []string

	if meta != nil && meta.Timestamp != "" {
		s := fmt.Sprintf("[%s] [msg:%d]", meta.Timestamp, msgN)
		if meta.Delta != "" {
			s += " [+" + meta.Delta + "]"
		}
		parts = append(parts, s)
	} else {
		parts = append(parts, fmt.Sprintf("[msg:%d]", msgN))
	}

	if msgN == 1 {
		parts = append(parts, "[ts-hint] "+TimestampHint)
	}

	if meta != nil {
		if meta.ThinkReminder != "" {
			parts = append(parts, "[think-reminder] "+meta.ThinkReminder)
		}
		if meta.SkillEval != "" {
			// buildSkillEvalBlock already wraps content in [skill-eval]...[/skill-eval] markers.
			// Only prepend the tag when the content lacks them (e.g. test fixtures).
			if strings.HasPrefix(meta.SkillEval, "[skill-eval]") {
				parts = append(parts, meta.SkillEval)
			} else {
				parts = append(parts, "[skill-eval] "+meta.SkillEval)
			}
		}
		if meta.Rules != "" {
			parts = append(parts, "[rules] "+meta.Rules)
		}
		if meta.AssocContext != "" {
			parts = append(parts, "[assoc-context] "+meta.AssocContext)
		}
		if meta.DocContext != "" {
			parts = append(parts, "[doc-context] "+meta.DocContext)
		}
		if meta.LoopWarning != "" {
			parts = append(parts, "[loop-warning] "+meta.LoopWarning)
		}
		if meta.PlanCheckpoint != "" {
			parts = append(parts, "[plan-checkpoint] "+meta.PlanCheckpoint)
		}
		if meta.PlanNudge != "" {
			parts = append(parts, "[plan-nudge] "+meta.PlanNudge)
		}
		if meta.DocsHint != "" {
			parts = append(parts, "[docs-hint] "+meta.DocsHint)
		}
		if meta.Channel != "" {
			parts = append(parts, "[channel] "+meta.Channel)
		}
	}

	return strings.Join(parts, "\n")
}

// InjectTimestamps injects [msg:N] (+ timestamp/delta if available) on messages.
// msg:N = offset + position among ALL messages (not just user). Returns injection count.
// stubsCount: frozen stubs at the start of the array are skipped (already annotated).
// Fresh tail messages are always re-annotated (strips any existing [msg:N] prefix first).
func InjectTimestamps(store *TimestampStore, threadID string, msgs []any, endIdx, offset, stubsCount int) int {
	if store == nil {
		return 0
	}
	if endIdx > len(msgs) {
		endIdx = len(msgs)
	}
	injected := 0
	for i := 0; i < endIdx; i++ {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if i < stubsCount {
			continue // frozen stubs already have persistent annotations
		}
		stripMetaPrefix(msg) // remove any existing [timestamp] [msg:N] [+delta] prefix
		msgN := offset + i + 1
		meta, _ := store.Get(threadID, msgN)
		prependMeta(msg, BuildMeta(msgN, meta))
		injected++
	}
	return injected
}

// msgHasMetaPrefix checks if a message already has a timestamp/msg annotation.
func msgHasMetaPrefix(msg map[string]any) bool {
	switch content := msg["content"].(type) {
	case string:
		return hasMetaPrefix(content)
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "text" {
				continue
			}
			text, _ := b["text"].(string)
			return hasMetaPrefix(text)
		}
	}
	return false
}

// msgHasTextBlock returns true if the message has at least one text content block.
func msgHasTextBlock(msg map[string]any) bool {
	switch content := msg["content"].(type) {
	case string:
		return content != ""
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if ok && b["type"] == "text" {
				return true
			}
		}
	}
	return false
}

// CountUserMessages counts role="user" messages in msgs[0:endIdx].
func CountUserMessages(msgs []any, endIdx int) int {
	if endIdx > len(msgs) {
		endIdx = len(msgs)
	}
	n := 0
	for i := 0; i < endIdx; i++ {
		msg, ok := msgs[i].(map[string]any)
		if ok && msg["role"] == "user" {
			n++
		}
	}
	return n
}

// hasMetaPrefix is defined in proxy_helpers.go — uses strings package
var _ = strings.Contains // ensure import
