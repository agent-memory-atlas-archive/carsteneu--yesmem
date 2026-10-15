package mcp

import (
	"sync"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// TestWatchParentExitsWhenParentChanges pins the orphan guard: while the parent
// stays the same the loop keeps polling, and the first changed ppid ends the
// process with a clean exit code.
func TestWatchParentExitsWhenParentChanges(t *testing.T) {
	origGetppid, origExit := getppid, exitProcess
	defer func() { getppid, exitProcess = origGetppid, origExit }()

	var mu sync.Mutex
	parentPid := 4242
	getppid = func() int {
		mu.Lock()
		defer mu.Unlock()
		return parentPid
	}
	exited := make(chan int, 1)
	exitProcess = func(code int) { exited <- code }

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		watchParent(4242, ticks)
		close(done)
	}()

	ticks <- time.Now()
	select {
	case code := <-exited:
		t.Fatalf("exit(%d) although the parent is unchanged", code)
	case <-time.After(50 * time.Millisecond):
	}

	mu.Lock()
	parentPid = 1
	mu.Unlock()
	ticks <- time.Now()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit after the parent changed")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchParent did not return after the exit")
	}
}

// TestStartParentWatchExitsWhenParentDies covers the watch loop: the started
// watch must stay quiet while the parent lives and end the process once it is
// gone.
func TestStartParentWatchExitsWhenParentDies(t *testing.T) {
	origGetppid, origExit, origInterval := getppid, exitProcess, parentPollInterval
	defer func() {
		getppid, exitProcess, parentPollInterval = origGetppid, origExit, origInterval
	}()
	parentPollInterval = time.Millisecond

	var mu sync.Mutex
	parentPid := 4242
	getppid = func() int {
		mu.Lock()
		defer mu.Unlock()
		return parentPid
	}
	exited := make(chan int, 1)
	exitProcess = func(code int) { exited <- code }

	startParentWatch(4242)
	select {
	case code := <-exited:
		t.Fatalf("exit(%d) although the parent is alive", code)
	case <-time.After(30 * time.Millisecond):
	}

	mu.Lock()
	parentPid = 1
	mu.Unlock()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit after the parent died")
	}
}

// TestStartParentWatchLeavesAlreadyOrphanedHelperAlone pins the false-positive
// guard: a helper a wrapper detached before startup has ppid 1 from the first
// moment and must not be killed.
func TestStartParentWatchLeavesAlreadyOrphanedHelperAlone(t *testing.T) {
	origGetppid, origExit, origInterval := getppid, exitProcess, parentPollInterval
	defer func() {
		getppid, exitProcess, parentPollInterval = origGetppid, origExit, origInterval
	}()
	parentPollInterval = time.Millisecond
	getppid = func() int { return 1 }
	exited := make(chan int, 1)
	exitProcess = func(code int) { exited <- code }

	// A helper whose parent was already gone at startup: the pid the caller would
	// have captured is 1, so there is no client to watch.
	startParentWatch(1)
	select {
	case code := <-exited:
		t.Fatalf("exit(%d) for a helper that was orphaned before startup", code)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestStartParentWatchUsesOriginCapturedByCaller covers the startup window: the
// client can die after its pid was captured but before the stdio server takes
// over. The watch must compare against that captured origin — reading the parent
// again at that moment returns the subreaper that adopted this process, whose pid
// never changes again, and the helper would then stay alive forever.
func TestStartParentWatchUsesOriginCapturedByCaller(t *testing.T) {
	origGetppid, origExit, origInterval := getppid, exitProcess, parentPollInterval
	defer func() {
		getppid, exitProcess, parentPollInterval = origGetppid, origExit, origInterval
	}()
	parentPollInterval = time.Millisecond

	// The client died during startup and a subreaper adopted this process.
	getppid = func() int { return 1 }
	exited := make(chan int, 1)
	exitProcess = func(code int) { exited <- code }

	startParentWatch(4242)

	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("helper whose client died during startup was not reaped")
	}
}

// TestServeStdioArmsParentWatch covers the line that actually activates the
// feature: ServeStdio must start the watch before the server takes over.
func TestServeStdioArmsParentWatch(t *testing.T) {
	origServe := serveStdio
	defer func() { serveStdio = origServe }()

	origGetppid, origExit, origInterval := getppid, exitProcess, parentPollInterval
	defer func() {
		getppid, exitProcess, parentPollInterval = origGetppid, origExit, origInterval
	}()
	parentPollInterval = time.Millisecond

	var mu sync.Mutex
	parentPid := 4242
	getppid = func() int {
		mu.Lock()
		defer mu.Unlock()
		return parentPid
	}
	exited := make(chan int, 1)
	exitProcess = func(code int) { exited <- code }
	server := mcpserver.NewMCPServer("yesmem-test", "0", mcpserver.WithToolCapabilities(false))
	var served *mcpserver.MCPServer
	serveStdio = func(s *mcpserver.MCPServer, _ ...mcpserver.StdioOption) error {
		served = s
		return nil
	}

	if err := (&Server{srv: server}).ServeStdio(4242); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if served != server {
		t.Fatalf("serveStdio got %p, want the server %p", served, server)
	}

	mu.Lock()
	parentPid = 1
	mu.Unlock()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStdio did not arm the parent watch")
	}
}
