package daemon

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/carsteneu/yesmem/internal/briefing"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func (h *Handler) handleGetCompactedStubs(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)
	// Fallback to session_id for backwards compat
	if threadID == "" {
		threadID, _ = params["session_id"].(string)
	}
	if threadID == "" {
		return errorResponse("thread_id required")
	}

	// Optional: filter by range
	fromIdx := 0
	toIdx := 0
	if f, ok := params["from_idx"].(float64); ok {
		fromIdx = int(f)
	}
	if t, ok := params["to_idx"].(float64); ok {
		toIdx = int(t)
	}

	// Read frozen stubs from proxy_state (runtime.db), not the dead compacted_blocks table.
	// Frozen stubs are persisted by sawtooth via SetProxyState with key "frozen:<threadID>".
	frozenJSON, err := h.store.GetProxyState("frozen:" + threadID)
	if err != nil {
		return errorResponse(fmt.Sprintf("get frozen stubs: %v", err))
	}
	if frozenJSON == "" {
		// An invalidated prefix leaves its archive ranges behind under
		// frozen_markers:<threadID>: the stubbed messages are gone, but the
		// range they advertised is still reachable through expand_context and
		// deep_search, so the marker list must not degrade to an empty answer.
		markersJSON, mErr := h.store.GetProxyState(frozenMarkersKeyPrefix + threadID)
		if mErr != nil {
			return errorResponse(fmt.Sprintf("get frozen markers: %v", mErr))
		}
		if markersJSON != "" {
			blocks, pErr := parseFrozenMarkers(markersJSON, threadID, fromIdx, toIdx)
			if pErr != nil {
				return errorResponse(fmt.Sprintf("parse frozen markers: %v", pErr))
			}
			h.enrichBlocksWithMessageContent(threadID, blocks)
			return jsonResponse(blocks)
		}

		// No frozen stubs for this thread — try compacted_blocks as fallback (legacy path)
		var blocks []*storage.CompactedBlock
		if fromIdx > 0 || toIdx > 0 {
			if toIdx == 0 {
				toIdx = 999999
			}
			blocks, err = h.store.GetCompactedBlocksInRange(threadID, fromIdx, toIdx)
		} else {
			blocks, err = h.store.GetCompactedBlocks(threadID)
		}
		if err != nil {
			return errorResponse(fmt.Sprintf("get compacted stubs: %v", err))
		}
		return jsonResponse(blocks)
	}

	// Parse frozen stubs to extract archived message ranges
	blocks, err := parseFrozenStubs(frozenJSON, threadID, fromIdx, toIdx)
	if err != nil {
		return errorResponse(fmt.Sprintf("parse frozen stubs: %v", err))
	}

	// Enrich blocks with actual message content from messages.db (real-time indexed)
	h.enrichBlocksWithMessageContent(threadID, blocks)

	return jsonResponse(blocks)
}

// enrichBlocksWithMessageContent replaces the archive summary of a block with the
// messages themselves when the range is small enough to inline. messages.db holds
// them independently of the frozen prefix, so this works for a live prefix and
// for the ranges left behind by an invalidation alike.
func (h *Handler) enrichBlocksWithMessageContent(threadID string, blocks []*storage.CompactedBlock) {
	for i, b := range blocks {
		if b.EndIdx-b.StartIdx > 200 {
			continue // too many messages, keep marker as summary
		}
		msgs, err := h.store.GetMessagesBySessionRange(threadID, b.StartIdx, b.EndIdx)
		if err != nil || len(msgs) == 0 {
			continue
		}
		blocks[i].Content = formatMessageRange(msgs, threadID, b.StartIdx, b.EndIdx)
	}
}

// handleExpandContext allows Claude to actively expand archived/compacted context.
// Supports two modes: query-based search or explicit message range.
func (h *Handler) handleExpandContext(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)
	if threadID == "" {
		threadID, _ = params["session_id"].(string)
	}
	query, _ := params["query"].(string)
	msgRange, _ := params["message_range"].(string)

	if query == "" && msgRange == "" {
		return errorResponse("query or message_range required")
	}

	// Mode 1: Explicit message range (e.g. "200-250")
	if msgRange != "" && threadID != "" {
		var from, to int
		if _, err := fmt.Sscanf(msgRange, "%d-%d", &from, &to); err != nil {
			return errorResponse(fmt.Sprintf("invalid message_range format (expected 'from-to'): %v", err))
		}

		// Try to fetch actual message content from messages.db (real-time indexed by OpencodeScanner)
		msgs, err := h.store.GetMessagesBySessionRange(threadID, from, to)
		if err != nil {
			return errorResponse(fmt.Sprintf("expand range: %v", err))
		}
		if len(msgs) > 0 {
			content := formatMessageRange(msgs, threadID, from, to)
			blocks := []*storage.CompactedBlock{{
				ThreadID: threadID,
				StartIdx: from,
				EndIdx:   to,
				Content:  content,
			}}
			return jsonResponse(map[string]any{"message": fmt.Sprintf("%d messages in range %d-%d", len(msgs), from, to), "blocks": blocks})
		}

		// Fallback: read frozen stubs from proxy_state (sawtooth-persisted markers)
		frozenJSON, err := h.store.GetProxyState("frozen:" + threadID)
		if err != nil {
			return errorResponse(fmt.Sprintf("expand range: %v", err))
		}
		if frozenJSON != "" {
			blocks, err := parseFrozenStubs(frozenJSON, threadID, from, to)
			if err != nil {
				return errorResponse(fmt.Sprintf("parse frozen stubs: %v", err))
			}
			if len(blocks) == 0 {
				return jsonResponse(map[string]any{"message": fmt.Sprintf("No archived content in range %d-%d", from, to), "blocks": []any{}})
			}
			return jsonResponse(map[string]any{"message": fmt.Sprintf("%d blocks in range %d-%d", len(blocks), from, to), "blocks": blocks})
		}

		// Fallback: archive ranges captured when the prefix was invalidated
		markersJSON, mErr := h.store.GetProxyState(frozenMarkersKeyPrefix + threadID)
		if mErr != nil {
			return errorResponse(fmt.Sprintf("expand range: %v", mErr))
		}
		if markersJSON != "" {
			blocks, pErr := parseFrozenMarkers(markersJSON, threadID, from, to)
			if pErr != nil {
				return errorResponse(fmt.Sprintf("parse frozen markers: %v", pErr))
			}
			if len(blocks) == 0 {
				return jsonResponse(map[string]any{"message": fmt.Sprintf("No archived content in range %d-%d", from, to), "blocks": []any{}})
			}
			return jsonResponse(map[string]any{"message": fmt.Sprintf("%d blocks in range %d-%d", len(blocks), from, to), "blocks": blocks})
		}

		// Fallback: legacy compacted_blocks table
		blocks, err := h.store.GetCompactedBlocksInRange(threadID, from, to)
		if err != nil {
			return errorResponse(fmt.Sprintf("expand range: %v", err))
		}
		if len(blocks) == 0 {
			return jsonResponse(map[string]any{"message": fmt.Sprintf("No archived content in range %d-%d", from, to), "blocks": []any{}})
		}
		return jsonResponse(map[string]any{"message": fmt.Sprintf("%d blocks in range %d-%d", len(blocks), from, to), "blocks": blocks})
	}

	// Mode 2: Query-based search across archived messages
	if query != "" {
		result := h.handleDeepSearch(map[string]any{"query": query, "limit": float64(5)})
		return result
	}

	return errorResponse("unexpected state")
}

