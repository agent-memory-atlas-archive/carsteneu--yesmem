package daemon

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// YM-206 / OPS-41: ai-jail 2.x replaced the port concept with an explicit host
// allowlist and made lockdown genuinely offline. The flag set therefore has to
// follow the binary's version.
func stubAiJailVersion(t *testing.T, major int) {
	t.Helper()
	original := sandboxVersionProbe
	sandboxVersionProbe = func(string) int { return major }
	t.Cleanup(func() { sandboxVersionProbe = original })
}

func TestParseAiJailMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"ai-jail 2.10.0", 2},
		{"ai-jail 2.0.1\n", 2},
		{"ai-jail v2.9.0", 2},
		{"ai-jail 0.9.2", 0},
		{"ai-jail 1.4.0", 1},
		{"", aiJailUnknownVersion},
		{"not a version", aiJailUnknownVersion},
	}
	for _, tc := range cases {
		if got := parseAiJailMajor(tc.in); got != tc.want {
			t.Errorf("parseAiJailMajor(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// An unreadable version must not fall back to the flag set with the open network:
// only a version that reads as 0.x/1.x selects the legacy set.
func TestSandboxFlags_UnknownVersionUsesTheStricterSet(t *testing.T) {
	stubAiJailVersion(t, aiJailUnknownVersion)
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{ForwardPorts: []int{9099}}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--no-network") {
		t.Errorf("an unreadable version must not open the standard profile up, got %v", args)
	}
	if !strings.Contains(joined, "--forward-port 9099") {
		t.Errorf("the relay belongs to the stricter set, got %v", args)
	}

	stubAiJailVersion(t, 0)
	legacy := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{ForwardPorts: []int{9099}}}
	_, args, err = legacy.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap legacy: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--no-network") {
		t.Errorf("a real 0.x binary keeps the legacy set, got %v", args)
	}
}

// 2.x standard cannot use lockdown (2.x drops --map there, so the daemon socket
// would be unreachable) — it gets its strictness from the private home, the
// disabled agent state and toolchains, and a deny-by-default egress with an
// explicit host allowlist.
func TestSandboxFlagsV2_StandardKeepsSocketAndRestrictsEgress(t *testing.T) {
	stubAiJailVersion(t, 2)
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		MaskedPaths:    []string{"/home/u/.ssh"},
		ReadOnlyMounts: []string{"/home/u/.local/bin/yesmem", "/home/u/.claude/yesmem/daemon.sock"},
		AllowedHosts:   []string{"api.telegram.org"},
	}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--no-save-config",
		"--clean",
		"--no-network",
		"--allow-host api.telegram.org",
		"--no-agent-state",
		"--no-toolchains",
		"--landlock",
		"--seccomp",
		"--mask /home/u/.ssh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("2.x standard must pass %s, got %v", want, args)
		}
	}
	if strings.Contains(joined, "--lockdown") {
		t.Errorf("2.x standard must not lock down: 2.x ignores --map there, so the daemon socket would be gone, got %v", args)
	}
	if !strings.Contains(joined, "--map /home/u/.claude/yesmem/daemon.sock") {
		t.Errorf("2.x standard must mount the daemon socket read-only, got %v", args)
	}
	if !strings.Contains(joined, "--map /home/u/.local/bin/yesmem") {
		t.Errorf("2.x standard must mount the yesmem CLI read-only, got %v", args)
	}
	if strings.Contains(joined, "--rw-map") {
		t.Errorf("no writable mounts allowed, got %v", args)
	}
	if strings.Contains(joined, "--allow-tcp-port") {
		t.Errorf("2.x rejects --allow-tcp-port at launch, got %v", args)
	}
}

