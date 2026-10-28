package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// YM-338 (Los 24d): a failed chmod on the socket was a hard start error. On a
// filesystem that does not support chmod (some FUSE and network mounts) that made
// the daemon unstartable, although the socket lives in a 0700 data directory.
func TestSocketServerSurvivesAFileSystemWithoutChmod(t *testing.T) {
	sockPath := filepath.Join(shortSockDir(t), "los24d-nofchmod.sock")

	previous := chmodSocket
	chmodSocket = func(string, os.FileMode) error { return syscall.EPERM }
	t.Cleanup(func() { chmodSocket = previous })

	srv, err := NewSocketServerAt(sockPath, func(Request) Response { return Response{} })
	if err != nil {
		t.Fatalf("NewSocketServerAt on a filesystem without chmod: %v", err)
	}
	defer srv.Close()

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file missing after a refused chmod: %v", err)
	}
}
