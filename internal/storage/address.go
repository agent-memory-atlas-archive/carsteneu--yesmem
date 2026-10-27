package storage

import (
	"fmt"
	"strings"
)

// Channel addresses identify one session in agent_messages.target. Every
// identity has exactly one canonical address; fuzziness (short prefixes, agent
// ids) is resolved once at send time, never at read time.
//
// Three session address forms exist (see internal/mcp/server.go
// resolveClientSessionID and internal/proxy/opencode_session.go
// anthropicThreadID):
//
//	claude    bare uuid, no namespace
//	codex     "codex:<uuid>"
//	opencode  "opencode:<ses_id>"; a bare "ses_*" is the same thing unprefixed
//
// The opencode namespace does not guarantee a backend id behind it: the MCP
// layer prefixes whatever session id the client reports (server.go
// resolveClientSessionID) and the proxy prefixes its own, so daemon-spawned
// opencode agents also produce "opencode:<synthetic-uuid>" — the same namespace
// around the daemon's session uuid, until the backend "ses_*" id is captured.
// Both are the same kind of address; neither may be assumed to look like a
// backend id.
//
// Plus named channels, which are plain literals: "orchestrator",
// "telegram-bridge-48631", and "daemon:<subsystem>" — the address every
// daemon-originated notice carries (internal/daemon/daemon_sender.go). A daemon
// address names no session: it is never expanded into an address set, and
// handleSendTo skips its implicit ACK for it.
//
// Addresses are stored canonicalised (SendChannelMessage) and matched exactly
// against the reader's address set (ChannelAddresses) — never verbatim-vs-anything.

// minAddressPrefix is the shortest truncated id that may be resolved to an agent.
const minAddressPrefix = 8

// sesIDLength is the length of a complete opencode session id: "ses_" + 26 chars.
const sesIDLength = 30

// maxAmbiguityCandidates bounds the candidate list in an ambiguity error.
const maxAmbiguityCandidates = 5

// CanonicalAddress normalises a raw channel address into its stored form:
// "claude:<uuid>" loses its prefix, a bare "ses_*" gains the opencode
// namespace, named channels and bare uuids stay as they are.
func CanonicalAddress(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if bare, ok := strings.CutPrefix(s, "claude:"); ok {
		return bare
	}
	if strings.HasPrefix(s, "opencode:") || strings.HasPrefix(s, "codex:") {
		return s
	}
	if strings.HasPrefix(s, "ses_") {
		return "opencode:" + s
	}
	return s
}

// AgentAddresses returns every valid channel address of an agent: the synthetic
// daemon session id plus the prefixed backend id of each captured backend.
// Each entry is canonicalised, so a session id that already looks like a backend
// id (present in production) yields the same address as AgentCanonicalAddress.
func AgentAddresses(a *Agent) []string {
	if a == nil {
		return nil
	}
	var out []string
	addAddress(&out, CanonicalAddress(a.SessionID))
	if a.OpencodeSessionID != "" {
		addAddress(&out, "opencode:"+a.OpencodeSessionID)
	}
	if a.CodexSessionID != "" {
		addAddress(&out, "codex:"+a.CodexSessionID)
	}
	return out
}

// LearningSessionKeys returns every session_id form the agent's learnings can be
// keyed under: each channel address, its bare form, and the backend namespace
// around the synthetic session id (the MCP and proxy layers prefix whatever id
// the client reports, so "opencode:<daemon uuid>" is a real key as well).
func LearningSessionKeys(a *Agent) []string {
	var out []string
	for _, addr := range AgentAddresses(a) {
		addAddress(&out, addr)
		addAddress(&out, stripAgentPrefix(addr))
	}
	if a.SessionID != "" && !strings.Contains(a.SessionID, ":") {
		if ns := agentNamespace(a.Backend); ns != "" {
			addAddress(&out, ns+":"+a.SessionID)
		}
	}
	return out
}

