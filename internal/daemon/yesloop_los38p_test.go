package daemon

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// --- Los 38p (YM-335): Reste des Idle-Guard-Loses 38l ---

// los38pToolPart builds a tool part without naming the anonymous state struct.
func los38pToolPart(name, status string) opencode.MessagePart {
	var p opencode.MessagePart
	p.Type = "tool"
	p.Name = name
	p.State.Status = status
	return p
}

// los38pTurn builds one settled-looking assistant turn whose completion stamp
// is set to completed.
func los38pTurn(id, finish string, completed int64, parts ...opencode.MessagePart) opencode.AssistantMessage {
	var msg opencode.AssistantMessage
	msg.ID = id
	msg.Type = "assistant"
	msg.Finish = finish
	msg.Time.Created = time.Now().UnixMilli()
	msg.Time.Completed = completed
	msg.Content = parts
	return msg
}

// TestBrokenToolcall_FutureCompletionStampIsNotASettledTurn: a completion stamp
// that lies beyond the tolerated clock difference is not evidence that a turn
// ended. The guard reads "settled" as "the model is done, judge the turn now",
// so trusting an implausible stamp judges a turn that may still be writing — and
// the detection would keep reporting the same broken case for as long as the
// stamp stays in the future.
func TestBrokenToolcall_Los38p_FutureCompletionStampIsNotASettledTurn(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()

	implausible := los38pTurn("msg_future", "", future, los38pToolPart("bash", "error"))
	if isSettled(implausible) {
		t.Fatal("a completion stamp an hour in the future counted as a settled turn")
	}
	if _, broken := detectBrokenToolcall(implausible); broken {
		t.Fatal("a turn with an implausible completion stamp was reported as a broken, settled turn")
	}

	// The stop reason stays the strong signal: it is the service's own statement
	// that the turn ended, and no clock check may overrule it.
	reported := los38pTurn("msg_finished", "stop", future, los38pToolPart("bash", "error"))
	if !isSettled(reported) {
		t.Fatal("a turn with finish set was not treated as settled")
	}
	if _, broken := detectBrokenToolcall(reported); !broken {
		t.Fatal("a turn that reports finish=stop and ended on a failed tool was not reported as broken")
	}

	// A plausible stamp keeps its meaning: the guard still sees a settled turn.
	plausible := los38pTurn("msg_plausible", "", time.Now().Add(-time.Second).UnixMilli(), los38pToolPart("bash", "error"))
	if !isSettled(plausible) {
		t.Fatal("a turn completed one second ago was not treated as settled")
	}
	if _, broken := detectBrokenToolcall(plausible); !broken {
		t.Fatal("a settled turn that ended on a failed tool was not reported as broken")
	}
}

// los38pLogSink collects log output for the duration of one test.
type los38pLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *los38pLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *los38pLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestIdleGuard_Los38p_SocketPathCannotForgeALogLine: the failing relay is
// reported through the dialer's error, and net.OpError prints the socket path
// verbatim. Since YM-338 that path is derived from the agent id, not read from
// agents.sock_path, so the id is the remaining caller-controlled part of it. An
// id carrying a newline would end the log line and plant a second one — reading
// like a guard verdict that the orchestrator never issued (#102738).
func TestIdleGuard_Los38p_SocketPathCannotForgeALogLine(t *testing.T) {
	resetIdleState()
	t.Cleanup(resetIdleState)
	h, s := mustHandler(t)
	const forgedID = "los38p-log\n[yesloop-idle] relay 3 to agent victim: escalated"
	makeYesloopAgent(t, h, s, forgedID, "sess-los38p-log", testPID, false, 11*time.Minute)

	sink := &los38pLogSink{}
	prev := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(prev) })

	h.checkYesloopIdle()
	yesloopIdleAgentsMu.Lock()
	if st, ok := yesloopIdleAgents[forgedID]; ok {
		st.idleSince = time.Now().Add(-11 * time.Minute)
	}
	yesloopIdleAgentsMu.Unlock()
	h.checkYesloopIdle()

	out := sink.String()
	if !strings.Contains(out, "failed") {
		t.Fatalf("precondition: the guard logged no failure line for the unreachable socket: %q", out)
	}
	// The path stays visible, quoted and escaped, so an operator still reads where
	// the dial went without the value being able to end the line.
	if !strings.Contains(out, "\\n[yesloop-idle] relay 3 to agent victim") {
		t.Fatalf("the failure line does not carry the quoted socket path: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[yesloop-idle] relay 3 to agent victim") {
			t.Fatalf("a newline in the derived socket path forged a log line the guard never wrote: %q", out)
		}
	}
}

