package indexer

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// The lock map is what makes two indexing passes over one path, or two passes
// over one session id, take turns. Its two failure modes are both silent: a
// release that forgets an entry leaks it, and a lock that lets two holders in at
// once stops serializing anything.
func TestKeyedLocksExcludeAndDoNotLeak(t *testing.T) {
	var locks keyedLocks

	const (
		keys       = 10
		goroutines = 24
		rounds     = 200
	)
	// Holders are counted per key: two goroutines may work on different keys at
	// the same time, but never on the same one.
	var mu sync.Mutex
	holders := make(map[string]int, keys)
	overlaps := 0

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				key := "key-" + string(rune('a'+((g+i)%keys)))

				unlock := locks.lock(key)
				mu.Lock()
				holders[key]++
				if holders[key] > 1 {
					overlaps++
				}
				mu.Unlock()

				mu.Lock()
				holders[key]--
				mu.Unlock()
				unlock()
			}
		}(g)
	}
	wg.Wait()

	if overlaps != 0 {
		t.Errorf("two holders were inside one key %d times, want 0", overlaps)
	}

	locks.mu.Lock()
	leaked := len(locks.locks)
	locks.mu.Unlock()
	if leaked != 0 {
		keys := make([]string, 0, leaked)
		locks.mu.Lock()
		for k := range locks.locks {
			keys = append(keys, k)
		}
		locks.mu.Unlock()
		t.Errorf("%d lock entries survived their last holder: %s", leaked, strings.Join(keys, ", "))
	}
}

// A holder that panics must still release: the lock is taken with defer at every
// call site, and a path that stays claimed is never indexed again.
func TestKeyedLocksReleaseOnPanic(t *testing.T) {
	var locks keyedLocks

	func() {
		defer func() { _ = recover() }()
		defer locks.lock("panics")()
		panic("boom")
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		locks.lock("panics")()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was never released by the panicking holder")
	}
}