// agentNamespace is the address namespace a backend's sessions carry. Claude has
// none: a "claude:" prefix is stripped rather than stored.
func agentNamespace(backend string) string {
	switch backend {
	case "opencode", "codex":
		return backend
	}
	return ""
}

// AgentCanonicalAddress returns the one address messages for this agent are
// stored under. The backend id wins because that is the address the agent
// itself reports (whoami) and the one its proxy shows.
func AgentCanonicalAddress(a *Agent) string {
	if a == nil {
		return ""
	}
	if a.OpencodeSessionID != "" {
		return CanonicalAddress("opencode:" + a.OpencodeSessionID)
	}
	if a.CodexSessionID != "" {
		return CanonicalAddress("codex:" + a.CodexSessionID)
	}
	return CanonicalAddress(a.SessionID)
}

// ChannelAddresses expands a session id into every address that must be matched
// when reading messages for it. The caller-supplied address is always part of
// the set — an agent whose backend column is not captured yet stays reachable
// under the address it came in with.
func (s *Store) ChannelAddresses(sessionID string) []string {
	var out []string
	addAddress(&out, CanonicalAddress(sessionID))

	// Prefer the running row. After a restart, several historical rows can share
	// one backend address; merging their synthetic session ids would let this
	// session read — and acknowledge — messages addressed to a different one.
	a, err := s.AgentGetBySession(sessionID)
	if err != nil || a == nil {
		a, _ = s.AgentGetAnyBySession(sessionID)
	}
	if a == nil {
		// An agent id is a valid send target, so it has to be a readable address too.
		a, _ = s.AgentGet(sessionID)
	}
	if a != nil {
		for _, addr := range AgentAddresses(a) {
			addAddress(&out, addr)
		}
	}
	return out
}

// ResolveChannelTarget turns a send_to target into the address a message is
// stored under. Named channels pass through literally. An id-like target is
// resolved against the known agents: an exact hit wins, otherwise a unique
// truncated (>= minAddressPrefix) hit or agent id is used, and anything else is
// refused instead of being black-holed.
func (s *Store) ResolveChannelTarget(target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", fmt.Errorf("target required")
	}
	if isTruncatedNamespace(t) {
		return "", fmt.Errorf("target %q is a truncated namespace, not an address — use the full id or an agent id", t)
	}
	canonical := CanonicalAddress(t)
	if !looksLikeChannelID(t) {
		return canonical, nil
	}

	agents, err := s.AgentList("")
	if err != nil {
		return "", err
	}
	key := stripAgentPrefix(canonical)

	// Exact hits are precedence, not correctness: collapseMatches already drops
	// the restart duplicates that make a shared backend address look like several
	// candidates. The phase only decides the tie when an exact key is also a
	// prefix of another agent's key (0 such keys in the live registry).
	var exact []Agent
	for i := range agents {
		if agentHasAddress(&agents[i], key) {
			exact = append(exact, agents[i])
		}
	}
	if addr, ok, err := collapseMatches(exact, t); ok || err != nil {
		return addr, err
	}

	var prefixed []Agent
	for i := range agents {
		if agentMatchesKey(&agents[i], key) {
			prefixed = append(prefixed, agents[i])
		}
	}
	if addr, ok, err := collapseMatches(prefixed, t); ok || err != nil {
		return addr, err
	}

	// No agent claims the target. A complete address names a session outright —
	// nothing to resolve; a truncated form would be a dead address, so refuse it.
	if isCompleteAddress(canonical) {
		return canonical, nil
	}
	return "", fmt.Errorf("no known agent for target %q (checked session_id, opencode_session_id, codex_session_id)", t)
}

