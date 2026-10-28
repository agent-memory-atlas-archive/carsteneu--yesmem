package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// --- YM-309 (Los 38l): four leftovers the Los-38k guard left behind ---
//
//  1. the docs quote the six-phase question as the unconditional first relay,
//     although the daemon picks the wording per delivery (see
//     yesloop_idle_relay_docs_los38l_test.go);
//  2. workingSince survives the re-arm paths, so a fresh working turn is judged
//     against an aged window and the guard escalates a working agent;
//  3. a completion stamp in the future reports work on every read;
//  4. the unreadable-session hold also runs for an agent that can never receive
//     the relay it protects (no sock_path);
// plus the live finding of 18.10.: a background shell job the agent is waiting
// for looks like a standstill.

// los38lSessionStub serves the opencode v2 message endpoint with the two shapes
// this ticket adds: a settled turn whose bash tool still runs, and a settled
// turn whose bash tool moved the command into a background job.
type los38lSessionStub struct {
	mu       sync.Mutex
	fail     bool
	toolFrom time.Time
	jobFrom  time.Time
}

func (s *los38lSessionStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/message") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		fail, toolFrom, jobFrom := s.fail, s.toolFrom, s.jobFrom
		s.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"service busy"}`, http.StatusInternalServerError)
			return
		}
		var msg map[string]any
		switch {
		case !toolFrom.IsZero():
			msg = map[string]any{
				"id": "msg_tool", "type": "assistant", "finish": "tool-calls",
				"content": []any{map[string]any{
					"type": "tool", "name": "bash",
					"state": map[string]any{"status": "running"},
				}},
				"time": map[string]any{
					"created":   toolFrom.UnixMilli(),
					"completed": toolFrom.Add(2 * time.Second).UnixMilli(),
				},
			}
		case !jobFrom.IsZero():
			msg = map[string]any{
				"id": "msg_job", "type": "assistant", "finish": "tool-calls",
				"content": []any{map[string]any{
					"type": "tool", "name": "bash",
					"state": map[string]any{
						"status":   "completed",
						"metadata": map[string]any{"status": "running", "shellID": "sh_1"},
					},
				}},
				"time": map[string]any{
					"created":   jobFrom.UnixMilli(),
					"completed": jobFrom.Add(2 * time.Second).UnixMilli(),
				},
			}
		default:
			now := time.Now().UnixMilli()
			msg = map[string]any{
				"id": "msg_quiet", "type": "assistant", "finish": "stop",
				"time": map[string]any{"created": now - 3000, "completed": now - 2000},
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{msg}})
	})
	return mux
}

func newLos38lSessionStub(t *testing.T) *los38lSessionStub {
	t.Helper()
	stub := &los38lSessionStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	stubV2Service(t, srv)
	return stub
}

func (s *los38lSessionStub) startRunningTool(started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolFrom = started
}

func (s *los38lSessionStub) startBackgroundJob(started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobFrom = started
}

func (s *los38lSessionStub) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

// idleStateSnapshot copies the fields the assertions look at.
func idleStateSnapshot(id string) (state int, workingSince, workingTurnStart, backgroundSince time.Time, ok bool) {
	yesloopIdleAgentsMu.Lock()
	defer yesloopIdleAgentsMu.Unlock()
	st, ok := yesloopIdleAgents[id]
	if !ok {
		return 0, time.Time{}, time.Time{}, time.Time{}, false
	}
	return st.state, st.workingSince, st.workingTurnStart, st.backgroundSince, true
}

// The DONE re-arm starts a new supervision episode. A workingSince from the old
// one is 20 minutes old in the worst case and the very next working turn of the
// agent is then judged as an aborted one: the guard stops seeing work and
// escalates a working agent (YM-309).
func TestLos38l_DoneReArmClearsTheWorkingWindow(t *testing.T) {
	resetIdleState()
	h, s := mustHandler(t)
	const id = "los38l-rearm-done"

	makeYesloopAgent(t, h, s, id, "sess-"+id, testPID, true, 0)

	yesloopIdleAgentsMu.Lock()
	yesloopIdleAgents[id] = &yesloopIdleState{
		state:            yesloopIdleStateDone,
		workingSince:     time.Now().Add(-(yesloopIdleWorkingMax + 5*time.Minute)),
		workingTurnStart: time.Now().Add(-(yesloopIdleWorkingMax + 6*time.Minute)),
	}
	yesloopIdleAgentsMu.Unlock()

	h.checkYesloopIdle()

	state, workingSince, workingTurnStart, backgroundSince, ok := idleStateSnapshot(id)
	if !ok {
		t.Fatal("no idle state after the re-arm tick")
	}
	if state != yesloopIdleStateWorking {
		t.Errorf("state = %s, want WORKING after stream activity", stateNames[state])
	}
	if !workingSince.IsZero() {
		t.Errorf("workingSince = %v, want zero: the re-arm starts a new window", workingSince)
	}
	if !workingTurnStart.IsZero() {
		t.Errorf("workingTurnStart = %v, want zero: the re-arm starts a new window", workingTurnStart)
	}
	if !backgroundSince.IsZero() {
		t.Errorf("backgroundSince = %v, want zero: the job credit belongs to the old episode", backgroundSince)
	}
}

// The same re-arm at the end of a stand-down: the budget window passed, the
// guard takes the agent back, and the old episode's working window must not
// survive into the new one (YM-309).
func TestLos38l_StandDownEndClearsTheWorkingWindow(t *testing.T) {
	resetIdleState()
	h, s := mustHandler(t)
	const id = "los38l-rearm-standdown"

	makeYesloopAgent(t, h, s, id, "sess-"+id, testPID, false, 11*time.Minute)

	yesloopIdleAgentsMu.Lock()
	yesloopIdleAgents[id] = &yesloopIdleState{
		state:            yesloopIdleStateDone,
		stoodDown:        true,
		idleSince:        time.Now().Add(-11 * time.Minute),
		workingSince:     time.Now().Add(-(yesloopIdleWorkingMax + 5*time.Minute)),
		workingTurnStart: time.Now().Add(-(yesloopIdleWorkingMax + 6*time.Minute)),
	}
	yesloopIdleRelays[id] = &idleRelayBudget{
		count: yesloopIdleMaxRelays,
		since: time.Now().Add(-(yesloopIdleRelayBudgetWindow + time.Minute)),
	}
	yesloopIdleAgentsMu.Unlock()

	h.checkYesloopIdle()

	state, workingSince, workingTurnStart, backgroundSince, ok := idleStateSnapshot(id)
	if !ok {
		t.Fatal("no idle state after the stand-down tick")
	}
	if state != yesloopIdleStateWorking {
		t.Errorf("state = %s, want WORKING after the budget window passed", stateNames[state])
	}
	if !workingSince.IsZero() {
		t.Errorf("workingSince = %v, want zero: the stand-down end starts a new window", workingSince)
	}
	if !workingTurnStart.IsZero() {
		t.Errorf("workingTurnStart = %v, want zero: the stand-down end starts a new window", workingTurnStart)
	}
	if !backgroundSince.IsZero() {
		t.Errorf("backgroundSince = %v, want zero: the job credit belongs to the old episode", backgroundSince)
	}
}

// A new working turn gets its own window even when the guard never saw a settled
// reading in between: consecutive working turns otherwise share one 20-minute
// budget and the second one is judged as an aborted turn (YM-309).
func TestLos38l_NewWorkingTurnGetsItsOwnWindow(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-turn-window"

	driveBoundYesloopIntoProgression(t, h, s, id)
	giveIdleAgentASocket(t, h, s, id)

	// The guard's window still hangs on a turn from the previous episode.
	yesloopIdleAgentsMu.Lock()
	st := yesloopIdleAgents[id]
	st.workingSince = time.Now().Add(-(yesloopIdleWorkingMax + time.Minute))
	st.workingTurnStart = time.Now().Add(-(yesloopIdleWorkingMax + 2*time.Minute))
	yesloopIdleAgentsMu.Unlock()

	// The session reports a turn that started two minutes ago and whose tool runs.
	stub.startRunningTool(time.Now().Add(-2 * time.Minute))

	expireIdleRelay(id)
	h.checkYesloopIdle()

	if got := getIdleStateRefireCount(id); got != 0 {
		t.Errorf("refireCount = %d while a two-minute-old tool runs, want 0", got)
	}
}

// An agent without an inject socket can never receive the relay whose silence
// the unreadable-session hold protects. Holding anyway leaves it unsupervised
// for up to yesloopIdleWorkingMax without a single delivery (YM-309).
func TestLos38l_UnreadableHoldNeedsADeliverableRelay(t *testing.T) {
	unreadable := idleSessionReading{unreadable: true}

	h, _ := mustHandler(t)
	// No derivable path: nothing can be delivered, so the hold protects nothing.
	h.dataDir = ""
	withoutSocket := storage.Agent{ID: "los38l-nosock", Section: "yesloop-los38l-nosock"}
	if h.withholdRefireOnUnreadableSession(withoutSocket, &yesloopIdleState{}, unreadable) {
		t.Error("no derivable socket path: the hold protects nothing, the guard must escalate instead")
	}

	h.dataDir = t.TempDir()
	withSocket := storage.Agent{ID: "los38l-sock", Section: "yesloop-los38l-sock"}
	if !h.withholdRefireOnUnreadableSession(withSocket, &yesloopIdleState{}, unreadable) {
		t.Error("with a derivable path the first unreadable read must open the hold window")
	}
}

// The tick-level effect of the same rule: an agent with no derivable socket does
// not get a 20-minute grace period for a relay that never left.
func TestLos38l_NoSocketNoUnreadableGrace(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-nosock-tick"

	driveBoundYesloopIntoProgression(t, h, s, id)
	// The column is irrelevant since YM-338; an underivable path is what makes a
	// relay impossible.
	h.dataDir = ""
	stub.setFail(true)

	expireIdleRelay(id)
	h.checkYesloopIdle()

	if got := getIdleStateRefireCount(id); got != 1 {
		t.Errorf("refireCount = %d after an unreadable read without a socket, want 1", got)
	}
}

// The live case of 18.10. (agents agent-20261018-24 and agent-20261018-30): the
// agent ended its turn to wait for the notification of a background shell job.
// The session shows a settled turn, so the open-turn and running-tool checks see
// nothing — the job itself is the work (YM-309).
func TestLos38l_BackgroundJobIsWork(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-background"

	driveBoundYesloopIntoProgression(t, h, s, id)
	giveIdleAgentASocket(t, h, s, id)

	stub.startBackgroundJob(time.Now().Add(-3 * time.Minute))

	for i := 0; i < 3; i++ {
		expireIdleRelay(id)
		h.checkYesloopIdle()
	}

	if got := getIdleStateRefireCount(id); got != 0 {
		t.Errorf("refireCount = %d while a background job runs, want 0", got)
	}
	row, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.Status != "running" {
		t.Errorf("an agent waiting for its background job must not be paused, status = %q (progress=%q)",
			row.Status, row.Progress)
	}
}

// A job is not held to the 20-minute bound of an open turn: measured on this
// install, 15 percent of the real jobs run longer than ten minutes and 3 percent
// longer than twenty. A job the agent is waiting for is work for as long as
// yesloopIdleBackgroundMax (5.3 finding 1).
func TestLos38l_BackgroundJobOutlivesTheTurnBound(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-background-long"

	driveBoundYesloopIntoProgression(t, h, s, id)
	giveIdleAgentASocket(t, h, s, id)

	stub.startBackgroundJob(time.Now().Add(-(yesloopIdleWorkingMax + 5*time.Minute)))

	for i := 0; i < 3; i++ {
		expireIdleRelay(id)
		h.checkYesloopIdle()
	}

	if got := getIdleStateRefireCount(id); got != 0 {
		t.Errorf("refireCount = %d for a 25-minute-old job, want 0 (a job gets its own bound)", got)
	}
}

// The other direction: the job's metadata keeps saying "running" after it
// finished, so an old job must not hold the guard off forever. The bound for a
// job is its own, and the age is read from the turn's own start.
func TestLos38l_OldBackgroundJobDoesNotHoldTheGuard(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-background-old"

	driveBoundYesloopIntoProgression(t, h, s, id)
	giveIdleAgentASocket(t, h, s, id)

	stub.startBackgroundJob(time.Now().Add(-(yesloopIdleBackgroundMax + 5*time.Minute)))

	expireIdleRelay(id)
	h.checkYesloopIdle()

	if got := getIdleStateRefireCount(id); got != 1 {
		t.Errorf("refireCount = %d for a job older than the background bound, want 1", got)
	}
}

// A relay that cannot be dialled must still keep the 90-second cadence: without
// the attempt stamp the interval never gates, every tick burns a refire slot and
// an agent without a socket is paused about 90 seconds after entering the rung
// (5.3 finding 2).
func TestLos38l_UndeliverableRelayKeepsTheRefireCadence(t *testing.T) {
	resetIdleState()
	newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-cadence"

	driveBoundYesloopIntoProgression(t, h, s, id)
	// The derived socket path has no listener here, so the dial fails — that is
	// the undeliverable relay this test is about (YM-338: the column is no gate).

	expireIdleRelay(id)
	h.checkYesloopIdle()
	if got := getIdleStateRefireCount(id); got != 1 {
		t.Fatalf("refireCount = %d after the first undeliverable relay, want 1", got)
	}

	// The next heartbeat tick is 30 seconds later, well inside the 90-second
	// refire interval.
	h.checkYesloopIdle()
	if got := getIdleStateRefireCount(id); got != 1 {
		t.Errorf("refireCount = %d one tick later, want 1 (the refire interval counts undelivered attempts too)", got)
	}

	expireIdleRelay(id)
	h.checkYesloopIdle()
	if got := getIdleStateRefireCount(id); got != 2 {
		t.Errorf("refireCount = %d after the interval passed, want 2", got)
	}
}

// The suppression of the generic idle relay for an agent whose turn died on a
// tool call is a re-arm point too: the guard resets the idle window there so the
// protocol starts fresh when the suppression lifts. The working window belongs
// to the same episode and has to go with it (5.2 finding 2).
func TestLos38l_BrokenToolcallResetClearsTheWorkingWindow(t *testing.T) {
	resetIdleState()
	h, s := mustHandler(t)
	const id = "los38l-broken-reset"

	makeYesloopAgent(t, h, s, id, "sess-"+id, testPID, false, 11*time.Minute)
	row, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}

	yesloopBrokenToolcallAgentsMu.Lock()
	yesloopBrokenToolcallAgents[id] = &brokenToolcallState{
		state:      brokenToolcallStatePending,
		lastSeenAt: time.Now(),
		createdAt:  row.CreatedAt,
	}
	yesloopBrokenToolcallAgentsMu.Unlock()

	yesloopIdleAgentsMu.Lock()
	yesloopIdleAgents[id] = &yesloopIdleState{
		state:            yesloopIdleStateWorking,
		idleSince:        time.Now().Add(-11 * time.Minute),
		workingSince:     time.Now().Add(-(yesloopIdleWorkingMax + 5*time.Minute)),
		workingTurnStart: time.Now().Add(-(yesloopIdleWorkingMax + 6*time.Minute)),
	}
	yesloopIdleAgentsMu.Unlock()

	h.checkYesloopIdle()

	_, workingSince, workingTurnStart, _, ok := idleStateSnapshot(id)
	if !ok {
		t.Fatal("no idle state after the suppressed tick")
	}
	if !workingSince.IsZero() || !workingTurnStart.IsZero() {
		t.Errorf("workingSince = %v, workingTurnStart = %v, want both zero: the suppression reset starts a new window",
			workingSince, workingTurnStart)
	}
}

// A turn that reports no start stamp at all is a turn change as well: without
// re-anchoring, it inherits the rest of the previous turn's budget and is
// dropped as aborted while it runs (5.2 finding 1). Two consecutive turns
// without any stamp stay indistinguishable and keep their window.
func TestLos38l_KnownToStamplessTurnReanchorsTheWindow(t *testing.T) {
	reanchored := &yesloopIdleState{
		workingSince:     time.Now().Add(-19 * time.Minute),
		workingTurnStart: time.Now().Add(-(yesloopIdleWorkingMax + 4*time.Minute)),
	}
	if !workingTurnCounts(reanchored, time.Time{}, yesloopIdleWorkingMax) {
		t.Fatal("a stampless turn must count as work: it just started from the guard's point of view")
	}
	if age := time.Since(reanchored.workingSince); age > time.Minute {
		t.Errorf("window age = %v, want a fresh window after the turn change", age)
	}

	unchanged := &yesloopIdleState{
		workingSince:     time.Now().Add(-19 * time.Minute),
		workingTurnStart: time.Time{},
	}
	if !workingTurnCounts(unchanged, time.Time{}, yesloopIdleWorkingMax) {
		t.Fatal("a stampless turn inside the guard's window counts as work")
	}
	if age := time.Since(unchanged.workingSince); age < 18*time.Minute {
		t.Errorf("window age = %v, want the kept window: two stampless turns are indistinguishable", age)
	}
}

// A job credit is per supervision episode, not per turn: an agent that starts a
// cheap background job as the last action of every turn would otherwise keep the
// signal alive forever and never be escalated (5.4 review). A second job inside
// the same episode shares the remaining credit.
func TestLos38l_SecondJobDoesNotRenewTheCredit(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-job-chain"

	driveBoundYesloopIntoProgression(t, h, s, id)
	giveIdleAgentASocket(t, h, s, id)

	// The episode's credit was opened by an earlier job and is now spent.
	yesloopIdleAgentsMu.Lock()
	yesloopIdleAgents[id].backgroundSince = time.Now().Add(-(yesloopIdleBackgroundMax + time.Minute))
	yesloopIdleAgentsMu.Unlock()

	// A fresh job in a fresh turn is still only the same episode's second job.
	stub.startBackgroundJob(time.Now().Add(-time.Minute))

	spentSince := time.Time{}
	yesloopIdleAgentsMu.Lock()
	spentSince = yesloopIdleAgents[id].backgroundSince
	yesloopIdleAgentsMu.Unlock()

	expireIdleRelay(id)
	h.checkYesloopIdle()

	if got := getIdleStateRefireCount(id); got != 1 {
		t.Errorf("refireCount = %d after a second job inside a spent episode, want 1", got)
	}
	// The refire alone is reachable through other paths too; the credit is what
	// the test is about, so the stamp has to stay where the episode put it.
	if _, _, _, backgroundSince, ok := idleStateSnapshot(id); !ok || !backgroundSince.Equal(spentSince) {
		t.Errorf("backgroundSince = %v, want %v unchanged: a second job must not renew the episode credit", backgroundSince, spentSince)
	}
}

// --- Findings of the 5.2/5.3/5.4 reviews of this ticket (18.10.2026) ---

// A supervision episode that ended with a spent abstention window must not hand
// the spent window to the next one: the re-armed agent worked, and the first
// unreadable read of the new episode would escalate it right away, although a
// freshly registered agent gets the full window (5.2 finding 2).
func TestLos38l_NewEpisodeGetsAFreshUnreadableWindow(t *testing.T) {
	unreadable := idleSessionReading{unreadable: true}
	h, _ := mustHandler(t)
	agent := storage.Agent{
		ID: "los38l-fresh-window", Section: "yesloop-los38l-fresh-window",
	}
	state := &yesloopIdleState{lastReadAt: time.Now().Add(-(yesloopIdleWorkingMax + 5*time.Minute))}
	if h.withholdRefireOnUnreadableSession(agent, state, unreadable) {
		t.Fatal("precondition: the spent window must not hold any more")
	}

	clearEpisode(state)

	if !h.withholdRefireOnUnreadableSession(agent, state, unreadable) {
		t.Error("the first unreadable read of a new episode must open the hold window again")
	}
}

// The per-turn window must not turn into an unbounded streak: an agent whose
// newest turn stays open and that mints a new open turn per relay would renew
// the signal forever and reset the ladder with it (5.2/5.3 finding).
func TestLos38l_OpenTurnStreakDoesNotOutliveTheEpisode(t *testing.T) {
	fresh := &yesloopIdleState{}
	if !workingTurnCounts(fresh, time.Now().Add(-2*time.Minute), yesloopIdleWorkingMax) {
		t.Fatal("a freshly observed working turn counts as work")
	}

	spent := &yesloopIdleState{turnEpisodeSince: time.Now().Add(-(yesloopIdleTurnEpisodeMax + time.Minute))}
	if workingTurnCounts(spent, time.Now().Add(-2*time.Minute), yesloopIdleWorkingMax) {
		t.Errorf("an open turn after %s of open-turn streak must not count as work any more", yesloopIdleTurnEpisodeMax)
	}

	// The streak ends with the first read that reports a settled turn.
	clearWorkingWindow(spent)
	if !workingTurnCounts(spent, time.Now().Add(-2*time.Minute), yesloopIdleWorkingMax) {
		t.Error("a settled reading ends the streak; the next working turn gets its own credit")
	}
}

// A set socket path keeps the unreadable-session hold even when the last dial
// failed, because the listener is transient. What the refusal must change is the
// escalation text, not the grace: an orchestrator must not read the pause as an
// ignored prompt (5.2 finding 1).
func TestLos38l_DeadSocketKeepsTheHoldButIsNamed(t *testing.T) {
	h, _ := mustHandler(t)
	agent := storage.Agent{
		ID: "los38l-dead-sock", Section: "yesloop-los38l-dead-sock",
	}
	state := &yesloopIdleState{relayUndeliverable: true}
	// A derivable path exists, so the hold still applies even though the last dial
	// failed: the listener is transient (a bridge that has not started yet), and
	// removing the hold would pause a merely starting agent.
	if !h.withholdRefireOnUnreadableSession(agent, state, idleSessionReading{unreadable: true}) {
		t.Error("a derivable socket path keeps the hold: the missing listener may be transient")
	}
}

// The guard remembers the failed dial. The mark does not remove the hold (the
// listener may still come up), but it has to reach the escalation text: without
// it the orchestrator reads the pause as "the agent ignored three prompts" while
// not a single relay left the daemon (5.2 finding 1, 5.3 finding 6).
func TestLos38l_FailedDialIsRemembered(t *testing.T) {
	resetIdleState()
	stub := newLos38lSessionStub(t)
	h, s := mustHandler(t)
	const id = "los38l-dead-dial"

	// The fixture's sock_path points into a directory that does not exist.
	driveBoundYesloopIntoProgression(t, h, s, id)

	expireIdleRelay(id)
	h.checkYesloopIdle()
	if !idleStateRelayUndeliverable(id) {
		t.Fatal("a relay whose dial failed must be remembered as undeliverable")
	}
	if got := getIdleStateRefireCount(id); got != 1 {
		t.Fatalf("refireCount = %d after the undeliverable relay, want 1", got)
	}

	// An unreadable read keeps the hold: the socket may start listening.
	stub.setFail(true)
	expireIdleRelay(id)
	h.checkYesloopIdle()
	if got := getIdleStateRefireCount(id); got != 1 {
		t.Errorf("refireCount = %d during the unreadable hold, want 1", got)
	}

	// Once the episode's abstention window is spent, the ladder goes on and the
	// pause names the dead socket.
	yesloopIdleAgentsMu.Lock()
	yesloopIdleAgents[id].lastReadAt = time.Now().Add(-(yesloopIdleWorkingMax + time.Minute))
	yesloopIdleAgentsMu.Unlock()
	expireIdleRelay(id)
	h.checkYesloopIdle()
	expireIdleRelay(id)
	h.checkYesloopIdle()

	row, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if !strings.Contains(row.Progress, "refused every relay") {
		t.Errorf("progress = %q, want the refused-relay clause: the orchestrator cannot tell an ignored relay from one that never left", row.Progress)
	}
}

// idleStateRelayUndeliverable reports the guard's memory of a failed delivery.
func idleStateRelayUndeliverable(id string) bool {
	yesloopIdleAgentsMu.Lock()
	defer yesloopIdleAgentsMu.Unlock()
	st, ok := yesloopIdleAgents[id]
	return ok && st.relayUndeliverable
}
