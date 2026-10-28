package storage

import (
	"fmt"
	"strings"
	"testing"
)

const (
	testClaudeSession = "a542b034-d69d-4e10-9553-a549eefa4d77"
	testAgentSession  = "69eb159f-b570-447a-b070-d96c65e67812"
	testOpencodeSes   = "ses_ebfd8cca4ffe51n2AGcKG04Gqw"
	testCodexThread   = "7f2c1b44-0000-4111-8222-333344445555"
)

func TestCanonicalAddress(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"padded", "  opencode:ses_A  ", "opencode:ses_A"},
		{"opencode prefixed kept", "opencode:ses_A", "opencode:ses_A"},
		{"codex prefixed kept", "codex:7f2c1b44", "codex:7f2c1b44"},
		{"claude prefix stripped", "claude:" + testClaudeSession, testClaudeSession},
		{"bare ses_ gets opencode namespace", "ses_A", "opencode:ses_A"},
		{"bare uuid unchanged", testClaudeSession, testClaudeSession},
		{"agent id unchanged", "agent-20261015-13", "agent-20261015-13"},
		{"named channel unchanged", "orchestrator", "orchestrator"},
		{"hyphenated channel unchanged", "telegram-bridge-48631", "telegram-bridge-48631"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanonicalAddress(c.in); got != c.want {
				t.Errorf("CanonicalAddress(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestAgentAddresses(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  []string
	}{
		{
			name:  "claude has only the synthetic session id",
			agent: Agent{ID: "a1", SessionID: testClaudeSession},
			want:  []string{testClaudeSession},
		},
		{
			name:  "opencode adds the prefixed backend id",
			agent: Agent{ID: "a2", SessionID: testAgentSession, OpencodeSessionID: testOpencodeSes},
			want:  []string{testAgentSession, "opencode:" + testOpencodeSes},
		},
		{
			name:  "codex adds the prefixed backend id",
			agent: Agent{ID: "a3", SessionID: testAgentSession, CodexSessionID: testCodexThread},
			want:  []string{testAgentSession, "codex:" + testCodexThread},
		},
		{
			name:  "both backends",
			agent: Agent{ID: "a4", SessionID: testAgentSession, CodexSessionID: testCodexThread, OpencodeSessionID: testOpencodeSes},
			want:  []string{testAgentSession, "opencode:" + testOpencodeSes, "codex:" + testCodexThread},
		},
		{
			name:  "empty session id contributes nothing",
			agent: Agent{ID: "a5"},
			want:  nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AgentAddresses(&c.agent)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("AgentAddresses = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAgentCanonicalAddress(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  string
	}{
		{"claude falls back to synthetic id", Agent{SessionID: testClaudeSession}, testClaudeSession},
		{"opencode wins", Agent{SessionID: testAgentSession, OpencodeSessionID: testOpencodeSes}, "opencode:" + testOpencodeSes},
		{"codex wins over synthetic", Agent{SessionID: testAgentSession, CodexSessionID: testCodexThread}, "codex:" + testCodexThread},
		{"opencode wins over codex", Agent{SessionID: testAgentSession, CodexSessionID: testCodexThread, OpencodeSessionID: testOpencodeSes}, "opencode:" + testOpencodeSes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AgentCanonicalAddress(&c.agent); got != c.want {
				t.Errorf("AgentCanonicalAddress = %q, want %q", got, c.want)
			}
		})
	}
}

func TestChannelAddresses(t *testing.T) {
	s := mustOpen(t)
	if err := s.AgentCreate(Agent{ID: "oc-1", Project: "p", Section: "s", SessionID: testAgentSession, OpencodeSessionID: testOpencodeSes, Status: "running"}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// Addressed by the backend id: the synthetic session id must be part of the set.
	got := s.ChannelAddresses("opencode:" + testOpencodeSes)
	if !contains(got, "opencode:"+testOpencodeSes) || !contains(got, testAgentSession) {
		t.Errorf("ChannelAddresses(backend id) = %v, want both %q and %q", got, "opencode:"+testOpencodeSes, testAgentSession)
	}

	// Addressed by the synthetic id: the backend address must be part of the set.
	got = s.ChannelAddresses(testAgentSession)
	if !contains(got, "opencode:"+testOpencodeSes) || !contains(got, testAgentSession) {
		t.Errorf("ChannelAddresses(synthetic id) = %v, want both %q and %q", got, "opencode:"+testOpencodeSes, testAgentSession)
	}

	// Unknown session: its own canonical address is the whole set.
	got = s.ChannelAddresses("opencode:ses_unknown")
	if len(got) != 1 || got[0] != "opencode:ses_unknown" {
		t.Errorf("ChannelAddresses(unknown) = %v, want [opencode:ses_unknown]", got)
	}
}

func TestResolveChannelTarget(t *testing.T) {
	s := mustOpen(t)
	agents := []Agent{
		{ID: "agent-20261015-13", Project: "p", Section: "s1", SessionID: testAgentSession, OpencodeSessionID: testOpencodeSes, Status: "running"},
		{ID: "agent-20261015-14", Project: "p", Section: "s2", SessionID: testClaudeSession, Status: "running"},
		// Agents 3 and 4 share the first eight characters of their synthetic id:
		// a short form addressing both must be refused, not guessed.
		{ID: "agent-20261015-15", Project: "p", Section: "s3", SessionID: "ebfd8cca-1111-4111-8111-111111111111", OpencodeSessionID: "ses_zzzother", Status: "running"},
		{ID: "agent-20261015-16", Project: "p", Section: "s4", SessionID: "ebfd8cca-2222-4222-8222-222222222222", OpencodeSessionID: "ses_yyyother", Status: "running"},
	}
	for _, a := range agents {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate(%s): %v", a.ID, err)
		}
	}

	cases := []struct {
		name    string
		target  string
		want    string
		wantErr string
	}{
		{name: "named channel stays literal", target: "orchestrator", want: "orchestrator"},
		{name: "hyphenated channel stays literal", target: "telegram-bridge-48631", want: "telegram-bridge-48631"},
		{name: "full opencode address resolves to itself", target: "opencode:" + testOpencodeSes, want: "opencode:" + testOpencodeSes},
		{name: "bare ses_ resolves to the opencode namespace", target: testOpencodeSes, want: "opencode:" + testOpencodeSes},
		{name: "unique short prefix of the backend id", target: testOpencodeSes[:9], want: "opencode:" + testOpencodeSes},
		{name: "unique short prefix of a claude uuid", target: testClaudeSession[:8], want: testClaudeSession},
		{name: "synthetic id resolves to the backend address", target: testAgentSession, want: "opencode:" + testOpencodeSes},
		{name: "agent id resolves to the canonical address", target: "agent-20261015-14", want: testClaudeSession},
		{name: "two agents share the prefix", target: "ebfd8cca", wantErr: "ambiguous"},
		{name: "short form without a match", target: "deadbeef", wantErr: "no known agent"},
		{name: "complete address without an agent row", target: "opencode:ses_0123456789abcdefghijklmnop", want: "opencode:ses_0123456789abcdefghijklmnop"},
		{name: "complete claude uuid without an agent row", target: "99887766-5544-4332-2110-ffeeddccbbaa", want: "99887766-5544-4332-2110-ffeeddccbbaa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.ResolveChannelTarget(c.target)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolveChannelTarget(%q) = %q, want error containing %q", c.target, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveChannelTarget(%q): %v", c.target, err)
			}
			if got != c.want {
				t.Errorf("ResolveChannelTarget(%q) = %q, want %q", c.target, got, c.want)
			}
		})
	}
}

