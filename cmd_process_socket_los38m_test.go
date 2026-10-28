package main

import (
	"net"
	"path/filepath"
	"testing"
)

// TestDaemonStillAnswers is the guard that keeps `yesmem restart` from starting a
// second daemon on the same database when the old one could not be verified (no
// /proc). It has to see a live socket and must not trip over an absent one.
func TestDaemonStillAnswers(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "daemon.sock")
	if daemonStillAnswers(sockPath) {
		t.Fatal("an absent socket reported a listener")
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if !daemonStillAnswers(sockPath) {
		t.Fatal("a listening socket was not recognised")
	}
}