func (h *Handler) handleGetProxyState(params map[string]any) Response {
	key, _ := params["key"].(string)
	if key == "" {
		return errorResponse("key required")
	}
	if err := h.proxyStateReadAllowed(params, key); err != nil {
		return errorResponse(err.Error())
	}
	value, err := h.store.GetProxyState(key)
	if err != nil {
		return errorResponse(fmt.Sprintf("get proxy state: %v", err))
	}
	return jsonResponse(map[string]string{"key": key, "value": value})
}

// proxyStateThreadPrefixes are the proxy_state families whose key suffix names
// the session/thread the row belongs to. Everything the proxy persists per
// request lives under one of them (sawtooth.go, decay.go, timestamp_store.go,
// eager_stub_memory.go, proxy.go session_model), and source_agent is the routing
// label the daemon stores for a session.
//
// session_override is the per-session token threshold. It used to be a suffix of
// config_override, next to the per-model slot, and the two could not be told apart:
// a session id and a model name are both free-form there, and the model segment is
// itself caller-chosen. A separate family makes the key answer the question
// instead of guessing from a name (YM-427).
//
// What stays out are the keys that name no thread (rate_limits, the session
// pointer active_session_opencode, cluster_hash:<project>, the global slot
// config_override:token_threshold and the per-model slots below it).
var proxyStateThreadPrefixes = []string{
	"frozen:",
	frozenMarkersKeyPrefix,
	"decay:",
	"timestamps:",
	"eagerstub:",
	"sawtooth:",
	"session_model:",
	"session_override:",
	"source_agent:",
}

// sessionOverrideStateKey prefixes the per-session token threshold slot. The proxy
// reads it through sessionOverrideKeys (internal/proxy/proxy.go), so both sides
// take the name from one place (internal/config).
const sessionOverrideStateKey = config.SessionOverrideStateKey

// proxyStateThreadKey reports the thread a proxy_state key is about. false means
// the key names no thread and the caller is not asked about ownership.
func proxyStateThreadKey(key string) (string, bool) {
	for _, prefix := range proxyStateThreadPrefixes {
		if rest, ok := strings.CutPrefix(key, prefix); ok && rest != "" {
			return rest, true
		}
	}
	return "", false
}

// proxyStateDeleteScope reports whether a delete prefix reaches per-session state.
// The family itself counts: the proxy's cache reset deletes frozen:, decay: and
// timestamps: wholesale, so the bare prefix is the most destructive form.
//
// The comparison runs in both directions because the store matches prefixes the
// other way round: DeleteProxyStatePrefix removes every key that STARTS WITH the
// given string (storage/compaction.go), so a shortened prefix like "session_over"
// or "sess" wipes the whole family and is not itself a prefix of the family name.
// A prefix that the family name starts with is a cut of it and reaches its rows.
func proxyStateDeleteScope(prefix string) bool {
	for _, family := range proxyStateThreadPrefixes {
		stem := strings.TrimSuffix(family, ":")
		if strings.HasPrefix(prefix, stem) || strings.HasPrefix(stem, prefix) {
			return true
		}
	}
	return false
}

// proxyStateForeignPeer reports whether the caller is a socket client that is not
// the proxy. A call without a peer is the daemon's own code, and a platform
// without peer credentials abstains: the exposure this closes is the socket.
func proxyStateForeignPeer(params map[string]any) bool {
	pid := peerPIDParam(params)
	return pid > 0 && procTableReadable && !peerIsProxy(pid)
}

// sessionOverrideWriteAllowed refuses a per-session runtime override for a session
// the daemon cannot bind this call to.
//
// The named value is a claim, so the session is resolved from the daemon's own
// records and the kernel's report instead (overrideCallerSession). That is a
// binding, not an authorization against a same-UID process: a caller that arranges
// its directory or environment to look like another session gets through, exactly
// as it gets through peerIsProxy's argv check. What it closes is the ordinary case
// the audit found — a session naming another session through the tool parameter —
// and a caller that cannot be bound at all is refused rather than served.
//
// The daemon's own calls carry no peer and are untouched, and the proxy, which
// serves every session's state by design, is let through.
func (h *Handler) sessionOverrideWriteAllowed(params map[string]any, sessionID string) error {
	peer := peerPIDParam(params)
	if peer <= 0 || !procTableReadable {
		// No peer at all (the daemon's own code), or a peer the kernel could not
		// report: both abstain, as every other ownership check in this package does.
		return nil
	}
	if peerIsProxy(peer) {
		return nil
	}
	caller := h.overrideCallerSession(params)
	if caller == "" {
		log.Printf("[config] refused a session override for %q by peer pid %d: the caller could not be bound to a session", sessionID, peer)
		return fmt.Errorf("set_config: this call could not be bound to a session, so a session override cannot be attributed. Set the global token_threshold, or set the override from the session itself")
	}
	if !sameSessionIdentity(caller, sessionID) {
		log.Printf("[config] refused a session override for %q by peer pid %d, which bound to %q", sessionID, peer, caller)
		return fmt.Errorf("set_config: the override for %q can only be set from that session, and this call did not bind to it. Set the global token_threshold instead", sessionID)
	}
	return nil
}