func TestResolveChannelTarget_AmbiguityListsCandidates(t *testing.T) {
	s := mustOpen(t)
	for _, a := range []Agent{
		{ID: "agent-1", Project: "p", Section: "s1", SessionID: "11111111-1111-4111-8111-111111111111", OpencodeSessionID: "ses_ebfd8cca4ffeAAA", Status: "running"},
		{ID: "agent-2", Project: "p", Section: "s2", SessionID: "22222222-2222-4222-8222-222222222222", OpencodeSessionID: "ses_ebfd8cca4ffeBBB", Status: "running"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
	}
	_, err := s.ResolveChannelTarget("ses_ebfd8cca4ffe")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	for _, want := range []string{"agent-1", "agent-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list candidate %q", err.Error(), want)
		}
	}
}

func TestResolveChannelTarget_CompleteAddressSharedBySeveralRows(t *testing.T) {
	s := mustOpen(t)
	const oc = "ses_sharedsharedsharedsharedsh"
	const addr = "opencode:" + oc
	// A restarted agent leaves several rows under the same backend address.
	for _, a := range []Agent{
		{ID: "agent-old", Project: "p", Section: "s1", SessionID: "aaaa1111-0000-4000-8000-000000000001", OpencodeSessionID: oc, Status: "stopped", CreatedAt: "2026-01-01T00:00:00+01:00"},
		{ID: "agent-new", Project: "p", Section: "s2", SessionID: "bbbb2222-0000-4000-8000-000000000002", OpencodeSessionID: oc, Status: "running", CreatedAt: "2026-02-01T00:00:00+01:00"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate(%s): %v", a.ID, err)
		}
	}

	got, err := s.ResolveChannelTarget(addr)
	if err != nil {
		t.Fatalf("complete address of a restarted agent must resolve, got: %v", err)
	}
	if got != addr {
		t.Errorf("ResolveChannelTarget(%q) = %q, want %q", addr, got, addr)
	}

	got, err = s.ResolveChannelTarget(oc[:12])
	if err != nil {
		t.Fatalf("short form of a shared address must resolve to the one address: %v", err)
	}
	if got != addr {
		t.Errorf("ResolveChannelTarget(%q) = %q, want %q", oc[:12], got, addr)
	}
}

