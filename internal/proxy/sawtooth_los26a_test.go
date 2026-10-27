package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// Regression tests for Los 26a (YM-236, YM-237, YM-238, YM-129, YM-143).
// Helpers carry the los26a prefix so parallel loops in this package cannot
// collide on a name.

const los26aThread = "opencode:ses_los26a"

func los26aSawtoothServer(global int, modelThresholds map[string]int) *Server {
	s := newTestServerWithThresholds(global, modelThresholds)
	s.logger = log.New(discardWriter{}, "", 0)
	s.sawtoothTrigger = NewSawtoothTrigger(24*time.Hour, global, global*8/10)
	return s
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// YM-236: the frozen prefix must be judged on the payload we actually send
// (frozen + fresh + overhead), not on the raw history the client resends.
func TestLos26a_FrozenPrefixNotInvalidatedByRawHistory(t *testing.T) {
	s := los26aSawtoothServer(700000, nil)

	// Live 17.10.2026 09:47: raw history 717k, combined payload 30k.
	if s.frozenPrefixExpired(1, "yesmem", los26aThread, "claude-opus-4-6", 717000, 16000, 0, 14000) {
		t.Fatal("frozen prefix expired although the combined payload (30k) is far below the 700k threshold")
	}

	// Counter-check: when the combined payload itself crosses the threshold the
	// prefix must still be rebuilt.
	if !s.frozenPrefixExpired(2, "yesmem", los26aThread, "claude-opus-4-6", 30000, 600000, 100000, 14000) {
		t.Fatal("frozen prefix kept although the combined payload (714k) exceeds the 700k threshold")
	}
}

// YM-237: the collapse budget must not shrink because the previous cycle saved
// a lot of tokens. A thread whose estimates run 40 % low needs a smaller raw
// budget, not the 10k floor.
func TestLos26a_CollapseBudgetKeepsWorkingAfterHeavyCollapse(t *testing.T) {
	s := los26aSawtoothServer(700000, nil)
	// Measured: the API charged 1.4x of our estimate. The budget must correct
	// for that ratio instead of shrinking by the previous cycle's savings.
	s.sawtoothTrigger.estimateFactor[los26aThread] = 1.4

	got := s.collapseContentBudget(los26aThread, 400000, 14000)
	// 400000/1.4 - 14000 = 271714
	if got < 200000 || got > 300000 {
		t.Fatalf("contentBudget = %d, want ~271k (tokenFloor corrected by the measured factor); a budget at the floor would leave only a handful of messages", got)
	}
}

// YM-238: a thread whose responses ran 38 % above the estimate must be
// estimated with that ratio, otherwise the hard context limit is reached
// before the emergency brake fires.
func TestLos26a_EstimateCalibratedToActual(t *testing.T) {
	s := los26aSawtoothServer(700000, nil)
	s.sawtoothTrigger.estimateFactor[los26aThread] = 1.38

	msgs := buildLargeMessages(20)
	raw := s.countMessageTokens(msgs) + 14000
	want := int(float64(raw) * 1.38)

	got := s.estimateTotalTokens(los26aThread, msgs, 14000)
	// Tight band on purpose: applying the factor twice (1.38^2) must not pass.
	if got < want*95/100 || got > want*105/100 {
		t.Fatalf("estimateTotalTokens = %d, want %d (raw %d scaled exactly once by 1.38)", got, want, raw)
	}
}

// YM-238: the calibration also applies to the delta branch (last actual plus
// the new tail), which is the path every following request takes.
func TestLos26a_EstimateCalibratesDeltaBranch(t *testing.T) {
	s := los26aSawtoothServer(700000, nil)
	s.sawtoothTrigger.estimateFactor[los26aThread] = 1.4

	msgs := buildLargeMessages(20)
	s.sawtoothTrigger.UpdateAfterResponse(los26aThread, 500000, 10)

	delta := s.countMessageTokens(msgs[10:])
	want := 500000 + int(float64(delta)*1.4)
	if got := s.estimateTotalTokens(los26aThread, msgs, 14000); got != want {
		t.Fatalf("estimateTotalTokens = %d, want %d (500k actual + calibrated delta %d*1.4)", got, want, delta)
	}
}

// YM-238: a state blob written before this change has no estimate_factor and
// must load as uncalibrated instead of failing or producing a zero factor.
func TestLos26a_CalibrationOldStateLoads(t *testing.T) {
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	st.SetLoadFunc(func(key string) (string, bool) {
		return `{"tokens":100000,"msg_count":30}`, true
	})

	if got := st.GetLastTokens(los26aThread); got != 100000 {
		t.Fatalf("GetLastTokens = %d, want 100000 from the old state blob", got)
	}
	if got := st.estimateFactorFor(los26aThread); got != 1 {
		t.Fatalf("factor = %.3f after loading an old state blob, want 1.0", got)
	}
}

// YM-129: the stub trigger must use the model-specific threshold, not the
// global scalar. Between both values the full raw history would go upstream.
func TestLos26a_TriggerUsesModelThreshold(t *testing.T) {
	s := los26aSawtoothServer(200000, map[string]int{"opus": 120000})

	// 125k: above the opus threshold (120k), below the global one (200k),
	// below the emergency margin (130k).
	if got := s.shouldTriggerSawtooth(los26aThread, "claude-opus-4-6", 125000); got != TriggerTokens {
		t.Fatalf("shouldTriggerSawtooth(opus, 125k) = %q, want %q (model threshold 120k)", got, TriggerTokens)
	}

	// Counter-check: a model without an override keeps the global 200k.
	if got := s.shouldTriggerSawtooth(los26aThread, "mystery-model", 125000); got != TriggerNone {
		t.Fatalf("shouldTriggerSawtooth(mystery-model, 125k) = %q, want %q (global threshold 200k)", got, TriggerNone)
	}
}

// YM-143: an opencode subagent is identified by its parent-session header.
// The thinking heuristic never fires for opencode, so subagents ran through
// the full pipeline while stream tracking already classified them as children.
func TestLos26a_SubagentDetectedByParentHeader(t *testing.T) {
	msgs := buildLargeMessages(5)
	req := mainSessionReq() // carries thinking, so the body heuristic says "main session"

	// Exactly the expressions the two handlers use, so a revert of the header
	// lookup in proxy.go / openai_handler.go cannot pass the unit tests silently.
	r := rawHeaderRequest(map[string]string{"x-opencode-parent-session-id": "ses_parent"})
	if !isSubagentRequest(r.Header, msgs, req) {
		t.Fatal("handler expression with a parent-session header was not detected as subagent")
	}
	if isSubagentRequest(rawHeaderRequest(nil).Header, msgs, req) {
		t.Fatal("handler expression without a parent-session header was wrongly detected as subagent")
	}
	if ctx := los26aParityContext(r, req); !ctx.Subagent {
		t.Fatal("prepareOpenAIRequestContext with a parent-session header did not mark a subagent")
	}

	h := http.Header{}
	h.Set("X-Opencode-Parent-Session-Id", "ses_parent")
	if !isSubagentRequest(h, msgs, req) {
		t.Fatal("request with a parent-session header was not detected as subagent")
	}

	if isSubagentRequest(http.Header{}, msgs, req) {
		t.Fatal("request without a parent-session header was wrongly detected as subagent")
	}
}

// los26aParityContext mirrors the handler call: the parity context gets the
// request header map, so the same header set decides on both wires.
func los26aParityContext(r *http.Request, req map[string]any) openAIRequestContext {
	s := los26aSawtoothServer(200000, nil)
	return s.prepareOpenAIRequestContext(req, 1, "", "ses_child", "opencode", r.Header)
}

// YM-237/YM-238: the ratio must be derived from the real API answer compared
// with the FINAL estimate of the payload that was sent, and each sample may be
// used exactly once.
func TestLos26a_CalibrationDerivedFromApiResponse(t *testing.T) {
	st := NewSawtoothTrigger(24*time.Hour, 700000, 400000)

	st.SetSentEstimate(los26aThread, 700000)
	st.UpdateAfterResponse(los26aThread, 965000, 40) // live: estimate 704k, actual 974k

	if got := st.applyEstimateFactor(los26aThread, 100000); got < 137000 || got > 139000 {
		t.Fatalf("applyEstimateFactor(100000) = %d, want about 138000 (965000/700000)", got)
	}

	// A response without a pending estimate must not move the factor.
	st.UpdateAfterResponse(los26aThread, 100000, 41)
	if got := st.estimateFactorFor(los26aThread); got < 1.37 || got > 1.39 {
		t.Fatalf("factor moved to %.3f although no sent estimate was pending", got)
	}

	// Out-of-band samples are not measurements of our counting error and must be
	// rejected instead of clamped: the factor stays where it was.
	st.SetSentEstimate(los26aThread, 1000)
	st.UpdateAfterResponse(los26aThread, 5000000, 42)
	if got := st.estimateFactorFor(los26aThread); got < 1.37 || got > 1.39 {
		t.Fatalf("factor = %.3f after an out-of-band sample, want the unchanged 1.38", got)
	}

	// A sample left over from a request whose response never reported usage must
	// not be consumed by the next response.
	st.SetSentEstimate(los26aThread, 700000)
	st.ClearSentEstimate(los26aThread)
	st.UpdateAfterResponse(los26aThread, 100000, 43)
	if got := st.estimateFactorFor(los26aThread); got < 1.37 || got > 1.39 {
		t.Fatalf("factor = %.3f after a stale sample was consumed, want the unchanged 1.38", got)
	}
}

// YM-238: the measured ratio must survive a proxy restart, otherwise the first
// requests after every deploy estimate with 1.0 again.
func TestLos26a_CalibrationPersists(t *testing.T) {
	var saved string
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	st.SetPersistFunc(func(key, value string) { saved = value })
	st.SetLoadFunc(func(key string) (string, bool) { return saved, saved != "" })

	st.SetSentEstimate(los26aThread, 700000)
	st.UpdateAfterResponse(los26aThread, 965000, 40)
	if saved == "" {
		t.Fatal("no state persisted after the response")
	}

	cold := NewSawtoothTrigger(time.Hour, 700000, 400000)
	cold.SetLoadFunc(func(key string) (string, bool) { return saved, true })
	_ = cold.GetLastTokens(los26aThread) // triggers the lazy load
	if got := cold.estimateFactorFor(los26aThread); got < 1.37 || got > 1.39 {
		t.Fatalf("factor after cold start = %.3f, want about 1.38 (persisted value)", got)
	}
}

// YM-143 (wiring): the handler must pass the request headers into the
// subagent decision. This is the only test that exercises handleMessages, so a
// revert of `isSubagentRequest(r.Header, ...)` to `isSubagent(...)` fails here.
func TestLos26a_HandlerPassesParentHeaderToSubagentCheck(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parent     string
		wantSame   bool
		wantStatus int
	}{
		{name: "child-session-passthrough", parent: "ses_parent", wantSame: true, wantStatus: http.StatusOK},
		{name: "main-session-pipeline", parent: "", wantSame: false, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received []byte
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
			}))
			defer backend.Close()

			s := los26aHandlerServer(t, backend.URL)

			body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},"messages":[` +
				`{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.Header.Set("X-Claude-Code-Session-Id", "ses_los26a_main")
			req.Header.Set("x-opencode-session-id", "ses_los26a_main")
			if tc.parent != "" {
				req.Header.Set("X-Opencode-Parent-Session-Id", tc.parent)
			}
			rec := httptest.NewRecorder()

			s.handleMessages(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if received == nil {
				t.Fatal("request never reached the upstream")
			}
			same := string(received) == body
			if same != tc.wantSame {
				t.Fatalf("upstream got the body unchanged = %v, want %v; the handler wiring for the parent header is broken", same, tc.wantSame)
			}
		})
	}
}

