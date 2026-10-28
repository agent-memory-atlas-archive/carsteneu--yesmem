package proxy

import (
	"sync/atomic"
	"testing"
)

// Every request rewrites the frozen prefix snapshot, but the bytes only change
// when the pipeline actually rewrote something inside the frozen range. Before
// the fix the whole prefix (several MB) went to the daemon on every round.
func TestLos26_UpdateMessagesSkipsPersistForIdenticalPrefix(t *testing.T) {
	f := NewFrozenStubs()
	var persists int32
	f.SetPersistFunc(func(key, value string) { atomic.AddInt32(&persists, 1) })

	msgs := []any{map[string]any{"role": "user", "content": "stub"}}
	f.Store("t1", msgs, 1, msgs[0], 100, 200)
	atomic.StoreInt32(&persists, 0)

	for round := 0; round < 5; round++ {
		f.UpdateMessages("t1", msgs)
	}

	if n := atomic.LoadInt32(&persists); n != 0 {
		t.Errorf("byte-identical prefix was written to the daemon %d times, want 0", n)
	}
}

// The suppression must not swallow real changes: a rewritten prefix has to be
// stored and persisted exactly once.
func TestLos26_UpdateMessagesPersistsChangedPrefix(t *testing.T) {
	f := NewFrozenStubs()
	var persists int32
	f.SetPersistFunc(func(key, value string) { atomic.AddInt32(&persists, 1) })

	msgs := []any{map[string]any{"role": "user", "content": "stub"}}
	f.Store("t1", msgs, 1, msgs[0], 100, 200)
	atomic.StoreInt32(&persists, 0)

	changed := []any{map[string]any{"role": "user", "content": "rewritten by the pipeline"}}
	if !f.UpdateMessages("t1", changed) {
		t.Fatal("UpdateMessages must report a change for different bytes")
	}
	if n := atomic.LoadInt32(&persists); n != 1 {
		t.Errorf("changed prefix persisted %d times, want 1", n)
	}
}