// 2.x strict is the offline profile: lockdown plus an explicit --no-network.
func TestSandboxFlagsV2_StrictLocksDownWithoutNetwork(t *testing.T) {
	stubAiJailVersion(t, 2)
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		MaskedPaths:    []string{"/home/u/.ssh"},
		ReadOnlyMounts: []string{"/home/u/.claude/yesmem/daemon.sock"},
		AllowedHosts:   []string{"api.telegram.org"},
	}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStrict)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--lockdown") || !strings.Contains(joined, "--no-network") {
		t.Errorf("2.x strict must lock down and disable the network, got %v", args)
	}
	if !strings.Contains(joined, "--clean") {
		t.Errorf("2.x strict must ignore a project .ai-jail, got %v", args)
	}
	for _, banned := range []string{"--map", "--mask", "--allow-host", "--rw-map"} {
		if strings.Contains(joined, banned) {
			t.Errorf("2.x strict adds nothing beyond lockdown, got %s in %v", banned, args)
		}
	}
}

// The 0.9.x flag set must stay exactly as it was: that is the fallback for the
// binary the host actually has.
func TestSandboxFlagsV09_UnchangedByTheTwoXSupport(t *testing.T) {
	stubAiJailVersion(t, 0)
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		MaskedPaths:    []string{"/home/u/.ssh"},
		ReadOnlyMounts: []string{"/home/u/.claude/yesmem/daemon.sock"},
		AllowedHosts:   []string{"api.telegram.org"},
	}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--no-network") || strings.Contains(joined, "--allow-host") {
		t.Errorf("0.9.x does not know the 2.x network flags, got %v", args)
	}
	if !strings.Contains(joined, "--map /home/u/.claude/yesmem/daemon.sock") {
		t.Errorf("0.9.x takes the bare map form, got %v", args)
	}
}

// fetchPinnedAiJail unpacks the pinned release into a temp dir. Never into
// ~/.local/bin: the host's installed binary is the host's business.
// requireBubblewrap skips when ai-jail cannot run at all: it refuses to start any
// profile without bubblewrap, and a runner image does not carry it. A runner can
// also carry bubblewrap and still be unable to create the namespaces it needs
// (Ubuntu 24.04 restricts unprivileged user namespaces through AppArmor), which
// surfaces as "bwrap: loopback: Failed RTM_NEWADDR". Both cases are skipped here
// instead of reported as a broken sandbox further down.
func requireBubblewrap(t *testing.T) {
	t.Helper()
	bin := os.Getenv("BWRAP_BIN")
	if bin == "" {
		found, err := exec.LookPath("bwrap")
		if err != nil {
			t.Skip("bubblewrap is not installed; ai-jail cannot run a profile without it")
		}
		bin = found
	}
	if out, err := exec.Command(bin, "--unshare-net", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot create a sandbox here: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// requireAiJailPlatform skips a real ai-jail run on a platform the pinned
// release has no build for. The daemon host is Linux; darwin is not a target.
func requireAiJailPlatform(t *testing.T) {
	t.Helper()
	asset, err := aiJailAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("ai-jail has no build for this platform: %v", err)
	}
	// The release names a darwin asset but pins no checksum for it, and the
	// installer refuses an unverified build.
	if _, ok := aiJailAssetDigests[asset]; !ok {
		t.Skipf("ai-jail v%s ships no pinned build for %s", aiJailPinnedVersion, asset)
	}
}

// requireLinuxSandbox skips an assertion about the sandbox as the daemon host
// runs it. The profiles are a Linux contract (systemd unit, ai-jail under
// bubblewrap); the same binary on darwin enforces different rules, so these
// assertions would pin the wrong behaviour there.
func requireLinuxSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the sandbox contract is asserted for its Linux host, not %s", runtime.GOOS)
	}
}

