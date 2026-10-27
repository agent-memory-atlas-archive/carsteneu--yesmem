package daemon

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// On-demand binding of opencode session ids.
//
// The spawn-time capture can miss a session (the TUI was slow, the daemon
// restarted). Instead of polling for every unbound agent on a timer, the id is
// looked up only when something needs it: get_agent, send_to to that agent,
// whoami from the agent's own session, and telemetry from its thread. One
// marker query against the live v2 service per attempt, no polling, and at most
// one attempt per agent per cooldown, so an agent whose session cannot be found
// (for example a terminal agent without startup prompt) costs nothing between
// attempts.

// opencodeBindCooldown is the minimum time between two lookups for the same key.
const opencodeBindCooldown = 2 * time.Minute

// opencodeBindQueryTimeout bounds one on-demand lookup.
const opencodeBindQueryTimeout = 2 * time.Second

// opencodeFindSessionFn is the lookup seam: one marker query against a live v2
// service, without polling or table fallback. Returns "" when no service runs.
// The package's tests replace it (see TestMain) and restore the production
// behaviour with defaultOpencodeFindSession.
var opencodeFindSessionFn = defaultOpencodeFindSession

// defaultOpencodeFindSession is the production lookup behind the seam.
func defaultOpencodeFindSession(ctx context.Context, q opencode.MarkerQuery) (string, error) {
	client := opencodeV2Service()
	if client == nil {
		return "", nil
	}
	return client.FindSession(ctx, q)
}

// bindThrottle remembers when a key was last attempted. The zero value is ready.
type bindThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// bindThrottleMaxKeys bounds the throttle map. Keys are agent and thread ids and
// are never removed, so without a cap the map grows with every agent the daemon
// has ever seen. Clearing it early only costs one extra lookup per key.
const bindThrottleMaxKeys = 4096

func (t *bindThrottle) try(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	if len(t.last) >= bindThrottleMaxKeys {
		// Drop the expired entries first: keys younger than the cooldown are
		// still enforcing a rate limit, and clearing them all would let every
		// unbound agent be scanned again at once. Only if that frees nothing
		// (more than 4096 agents bound within one cooldown) is the map reset.
		for k, at := range t.last {
			if now.Sub(at) >= opencodeBindCooldown {
				delete(t.last, k)
			}
		}
		if len(t.last) >= bindThrottleMaxKeys {
			t.last = make(map[string]time.Time)
		}
	}
	if last, ok := t.last[key]; ok && now.Sub(last) < opencodeBindCooldown {
		return false
	}
	t.last[key] = now
	return true
}

// ensureOpencodeSession binds the session id of an unbound opencode agent and
// returns the agent, updated when the bind succeeded. Anything else is
// returned unchanged.
func (h *Handler) ensureOpencodeSession(a *storage.Agent) *storage.Agent {
	if a == nil || a.Backend != "opencode" || a.OpencodeSessionID != "" {
		return a
	}
	if !h.opencodeBind.try("agent:"+a.ID, time.Now()) {
		return a
	}
	created, ok := parseAgentCreatedAt(a.CreatedAt)
	if !ok {
		return a
	}
	q := opencode.MarkerQuery{
		// The recorded work_dir is what keeps the scan inside this agent's own
		// tree: the project root holds the orchestrator's session, and a
		// store-wide scan cannot tell the two worktrees of one project apart.
		Directory: a.WorkDir,
		AgentID:   a.ID,
		Section:   a.Section,
		Since:     created.Add(-markerSinceSlack),
		Until:     created.Add(markerWindowAfter),
	}
	if a.WorkDir == "" {
		// No directory to narrow the scan (a row from before work_dir was
		// persisted): it becomes a store-wide window ordered by update time,
		// so it has to reach further back.
		q.Limit = lateBindScanLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeBindQueryTimeout)
	defer cancel()
	sid, err := opencodeFindSessionFn(ctx, q)
	if err != nil {
		log.Printf("[agent_bind] agent %s: opencode lookup failed: %v", a.ID, err)
		return a
	}
	if sid == "" || !h.bindAgentSession(a, sid, "on demand") {
		return a
	}
	bound := *a
	bound.OpencodeSessionID = sid
	bound.Progress = clearedUnresumable(bound.Progress)
	return &bound
}

// stripSessionPrefix removes the "opencode:"/"codex:" prefix the MCP layer
// attaches to a session id, mirroring the storage boundary (stripAgentPrefix).
// The row stores bare ids, so a value kept in memory for a later call has to be
// stripped as well.
func stripSessionPrefix(sid string) string {
	for _, p := range []string{"opencode:", "codex:", "claude:"} {
		if s, ok := strings.CutPrefix(sid, p); ok {
			return s
		}
	}
	return sid
}