// sessionOverrideReadAllowed is the same rule for the read side. A caller that
// cannot be bound is refused rather than served the global value under a session
// scope; it can still ask for the global value, without a session_id.
func (h *Handler) sessionOverrideReadAllowed(params map[string]any, sessionID string) error {
	peer := peerPIDParam(params)
	if peer <= 0 || !procTableReadable {
		return nil
	}
	if peerIsProxy(peer) {
		return nil
	}
	caller := h.overrideCallerSession(params)
	if caller == "" || !sameSessionIdentity(caller, sessionID) {
		log.Printf("[config] refused a read of the session override for %q by peer pid %d, which bound to %q", sessionID, peer, caller)
		return fmt.Errorf("get_config: the override for %q is only visible to that session, and this call did not bind to it. Read the global token_threshold instead", sessionID)
	}
	return nil
}

// overrideCallerSession resolves the session a config call comes from, or "" when
// the daemon cannot bind the caller to one.
//
// Only two sources count, and neither is a credential against a same-UID process:
// the verified working directory together with the daemon's own agent row, and the
// environment the peer process was started with. What they do close is the ordinary
// case — a session that names another session through the tool parameter — and what
// they cannot close is a process that arranges its own directory or environment to
// look like the victim. That is the same boundary peerIsProxy draws with argv: a
// same-UID process can claim the label, and the socket is the boundary.
func (h *Handler) overrideCallerSession(params map[string]any) string {
	if agent := h.agentByVerifiedCallerCWD(params); agent != nil && agent.SessionID != "" {
		return agent.SessionID
	}
	// The peer chain maps a pid onto the session the daemon registered for it. That
	// is exact for a Claude Code or codex session, whose own process (or TUI) is in
	// the map. It is not exact for an opencode session under the shared service: the
	// plugin registers the service pid once per session, so the map holds whichever
	// opencode session was created last machine-wide. Using that as evidence would
	// refuse the caller its own override and hand it a foreign one, so an opencode
	// id resolved this way does not count.
	if sid := h.sessionFromPeerChain(params); sid != "" && !strings.HasPrefix(sid, "opencode:") {
		return sid
	}
	return peerEnvSession(peerPIDParam(params))
}

// peerEnvSession returns a session id the peer process carries in its own
// environment, or "". The parents are not consulted: a child inherits the
// environment, so the parent adds nothing, and under the shared opencode service the
// parent's value names whatever session started the service.
//
// This is a label, not an identity: whoever starts a process sets its environment,
// so a same-UID caller can start itself with another session's id. It closes the
// ordinary mixup — the session the MCP child belongs to is the one it was started
// with — and nothing more.
func peerEnvSession(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/environ", procRoot, pid))
	if err != nil {
		return ""
	}
	entries := strings.Split(string(data), "\x00")
	// The names the MCP layer itself reads (internal/mcp/server.go), so both sides
	// agree on what a process' own session is called.
	for _, name := range []string{
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_SESSION_ID",
		"CODEX_THREAD_ID",
		"CODEX_SESSION_ID",
		"YESMEM_SESSION_ID",
	} {
		want := name + "="
		for _, entry := range entries {
			if value, ok := strings.CutPrefix(entry, want); ok && value != "" {
				return value
			}
		}
	}
	return ""
}

// sameSessionIdentity compares two session ids the way the override key is read:
// the proxy accepts the bare backend id next to the prefixed one
// (sessionOverrideKeys), so "opencode:ses_x" and "ses_x" are the same session here
// too.
func sameSessionIdentity(a, b string) bool {
	if a == b {
		return true
	}
	if _, rest, ok := strings.Cut(a, ":"); ok && rest == b {
		return true
	}
	if _, rest, ok := strings.Cut(b, ":"); ok && rest == a {
		return true
	}
	return false
}

// proxyStateWriteAllowed closes the same hole the channel endpoints were closed
// against (YM-204, YM-235, YM-414): a value the caller sends is not proof of
// ownership, so the peer the kernel reports on the connection decides. Per-thread
// state has exactly one legitimate writer that is not the thread itself — the
// proxy, which serves every thread and persists their state through this RPC (the
// pieces of the pipeline do not run inside the session's own process). Any other
// socket client would change what the proxy does for someone else's thread: drop
// their frozen prefix, age their decay, rewrite their timestamps or model.
//
// The HTTP transport carries no peer and does not serve this method at all
// (httpRPCMethods), so the abstention cannot be reached from outside the socket.
func (h *Handler) proxyStateWriteAllowed(params map[string]any, key string) error {
	if !proxyStateForeignPeer(params) {
		return nil
	}
	if _, scoped := proxyStateThreadKey(key); !scoped {
		return nil
	}
	// Logged, not silent: a refusal here means something besides the proxy
	// tried to write another thread's state, or the proxy's own argv no longer
	// matches, which would stop the pipeline's persistence until noticed.
	log.Printf("[proxy-state] refused a write to %q by peer pid %d, which is not the proxy", key, peerPIDParam(params))
	return fmt.Errorf("proxy state for a session may only be written by the proxy")
}

// proxyStateReadAllowed is the same rule for the read side. The write side alone
// only moved the attack one method over: whoever knew a key read another session's
// frozen prefix, decay ages, timestamps or budget, which is the state the proxy
// decides on for that thread.
func (h *Handler) proxyStateReadAllowed(params map[string]any, key string) error {
	if !proxyStateForeignPeer(params) {
		return nil
	}
	if _, scoped := proxyStateThreadKey(key); !scoped {
		return nil
	}
	log.Printf("[proxy-state] refused a read of %q by peer pid %d, which is not the proxy", key, peerPIDParam(params))
	return fmt.Errorf("proxy state for a session may only be read by the proxy")
}

