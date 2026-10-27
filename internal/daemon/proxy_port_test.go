package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestProxyForwardPort(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{":9099", 9099},
		{"127.0.0.1:9099", 9099},
		{"localhost:9099", 9099},
		{"9099", 9099},
		{"", 9099}, // the proxy's own default
		{"0.0.0.0:1234", 1234},
		{"not-an-address", 0},
		{":0", 0},
		{":99999", 0},
	}
	for _, tc := range cases {
		if got := proxyForwardPort(tc.in); got != tc.want {
			t.Errorf("proxyForwardPort(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The relay is a 2.x-only flag: 0.9.x shares the host loopback and would not
// know it.
func TestSandboxFlags_ForwardPortsOnlyOn2x(t *testing.T) {
	cfg := SandboxConfig{ForwardPorts: []int{9099}}

	stubAiJailVersion(t, 2)
	v2 := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: cfg}
	_, args, err := v2.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap 2.x: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--forward-port 9099") {
		t.Errorf("2.x standard must relay the proxy port, got %v", args)
	}

	stubAiJailVersion(t, 0)
	legacy := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: cfg}
	_, args, err = legacy.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap 0.9.x: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--forward-port") {
		t.Errorf("0.9.x has no --forward-port, got %v", args)
	}
}

// A sandboxed agent reaches the LLM through the proxy on the host loopback. On
// 2.x that only works when the port is relayed into the sandbox.
func TestSandboxV2_AgentReachesProxyOnlyWithForwardPort(t *testing.T) {
	needsPython3(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("PROXY"))
			conn.Close()
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	probe := tcpProbeCommand(port)

	// Lockdown is the stricter profile here: a forwarded port has to compose
	// with it, because that is the profile an agent run may use.
	relayed := realAiJail2xSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		ForwardPorts:        []int{port},
	})
	out, code, err := relayed.RunWithProfile(probe, 30, ProfileStrict)
	assertLaunched(t, out)
	if err != nil || code != 0 {
		t.Fatalf("relayed run failed: exit=%d err=%v out=%q", code, err, out)
	}
	if !strings.Contains(out, "PROXY=REACHABLE") {
		t.Errorf("--forward-port must expose the proxy port, got %q", out)
	}

	// Same probe, same profile, no relay: this is what a 2.x upgrade without the
	// forward would leave a scheduled agent job with.
	plain := realAiJail2xSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})
	out, _, _ = plain.RunWithProfile(probe, 30, ProfileStrict)
	assertLaunched(t, out)
	if strings.Contains(out, "PROXY=REACHABLE") {
		t.Errorf("without a forward the sandbox must not reach the host loopback, got %q", out)
	}
}

func tcpProbeCommand(port int) string {
	return fmt.Sprintf(`python3 -c "import socket,sys
s=socket.socket(); s.settimeout(4)
try:
    s.connect(('127.0.0.1', %d)); print('PROXY=REACHABLE')
except Exception as e:
    print('PROXY=UNREACHABLE'); sys.exit(1)"`, port)
}
