package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// stubRPCLOS11i records which daemon methods reached the handler.
type stubRPCLOS11i struct {
	calls []string
}

func (s *stubRPCLOS11i) Handle(req RPCRequest) RPCResponse {
	s.calls = append(s.calls, req.Method)
	return RPCResponse{Result: json.RawMessage(`{"ok":true}`)}
}

// TestLos11iRPCEndpointOnlyPassesListedMethods is the regression test for
// YM-345 (3): POST /api/rpc/{method} forwarded any method the caller named, so
// the HTTP surface was as wide as the daemon's whole method table. It must be
// an explicit allowlist instead, failing closed for everything else.
func TestLos11iRPCEndpointOnlyPassesListedMethods(t *testing.T) {
	cases := []struct {
		method     string
		wantStatus int
		wantCalled bool
		why        string
	}{
		{"scratchpad_read", http.StatusOK, true, "scratchpad access is the documented HTTP use"},
		{"hybrid_search", http.StatusOK, true, "memory reads are what an external agent needs"},
		{"execute_cap", http.StatusForbidden, false, "executing a cap runs code on the host"},
		{"remove_docs", http.StatusForbidden, false, "deleting indexed documents is not an external-agent operation"},
		{"remember", http.StatusForbidden, false, "the trust level and force_supersede come from the request, so this write stays on the local socket"},
		{"scratchpad_delete", http.StatusForbidden, false, "deleting a project's scratchpad and assets is irreversible"},
		{"set_config", http.StatusForbidden, false, "runtime configuration belongs to the local socket"},
		{"send_to", http.StatusForbidden, false, "steering another agent is not an external-agent operation"},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			stub := &stubRPCLOS11i{}
			srv := New(stub, "127.0.0.1:0", "test-token", log.New(os.Stderr, "", 0))

			req := httptest.NewRequest("POST", "/api/rpc/"+tc.method, nil)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Authorization", "Bearer test-token")
			w := httptest.NewRecorder()
			srv.httpSrv.Handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("%s: status = %d, want %d (%s)", tc.method, w.Code, tc.wantStatus, tc.why)
			}
			called := len(stub.calls) > 0
			if called != tc.wantCalled {
				t.Errorf("%s: daemon handler called = %v, want %v (calls=%v)", tc.method, called, tc.wantCalled, stub.calls)
			}
		})
	}
}

// TestLos11iRPCEndpointChecksAuthBeforeTheAllowlist pins the ordering: an
// unauthenticated caller must get 401 even for a denied method, otherwise the
// 403 tells it whether a name is listed before it proves it holds the token.
func TestLos11iRPCEndpointChecksAuthBeforeTheAllowlist(t *testing.T) {
	stub := &stubRPCLOS11i{}
	srv := New(stub, "127.0.0.1:0", "test-token", log.New(os.Stderr, "", 0))

	req := httptest.NewRequest("POST", "/api/rpc/execute_cap", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (auth runs before the allowlist)", w.Code)
	}
	if len(stub.calls) != 0 {
		t.Errorf("daemon handler called without a token: %v", stub.calls)
	}
}