func fetchPinnedAiJail(t *testing.T) string {
	t.Helper()
	requireBubblewrap(t)
	if testing.Short() {
		t.Skip("needs the network to fetch the pinned ai-jail release")
	}
	assetName, err := aiJailAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no asset for this platform: %v", err)
	}
	url, err := resolveAiJailAssetURL(assetName)
	if err != nil {
		t.Skipf("cannot resolve the pinned ai-jail release: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), assetName)
	if err := downloadToFile(url, archivePath); err != nil {
		t.Skipf("cannot download ai-jail v%s: %v", aiJailPinnedVersion, err)
	}
	if got, err := fileSHA256(archivePath); err != nil || got != aiJailAssetDigests[assetName] {
		t.Fatalf("pinned %s has unexpected content (digest %s): %v", assetName, got, err)
	}

	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	binary := filepath.Join(t.TempDir(), "ai-jail")
	if err := extractAiJailFromTarGz(archive, binary); err != nil {
		t.Fatalf("extract pinned ai-jail: %v", err)
	}
	return binary
}

func realAiJail2xSandbox(t *testing.T, cfg SandboxConfig) *Sandbox {
	t.Helper()
	binary := fetchPinnedAiJail(t)

	original := sandboxLookPath
	sandboxLookPath = func(name string) (string, error) {
		if name == "ai-jail" {
			return binary, nil
		}
		return original(name)
	}
	t.Cleanup(func() { sandboxLookPath = original })
	t.Chdir(t.TempDir())

	// ai-jail also reads ~/.ai-jail. A host whose global config still carries the
	// 2.x-removed allow_tcp_ports fails every launch with "--allow-tcp-port is
	// disabled", so pin HOME and exercise our flags instead of the developer's
	// ai-jail configuration.
	t.Setenv("HOME", t.TempDir())

	s := NewSandbox(cfg)
	if !s.Available() {
		t.Skip("ai-jail not available, cannot exercise a real sandboxed run")
	}
	if major := s.aiJailMajor(); major < 2 {
		t.Fatalf("expected the pinned 2.x binary, probed major version %d", major)
	}
	return s
}

// assertLaunched fails when ai-jail refused the flag set instead of running the
// command. Without it a negative test (no socket, no network) would pass simply
// because the launch errored out.
func assertLaunched(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "✗") {
		t.Fatalf("ai-jail rejected the flag set: %q", out)
	}
}

// needsPython3 guards the socket probe below.
func needsPython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH, cannot speak to a Unix socket from the sandbox")
	}
}

// startUnixSocketServer listens on path and answers every connection with PONG.
func startUnixSocketServer(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("PONG"))
			conn.Close()
		}
	}()
}

// D13(3): a cap whose handler calls store() over the daemon socket has to keep
// working under standard — that is the rocketchat-poll shape. The socket lives
// outside the project, so it is only reachable because of the mount flag.
func TestSandboxV2_StandardKeepsTheDaemonSocketReachable(t *testing.T) {
	needsPython3(t)
	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "daemon.sock")
	startUnixSocketServer(t, sock)

	s := realAiJail2xSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		ReadOnlyMounts:      []string{sock},
	})

	out, code, err := s.RunWithProfile(socketProbeCommand(sock), 30, ProfileStandard)
	assertLaunched(t, out)
	if err != nil || code != 0 {
		t.Fatalf("standard must reach the daemon socket: exit=%d err=%v out=%q", code, err, out)
	}
	if !strings.Contains(out, "SOCKET=PONG") {
		t.Errorf("socket reply missing from the sandbox run: %q", out)
	}
}

// The contrast that makes the test above meaningful: under strict the mount is
// dropped by ai-jail 2.x, so the same probe runs and reports no socket.
func TestSandboxV2_StrictCannotReachTheDaemonSocket(t *testing.T) {
	needsPython3(t)
	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "daemon.sock")
	startUnixSocketServer(t, sock)

	s := realAiJail2xSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		ReadOnlyMounts:      []string{sock},
	})

	out, _, _ := s.RunWithProfile(socketProbeCommand(sock), 30, ProfileStrict)
	assertLaunched(t, out)
	if !strings.Contains(out, "SOCKET=UNREACHABLE") {
		t.Fatalf("the socket probe did not run inside the sandbox: %q", out)
	}
	if strings.Contains(out, "SOCKET=PONG") {
		t.Errorf("strict must not reach the host daemon socket: %q", out)
	}
}

// strict is the offline profile on 2.x — the promise 0.9.x could not keep, where
// lockdown left 22/80/443 open. Paired with the standard run below, so the pair
// cannot pass for the wrong reason on a host without connectivity.
func TestSandboxV2_StrictHasNoEgress(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	s := realAiJail2xSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})

	out, _, _ := s.RunWithProfile(egressProbeCommand(), 30, ProfileStrict)
	assertLaunched(t, out)
	if got := probeValue(t, out, "HTTP_CODE"); got != "000" {
		t.Errorf("strict must not reach the network, got http_code=%s in %q", got, out)
	}
}