func TestResolveChannelTarget_TruncatedNamespaceRefused(t *testing.T) {
	s := mustOpen(t)
	// The old send_to advertised exactly these stubs (it returned target[:8]).
	for _, token := range []string{"opencode", "codex", "claude", "opencode:", "codex:", "claude:"} {
		if _, err := s.ResolveChannelTarget(token); err == nil {
			t.Errorf("ResolveChannelTarget(%q) must be refused as a truncated namespace", token)
		} else if !strings.Contains(err.Error(), "truncated namespace") {
			t.Errorf("ResolveChannelTarget(%q) error = %q, want a truncated-namespace message", token, err.Error())
		}
	}
}

func TestResolveChannelTarget_UnknownAgentIDRefused(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.ResolveChannelTarget("agent-20260101-99"); err == nil {
		t.Fatal("an unknown agent id must be refused, not stored verbatim")
	} else if !strings.Contains(err.Error(), "no known agent") {
		t.Errorf("error = %q, want it to name the missing agent", err.Error())
	}
}

func TestChannelAddresses_AgentIDIsReadable(t *testing.T) {
	s := mustOpen(t)
	const oc = "ses_agentidagentidagentidagent"
	const synthetic = "99998888-7777-4666-8555-444433332222"
	if err := s.AgentCreate(Agent{ID: "agent-byid", Project: "p", Section: "s", SessionID: synthetic, OpencodeSessionID: oc, Status: "running"}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// An agent id is a valid send target, so it has to be a readable address too.
	addrs := s.ChannelAddresses("agent-byid")
	if !contains(addrs, "opencode:"+oc) || !contains(addrs, synthetic) {
		t.Errorf("ChannelAddresses(agent id) = %v, want the agent's addresses", addrs)
	}
}

func TestChannelAddresses_PrefersRunningRow(t *testing.T) {
	s := mustOpen(t)
	const oc = "ses_duplicateduplicateduplicat"
	const runningSession = "cccc3333-0000-4000-8000-000000000003"
	const stoppedSession = "dddd4444-0000-4000-8000-000000000004"
	for _, a := range []Agent{
		{ID: "agent-run", Project: "p", Section: "s1", SessionID: runningSession, OpencodeSessionID: oc, Status: "running", CreatedAt: "2026-01-01T00:00:00+01:00"},
		{ID: "agent-stop", Project: "p", Section: "s2", SessionID: stoppedSession, OpencodeSessionID: oc, Status: "stopped", CreatedAt: "2026-02-01T00:00:00+01:00"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate(%s): %v", a.ID, err)
		}
	}

	addrs := s.ChannelAddresses("opencode:" + oc)
	if !contains(addrs, runningSession) {
		t.Errorf("address set = %v, want the running row's session id", addrs)
	}
	if contains(addrs, stoppedSession) {
		t.Errorf("address set = %v must not merge a stopped duplicate's session id — that would let one session read another's mail", addrs)
	}
}

func TestResolveChannelTarget_AmbiguityCandidatesAreCapped(t *testing.T) {
	s := mustOpen(t)
	// Seven agents whose synthetic ids share the same eight characters.
	for i := 0; i < 7; i++ {
		a := Agent{
			ID:        fmt.Sprintf("agent-cap-%d", i),
			Project:   "p",
			Section:   fmt.Sprintf("s%d", i),
			SessionID: fmt.Sprintf("cafe0000-0000-4000-8000-00000000000%d", i),
			Status:    "running",
		}
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
	}

	_, err := s.ResolveChannelTarget("cafe0000")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if !strings.Contains(err.Error(), "… (+2 more)") {
		t.Errorf("candidate list must be capped at %d with a remainder marker, got %q", maxAmbiguityCandidates, err.Error())
	}
}

func TestChannelAddresses_ClaudePrefixedLookup(t *testing.T) {
	s := mustOpen(t)
	const synthetic = "abcd1234-0000-4000-8000-00000000000c"
	if err := s.AgentCreate(Agent{ID: "agent-cl", Project: "p", Section: "s", SessionID: synthetic, Status: "running"}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// A "claude:"-prefixed address must resolve to the same session, not to an
	// address set without the agent's other addresses.
	addrs := s.ChannelAddresses("claude:" + synthetic)
	if !contains(addrs, synthetic) {
		t.Errorf("ChannelAddresses(claude:<uuid>) = %v, want it to include %q", addrs, synthetic)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Learnings of a daemon-spawned agent can be keyed under the backend namespace
// around its synthetic session id: the MCP and proxy layers prefix whatever id
// the client reports (see the address package header), so a quarantine that only
// knows the bare uuid misses them.
func TestLearningSessionKeys_CoversTheNamespacedSyntheticID(t *testing.T) {
	uuid := "11111111-2222-3333-4444-555555555555"

	cases := []struct {
		name  string
		agent Agent
		want  []string
	}{
		{
			name:  "opencode prefixes the synthetic id too",
			agent: Agent{SessionID: uuid, Backend: "opencode", OpencodeSessionID: testOpencodeSes},
			want:  []string{uuid, "opencode:" + testOpencodeSes, testOpencodeSes, "opencode:" + uuid},
		},
		{
			name:  "codex prefixes the synthetic id too",
			agent: Agent{SessionID: uuid, Backend: "codex", CodexSessionID: testCodexThread},
			want:  []string{uuid, "codex:" + testCodexThread, testCodexThread, "codex:" + uuid},
		},
		{
			name:  "claude has no namespace",
			agent: Agent{SessionID: uuid, Backend: "claude"},
			want:  []string{uuid},
		},
		{
			name:  "an already namespaced session id is not prefixed twice",
			agent: Agent{SessionID: "opencode:" + testOpencodeSes, Backend: "opencode"},
			want:  []string{"opencode:" + testOpencodeSes, testOpencodeSes},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LearningSessionKeys(&c.agent)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("LearningSessionKeys = %v, want %v", got, c.want)
			}
		})
	}
}