// proxyStateDeleteAllowed is the same rule for the destructive sibling: a write
// guard that left the delete path open would only move the attack one call over
// (review finding on YM-414). The proxy's cache reset is the only legitimate
// caller of a thread-family prefix.
func (h *Handler) proxyStateDeleteAllowed(params map[string]any, prefix string) error {
	if !proxyStateDeleteScope(prefix) {
		return nil
	}
	if proxyStateForeignPeer(params) {
		log.Printf("[proxy-state] refused a delete of %q by peer pid %d, which is not the proxy", prefix, peerPIDParam(params))
		return fmt.Errorf("proxy state for a session may only be deleted by the proxy")
	}
	return nil
}

func (h *Handler) handleSetProxyState(params map[string]any) Response {
	key, _ := params["key"].(string)
	if key == "" {
		return errorResponse("key required")
	}
	value, _ := params["value"].(string)

	if err := h.proxyStateWriteAllowed(params, key); err != nil {
		return errorResponse(err.Error())
	}

	// An empty value on a frozen:* key is the proxy's invalidation signal
	// (FrozenStubs.Invalidate). Capture the archive ranges the row advertised
	// before it is dropped: the stubbed messages are gone, but the range they
	// cover is still readable through expand_context and deep_search.
	//
	// A failed marker write must not cancel the invalidation. The proxy drops
	// the in-memory prefix first and has no retry, so refusing here would leave
	// a multi-megabyte row that the next cold start loads again.
	if value == "" && strings.HasPrefix(key, "frozen:") {
		threadID := strings.TrimPrefix(key, "frozen:")
		if markers := h.frozenMarkersValue(key); markers != "" {
			if err := h.store.SetProxyState(frozenMarkersKeyPrefix+threadID, markers); err != nil {
				log.Printf("[frozen] marker record for %q not stored, invalidating anyway: %v", threadID, err)
			}
		}
	} else if value != "" && strings.HasPrefix(key, "frozen:") {
		// A new prefix supersedes the ranges of the invalidated one, so the
		// marker row does not shadow a live prefix or the legacy block table.
		threadID := strings.TrimPrefix(key, "frozen:")
		if err := h.store.DeleteProxyState(frozenMarkersKeyPrefix + threadID); err != nil {
			log.Printf("[frozen] stale marker record for %q not removed: %v", threadID, err)
		}
	}

	if err := h.store.SetProxyState(key, value); err != nil {
		return errorResponse(fmt.Sprintf("set proxy state: %v", err))
	}
	return jsonResponse(map[string]string{"status": "ok"})
}

// frozenMarkersValue renders the marker record for the frozen row that is about
// to be invalidated. It returns "" when the row carries no archive range, so an
// invalidation without markers still frees the row instead of leaving a second,
// useless entry behind.
func (h *Handler) frozenMarkersValue(key string) string {
	raw, err := h.store.GetProxyState(key)
	if err != nil || raw == "" {
		return ""
	}
	// Keep the message array opaque: a full []any decode of a 10 MB row costs
	// about 90 ms and this runs synchronously on the proxy's request path.
	var head struct {
		Messages []frozenStubMessage `json:"messages"`
		Cutoff   int                 `json:"cutoff"`
		Tokens   int                 `json:"tokens"`
	}
	if json.Unmarshal([]byte(raw), &head) != nil {
		return ""
	}
	markers := archiveMarkersFromStubMessages(head.Messages)
	if len(markers) == 0 {
		return ""
	}
	data, err := json.Marshal(frozenMarkersRecord{Markers: markers, Cutoff: head.Cutoff, Tokens: head.Tokens})
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *Handler) handleDeleteProxyStatePrefix(params map[string]any) Response {
	prefix, _ := params["prefix"].(string)
	if prefix == "" {
		return errorResponse("prefix required")
	}
	if err := h.proxyStateDeleteAllowed(params, prefix); err != nil {
		return errorResponse(err.Error())
	}
	n, err := h.store.DeleteProxyStatePrefix(prefix)
	if err != nil {
		return errorResponse(fmt.Sprintf("delete proxy state prefix: %v", err))
	}
	return jsonResponse(map[string]any{"deleted": n})
}