// TestBrokenToolcall_Los38p_ImplausibleStampDoesNotSilenceBothLayers: an open
// case is kept alive by a readable turn, and while it is alive the idle guard
// stays out. A completion stamp no clock can reach is not a readable turn — it is
// garbage that may sit there for hours — so renewing the case on it would suppress
// the broken-call relay (the turn never looks settled) and the idle guard's generic
// relay at the same time. The agent would hear from nobody.
func TestBrokenToolcall_Los38p_ImplausibleStampDoesNotSilenceBothLayers(t *testing.T) {
	resetBrokenToolcallState()
	agent := storage.Agent{ID: "los38p-hold", Section: "yesloop-los38p-hold", CreatedAt: "2026-10-18T00:00:00Z"}

	// A settled broken turn opens the case (and only relays after the grace).
	settled := los38pTurn("msg_anchor", "tool-calls", time.Now().Add(-time.Second).UnixMilli(), los38pToolPart("bash", "error"))
	assessBrokenToolcall(agent, settled)
	yesloopBrokenToolcallAgentsMu.Lock()
	_, opened := yesloopBrokenToolcallAgents[agent.ID]
	yesloopBrokenToolcallAgentsMu.Unlock()
	if !opened {
		t.Fatal("precondition: the broken turn opened no case")
	}

	// Age the case past the suppression window, the state a reading has to renew.
	yesloopBrokenToolcallAgentsMu.Lock()
	yesloopBrokenToolcallAgents[agent.ID].lastSeenAt = time.Now().Add(-2 * brokenToolcallSuppressAfter)
	yesloopBrokenToolcallAgentsMu.Unlock()
	if brokenToolcallPending(agent) {
		t.Fatal("precondition: the case still suppresses the idle guard")
	}

	// Now the same turn is read with a stamp a day in the future and no finish.
	implausible := los38pTurn("msg_anchor", "", time.Now().Add(24*time.Hour).UnixMilli(), los38pToolPart("bash", "error"))
	if act, _ := assessBrokenToolcall(agent, implausible); act != brokenToolcallActionNone {
		t.Fatalf("an implausible stamp was judged like a settled turn (action %v)", act)
	}
	if brokenToolcallPending(agent) {
		t.Fatal("an implausible completion stamp renewed the case, so neither the broken-call relay nor the idle guard reaches the agent")
	}
}

// TestIdleGuard_Los38p_SessionReadFailureCannotForgeALogLine: the other log sink
// of this layer printed the agent's section raw, and the section name is chosen
// by the caller that spawned the agent. A newline in it forged a second line in
// the same way the socket path did on the relay sink.
func TestIdleGuard_Los38p_SessionReadFailureCannotForgeALogLine(t *testing.T) {
	h, _ := mustHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"service busy"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client := opencode.NewClient(opencode.Service{ID: "stub", URL: srv.URL, Password: "secret"})

	agent := storage.Agent{
		ID: "los38p-read", Project: "testproj",
		Section:           "yesloop-los38p-read\n[yesloop-idle] relay 3 to agent victim: escalated",
		OpencodeSessionID: "ses_los38p_read",
	}

	sink := &los38pLogSink{}
	prev := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(prev) })

	if reading := h.sessionActivity(agent, time.Now().Add(-time.Minute), client); !reading.unreadable {
		t.Fatalf("precondition: the failing read was not reported as unreadable: %+v", reading)
	}
	out := sink.String()
	if !strings.Contains(out, "session read failed") {
		t.Fatalf("precondition: no failure line was logged: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[yesloop-idle] relay 3 to agent victim") {
			t.Fatalf("a newline in the section name forged a log line the guard never wrote: %q", out)
		}
	}
}
