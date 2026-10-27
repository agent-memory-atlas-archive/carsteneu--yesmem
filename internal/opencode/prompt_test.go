package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// promptRecorder is a stub service that records the session.prompt request it
// receives and answers with a canned inbox entry.
type promptRecorder struct {
	mu         sync.Mutex
	method     string
	path       string
	requestURI string
	rawBody    []byte
	body       map[string]any
	calls      int
	status     int
	response   string
}

func newPromptServer(t *testing.T) (*httptest.Server, *promptRecorder) {
	t.Helper()
	rec := &promptRecorder{response: `{"data":{"id":"msg_created1","sessionID":"x"}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rec.mu.Lock()
		rec.calls++
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.requestURI = r.RequestURI
		rec.rawBody = raw
		rec.body = body
		status := rec.status
		resp := rec.response
		rec.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"_tag":"InvalidRequestError","message":"bad request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// snap returns the recorded request, so assertions never race the server.
func (p *promptRecorder) snap() (method, path string, body map[string]any, raw []byte, calls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.method, p.path, p.body, p.rawBody, p.calls
}

func TestPromptUserTurn_PostsTurnIntoTheSession(t *testing.T) {
	srv, rec := newPromptServer(t)

	id, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "hallo", PromptOptions{
		Delivery: DeliveryQueue,
		Resume:   true,
	})
	if err != nil {
		t.Fatalf("PromptUserTurn: %v", err)
	}
	if id != "msg_created1" {
		t.Errorf("returned message id = %q, want the created inbox entry", id)
	}
	method, path, body, _, calls := rec.snap()
	if calls != 1 {
		t.Fatalf("expected one request, got %d", calls)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if want := "/api/session/ses_abc/prompt"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if body["text"] != "hallo" {
		t.Errorf("text = %v, want hallo", body["text"])
	}
	if body["delivery"] != DeliveryQueue {
		t.Errorf("delivery = %v, want %s", body["delivery"], DeliveryQueue)
	}
	if body["resume"] != true {
		t.Errorf("resume = %v, want true", body["resume"])
	}
}

// The spec sets additionalProperties=false on the request body, so a field the
// service does not know is a rejected request rather than a no-op.
func TestPromptUserTurn_SendsOnlySpecifiedFields(t *testing.T) {
	srv, rec := newPromptServer(t)

	if _, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{}); err != nil {
		t.Fatalf("PromptUserTurn: %v", err)
	}
	_, _, body, _, _ := rec.snap()
	for key := range body {
		switch key {
		case "text", "delivery", "resume", "metadata":
		default:
			t.Errorf("body carries a field the spec does not define: %q", key)
		}
	}
	if _, present := body["delivery"]; present {
		t.Error("an unset delivery must be omitted, so the service default applies")
	}
	if _, present := body["metadata"]; present {
		t.Error("unset metadata must be omitted")
	}
	if _, present := body["resume"]; !present {
		t.Error("resume must be sent explicitly — false means 'leave the session asleep'")
	}
}

func TestPromptUserTurn_SendsMetadataVerbatim(t *testing.T) {
	srv, rec := newPromptServer(t)

	meta := map[string]any{"sender": "orchestrator", "message_id": float64(12), "msg_type": "command"}
	if _, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{Metadata: meta}); err != nil {
		t.Fatalf("PromptUserTurn: %v", err)
	}
	_, _, body, _, _ := rec.snap()
	got, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %v", body["metadata"])
	}
	if got["sender"] != "orchestrator" || got["msg_type"] != "command" {
		t.Errorf("metadata = %v, want sender and msg_type preserved", got)
	}
}

func TestPromptUserTurn_ErrorsOnNonOKStatus(t *testing.T) {
	srv, rec := newPromptServer(t)
	rec.mu.Lock()
	rec.status = http.StatusNotFound
	rec.mu.Unlock()

	_, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{})
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error must name the status, got %q", err.Error())
	}
}

// A caller deciding whether to hand a message back has only one thing to go on:
// a refusal proves the service did not take it, a dead connection or a deadline
// proves nothing.
// The boundary that decides whether a message is handed back: a refusal is a
// status decided before the work, everything else proves nothing.
func TestIsRefusalStatus_CoversOnlyDecisionsMadeBeforeActing(t *testing.T) {
	refusals := []int{300, 301, 307, 400, 401, 403, 404, 409, 422}
	for _, code := range refusals {
		if !isRefusalStatus(code) {
			t.Errorf("status %d is decided before the work and must count as a refusal", code)
		}
	}
	unknown := []int{200, 202, 408, 429, 500, 502, 503}
	for _, code := range unknown {
		if isRefusalStatus(code) {
			t.Errorf("status %d proves nothing about whether the turn was taken", code)
		}
	}
}

// A write is never redirected: net/http re-sends the Authorization header when
// only the port changes, and replays the body for 307/308. The credential and
// the message must not travel to a second listener.
func TestPromptUserTurn_DoesNotFollowARedirect(t *testing.T) {
	var reached int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"id":"msg_should_not_happen"}}`))
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		http.Redirect(w, r, second.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	_, err := testClient(first).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{})
	if err == nil {
		t.Fatal("a redirect must not be answered as a delivery")
	}
	if reached != 0 {
		t.Errorf("the message reached a second listener %d time(s)", reached)
	}
	if !Rejected(err) {
		t.Errorf("an unfollowed redirect means nothing was carried out, so it is a refusal: %v", err)
	}
}

