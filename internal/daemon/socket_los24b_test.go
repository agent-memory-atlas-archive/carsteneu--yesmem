package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-250: both listeners come from net.Listen, which applies the process umask
// (measured 0775 on this repo's shell). The full socket answers write RPCs
// (remember, register_pid, schedule), so a writable socket in a traversable
// directory lets any local user drive them. A client runs as the same user, so
// owner-only mode costs nothing.
func TestSocketServerCreatesOwnerOnlySockets(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}

	paths := []string{SocketPath(dir), filepath.Join(jobDir, jobSockName)}
	for _, path := range paths {
		srv, err := NewSocketServerAt(path, func(Request) Response { return Response{} })
		if err != nil {
			t.Fatalf("listen %s: %v", path, err)
		}
		defer srv.Close()
	}

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %04o, want 0600 (no group/other access)", filepath.Base(path), perm)
		}
	}
}
