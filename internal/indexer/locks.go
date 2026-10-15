package indexer

import "sync"

// keyedLocks hands out one mutex per key, so work on a single path or session
// is serialized while unrelated keys run in parallel.
//
// Entries are reference counted and disappear with their last holder: the key
// space is unbounded (every session file the daemon has ever seen, every session
// id), and a mutex kept per key forever would grow with the process lifetime.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is one entry: the mutex plus how many callers are holding or waiting
// for it.
type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock acquires the mutex for key and returns its release function. The release
// function must be called exactly once.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedLock)
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()

	return func() {
		// Release the critical section first: a caller arriving after this point
		// must not be made to wait for the bookkeeping below.
		entry.mu.Unlock()

		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
