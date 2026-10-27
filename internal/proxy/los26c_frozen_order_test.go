package proxy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLos26c_PersistLockIsPerThreadNotGlobal pins the fix for the cold review's
// first Important finding: the persistence lock must order writes of one thread,
// not stall every other thread behind a daemon round trip.
func TestLos26c_PersistLockIsPerThreadNotGlobal(t *testing.T) {
	f := NewFrozenStubs()

	// Two thread ids that land in different slots.
	a := "los26c-lock-a"
	b := ""
	for i := 0; i < 256; i++ {
		candidate := fmt.Sprintf("los26c-lock-b%d", i)
		if f.persistLock(a) != f.persistLock(candidate) {
			b = candidate
			break
		}
	}
	if b == "" {
		t.Fatal("every thread id maps to the same lock slot: the persistence lock is global and one slow daemon round trip stalls every other thread")
	}

	release := make(chan struct{})
	started := make(chan struct{})
	f.SetPersistFunc(func(key, value string) {
		if strings.HasSuffix(key, a) {
			close(started)
			<-release
		}
	})

	stub := []any{map[string]any{"role": "user", "content": "stub"}}
	boundary := map[string]any{"role": "user", "content": "boundary"}
	go f.Store(a, stub, 3, boundary, 10, 10)
	<-started

	done := make(chan struct{})
	go func() {
		f.Store(b, stub, 3, boundary, 10, 10)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second thread waited for the first thread's persist round trip")
	}
	close(release)
}

// TestLos26c_InvalidateWaitsForInFlightPersist pins finding 5 of YM-315. The
// persist callback runs a daemon round trip; a freeze on one request and an
// invalidation on another could therefore reach the daemon in the opposite
// order. The tombstone then lost the race, the old prefix stayed in
// proxy_state, and a later cold start loaded it again through the lazy DB load.
func TestLos26c_InvalidateWaitsForInFlightPersist(t *testing.T) {
	f := NewFrozenStubs()

	release := make(chan struct{})
	started := make(chan struct{})
	writes := make(chan string, 8)
	var first sync.Once
	f.SetPersistFunc(func(key, value string) {
		first.Do(func() {
			close(started)
			<-release // simulate a slow daemon round trip
		})
		writes <- value
	})

	stub := []any{map[string]any{"role": "user", "content": "stub"}}
	boundary := map[string]any{"role": "user", "content": "boundary"}

	storeDone := make(chan struct{})
	go func() {
		f.Store("los26c-order", stub, 3, boundary, 100, 100)
		close(storeDone)
	}()
	<-started

	invalidateDone := make(chan struct{})
	go func() {
		f.Invalidate("los26c-order")
		close(invalidateDone)
	}()

	// While the payload write is held, the invalidation must not reach the
	// daemon: a tombstone written now is overtaken by the payload afterwards,
	// and the row survives a restart. It must also not drop the in-memory state
	// early: the payload was captured before this point, so the invalidation
	// has to wait for the whole capture-and-write step, not just the callback.
	waitUntil := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(waitUntil) {
		select {
		case v := <-writes:
			t.Fatalf("the invalidation wrote %q while a prefix persist was still in flight", v)
		default:
		}
		if !f.Has("los26c-order") {
			t.Fatal("the invalidation dropped the in-memory prefix while its payload was still being written: the snapshot was captured outside the lock")
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(release)
	<-storeDone
	<-invalidateDone

	// The tombstone has to be the last word.
	last := ""
	for {
		select {
		case v := <-writes:
			last = v
			continue
		default:
		}
		break
	}
	if last != "" {
		t.Fatalf("last persisted value after the invalidation = %q, want the empty tombstone", last)
	}
	if f.Has("los26c-order") {
		t.Fatal("the invalidation did not drop the in-memory prefix")
	}
}
