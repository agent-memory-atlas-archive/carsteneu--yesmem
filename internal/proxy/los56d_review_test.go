package proxy

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// Review round 1 (cold + consequence): CapState trimmed the in-memory map, but
// Load ran at the start of every request and restored the untrimmed entry set
// from the persisted blob — the cap was undone by the next request of the same
// thread, and the blob kept growing.
func TestLos56dTimestampStoreCapSurvivesLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	total := timestampEntriesPerThread + 10
	entries := make(map[int]*TimestampMeta, total)
	for i := 1; i <= total; i++ {
		entries[i] = &TimestampMeta{Timestamp: "Mo 2026-10-20 10:00:00"}
	}
	ts := NewTimestampStore()
	ts.SetPersistFunc(func(key, value string) { _ = os.WriteFile(path, []byte(value), 0o600) })
	ts.SetLoadFunc(func(key string) (string, bool) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		return string(raw), true
	})
	ts.mu.Lock()
	ts.threads["tid-load"] = &threadTimestamps{Entries: entries}
	ts.mu.Unlock()
	ts.Persist("tid-load")

	// Drop the thread, then reload: the restored set must respect the cap.
	ts.mu.Lock()
	delete(ts.threads, "tid-load")
	ts.mu.Unlock()
	ts.Load("tid-load")

	ts.mu.RLock()
	got := len(ts.threads["tid-load"].Entries)
	ts.mu.RUnlock()
	if got != timestampEntriesPerThread {
		t.Errorf("restored entries = %d, want the cap %d", got, timestampEntriesPerThread)
	}
	ts.mu.RLock()
	_, newest := ts.threads["tid-load"].Entries[total]
	ts.mu.RUnlock()
	if !newest {
		t.Error("the newest msg:N was dropped on Load")
	}
}

// Review round 1 (security): the write gate kept new content out of a derived
// thread key, but entries stored before it still replayed another session's
// frozen blocks. The read path scrubs them.
func TestLos56dScrubDropsForeignSessionContent(t *testing.T) {
	ts := NewTimestampStore()
	ts.Store("derived-key", 3, &TimestampMeta{
		Timestamp:      "Mo 2026-10-20 10:00:00",
		Delta:          "12s",
		Rules:          "FOREIGN-RULES",
		PlanCheckpoint: "FOREIGN-PLAN",
		Channel:        "SESSION_ID: opencode:foreign",
		AssocContext:   "FOREIGN-ASSOC",
	})

	if n := ts.ScrubPerSessionContent("derived-key"); n != 1 {
		t.Fatalf("scrubbed %d entries, want 1", n)
	}
	meta, ok := ts.Get("derived-key", 3)
	if !ok || meta == nil {
		t.Fatal("entry disappeared")
	}
	for name, val := range map[string]string{
		"rules":           meta.Rules,
		"plan checkpoint": meta.PlanCheckpoint,
		"channel":         meta.Channel,
		"assoc":           meta.AssocContext,
	} {
		if val != "" {
			t.Errorf("%s survived the scrub: %q", name, val)
		}
	}
	if meta.Timestamp == "" || meta.Delta != "12s" {
		t.Errorf("temporal data was scrubbed too: %+v", meta)
	}
}