// los26aHandlerServer builds the minimum server state handleMessages needs.
// The cache status writer and the TTL detector get a temp dir: they must never
// write into the package directory. DataDir stays empty so the daemon lookups
// fail fast instead of dialing a socket in a temp dir.
func los26aHandlerServer(t *testing.T, targetURL string) *Server {
	t.Helper()
	dataDir := t.TempDir()
	// The writer owns a 1s ticker; leaving it running outlives the test and
	// allocates inside every later AllocsPerRun window (YM-396).
	cacheStatusWriter := NewCacheStatusWriter(dataDir, "", 700000, 0)
	t.Cleanup(cacheStatusWriter.Stop)
	s := &Server{
		cfg: Config{
			TargetURL:      targetURL,
			TokenThreshold: 700000,
			FeatureDefaults: &config.FeatureGates{
				Timestamps: true,
			},
		},
		httpClient:          &http.Client{Timeout: 5 * time.Second},
		logger:              log.New(discardWriter{}, "", 0),
		annotations:         make(map[string]string),
		narratives:          make(map[string]*Narrative),
		responseTimes:       make(map[string]time.Time),
		lastInjectedIDs:     make(map[string]map[int64]string),
		sessionInjectCounts: make(map[string]map[int64]int),
		lastTurnInjected:    make(map[string]map[int64]bool),
		loopStates:          make(map[string]*LoopState),
		timestampStore:      NewTimestampStore(),
		cacheStatusWriter:   cacheStatusWriter,
		frozenStubs:         NewFrozenStubsWithTTL(time.Hour),
		eagerStubMemory:     NewEagerStubMemory(),
		capsCache:           NewCapsCache(),
		sawtoothTrigger:     NewSawtoothTrigger(time.Hour, 700000, 400000),
		cacheGate:           NewCacheGate(time.Hour),
		skillTracker:        newSkillHintTracker(),
		cacheTTLDetector:    NewCacheTTLDetectorWithPersist(dataDir),
		msgCounters:         newMsgCounters(),
		thinkCounters:       make(map[string]int),
		routingWarned:       make(map[string]bool),
		rewriteMissLog:      make(map[string]time.Time),
		channelInjectCount:  make(map[string]int),
		forkState:           NewForkState(0, 60000, 3, 1),
	}
	return s
}

func TestLos26a_ParitySubagentDetectedByParentHeader(t *testing.T) {
	s := los26aSawtoothServer(200000, nil)
	req := map[string]any{"model": "glm-5.2", "messages": buildLargeMessages(5)}

	parentH := http.Header{}
	parentH.Set("X-Opencode-Parent-Session-Id", "ses_parent")
	ctx := s.prepareOpenAIRequestContext(req, 1, "", "ses_child", "opencode", parentH)
	if !ctx.Subagent {
		t.Fatal("parity context did not mark a request with a parent-session header as subagent")
	}

	main := s.prepareOpenAIRequestContext(req, 2, "", "ses_main", "opencode", nil)
	if main.Subagent {
		t.Fatal("parity context marked a main session as subagent")
	}
}
