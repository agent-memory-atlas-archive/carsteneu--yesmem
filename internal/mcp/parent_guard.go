package mcp

import (
	"log"
	"os"
	"time"
)

// parentPollInterval is how often a stdio server checks whether the client that
// spawned it is still alive.
var parentPollInterval = 5 * time.Second

// getppid and exitProcess are indirected so tests can drive the guard loop
// without a real process tree.
var (
	getppid     = os.Getppid
	exitProcess = os.Exit
)

// watchParent exits the process once its parent pid changed, which means the
// client that spawned this stdio server is gone. stdin EOF normally ends the
// server by itself, but a wrapper or PTY bridge that keeps the pipe open would
// leave an orphan helper behind forever. originPpid is the parent the caller
// captured, not a value read here.
//
// On Unix-like systems a process whose parent died is reparented to init (or to
// the nearest subreaper), so the ppid changes and no PDEATHSIG is needed —
// PDEATHSIG is Linux-only and redundant where that guarantee holds. Windows does
// not reparent: os.Getppid keeps returning the dead parent's pid, so the guard
// simply never fires there, which costs a leaked helper rather than killing a
// live one.
func watchParent(originPpid int, ticks <-chan time.Time) {
	for range ticks {
		if getppid() != originPpid {
			log.Printf("[mcp] parent %d is gone — exiting", originPpid)
			exitProcess(0)
			return
		}
	}
}

// startParentWatch polls the parent pid in the background. The goroutine is
// never stopped: it dies with the process, and nothing waits on it, so it
// cannot keep the server alive.
//
// originPpid must be captured as early as possible by the caller. Reading it here
// instead would lose the startup window: if the client dies before this point, a
// subreaper has already adopted this process, the fresh read returns the
// subreaper, and a pid that never changes again would be watched forever. A
// client that dies before the caller's read is indistinguishable from a helper
// that was detached on purpose, and is therefore not watched at all.
//
// A helper whose parent is already gone at startup (a wrapper that detached it)
// is left alone — there is no client to watch, and exiting would break a
// deliberately backgrounded server.
func startParentWatch(originPpid int) {
	if originPpid <= 1 {
		return
	}
	ticker := time.NewTicker(parentPollInterval)
	go watchParent(originPpid, ticker.C)
}