// Review round 1 (cold + consequence): gating the frozen entry dropped four
// features for a client without a session header instead of moving them to the
// tail message, the way plan nudge and channel already do.
func TestLos56dDerivedKeyCarriesSessionContentOnTheTail(t *testing.T) {
	s := los26aHandlerServer(t, "http://127.0.0.1:1")
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, RulesReminder: true, PlanCheckpoint: true}
	los33DaemonSocket(t, s, func(method string, params map[string]any) string {
		switch method {
		case "get_plan":
			return `{"exists":true,"plan":"LOS56D-PLAN","status":"active"}`
		case "get_docs_hint":
			return `{"docs_hint":"LOS56D-DOCS"}`
		case "get_rules_block":
			return `{"content":"LOS56D-RULES","exists":true}`
		default:
			return "null"
		}
	})

	ctx := &openAIRequestContext{ThreadID: "derived-hash", RawMsgCount: 3, ThreadIsSession: false}
	tokens := 1
	last := func() string {
		req := map[string]any{"model": "gpt-5.4", "messages": []any{
			map[string]any{"role": "user", "content": "start"},
			map[string]any{"role": "assistant", "content": "on it"},
			map[string]any{"role": "user", "content": "next step"},
		}}
		s.annotateOpenAIMessageMetadata(req, ctx, req["messages"].([]any), "gpt-5.4", tokens)
		tokens += 300000
		msgs := req["messages"].([]any)
		msg := msgs[len(msgs)-1].(map[string]any)
		text, _ := msg["content"].(string)
		return text
	}
	// First pass sets the interval baselines.
	last()
	body := last()

	for _, marker := range []string{"[rules]", "LOS56D-RULES", "LOS56D-DOCS", "[plan-checkpoint]"} {
		if !strings.Contains(body, marker) {
			t.Errorf("tail message lacks %q for a derived key:\n%s", marker, body)
		}
	}

	meta, ok := s.timestampStore.Get("derived-hash", 3)
	if !ok || meta == nil {
		t.Fatal("no stored entry")
	}
	if meta.Rules != "" || meta.DocsHint != "" || meta.PlanCheckpoint != "" {
		t.Errorf("session content still frozen under a derived key: %+v", meta)
	}
}

// ensure the helper stays honest about what it counts
func TestLos56dTimestampStoreCapKeepsNewest(t *testing.T) {
	tt := &threadTimestamps{Entries: map[int]*TimestampMeta{}}
	for i := 1; i <= 10; i++ {
		tt.Entries[i] = &TimestampMeta{}
	}
	if removed := capThreadEntries(tt, 4); removed != 6 {
		t.Fatalf("removed %d, want 6", removed)
	}
	for _, want := range []int{7, 8, 9, 10} {
		if _, ok := tt.Entries[want]; !ok {
			t.Errorf("msg:%d missing", want)
		}
	}
}

// Review round 3: the fail fingerprint must survive the noise a re-run adds
// (duration, build dir, test temp dir, panic pointer), or a failing build loop
// counts once per turn again.
func TestLos56dFailFingerprintIgnoresRerunNoise(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0), failFingerprints: make(map[string]map[int64]uint64)}

	first := "--- FAIL: TestFoo (0.01s)\nopen /tmp/TestFoo123/001/yesmem.db: permission denied"
	tempRerun := "--- FAIL: TestFoo (12.34ms)\nopen /tmp/TestFoo987/001/yesmem.db: permission denied"
	buildRerun := "--- FAIL: TestBar (0.01s)\nopen /tmp/go-build111/b001/x: permission denied"
	buildRerun2 := "--- FAIL: TestBar (0.01s)\nopen /tmp/go-build222/b001/x: permission denied"

	if got := s.freshFailIDs("tid", []int64{5}, first); len(got) != 1 {
		t.Fatalf("first failure not counted: %v", got)
	}
	if got := s.freshFailIDs("tid", []int64{5}, tempRerun); len(got) != 0 {
		t.Errorf("a re-run with new temp dir and duration counts again: %v", got)
	}
	if got := s.freshFailIDs("tid", []int64{5}, buildRerun); len(got) != 1 {
		t.Errorf("a new failure must count, got %v", got)
	}
	if got := s.freshFailIDs("tid", []int64{5}, buildRerun2); len(got) != 0 {
		t.Errorf("a re-run with a new build dir counts again: %v", got)
	}
	if got := s.freshFailIDs("tid", []int64{5}, "--- FAIL: TestBaz (0.01s)"); len(got) != 1 {
		t.Errorf("a different failure must count, got %v", got)
	}
}