// handleSetConfig sets a config value.
// Without session_id: writes to config.yaml (persistent, by dot-path).
// With session_id: writes to proxy_state only (runtime override for that session).
// e.g. set_config("extraction.model", "opus")
//
//	set_config("token_threshold", "300000", "session_id": "sess-1")
func (h *Handler) handleSetConfig(params map[string]any) Response {
	key, _ := params["key"].(string)
	value, ok := params["value"].(string)
	if key == "" {
		return errorResponse("key required")
	}
	// The empty string is a value: it is the documented off state of
	// proxy.effort_floor, and it is how a session override is cleared. Only a
	// missing parameter is an error.
	if !ok {
		return errorResponse(`value required (send "value": "" to clear a key)`)
	}

	// Session-specific: proxy_state runtime override (not persistent)
	if sessionID, ok := params["session_id"].(string); ok && sessionID != "" {
		normKey, err := config.NormalizeSetPath(key)
		if err != nil {
			return errorResponse(fmt.Sprintf("set config: %v", err))
		}
		// The proxy addresses a per-model threshold as
		// config_override:token_threshold:<model> and a session override as
		// session_override:token_threshold:<session>. The combination would land
		// in a key no reader builds: the call reported success and changed
		// nothing. Refusing it is the only honest answer short of inventing a
		// second read path.
		if model, isPerModel := strings.CutPrefix(normKey, "proxy.token_thresholds."); isPerModel {
			return errorResponse(fmt.Sprintf("session_id cannot be combined with the per-model key %q: a session override is stored as %s:<session>. Set token_threshold:%q without session_id, or token_threshold for the session", model, sessionOverrideStateKey, model))
		}
		// A session override is a proxy_state slot the proxy re-reads for this
		// session (refreshSessionOverride). Only the token-threshold family has
		// that reader; for any other key the call would report success and change
		// nothing, which is the same dead slot 55c refused for the model suffix.
		if _, hasRuntimeSlot := config.ProxyRuntimeStateKey(normKey); !hasRuntimeSlot {
			return errorResponse(fmt.Sprintf("session_id cannot be used with %q: the proxy re-reads only the token-threshold family per session, so this override would never take effect. Set %q without session_id to change it persistently.", normKey, key))
		}
		coerced, err := config.CoerceValueForPath(normKey, config.CoerceValue(value))
		if err != nil {
			return errorResponse(fmt.Sprintf("set config: %v", err))
		}
		stateKey := sessionOverrideStateKey + ":" + sessionID
		// The slot is one session's state, and the session id is a parameter the
		// caller chose. Without this guard any socket client wrote the override
		// under a session it named, which is the hole the write side of
		// set_proxy_state was closed against (YM-414) reached through the config
		// path instead.
		if err := h.sessionOverrideWriteAllowed(params, sessionID); err != nil {
			return errorResponse(err.Error())
		}
		if err := h.store.SetProxyState(stateKey, fmt.Sprintf("%v", coerced)); err != nil {
			return errorResponse(fmt.Sprintf("set session config: %v", err))
		}
		return jsonResponse(map[string]string{
			"status": "ok", "key": key, "value": fmt.Sprintf("%v", coerced),
			"scope": "session:" + sessionID, "source": "proxy_state",
		})
	}

	// Global: write to config.yaml (persistent). The key is normalized onto its
	// schema location first: a known proxy key given without the "proxy."
	// prefix belongs under proxy:, where the loader reads it, and an unknown key
	// is rejected rather than written into a place no code looks at.
	normKey, err := config.NormalizeSetPath(key)
	if err != nil {
		return errorResponse(fmt.Sprintf("set config: %v", err))
	}
	coerced, err := config.CoerceValueForPath(normKey, config.CoerceValue(value))
	if err != nil {
		return errorResponse(fmt.Sprintf("set config: %v", err))
	}
	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	if err := config.SetValue(cfgPath, normKey, coerced); err != nil {
		return errorResponse(fmt.Sprintf("set config: %v", err))
	}

	// Dual-write to proxy_state for keys the proxy reads at runtime.
	// refreshConfigOverrides (internal/proxy/proxy.go) reads config_override:<key>
	// directly from proxy_state — it does NOT go through handleGetConfig. A
	// per-model threshold lives in the file as proxy.token_thresholds.<model>
	// but is read as token_threshold:<model>, so the state key is derived from
	// the schema path instead of from the raw request.
	//
	// Only the token-threshold family has a reader: every other proxy key is
	// applied when the proxy starts. A slot written for one of those would never
	// be read, and get_config would report it as an effective runtime override
	// (YM-392), so the mirror uses the runtime-slot derivation.
	if stateKey, ok := config.ProxyRuntimeStateKey(normKey); ok {
		if err := h.store.SetProxyState(stateKey, fmt.Sprintf("%v", coerced)); err != nil {
			// Non-fatal: config.yaml is the primary store
			log.Printf("set config proxy_state dual-write: %v", err)
		}
	}

	return jsonResponse(map[string]string{
		"status": "ok", "key": normKey, "value": fmt.Sprintf("%v", coerced),
		"scope": "config.yaml", "source": "config.yaml",
	})
}

// configAlias maps a key form that is not itself a schema path onto its schema
// path. token_threshold:<model> is the per-model override documented since
// 4903cfa4; its persistent home is proxy.token_thresholds.<model>, from which
// handleSetConfig derives the state key the proxy reads.
// handleGetConfig reads a config value.
// Priority: 1) session-specific proxy_state 2) global proxy_state 3) config.yaml
// e.g. get_config("extraction.model")
// Optional session_id: checks session-specific override first.
func (h *Handler) handleGetConfig(params map[string]any) Response {
	key, _ := params["key"].(string)
	if key == "" {
		return errorResponse("key required")
	}

	// Runtime slots exist only for the token-threshold family: the running proxy
	// re-reads exactly those from proxy_state (refreshConfigOverrides). A slot
	// under any other key was written by an older version and has no reader, so
	// reporting it would claim an effect that does not exist (YM-392). Those keys
	// are read from config.yaml below.
	runtimeKey, hasRuntimeSlot := config.ProxyRuntimeStateKey(key)
	// A session-scoped question for a key without a runtime slot cannot be
	// answered. set_config refuses the same combination; answering it here with
	// the global value would look like the session had been taken into account.
	if sessionID, _ := params["session_id"].(string); sessionID != "" && !hasRuntimeSlot {
		return errorResponse(fmt.Sprintf("session_id cannot be used with %q: the proxy re-reads only the token-threshold family per session, so there is no session value to report", key))
	}

	// 1. Check session-specific proxy_state override
	if sessionID, ok := params["session_id"].(string); ok && sessionID != "" && hasRuntimeSlot {
		// The slot belongs to that session, and the read answers the same question
		// as the write: without the guard, every caller read every session's
		// budget through this method after get_proxy_state was closed.
		if err := h.sessionOverrideReadAllowed(params, sessionID); err != nil {
			return errorResponse(err.Error())
		}
		stateKey := sessionOverrideStateKey + ":" + sessionID
		value, err := h.store.GetProxyState(stateKey)
		if err != nil {
			return errorResponse(fmt.Sprintf("get config: %v", err))
		}
		// The proxy reads this override under both spellings of the thread id — the
		// one it serves and, if that one is prefixed, the bare backend id
		// (sessionOverrideKeys). A row stored under either belongs to this session,
		// so the read follows the same two forms instead of reporting "no override"
		// for a row that is in effect.
		if value == "" {
			if _, rest, ok := strings.Cut(sessionID, ":"); ok && rest != "" {
				value, err = h.store.GetProxyState(sessionOverrideStateKey + ":" + rest)
				if err != nil {
					return errorResponse(fmt.Sprintf("get config: %v", err))
				}
			}
		}
		if value != "" {
			return jsonResponse(map[string]string{
				"key": key, "value": value, "scope": "session:" + sessionID, "source": "proxy_state",
			})
		}
	}

	// 2. Check global proxy_state override
	if hasRuntimeSlot {
		value, err := h.store.GetProxyState(runtimeKey)
		if err != nil {
			return errorResponse(fmt.Sprintf("get config: %v", err))
		}
		if value != "" {
			return jsonResponse(map[string]string{
				"key": key, "value": value, "scope": "global", "source": "proxy_state",
			})
		}
	}

	// 3. Read from config.yaml
	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	raw, err := config.GetValue(cfgPath, key)
	if err != nil {
		return errorResponse(fmt.Sprintf("read config: %v", err))
	}
	if raw != nil {
		str := fmt.Sprintf("%v", raw)
		return jsonResponse(map[string]string{
			"key": key, "value": str, "scope": "config.yaml", "source": "config.yaml",
		})
	}

	return jsonResponse(map[string]string{
		"key": key, "value": "", "scope": "", "source": "",
	})
}

