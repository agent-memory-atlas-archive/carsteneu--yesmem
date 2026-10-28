package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// Codex registers its mailbox on every request — its identity comes from the
// connection, not a header — and one interaction sends dozens of requests. Each
// registration made the daemon rewrite two proxy_state rows and the PID file
// (audit 16.10.2026, YM-328). A repeat of the same channel carries no new
// information, so it must not reach the daemon again.
func TestCodexRepeatRequestRegistersOnce(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)
	codexRegisteredChannel.Range(func(k, _ any) bool {
		codexRegisteredChannel.Delete(k)
		return true
	})
	t.Cleanup(func() {
		codexRegisteredChannel.Range(func(k, _ any) bool {
			codexRegisteredChannel.Delete(k)
			return true
		})
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-5.5",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	}))
	defer upstream.Close()

	var mu sync.Mutex
	var registers int
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method == "register_pid" {
			mu.Lock()
			registers++
			mu.Unlock()
		}
		return "null"
	})
	s.cfg.TargetURL = upstream.URL
	s.cfg.OpenAITargetURL = upstream.URL
	s.httpClient = http.DefaultClient
	s.timestampStore = NewTimestampStore()

	prevPeer := peerSessionPIDFn
	peerSessionPIDFn = func(string, string) int { return os.Getpid() }
	t.Cleanup(func() { peerSessionPIDFn = prevPeer })

	// The same request twice: same derived thread, same mailbox.
	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"los24c dedupe probe"}],"stream":false}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "codex-tui/0.153.4 (linux; x86_64)")
		s.handleOpenAICompletions(httptest.NewRecorder(), req)
	}

	mu.Lock()
	defer mu.Unlock()
	if registers != 1 {
		t.Errorf("register_pid calls = %d, want exactly one for two identical requests", registers)
	}
}

// The daemon's mapping is pid-based. A codex process that continues the same
// rollout file under a new pid therefore has to register again, or the daemon
// keeps the stale pid and the new process resolves to nothing.
func TestCodexRegistrationKey_SeparatesProcesses(t *testing.T) {
	a := codexRegistrationKey("/data", "thread-1", 100)
	if same := codexRegistrationKey("/data", "thread-1", 100); same != a {
		t.Errorf("key is not repeatable: %q != %q", same, a)
	}
	if other := codexRegistrationKey("/data", "thread-1", 101); other == a {
		t.Error("a new pid hits the same key — its registration would be skipped")
	}
	if other := codexRegistrationKey("/other", "thread-1", 100); other == a {
		t.Error("a different data dir hits the same key")
	}
	if other := codexRegistrationKey("/data", "thread-2", 100); other == a {
		t.Error("a different thread hits the same key")
	}
}