// collapseMatches reduces matched agents to their distinct canonical addresses.
// ok=false means "no match"; err lists the candidates when several different
// identities match.
func collapseMatches(matches []Agent, target string) (string, bool, error) {
	if len(matches) == 0 {
		return "", false, nil
	}
	seen := make(map[string]bool, len(matches))
	var addresses []string
	var listed []string
	for i := range matches {
		addr := AgentCanonicalAddress(&matches[i])
		if seen[addr] {
			continue
		}
		seen[addr] = true
		addresses = append(addresses, addr)
		listed = append(listed, fmt.Sprintf("%s=%s", matches[i].ID, addr))
	}
	if len(addresses) == 1 {
		return addresses[0], true, nil
	}
	if len(listed) > maxAmbiguityCandidates {
		listed = append(listed[:maxAmbiguityCandidates], fmt.Sprintf("… (+%d more)", len(listed)-maxAmbiguityCandidates))
	}
	return "", false, fmt.Errorf("ambiguous target %q — candidates: %s", target, strings.Join(listed, ", "))
}

// agentHasAddress reports whether key is one of the agent's addresses or its id.
func agentHasAddress(a *Agent, key string) bool {
	if key == "" {
		return false
	}
	return containsString(agentKeys(a), key)
}

// agentMatchesKey additionally accepts truncated addresses — but only from
// minAddressPrefix characters on, so an 8-character namespace token cannot act
// as a catch-all.
func agentMatchesKey(a *Agent, key string) bool {
	if key == "" {
		return false
	}
	for _, k := range agentKeys(a) {
		if addrMatchesKey(k, key) {
			return true
		}
	}
	return false
}

// agentKeys returns every comparison form of an agent's identity: the agent id
// and each address in both its namespaced and its bare form — a target may name
// either ("opencode:ses_X" and "ses_X" are the same session).
func agentKeys(a *Agent) []string {
	var out []string
	addAddress(&out, a.ID)
	for _, addr := range AgentAddresses(a) {
		addAddress(&out, addr)
		addAddress(&out, stripAgentPrefix(addr))
	}
	return out
}

func addrMatchesKey(addr, key string) bool {
	if addr == "" || key == "" {
		return false
	}
	if addr == key {
		return true
	}
	return len(key) >= minAddressPrefix && strings.HasPrefix(addr, key)
}

// isTruncatedNamespace catches the stubs the old send_to advertised
// (handleSendTo returned target[:8], so callers learned to address "opencode"
// and friends). They are neither a channel nor an address.
func isTruncatedNamespace(s string) bool {
	switch s {
	case "opencode", "codex", "claude", "opencode:", "codex:", "claude:":
		return true
	}
	return false
}

// LooksLikeChannelID reports whether s is an id (session, agent, backend
// address) rather than a named channel. Exported for the daemon's implicit-ACK
// trust decision; see looksLikeChannelID.
func LooksLikeChannelID(s string) bool { return looksLikeChannelID(s) }

// looksLikeChannelID separates ids from named channels ("orchestrator",
// "telegram-bridge-48631"). Only ids get resolved at send time. The heuristic
// is deliberately shallow: a namespace token, a "ses_"/"agent-" prefix, or a
// bare hex/uuid fragment of at least minAddressPrefix characters.
func looksLikeChannelID(s string) bool {
	if s == "" {
		return false
	}
	for _, prefix := range []string{"opencode:", "codex:", "claude:", "ses_", "agent-"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	if len(s) < minAddressPrefix {
		return false
	}
	for _, r := range s {
		if !isHexRune(r) && r != '-' {
			return false
		}
	}
	return true
}

// isCompleteAddress reports whether s names a session unambiguously without
// consulting the agent registry: a full uuid or a full backend id. Everything
// shorter is a truncated form and must resolve against the registry — agent ids
// included, since those are registry identifiers too.
func isCompleteAddress(s string) bool {
	key := stripAgentPrefix(s)
	if isUUID(key) {
		return true
	}
	return strings.HasPrefix(key, "ses_") && len(key) >= sesIDLength
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexRune(r) {
				return false
			}
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

func addAddress(out *[]string, addr string) {
	if addr == "" || containsString(*out, addr) {
		return
	}
	*out = append(*out, addr)
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// sqlPlaceholders returns "?,?,?" for n bind values.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func toArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}
