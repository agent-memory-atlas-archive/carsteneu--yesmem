package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// YM-391 (OPS-96): StartWALGuard started a goroutine that returned only on ctx,
// and Store.Close did not wait for it. A heal that ran while the pools closed
// recreated the database files, so the test that owns the temp dir failed in its
// cleanup with "directory not empty" — intermittently, because it needed a tick
// to land in exactly that window.
//
// Holding one tick open makes the window deterministic: Close may not return
// while a tick is running.
func TestLos56gCloseWaitsForARunningGuardTick(t *testing.T) {
	s, _ := los38tOpenStore(t)
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var released sync.Once
	releaseTick := func() { released.Do(func() { close(release) }) }
	// Without this, an early Fatalf below parks the guard goroutine in the hook, and
	// the store cleanup then blocks in Close forever: a real regression would turn
	// into a package timeout instead of a failure.
	defer releaseTick()
	var ticks atomic.Int64
	prevHook := walGuardTickHook
	walGuardTickHook = func() {
		ticks.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { walGuardTickHook = prevHook })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.StartWALGuard(ctx, time.Millisecond)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the guard never ran a tick")
	}

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while a guard tick was still running: the heal can recreate database files after the pools are gone, which is the 'directory not empty' cleanup failure")
	case <-time.After(300 * time.Millisecond):
	}

	releaseTick()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the running tick finished")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
	settled := ticks.Load()
	time.Sleep(150 * time.Millisecond)
	if got := ticks.Load(); got != settled {
		t.Errorf("the guard ran %d more tick(s) after Close returned", got-settled)
	}
}

// A store whose guard never started still closes; the wait must not invent a
// goroutine.
func TestLos56gCloseWithoutAGuardReturns(t *testing.T) {
	s, _ := los38tOpenStore(t)

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a guard that was never started")
	}
}
