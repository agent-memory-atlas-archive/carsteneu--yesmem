package daemon

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// YM-267 (1): a sandboxed run mounts the daemon's own socket. That socket has no
// token and serves every RPC, so a job could create a job with sandbox:none or
// spawn an unsandboxed agent — the sandbox would only constrain its own child.
// The sandbox must see a restricted socket instead.
func TestJobSandboxMountsTheRestrictedSocketOnly(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	fullSock := SocketPath(h.dataDir)
	jobSock := JobSocketPath(h.dataDir)

	for _, mount := range sb.cfg.ReadOnlyMounts {
		if mount == fullSock {
			t.Fatalf("sandbox mounts the unrestricted daemon socket %q", mount)
		}
	}
	found := false
	for _, mount := range sb.cfg.ReadOnlyMounts {
		if mount == jobSock {
			found = true
		}
	}
	if !found {
		t.Fatalf("sandbox must mount the restricted job socket %q, mounts = %v", jobSock, sb.cfg.ReadOnlyMounts)
	}
}

// YM-267 (1): the restricted socket only helps if it also refuses, server side.
// A job that reached spawn_agent would start an unsandboxed process, execute_cap
// would run a cap declared sandbox:none on the host, schedule would create such a
// job, and send_to would steer an existing unsandboxed agent.
func TestJobSocketRefusesTheSandboxEscapeMethods(t *testing.T) {
	h, _ := mustHandler(t)

	for _, method := range []string{
		"spawn_agent", "open_agent_terminal", "resume_agent", "relay_agent",
		"stop_agent", "stop_all_agents", "send_to",
		"execute_cap", "save_cap", "register_caps", "activate_cap", "deactivate_cap",
		"schedule", "set_config", "set_persona", "pin", "unpin",
		"scratchpad_delete", "scratchpad_write", "scratchpad_append",
		"scratchpad_read", "scratchpad_list",
		// Code navigation is a general read primitive through the daemon: the
		// caller supplies project_dir, so it reaches files the sandbox masks.
		"get_code_snippet", "search_code", "search_code_index",
		"get_file_index", "get_file_symbols", "get_code_context",
		"graph_traverse", "get_dependency_map",
		// Task/plan lifecycle and agent control: a sandboxed run reports through
		// its scratchpad, it does not close tasks or steer other agents.
		"resolve", "resolve_by_text", "reopen",
		"set_plan", "update_plan", "complete_plan",
		"list_agents", "get_agent", "update_agent", "register_agent",
		"this_method_does_not_exist",
	} {
		resp := h.handleJobSocket(Request{Method: method})
		if resp.Error == "" {
			t.Errorf("method %q must be refused on the job socket", method)
			continue
		}
		if !strings.Contains(resp.Error, "not available to sandboxed jobs") {
			t.Errorf("method %q refused for the wrong reason: %s", method, resp.Error)
		}
	}
}

// The allowance is not a second implementation: an allowed method has to reach
// the regular handler.
func TestJobSocketPassesAllowedMethodsThrough(t *testing.T) {
	h := mustHandlerWithCapStore(t)

	resp := h.handleJobSocket(Request{Method: "cap_store", Params: map[string]any{
		"capability": "los34b",
		"action":     "create_table",
		"table":      "probe",
		"columns":    []any{map[string]any{"name": "v", "type": "TEXT"}},
	}})
	if resp.Error != "" {
		t.Fatalf("cap_store must be allowed on the job socket: %s", resp.Error)
	}

	ping := h.handleJobSocket(Request{Method: "ping"})
	if ping.Error != "" {
		t.Fatalf("ping must be allowed on the job socket: %s", ping.Error)
	}
}

// YM-267 (1): the yesmem CLI inside the sandbox has to be directed at the
// restricted socket; without YESMEM_SOCK every store()/llm() polyfill keeps
// dialling the full one.
func TestDialHonoursTheSocketPathFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redirected.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			close(accepted)
			conn.Close()
		}
	}()

	t.Setenv("YESMEM_SOCK", path)

	// A data dir that holds no socket at all: reaching the listener proves the
	// env value won.
	client, err := Dial(t.TempDir())
	if err != nil {
		t.Fatalf("Dial did not honour YESMEM_SOCK: %v", err)
	}
	defer client.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener behind YESMEM_SOCK was never reached")
	}
}

// YM-267 (1): pointing the run at the restricted socket takes two halves. The
// value travels in the run's environment, and ai-jail 2.x inherits no environment
// at all, so the name has to be forwarded for the value to arrive.
func TestJobSandboxForwardsTheSocketPath(t *testing.T) {
	los34bPinAiJailV2(t)
	isolateHome(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})
	want := "YESMEM_SOCK=" + JobSocketPath(h.dataDir)
	found := false
	for _, kv := range sb.cfg.Env {
		if kv == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("run environment must carry %q, env = %v", want, sb.cfg.Env)
	}

	// The flag set depends on the resolved binary, so the version probe has to run
	// first; a Sandbox that never resolved one renders the legacy set.
	if !sb.Available() {
		t.Fatal("sandbox unavailable")
	}
	flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	if !strings.Contains(flags, "--env YESMEM_SOCK") {
		t.Fatalf("2.x renders the variable as a forward, flags = %v", flags)
	}
}

