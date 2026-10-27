package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnalyzeTurnRequestParsing(t *testing.T) {
	input := `{
		"session_id": "sess-abc",
		"project": "/tmp/proj",
		"injected_ids": [1, 2, 3],
		"messages": [{"role": "user", "content": "hello"}]
	}`
	var req AnalyzeTurnRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatal(err)
	}
	if req.SessionID != "sess-abc" {
		t.Errorf("session_id = %q, want sess-abc", req.SessionID)
	}
	if req.Project != "/tmp/proj" {
		t.Errorf("project = %q, want /tmp/proj", req.Project)
	}
	if len(req.InjectedIDs) != 3 {
		t.Errorf("injected_ids len = %d, want 3", len(req.InjectedIDs))
	}
	if req.InjectedIDs[0] != 1 || req.InjectedIDs[1] != 2 || req.InjectedIDs[2] != 3 {
		t.Errorf("injected_ids = %v, want [1 2 3]", req.InjectedIDs)
	}
	if len(req.Messages) != 1 {
		t.Errorf("messages len = %d, want 1", len(req.Messages))
	}
}

func TestAnalyzeTurnRequestParsingEmpty(t *testing.T) {
	input := `{"session_id": "s1", "project": "p", "messages": []}`
	var req AnalyzeTurnRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.InjectedIDs) != 0 {
		t.Errorf("injected_ids should be empty, got %v", req.InjectedIDs)
	}
}

func TestAnalyzeTurnCrossCheck(t *testing.T) {
	tests := []struct {
		name          string
		signalIDs     []int64
		injectedIDs   []int64
		wantUsedIDs   []int64
		wantNoiseIDs  []int64
	}{
		{
			name:         "all injected used",
			signalIDs:    []int64{1, 2, 3},
			injectedIDs:  []int64{1, 2, 3},
			wantUsedIDs:  []int64{1, 2, 3},
			wantNoiseIDs: []int64{},
		},
		{
			name:         "none used",
			signalIDs:    []int64{},
			injectedIDs:  []int64{1, 2, 3},
			wantUsedIDs:  []int64{},
			wantNoiseIDs: []int64{1, 2, 3},
		},
		{
			name:         "partial use",
			signalIDs:    []int64{1, 3},
			injectedIDs:  []int64{1, 2, 3},
			wantUsedIDs:  []int64{1, 3},
			wantNoiseIDs: []int64{2},
		},
		{
			name:         "signal mentions non-injected IDs — excluded from used",
			signalIDs:    []int64{1, 99},
			injectedIDs:  []int64{1, 2},
			wantUsedIDs:  []int64{1},
			wantNoiseIDs: []int64{2},
		},
		{
			name:         "empty injected",
			signalIDs:    []int64{5},
			injectedIDs:  []int64{},
			wantUsedIDs:  []int64{},
			wantNoiseIDs: []int64{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usedIDs, noiseIDs := crossCheckIDs(tc.signalIDs, tc.injectedIDs)
			if !int64SliceEqual(usedIDs, tc.wantUsedIDs) {
				t.Errorf("usedIDs = %v, want %v", usedIDs, tc.wantUsedIDs)
			}
			if !int64SliceEqual(noiseIDs, tc.wantNoiseIDs) {
				t.Errorf("noiseIDs = %v, want %v", noiseIDs, tc.wantNoiseIDs)
			}
		})
	}
}

// int64SliceEqual compares two int64 slices treating nil and empty as equal.
func int64SliceEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- endpoint guards: signals.enabled and the Scanned precondition ---

type analyzeCaptureHandler struct {
	mu     sync.Mutex
	calls  []string
	config string // value answered for get_config
}

func (h *analyzeCaptureHandler) Handle(req RPCRequest) RPCResponse {
	h.mu.Lock()
	h.calls = append(h.calls, req.Method)
	cfg := h.config
	h.mu.Unlock()
	if req.Method == "get_config" {
		return RPCResponse{Result: json.RawMessage(`{"key":"signals.enabled","value":"` + cfg + `"}`)}
	}
	return RPCResponse{Result: json.RawMessage(`{"ok":true}`)}
}

func (h *analyzeCaptureHandler) has(method string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.calls {
		if m == method {
			return true
		}
	}
	return false
}

func (h *analyzeCaptureHandler) waitFor(method string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if h.has(method) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// analyzeTurn performs the request against a server wired to the capture handler.
func analyzeTurn(cap *analyzeCaptureHandler, body string) *httptest.ResponseRecorder {
	s := New(cap, "127.0.0.1:0", testToken, newTestLogger())
	rec := httptest.NewRecorder()
	s.handleAnalyzeTurn(rec, httptest.NewRequest("POST", "/api/analyze-turn", strings.NewReader(body)))
	return rec
}

const analyzeTurnBody = `{"session_id":"s1","project":"/tmp/proj","injected_ids":[1,2],
	"messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"text","text":"Per [ID:1].\n<!-- [IDs: 1] -->"}]},{"role":"user","content":"next"}]}`

const analyzeTurnNoAnswerBody = `{"session_id":"s1","project":"/tmp/proj","injected_ids":[1,2],
	"messages":[{"role":"user","content":"first"}]}`

func TestAnalyzeTurn_ScannedAnswerBooksUse(t *testing.T) {
	cap := &analyzeCaptureHandler{config: "true"}
	analyzeTurn(cap, analyzeTurnBody)

	if !cap.waitFor("increment_use", 2*time.Second) {
		t.Errorf("increment_use not called; calls = %v", cap.calls)
	}
}

func TestAnalyzeTurn_NoAssistantAnswerBooksNoNoise(t *testing.T) {
	cap := &analyzeCaptureHandler{config: "true"}
	analyzeTurn(cap, analyzeTurnNoAnswerBody)

	if !cap.waitFor("get_config", 2*time.Second) {
		t.Fatal("get_config not called — signals.enabled was never read")
	}
	// Give the fire-and-forget goroutines time to land, then check nothing noisy ran.
	time.Sleep(100 * time.Millisecond)
	for _, method := range []string{"increment_noise", "increment_use"} {
		if cap.has(method) {
			t.Errorf("%s called for a turn without an assistant answer", method)
		}
	}
}

func TestAnalyzeTurn_SignalsDisabledWritesNothing(t *testing.T) {
	cap := &analyzeCaptureHandler{config: "false"}
	rec := analyzeTurn(cap, analyzeTurnBody)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "signals disabled") {
		t.Errorf("body = %s, want the skipped marker", rec.Body.String())
	}
	var out AnalyzeTurnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if out.Skipped == "" {
		t.Errorf("skipped marker not decodable from %s", rec.Body.String())
	}
	time.Sleep(100 * time.Millisecond)
	for _, method := range []string{"increment_use", "increment_noise", "track_gap", "flag_contradiction"} {
		if cap.has(method) {
			t.Errorf("%s called although signals are disabled", method)
		}
	}
}
