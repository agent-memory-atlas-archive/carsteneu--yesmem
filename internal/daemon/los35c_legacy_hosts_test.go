package daemon

import (
	"strings"
	"testing"
)

// YM-346/OPS-85, key 1: the bundled caps now name allowed_hosts so they keep
// their egress once ai-jail 2.x is live. The host this runs on still has 0.9.2,
// whose flag set has neither --allow-host nor --no-network — there, the field must
// change nothing at all. This pins both halves: inert under 0.x, one --allow-host
// per declared host under 2.x.
func TestLos35cAllowedHostsAreInertUnderLegacyAiJail(t *testing.T) {
	stubAiJailVersion(t, 0)

	withHosts := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		AllowedHosts: []string{"api.telegram.org", "chat.papoo-service.de"},
	}}
	withoutHosts := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{}}

	got := strings.Join(withHosts.sandboxFlags(ProfileStandard), " ")
	want := strings.Join(withoutHosts.sandboxFlags(ProfileStandard), " ")
	if got != want {
		t.Errorf("an allowlist must not change the 0.9.x flag set:\n with: %s\n without: %s", got, want)
	}
	for _, foreign := range []string{"--allow-host", "--no-network"} {
		if strings.Contains(got, foreign) {
			t.Errorf("0.9.x does not know %s, it must not be rendered: %s", foreign, got)
		}
	}
}

func TestLos35cAllowedHostsBecomeAllowHostFlagsOn2x(t *testing.T) {
	stubAiJailVersion(t, 2)

	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		AllowedHosts: []string{"api.telegram.org", "chat.papoo-service.de"},
	}}
	flags := strings.Join(s.sandboxFlags(ProfileStandard), " ")
	if n := strings.Count(flags, "--allow-host "); n != 2 {
		t.Errorf("expected one --allow-host per declared host, got %d in %s", n, flags)
	}
	// An unset list means no egress, so a cap that declares none keeps --no-network
	// without an exception: the bundled repl tools must not leak out on 2.x.
	bare := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{}}
	bareFlags := strings.Join(bare.sandboxFlags(ProfileStandard), " ")
	if strings.Contains(bareFlags, "--allow-host") {
		t.Errorf("no declared host, no exception: %s", bareFlags)
	}
	if !strings.Contains(bareFlags, "--no-network") {
		t.Errorf("2.x standard must stay deny-by-default: %s", bareFlags)
	}
}