// handleTrackGap records a knowledge gap via daemon.
func (h *Handler) handleTrackGap(params map[string]any) Response {
	topic, _ := params["topic"].(string)
	project, _ := params["project"].(string)
	if topic == "" {
		return errorResponse("topic required")
	}
	if err := h.store.TrackGap(topic, project); err != nil {
		return errorResponse(fmt.Sprintf("track gap: %v", err))
	}
	return jsonResponse(map[string]string{"status": "ok"})
}

func (h *Handler) handleResolveGap(params map[string]any) Response {
	topic, _ := params["topic"].(string)
	project, _ := params["project"].(string)
	if topic == "" {
		return errorResponse("topic required")
	}
	if err := h.store.ResolveGap(topic, project, 0); err != nil {
		return errorResponse(fmt.Sprintf("resolve gap: %v", err))
	}
	return jsonResponse(map[string]string{"status": "ok"})
}

func (h *Handler) handleGetActiveGaps(params map[string]any) Response {
	project, _ := params["project"].(string)
	limit := intOr(params, "limit", 10)
	gaps, err := h.store.GetActiveGaps(project, limit)
	if err != nil {
		return errorResponse(fmt.Sprintf("get active gaps: %v", err))
	}
	type gapEntry struct {
		ID       int64  `json:"id"`
		Topic    string `json:"topic"`
		HitCount int    `json:"hit_count"`
	}
	entries := make([]gapEntry, len(gaps))
	for i, g := range gaps {
		entries[i] = gapEntry{ID: g.ID, Topic: g.Topic, HitCount: g.HitCount}
	}
	return jsonResponse(map[string]any{"gaps": entries})
}

func (h *Handler) handleGenerateBriefing(params map[string]any) Response {
	project, _ := params["project"].(string)
	if project == "" {
		return errorResponse("project required")
	}

	// Use full CWD path for Code Map scanner, fall back to short name
	projectDir, _ := params["project_dir"].(string)
	if projectDir != "" {
		project = projectDir
	}

	sessionID, _ := params["session_id"].(string)

	// Persist source_agent if passed — belt-and-suspenders with register_pid
	// so briefings can identify non-Claude agents even on first request.
	if sa, _ := params["source_agent"].(string); sa != "" && sessionID != "" {
		_ = h.store.SetProxyState("source_agent:"+sessionID, sa)
	}

	result := briefing.GenerateFullBriefing(h.store, h.dataDir, project, sessionID)

	return jsonResponse(map[string]any{"text": result.Text, "code_map": result.CodeMap})
}

// handlePopRecentRemember returns and clears recently remembered learnings.
// The proxy calls this to inject fresh remember() content into the current session.
func (h *Handler) handlePopRecentRemember() Response {
	h.recentRememberMu.Lock()
	items := h.recentRemembered
	h.recentRemembered = nil
	h.recentRememberMu.Unlock()

	if len(items) == 0 {
		return jsonResponse(map[string]any{"items": []any{}})
	}
	return jsonResponse(map[string]any{"items": items})
}

// handlePin creates a new pinned learning.
func (h *Handler) handlePin(params map[string]any) Response {
	content, _ := params["content"].(string)
	if content == "" {
		return errorResponse("content required")
	}
	scope, _ := params["scope"].(string)
	if scope == "" {
		scope = "session"
	}
	if scope != "session" && scope != "permanent" {
		return errorResponse("scope must be 'session' or 'permanent'")
	}
	project, _ := params["project"].(string)
	source, _ := params["source"].(string)
	// The attribution decides whose /clear deletes the pin, so it must come from
	// a session the caller can be held to. The raw _session_id can be the
	// env-derived id of a shared opencode service, i.e. another session's id.
	sessionID := h.provenSessionID(params, "session_id")
	// A project-less session pin with no attributable session could never be
	// cleared: both clear conditions key on the project or on the session. Fall
	// back to the caller's working directory so the pin stays project-bound and
	// a /clear of that project reaches it. Permanent pins keep their documented
	// global visibility and are not bound here.
	if scope == "session" && project == "" && sessionID == "" {
		if cwd, _ := params["_cwd"].(string); cwd != "" {
			project = h.store.ResolveProjectShort(cwd)
		}
	}

	id, err := h.store.PinLearning(scope, project, content, source, sessionID)
	if err != nil {
		return errorResponse(fmt.Sprintf("pin failed: %v", err))
	}
	return jsonResponse(map[string]any{
		"id":      id,
		"scope":   scope,
		"project": project,
		"content": content,
	})
}

// handleUnpin removes a pinned learning by ID. Without a scope both tables are
// searched, because session and permanent pins keep separate ID sequences and the
// bare ID can match in both. That ambiguity is reported instead of deleting both:
// a permanent pin is the more durable of the two.
func (h *Handler) handleUnpin(params map[string]any) Response {
	id, ok := params["id"].(float64)
	if !ok || id <= 0 {
		return errorResponse("id required (positive number)")
	}
	scope, _ := params["scope"].(string)
	if scope != "" && scope != "session" && scope != "permanent" {
		return errorResponse("scope must be 'session' or 'permanent'")
	}

	if scope == "" {
		sessionHit, err := h.store.PinnedLearningExists("session", int64(id))
		if err != nil {
			return errorResponse(fmt.Sprintf("unpin failed: %v", err))
		}
		permanentHit, err := h.store.PinnedLearningExists("permanent", int64(id))
		if err != nil {
			return errorResponse(fmt.Sprintf("unpin failed: %v", err))
		}
		switch {
		case sessionHit && permanentHit:
			return errorResponse(fmt.Sprintf(
				"id %d exists as a session pin and as a permanent pin — pass scope to pick one", int64(id)))
		case sessionHit:
			scope = "session"
		case permanentHit:
			scope = "permanent"
		default:
			return errorResponse(fmt.Sprintf("no pin with id %d found in session or permanent scope", int64(id)))
		}
	}

	affected, err := h.store.UnpinLearning(scope, int64(id))
	if err != nil {
		return errorResponse(fmt.Sprintf("unpin failed: %v", err))
	}
	if affected == 0 {
		return errorResponse(fmt.Sprintf("no pin with id %d found in %s scope", int64(id), scope))
	}
	return jsonResponse(map[string]any{"unpinned_id": int64(id), "scope": scope})
}

