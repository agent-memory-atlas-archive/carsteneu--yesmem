package proxy

import (
	"sync"
	"testing"
	"time"
)

// YM-396 (PRX-P49): the status writer starts a ticker in its constructor and must
// end it. Without Stop the goroutine outlives every test that builds a writer,
// and each tick allocates, which pollutes testing.AllocsPerRun for the rest of
// the process.

// Stop ends the goroutine: the live-writer counter is back to its baseline and
// the loop's exit channel is closed by the time Stop returns. The counter is the
// same signal the package-final guard reads, so the assertion cannot drift from
// the guard.
func TestLos55cCacheStatusWriterStopEndsTheTicker(t *testing.T) {
	baseline := liveCacheStatusWriters.Load()
	w := NewCacheStatusWriter(t.TempDir(), "ephemeral", 250000, 100000)
	t.Cleanup(w.Stop)

	if got := liveCacheStatusWriters.Load(); got != baseline+1 {
		t.Fatalf("live writers after construction = %d, want %d: the writer did not start its loop", got, baseline+1)
	}

	w.Stop()

	select {
	case <-w.stopped:
	default:
		t.Fatal("Stop returned while the loop's exit channel was still open")
	}
	if got := liveCacheStatusWriters.Load(); got != baseline {
		t.Errorf("live writers after Stop = %d, want the baseline %d", got, baseline)
	}
}

// A second Stop must neither panic nor block: shutdown paths run from several
// places (signal handler, deferred cleanup, tests).
func TestLos55cCacheStatusWriterStopIsIdempotent(t *testing.T) {
	w := NewCacheStatusWriter(t.TempDir(), "ephemeral", 250000, 100000)
	t.Cleanup(w.Stop)
	w.Stop()

	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second Stop blocked")
	}
}

// Stop has to wait for a status write that is in flight, not return while the
// loop still touches the status file. The hold hook keeps one write open and the
// test observes that Stop does not return before it is released.
func TestLos55cCacheStatusWriterStopWaitsForTheWriteInFlight(t *testing.T) {
	w := NewCacheStatusWriter(t.TempDir(), "ephemeral", 250000, 100000)
	w.Update(time.Now(), 1000, 0, 0, "los55c-thread")

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing assertion below must not strand the writer in the hook: the
	// cleanup releases the write and stops the loop.
	t.Cleanup(func() {
		releaseWrite()
		w.Stop()
	})

	w.mu.Lock()
	w.writeStatusHold = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	w.mu.Unlock()

	// The hook fires on the next tick, at most one tick interval away.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no status write started within three tick intervals")
	}

	stopReturned := make(chan struct{})
	go func() {
		w.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while a status write was still running")
	case <-time.After(100 * time.Millisecond):
	}

	releaseWrite()
	select {
	case <-stopReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the in-flight write finished")
	}
}

// A writer that was never started (zero value) is stopped by the same shutdown
// code path; that must be a no-op instead of a panic on a nil channel.
func TestLos55cCacheStatusWriterStopAcceptsTheZeroValue(t *testing.T) {
	var w CacheStatusWriter

	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop on a zero-value writer blocked")
	}
}

// A writer with only half of its channels set (a hand-built literal) must be
// stopped instead of blocking on a nil channel.
func TestLos55cCacheStatusWriterStopAcceptsAPartialValue(t *testing.T) {
	w := CacheStatusWriter{done: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop on a partially built writer blocked")
	}
}
