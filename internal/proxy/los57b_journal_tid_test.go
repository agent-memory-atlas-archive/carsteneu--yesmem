package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// los57bForged is the payload a client plants in its thread id. It is a line of
// its own if the journal prints the id unchanged, and proxy_health counts journal
// lines: a forged line is a forged health signal. Six bytes, so the marker also
// survives the 8-byte preview (sid=%.8s) that some journal lines print.
const los57bForged = "FORGED"

// los57bForgedLines returns every journal line that STARTS with the marker. The
// marker itself may stay on the request line (the id is still readable there),
// but it must never begin a record of its own.
func los57bForgedLines(journal string) []string {
	var out []string
	for _, line := range strings.Split(journal, "\n") {
		if strings.HasPrefix(line, los57bForged) {
			out = append(out, line)
		}
	}
	return out
}

// YM-400 (PRX-P52): the keepalive journal prints the thread id raw. The id comes
// from a client header, and isRealUserSession accepts anything with the
// "opencode:ses_" prefix, so a newline in the header forges a second journal
// line that the proxy_health cap counts as its own signal.
func TestLos57bKeepaliveJournalTIDCannotForgeALine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":100}}`)
	}))
	defer srv.Close()

	logBuf := &lockedLogBuffer{}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 2, IntervalOverride: 10 * time.Millisecond,
		Logger: log.New(logBuf, "", 0),
	})
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
	ka.Reset("opencode:ses_los57b\n"+los57bForged+" forged keepalive line", body, "test-key", "")
	logBuf.waitFor(t, "[keepalive] ping", 2*time.Second)
	ka.Stop()

	journal := logBuf.String()
	if !strings.Contains(journal, "[keepalive] ping") {
		t.Fatalf("no keepalive ping line in the journal, got: %q", journal)
	}
	if forged := los57bForgedLines(journal); len(forged) > 0 {
		t.Errorf("thread id forged %d journal line(s): %q\njournal:\n%s", len(forged), forged, journal)
	}
}

// YM-400: same class on the request path, carried the way a client can actually
// send it: metadata.user_id is a JSON body field, so nothing validates its
// content. The session id inside it becomes the thread id and reaches every
// "tid=" journal line of the request.
func TestLos57bRequestJournalTIDCannotForgeALine(t *testing.T) {
	// Two id lengths on purpose: some journal lines print the whole id, others a
	// short preview (sid=%.8s in checkDialogMessages). A marker in the first eight
	// bytes covers the preview lines, the long form covers the rest.
	for name, session := range map[string]string{
		"long id":  "opencode:ses_los57b\n" + los57bForged + " forged request line",
		"short id": "a\n" + los57bForged + " forged preview line",
	} {
		t.Run(name, func(t *testing.T) { los57bRequestForge(t, session) })
	}
}

func los57bRequestForge(t *testing.T, session string) {
	t.Helper()
	backendURL, _ := recordingUpstream(t)
	s := los26aHandlerServer(t, backendURL)
	wireFakeDaemon(t, s, func(method string, params map[string]any) string { return "null" })
	logBuf := &lockedLogBuffer{}
	s.logger = log.New(logBuf, "", 0)

	// Claude Code sends user_id as a JSON *string* that itself holds JSON, so the
	// session id reaches the proxy through two levels of encoding.
	inner, err := json.Marshal(map[string]string{"session_id": session})
	if err != nil {
		t.Fatalf("marshal session id: %v", err)
	}
	userID, err := json.Marshal(string(inner))
	if err != nil {
		t.Fatalf("marshal user_id: %v", err)
	}
	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":"on it"},` +
		`{"role":"user","content":"check the prompt profile routing"}],` +
		`"metadata":{"user_id":` + string(userID) + `}}`
	req := messagesRequest(body, nil)
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	journal := logBuf.String()
	if !strings.Contains(journal, "tid=") {
		t.Fatalf("no thread log line in the journal, got: %q", journal)
	}
	if forged := los57bForgedLines(journal); len(forged) > 0 {
		t.Errorf("thread id forged %d journal line(s): %q\njournal:\n%s", len(forged), forged, journal)
	}
}