// handleGetPins returns all pinned learnings (session + permanent merged).
func (h *Handler) handleGetPins(params map[string]any) Response {
	project, _ := params["project"].(string)

	sessionPins, err := h.store.GetPinnedLearnings("session", project)
	if err != nil {
		sessionPins = nil
	}
	permanentPins, err := h.store.GetPinnedLearnings("permanent", project)
	if err != nil {
		permanentPins = nil
	}

	type pinItem struct {
		ID      int64  `json:"id"`
		Scope   string `json:"scope"`
		Content string `json:"content"`
		Project string `json:"project,omitempty"`
	}
	var items []pinItem
	for _, p := range sessionPins {
		items = append(items, pinItem{ID: p.ID, Scope: "session", Content: p.Content, Project: p.Project})
	}
	for _, p := range permanentPins {
		items = append(items, pinItem{ID: p.ID, Scope: "permanent", Content: p.Content, Project: p.Project})
	}
	return jsonResponse(map[string]any{"pins": items})
}

func (h *Handler) handleRelatedToFile(params map[string]any) Response {
	path, _ := params["path"].(string)
	assocs, err := h.store.GetAssociationsTo("file", path)
	if err != nil {
		return errorResponse(err.Error())
	}
	return jsonResponse(assocs)
}

func (h *Handler) handleGetCoverage(params map[string]any) Response {
	project, _ := params["project"].(string)
	cov, err := h.store.GetCoverageByProject(project)
	if err != nil {
		return errorResponse(err.Error())
	}
	return jsonResponse(cov)
}

func (h *Handler) handleGetProjectProfile(params map[string]any) Response {
	project, _ := params["project"].(string)
	profile, err := h.store.GetProjectProfile(project)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return jsonResponse(map[string]string{"message": fmt.Sprintf("No profile for %q available", project)})
		}
		return errorResponse(err.Error())
	}
	return jsonResponse(profile)
}

func (h *Handler) handleGetSelfFeedback(params map[string]any) Response {
	days := intOr(params, "days", 30)
	fbs, err := h.store.GetSelfFeedback(days)
	if err != nil {
		return errorResponse(err.Error())
	}
	return jsonResponse(fbs)
}

func (h *Handler) handleIndexStatus() Response {
	if h.IndexProgress == nil {
		return jsonResponse(map[string]any{
			"running": false,
			"total":   0,
			"done":    0,
			"skipped": 0,
		})
	}
	total, done, skipped, running := h.IndexProgress()
	return jsonResponse(map[string]any{
		"running": running,
		"total":   total,
		"done":    done,
		"skipped": skipped,
	})
}

// handleTrackSessionEnd records a session end that continues as a new session
// (clear) in the DB. Called by the session-end hook via daemon socket instead of
// direct DB access to avoid write-lock contention with the daemon's own
// transactions.
func (h *Handler) handleTrackSessionEnd(params map[string]any) Response {
	project, _ := params["project"].(string)
	sessionID, _ := params["session_id"].(string)
	reason, _ := params["reason"].(string)

	if project == "" || sessionID == "" || reason == "" {
		return errorResponse("project, session_id, and reason required")
	}

	if err := h.store.TrackSessionEnd(project, sessionID, reason); err != nil {
		return errorResponse(fmt.Sprintf("track session end: %v", err))
	}

	if reason == "clear" {
		projectShort := h.store.ResolveProjectShort(project)
		h.store.ClearSessionPins(projectShort, sessionID)
	}

	// New session data may add project paths → invalidate resolution cache.
	h.InvalidateProjectCache()

	return jsonResponse(map[string]any{"status": "tracked", "session_id": sessionID})
}

// archivePattern matches the archive marker written by collapse.go:buildArchiveBlock.
// Format: [Archiv: Messages 1-725 (725 msgs) — get_compacted_stubs('...', 1, 725) zum Reinzoomen]
// Keep in sync with the writer in internal/proxy/collapse.go line 145.
var archivePattern = regexp.MustCompile(`\[Archiv: Messages (\d+)-(\d+) \((\d+) msgs\)`)

// parseFrozenStubs extracts CompactedBlock entries from a frozen stubs JSON blob.
// Frozen stubs are stored in proxy_state with key "frozen:<threadID>".
// The JSON schema matches sawtooth.frozenPersisted:
//
//	{"messages": [...], "cutoff": 725, "boundary_hash": "...", "prefix_hash": "...", "tokens": 45678, "raw_tokens": 67890}
//
// The frozen messages contain archive blocks like:
//
//	"[Archiv: Messages 1-725 (725 msgs) — get_compacted_stubs(...)]"
//
// from which we extract the message range.
func parseFrozenStubs(frozenJSON, threadID string, fromIdx, toIdx int) ([]*storage.CompactedBlock, error) {
	var fp struct {
		Messages     []any                 `json:"messages"`
		Markers      []frozenArchiveMarker `json:"markers"`
		Cutoff       int                   `json:"cutoff"`
		BoundaryHash string                `json:"boundary_hash"`
		PrefixHash   string                `json:"prefix_hash"`
		Tokens       int                   `json:"tokens"`
		RawTokens    int                   `json:"raw_tokens"`
	}
	if err := json.Unmarshal([]byte(frozenJSON), &fp); err != nil {
		return nil, fmt.Errorf("unmarshal frozen stubs: %w", err)
	}

	// A live prefix advertises its ranges inside the stubbed messages; a record
	// written at invalidation time carries them as a field.
	markers := fp.Markers
	if len(markers) == 0 {
		markers = archiveMarkersFromMessages(fp.Messages)
	}
	return blocksFromArchiveMarkers(threadID, markers, fp.Cutoff, fp.Tokens, fromIdx, toIdx), nil
}

