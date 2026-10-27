package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// los34bShortenHeadlessTimeout makes a headless run hit its deadline in the test.
func los34bShortenHeadlessTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := headlessJobTimeout
	headlessJobTimeout = d
	t.Cleanup(func() { headlessJobTimeout = original })
}

// YM-267 (7): runHeadlessCommand killed only the process it started. A run that
// backgrounds a child (`claude` does, `bash` scripts do) left that child alive
// past the deadline: an orphan that keeps writing into the job dir and keeps the
// sandbox alive. The deadline has to take down the whole process group.
func TestHeadlessDeadlineKillsTheProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	los34bShortenHeadlessTimeout(t, 900*time.Millisecond)

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "sleep 60 & echo $! > " + pidFile + "; wait"

	var stdout, stderr bytes.Buffer
	err := runHeadlessCommand("bash", []string{"-c", script}, "", t.TempDir(), nil, &stdout, &stderr)
	if !errors.Is(err, errHeadlessTimeout) {
		t.Fatalf("expected a timeout error, got %v (stderr %q)", err, stderr.String())
	}

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("grandchild pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("grandchild pid %q: %v", raw, convErr)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the headless deadline", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The headless run starts claude, which starts the yesmem MCP server. That child
// dials YESMEM_SOCK, so the value has to reach the run: without it every MCP call
// inside the sandbox would look for a socket the sandbox does not mount.
func TestHeadlessRunGetsTheSocketPathInItsEnvironment(t *testing.T) {
	los34bShortenHeadlessTimeout(t, 10*time.Second)

	var stdout, stderr bytes.Buffer
	err := runHeadlessCommand("bash", []string{"-c", `printf %s "$YESMEM_SOCK"`}, "", t.TempDir(),
		[]string{"YESMEM_SOCK=/run/yesmem/job.sock"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v (stderr %q)", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "/run/yesmem/job.sock" {
		t.Fatalf("headless run saw YESMEM_SOCK=%q", got)
	}
}

// The same deadline rule has to hold on the bash-job path: every scheduled bash
// job runs through runProcess, and exec.CommandContext alone kills only the
// process it started. A job that backgrounds a child left it behind, and because
// CombinedOutput waits for the inherited pipes, the job goroutine then blocked
// until that child exited by itself — measured at 60s for `sleep 60`, with the
// job's own timeout at 1s.
func TestRunProcessDeadlineKillsTheProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "sleep 300 & echo $! > " + pidFile + "; wait"

	returned := make(chan error, 1)
	go func() {
		_, exitCode, err := runProcess("bash", []string{"-c", script}, 1, "", nil)
		if err == nil || exitCode != -1 {
			returned <- fmt.Errorf("expected the deadline to fire, got exit=%d err=%v", exitCode, err)
			return
		}
		returned <- nil
	}()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runProcess did not return after its 1s deadline")
	}

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("grandchild pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("grandchild pid %q: %v", raw, convErr)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the job deadline", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