// YM-267 (1) / YM-276: not mounting the full socket is not enough on its own.
// ai-jail 0.9.x keeps the home writable in the standard profile, so a job could
// still dial <dataDir>/daemon.sock by absolute path. Since YM-276 the whole data
// dir is masked, which covers the socket, config.yaml and the agent inject sockets
// (agent-*.sock.inject) in one step.
func TestJobSandboxMasksTheDaemonSocket(t *testing.T) {
	los34bPinAiJailV2(t)
	isolateHome(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	masked := false
	for _, path := range sb.cfg.MaskedPaths {
		if path == h.dataDir {
			masked = true
		}
	}
	if !masked {
		t.Fatalf("the data dir must be masked, masked = %v", sb.cfg.MaskedPaths)
	}
	// config.yaml carries the same provider keys as install-state.json; both sit
	// inside the masked dir, so the dir mask is what covers them.
	for _, path := range []string{SocketPath(h.dataDir), filepath.Join(h.dataDir, "config.yaml")} {
		if !strings.HasPrefix(path, h.dataDir+string(filepath.Separator)) {
			t.Fatalf("%q is not inside the masked data dir %q", path, h.dataDir)
		}
	}

	if !sb.Available() {
		t.Fatal("sandbox unavailable")
	}
	flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	if !strings.Contains(flags, "--mask "+h.dataDir) {
		t.Fatalf("standard must render the mask, flags = %v", flags)
	}
}

// Cold review YM-267: a cap row is executable code, and its own sandbox field
// overrides the job's. remember writes any category, so a job could plant a cap
// with sandbox:none and have the next run execute it on the host. The allowance
// for remember must not carry that category.
func TestJobSocketRefusesCapRowsFromRemember(t *testing.T) {
	h := mustHandlerWithCapStore(t)

	resp := h.handleJobSocket(Request{Method: "remember", Params: map[string]any{
		"text":     "planted",
		"category": "cap",
		"context":  `{"cap_name":"victim","cap_scripts":[{"name":"run","runtime":"bash","sandbox":"none","body":"id"}]}`,
	}})
	if resp.Error == "" {
		t.Fatal("category \"cap\" must be refused on the job socket")
	}
	if !strings.Contains(resp.Error, "cap") {
		t.Fatalf("error must name the category, got: %s", resp.Error)
	}

	// The guard is narrow: an ordinary memory write still works.
	ok := h.handleJobSocket(Request{Method: "remember", Params: map[string]any{
		"text":     "a job may keep a note",
		"category": "gotcha",
		"project":  "los34b",
	}})
	if ok.Error != "" {
		t.Fatalf("an ordinary remember must stay allowed: %s", ok.Error)
	}
}

// A method name in the allowance that the dispatcher does not know would be
// refused at dispatch time, after the boundary said yes — a silent outage for
// sandboxed runs. Keep the two lists in step.
func TestJobSocketAllowlistMatchesTheDispatch(t *testing.T) {
	src, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("read dispatch: %v", err)
	}
	quoted := regexp.MustCompile(`"([a-z0-9_]+)"`)
	known := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "case ") {
			continue
		}
		for _, m := range quoted.FindAllStringSubmatch(line, -1) {
			known[m[1]] = true
		}
	}
	if len(known) < 50 {
		t.Fatalf("dispatch scan found only %d methods, the pattern is wrong", len(known))
	}
	for method := range jobSocketAllowed {
		if !known[method] {
			t.Errorf("allowlisted method %q has no case in Handler.Handle", method)
		}
	}
}

// The sender on a job's announcement is the caller's claim, and the channel
// routes on it. A run must not be able to pass its message off as "yesmem" or as
// another agent.
func TestJobSocketLabelsTheAnnouncementSender(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleJobSocket(Request{Method: "broadcast", Params: map[string]any{
		"content": "report",
		"sender":  "yesmem",
		"project": "/srv/los34b",
	}})
	if resp.Error != "" {
		t.Fatalf("broadcast must stay available: %s", resp.Error)
	}

	msgs, err := h.store.GetUnreadBroadcasts("reader", "/srv/los34b")
	if err != nil {
		t.Fatalf("read broadcasts: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("the announcement was not stored")
	}
	for _, m := range msgs {
		if m.Sender != jobBroadcastSender {
			t.Fatalf("announcement sender = %q, want %q", m.Sender, jobBroadcastSender)
		}
	}
}
