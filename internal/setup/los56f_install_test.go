package setup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/carsteneu/yesmem/internal/update"
)

// ── YM-313 (Los 56f): copyBinary atomar und unter dem Update-Lock ───────────

// copyBinary used to write through the target's inode. An existing copy of the
// binary under another name shares that inode, and a running yesmem (the
// daemon, the proxy, an MCP child) has it mapped: the write either fails with
// ETXTBSY in the middle or leaves a truncated binary behind. A rename installs
// the new bytes without ever touching the old inode.
func TestLos56fCopyBinaryReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "yesmem-new")
	if err := os.WriteFile(src, []byte("new-binary"), 0755); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst := filepath.Join(dir, "yesmem")
	if err := os.WriteFile(dst, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	// A second name for the installed inode, like the .bak copy the updater
	// keeps and the mapping of a running process.
	witness := filepath.Join(dir, "yesmem.bak")
	if err := os.Link(dst, witness); err != nil {
		t.Fatalf("link witness: %v", err)
	}

	if err := copyBinary(src, dst); err != nil {
		t.Fatalf("copyBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("dst = %q, want the new binary", got)
	}
	// The old inode is what a running process still maps. It must survive the
	// install untouched, otherwise the copy was made in place.
	old, err := os.ReadFile(witness)
	if err != nil {
		t.Fatalf("read witness: %v", err)
	}
	if string(old) != "old-binary" {
		t.Errorf("the installed inode was overwritten in place: witness = %q", old)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("dst mode = %o, want 0755", info.Mode().Perm())
	}
}

// A running daemon and proxy keep the inode they mapped. Before this copy went
// through a rename, the in-place write failed with ETXTBSY, the setup stopped both
// services to get past it, and the later `systemctl --user start` brought the new
// binary up. Without the restart the rename succeeds and the old inode keeps
// running next to a new file on disk.
func TestLos56fSetupSwitchesRunningServicesToTheNewBinary(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the service switch is a systemd path")
	}
	prev := systemctlRun
	t.Cleanup(func() { systemctlRun = prev })
	var got [][]string
	systemctlRun = func(_ context.Context, args ...string) error {
		got = append(got, args)
		return nil
	}

	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "yesmem")
	if err := os.WriteFile(src, []byte("new-binary"), 0755); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dest := ensurePermanentLocation(home, src)
	installed, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(installed) != "new-binary" {
		t.Fatalf("installed binary = %q", installed)
	}

	if len(got) != 1 {
		t.Fatalf("systemctl called %d times, want exactly one try-restart", len(got))
	}
	joined := strings.Join(got[0], " ")
	for _, want := range []string{"--user", "try-restart", "yesmem.service", "yesmem-proxy.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemctl invocation %q does not name %q", joined, want)
		}
	}
}

// The setup copy and the updater write the same path. Without the shared lock a
// setup running next to `yesmem update` or `make deploy` replaces a binary the
// other side is mid-way through.
func TestLos56fCopyBinaryRefusesWhileUpdateRuns(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "yesmem-new")
	if err := os.WriteFile(src, []byte("new-binary"), 0755); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst := filepath.Join(dir, "yesmem")
	if err := os.WriteFile(dst, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	// The lock file the updater and make deploy take, held here the way they hold
	// it. A separate open file description conflicts with copyBinary's own even
	// inside one process, because flock hangs on the description.
	held, err := os.OpenFile(filepath.Join(dir, update.UpdateLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take update lock: %v", err)
	}

	if err := copyBinary(src, dst); err == nil {
		t.Fatal("copyBinary replaced the binary while an update held the lock")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "old-binary" {
		t.Errorf("dst = %q, the refused copy changed the binary", got)
	}
}
