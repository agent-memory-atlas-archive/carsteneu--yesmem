package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// An empty socket path must not produce a bridge. net.Listen("unix", "") binds
// an abstract socket without an error, so the bridge used to look healthy while
// no console could ever reach it.
func TestNewAgentBridge_RejectsEmptySocketPath(t *testing.T) {
	if _, err := NewAgentBridge("cat", nil, "", t.TempDir()); err == nil {
		t.Fatal("NewAgentBridge accepted an empty socket path")
	}
}

// The agent socket bridges the console to the PTY, and the inject socket writes
// straight into the agent's terminal: whoever can connect can type commands into
// an unsandboxed agent session. net.Listen applies the umask, so on a host with
// umask 002 both files land as 0775 and every local account can reach them
// (audit 16.10.2026, YM-328). They must be owner-only, like daemon.sock since
// YM-250, and every client runs as the same user.
func TestNewAgentBridge_SocketsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "agent-20261018-01.sock")

	b, err := NewAgentBridge("cat", nil, sock, dir)
	if err != nil {
		t.Fatalf("NewAgentBridge: %v", err)
	}
	defer func() {
		if b.Cmd.Process != nil {
			b.Cmd.Process.Kill()
		}
		b.Close()
	}()

	for _, path := range []string{sock, sock + ".inject"} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("%s mode = %04o, want no group/other bits (0600)", filepath.Base(path), got)
		}
	}
}