// ... and the allowed host has to be reachable, otherwise the offline result
// above would be untrustworthy.
func TestSandboxV2_StandardReachesAnAllowedHost(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	s := realAiJail2xSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		AllowedHosts:        []string{"api.telegram.org"},
	})

	out, code, err := s.RunWithProfile(egressProbeCommand(), 30, ProfileStandard)
	assertLaunched(t, out)
	if err != nil || code != 0 {
		t.Fatalf("standard must reach an allowed host: exit=%d err=%v out=%q", code, err, out)
	}
	if got := probeValue(t, out, "HTTP_CODE"); got == "000" {
		t.Errorf("no HTTP response through the allowed host: %q", out)
	}
}

// YM-209: without --exec ai-jail prints a status banner to stderr ("Jail Active",
// "Landlock: fully enforced"), and runProcess merges stderr into the job output,
// so every bash job and every cap exec stored the banner as its result. --exec
// exists in 0.9.x and 2.x and keeps the exit code and stdin (verified live
// against 0.9.2 and the pinned 2.10.0).
func TestSandboxFlags_ExecKeepsStderrCleanInEveryFlagSet(t *testing.T) {
	for _, major := range []int{0, 2} {
		stubAiJailVersion(t, major)
		s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail"}
		for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
			bin, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, profile)
			if err != nil {
				t.Fatalf("major=%d profile=%s: wrap: %v", major, profile, err)
			}
			if bin != s.binaryPath {
				t.Fatalf("major=%d profile=%s: not wrapped: %q", major, profile, bin)
			}
			execIdx := -1
			for i, a := range args {
				if a == "--exec" {
					execIdx = i
				}
			}
			if execIdx < 0 {
				t.Errorf("major=%d profile=%s: missing --exec, the banner lands in the job output: %v", major, profile, args)
				continue
			}
			// It has to precede the command, otherwise ai-jail reads it as an argument.
			for i, a := range args {
				if a == "bash" && execIdx > i {
					t.Errorf("major=%d profile=%s: --exec after the command: %v", major, profile, args)
				}
			}
		}
	}
}

// YM-218: /tmp inside the jail is a throwaway tmpfs, so a cap log written there
// never reaches the host. This asserts the renderer; which profile gets a job dir
// at all is decided in bashSandboxConfig (strict cannot mount).
func TestSandboxFlags_JobDirIsMappedReadWrite(t *testing.T) {
	dir := t.TempDir()
	for _, major := range []int{0, 2} {
		stubAiJailVersion(t, major)
		s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{RWMapPaths: []string{dir}}}
		_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
		if err != nil {
			t.Fatalf("major=%d: wrap: %v", major, err)
		}
		if !containsString(args, "--rw-map") || !containsString(args, dir) {
			t.Errorf("major=%d: job dir not mapped read-write: %v", major, args)
		}
	}
}

// The job dir also has to reach the command as an environment variable, otherwise
// a cap cannot find it.
func TestSandboxRun_ExportsTheJobDir(t *testing.T) {
	dir := t.TempDir()
	s := &Sandbox{cfg: SandboxConfig{Env: []string{"YESMEM_JOB_DIR=" + dir}}}

	out, code, err := s.Run(`printf %s "$YESMEM_JOB_DIR"`, 10, ProfileNone)
	if err != nil || code != 0 {
		t.Fatalf("run: exit=%d err=%v out=%q", code, err, out)
	}
	if out != dir {
		t.Errorf("YESMEM_JOB_DIR = %q, want %q", out, dir)
	}
}

func socketProbeCommand(sock string) string {
	return `python3 -c "import socket,sys
s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
s.settimeout(4)
try:
    s.connect(sys.argv[1]); print('SOCKET=' + s.recv(16).decode().strip())
except Exception as e:
    print('SOCKET=UNREACHABLE'); sys.exit(1)" ` + sock
}

func egressProbeCommand() string {
	return `code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' https://api.telegram.org 2>/dev/null) || code=000
printf 'HTTP_CODE=%s\n' "${code:-000}"`
}