// bindAgentSession is the single entry point for a *discovered* opencode session
// id: spawn capture, late bind and on-demand bind all pass through it, so the
// ownership guard cannot be forgotten by one of them — a session that another
// agent row already owns is never taken over, however plausible the match looked.
//
// Every writer of the column goes through bindAgentSession (spawn capture, late
// bind, on-demand bind, open_agent_terminal's adoption, bootstrap_resume) or
// through the same guarded store statement directly (resume_agent's explicit
// session id), so no path can take over a session another row owns.
//
// The ownership test here is carried out by the store in the same statement that
// writes (AgentBindSession), because these calls run concurrently from the spawn
// goroutine, the heartbeat and request handlers. A successful bind also clears
// the "unresumable" marker, because resumability is exactly what the new id
// restores, and logs once with the path that produced it.
func (h *Handler) bindAgentSession(a *storage.Agent, sid, source string) bool {
	if a == nil || sid == "" {
		return false
	}
	// This read only enriches the log; the store decides.
	if owner, held := h.store.AgentOwnedByOtherSession(sid, a.ID); held {
		log.Printf("[agent_bind] agent %s: session %s already belongs to %s, not binding (%s)", a.ID, sid, owner, source)
		return false
	}
	// The store clears the unresumable marker in the same statement, evaluated
	// against the stored value rather than this snapshot: the spawn-time capture
	// may have written the marker a moment ago from its own goroutine.
	bound, err := h.store.AgentBindSession(a.ID, sid, unresumableProgressPrefix)
	if err != nil {
		log.Printf("[agent_bind] agent %s: persisting session %s failed (%s): %v", a.ID, sid, source, err)
		return false
	}
	if !bound {
		log.Printf("[agent_bind] agent %s: session %s not bound (%s): the row already carries another session or the id is owned elsewhere", a.ID, sid, source)
		return false
	}
	log.Printf("[agent_bind] agent %s: bound opencode session %s (%s)", a.ID, sid, source)
	return true
}

// unresumableProgressPrefix is the progress prefix that marks an opencode agent
// as having no backend session (opencodeUnresumableMsg). A bind clears any
// progress value starting with it.
const unresumableProgressPrefix = "unresumable"

// clearedUnresumable drops the opencodeUnresumableMsg marker from a progress
// string and leaves every other progress value alone: an agent's progress field
// also carries phase and pause reasons that a bind must not erase.
func clearedUnresumable(progress string) string {
	if strings.HasPrefix(progress, unresumableProgressPrefix) {
		return ""
	}
	return progress
}

// bindAgentForOpencodeThread answers "which agent is this opencode session?"
// for a session no agent row knows yet, by binding the unbound running or
// paused opencode agents on demand. The thread itself is throttled too, so a
// session that belongs to no agent (an orchestrator, a user session) triggers
// at most one scan per cooldown.
func (h *Handler) bindAgentForOpencodeThread(threadID string) *storage.Agent {
	sid := strings.TrimPrefix(threadID, "opencode:")
	if !strings.HasPrefix(sid, "ses_") {
		return nil
	}
	if !h.opencodeBind.try("thread:"+sid, time.Now()) {
		return nil
	}
	agents, err := h.store.AgentList("")
	if err != nil {
		return nil
	}
	for i := range agents {
		a := &agents[i]
		if a.Backend != "opencode" || a.OpencodeSessionID != "" {
			continue
		}
		if a.Status != "running" && a.Status != "paused" {
			continue
		}
		if b := h.ensureOpencodeSession(a); b.OpencodeSessionID == sid {
			return b
		}
	}
	return nil
}

// sendTargetUnboundWarning is returned by send_to when the addressed opencode
// agent has no backend session yet. The message is stored under the agent's
// synthetic session UUID — which its proxy never polls — so the send does not
// fail, but it stays undelivered until a bind resolves the real address.
const sendTargetUnboundWarning = "target not bound to a backend session; delivery waits for the bind"

// bindSendTarget makes sure an unbound opencode agent addressed by send_to has
// its session id before the target is resolved, so the message is stored under
// the address the agent's proxy polls. It returns sendTargetUnboundWarning when
// the target is an opencode agent that is still unbound after the attempt, and
// "" for every other target (channels, bound agents, unknown names).
func (h *Handler) bindSendTarget(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	a, err := h.store.AgentGet(t)
	if err != nil || a == nil {
		if a, err = h.store.AgentGetAnyBySession(t); err != nil || a == nil {
			return ""
		}
	}
	if b := h.ensureOpencodeSession(a); b.Backend == "opencode" && b.OpencodeSessionID == "" {
		return sendTargetUnboundWarning
	}
	return ""
}