// frozenMarkersKeyPrefix holds the archive ranges of an invalidated frozen
// prefix. The prefix itself is dropped on invalidation, so the ranges need a
// place of their own; a later freeze overwrites the frozen row and this record
// is pruned with it.
const frozenMarkersKeyPrefix = "frozen_markers:"

// frozenArchiveMarker is one [Archiv: Messages a-b (n msgs)] range.
type frozenArchiveMarker struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Count int `json:"count"`
}

// frozenMarkersRecord is the payload written under frozen_markers:<threadID>.
type frozenMarkersRecord struct {
	Markers []frozenArchiveMarker `json:"markers"`
	Cutoff  int                   `json:"cutoff,omitempty"`
	Tokens  int                   `json:"tokens,omitempty"`
}

// parseFrozenMarkers turns an invalidation record into stub blocks.
func parseFrozenMarkers(markersJSON, threadID string, fromIdx, toIdx int) ([]*storage.CompactedBlock, error) {
	var rec frozenMarkersRecord
	if err := json.Unmarshal([]byte(markersJSON), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal frozen markers: %w", err)
	}
	return blocksFromArchiveMarkers(threadID, rec.Markers, rec.Cutoff, rec.Tokens, fromIdx, toIdx), nil
}

// blocksFromArchiveMarkers renders the stub blocks for one archive range each,
// honouring the optional index filter of get_compacted_stubs.
func blocksFromArchiveMarkers(threadID string, markers []frozenArchiveMarker, cutoff, tokens, fromIdx, toIdx int) []*storage.CompactedBlock {
	var blocks []*storage.CompactedBlock
	for _, m := range markers {
		if toIdx > 0 && (m.Start > toIdx || m.End < fromIdx) {
			continue
		}
		if fromIdx > 0 && m.End < fromIdx {
			continue
		}
		blocks = append(blocks, &storage.CompactedBlock{
			ThreadID: threadID,
			StartIdx: m.Start,
			EndIdx:   m.End,
			Content: fmt.Sprintf("[Archiv: Messages %d-%d (%d msgs) — cutoff=%d, frozen_tokens=%d — use expand_context() or deep_search() for full content]",
				m.Start, m.End, m.Count, cutoff, tokens),
		})
	}
	return blocks
}

// archiveMarkersFromMessages extracts the archive ranges advertised by the
// stubbed messages of a frozen prefix.
func archiveMarkersFromMessages(messages []any) []frozenArchiveMarker {
	var markers []frozenArchiveMarker
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		for _, text := range messageTexts(m) {
			match := archivePattern.FindStringSubmatch(text)
			if len(match) < 4 {
				continue
			}
			if marker, ok := markerFromDigits(match[1], match[2], match[3]); ok {
				markers = append(markers, marker)
			}
		}
	}
	return markers
}

// archiveMarkersFromJSON is gone: a pure byte scan cannot tell a message's own
// content from a field of the same name inside a tool_use input (the Write shape
// carries "content" too), so the message array is decoded shallowly instead and
// only text is inspected.

// frozenStubMessage is the shallow view of one message of a frozen payload: the
// content stays raw until it is known to be message text.
type frozenStubMessage struct {
	Content json.RawMessage `json:"content"`
}

// frozenContentBlock is the view of one content block. Only text blocks carry
// message text; tool_use and tool_result payloads are never inspected.
type frozenContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// archiveMarkersFromStubMessages collects the archive ranges from the messages of
// a frozen payload without decoding tool payloads. Only a message's own content
// counts: the string itself or its text blocks.
func archiveMarkersFromStubMessages(messages []frozenStubMessage) []frozenArchiveMarker {
	var markers []frozenArchiveMarker
	for _, m := range messages {
		for _, text := range stubMessageTexts(m.Content) {
			match := archivePattern.FindStringSubmatch(text)
			if len(match) < 4 {
				continue
			}
			if marker, ok := markerFromDigits(match[1], match[2], match[3]); ok {
				markers = append(markers, marker)
			}
		}
	}
	return markers
}

// stubMessageTexts returns the text of one content field: the string itself, or
// the text blocks of a block array.
func stubMessageTexts(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if json.Unmarshal(trimmed, &text) != nil {
			return nil
		}
		return []string{text}
	case '[':
		var blocks []frozenContentBlock
		if json.Unmarshal(trimmed, &blocks) != nil {
			return nil
		}
		texts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return texts
	}
	return nil
}

// markerFromDigits converts the three captured digits of one archive marker.
func markerFromDigits(startText, endText, countText string) (frozenArchiveMarker, bool) {
	start, err1 := strconv.Atoi(startText)
	end, err2 := strconv.Atoi(endText)
	count, err3 := strconv.Atoi(countText)
	if err1 != nil || err2 != nil || err3 != nil {
		return frozenArchiveMarker{}, false
	}
	return frozenArchiveMarker{Start: start, End: end, Count: count}, true
}

// messageTexts returns the text payloads of one message: string content, or the
// text blocks of a block array.
func messageTexts(m map[string]any) []string {
	if text, ok := m["content"].(string); ok {
		return []string{text}
	}
	contentBlocks, ok := m["content"].([]any)
	if !ok {
		return nil
	}
	var texts []string
	for _, cb := range contentBlocks {
		if bm, ok := cb.(map[string]any); ok {
			if text, ok := bm["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

// formatMessageRange converts messages into a readable text block.
// Includes role, type, tool name, and timestamp for each message.
// Content is truncated at 5000 chars per message.
func formatMessageRange(msgs []models.Message, threadID string, from, to int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Messages %d-%d (%d msgs) for %s]\n", from, to, len(msgs), threadID)

	for _, m := range msgs {
		ts := m.Sequence
		content := m.Content
		if len(content) > 5000 {
			content = content[:5000] + "..."
		}
		if content == "" && len(m.ContentBlob) > 0 {
			content = string(m.ContentBlob)
			if len(content) > 5000 {
				content = content[:5000] + "..."
			}
		}

		typeStr := m.MessageType
		if m.ToolName != "" {
			typeStr += ":" + m.ToolName
		}
		fmt.Fprintf(&sb, "\n--- %s (%s, seq=%d, ts=%s) ---\n%s\n",
			m.Role, typeStr, ts, m.Timestamp.Format("15:04:05"), content)
	}
	return sb.String()
}