func TestPromptUserTurn_RefusalIsDistinguishableFromATransportFailure(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		srv, rec := newPromptServer(t)
		rec.mu.Lock()
		rec.status = http.StatusConflict
		rec.mu.Unlock()

		_, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !Rejected(err) {
			t.Errorf("a service refusal must be recognisable as one: %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := testClient(srv).PromptUserTurn(ctx, "ses_abc", "x", PromptOptions{})
		if err == nil {
			t.Fatal("expected an error")
		}
		if Rejected(err) {
			t.Errorf("a deadline is not a refusal — the turn may have been taken: %v", err)
		}
	})
	// A 5xx is raised on the way to an answer, not in place of acting: the
	// service may have taken the turn and failed afterwards.
	t.Run("server failure", func(t *testing.T) {
		srv, rec := newPromptServer(t)
		rec.mu.Lock()
		rec.status = http.StatusInternalServerError
		rec.mu.Unlock()

		_, err := testClient(srv).PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{})
		if err == nil {
			t.Fatal("expected an error")
		}
		if Rejected(err) {
			t.Errorf("a 5xx must not be treated as a refusal: %v", err)
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("the status must still be reported, got %q", err.Error())
		}
	})
}

// The error text reaches the daemon log, so the credential must not be in it.
func TestPromptUserTurn_ErrorNeverContainsPassword(t *testing.T) {
	srv, rec := newPromptServer(t)
	rec.mu.Lock()
	rec.status = http.StatusInternalServerError
	rec.mu.Unlock()

	const secret = "sup3r-secret-password"
	c := NewClient(Service{ID: "svc-1", URL: srv.URL, Password: secret})
	_, err := c.PromptUserTurn(context.Background(), "ses_abc", "x", PromptOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text leaks the password: %q", err.Error())
	}
}

// Nothing to deliver is a programming error, not a request worth sending.
func TestPromptUserTurn_RejectsEmptyInput(t *testing.T) {
	srv, rec := newPromptServer(t)
	c := testClient(srv)

	if _, err := c.PromptUserTurn(context.Background(), "", "x", PromptOptions{}); err == nil {
		t.Error("expected an error for an empty session id")
	}
	if _, err := c.PromptUserTurn(context.Background(), "ses_abc", "", PromptOptions{}); err == nil {
		t.Error("expected an error for empty text")
	}
	if _, _, _, _, calls := rec.snap(); calls != 0 {
		t.Errorf("no request may be sent for invalid input, got %d", calls)
	}
}

func TestPromptUserTurn_HonoursContextDeadline(t *testing.T) {
	// The handler outlives the client's deadline on purpose: a request that
	// keeps the server busy is what makes a missing context timeout visible.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := testClient(srv).PromptUserTurn(ctx, "ses_abc", "x", PromptOptions{}); err == nil {
		t.Fatal("expected the cancelled context to fail the call")
	}
}

// A session id is a path segment, so it must be escaped rather than allowed to
// change which endpoint is called.
func TestPromptUserTurn_EscapesTheSessionID(t *testing.T) {
	srv, rec := newPromptServer(t)

	if _, err := testClient(srv).PromptUserTurn(context.Background(), "ses_a/b?x", "t", PromptOptions{}); err != nil {
		t.Fatalf("PromptUserTurn: %v", err)
	}
	rec.mu.Lock()
	uri := rec.requestURI
	rec.mu.Unlock()
	// The raw request line is what the service routes on; the decoded
	// r.URL.Path cannot show whether the id stayed inside its segment.
	if got, want := uri, "/api/session/ses_a%2Fb%3Fx/prompt"; got != want {
		t.Errorf("request URI = %q, want %q", got, want)
	}
}
