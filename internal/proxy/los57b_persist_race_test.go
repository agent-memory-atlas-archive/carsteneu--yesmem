package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
)

// YM-401 (PRX-P53): Persist marshalled the thread's entry map after releasing the
// read lock. Store and ScrubPerSessionContent write that map, so the marshal
// raced them: json walks the map while another goroutine inserts into it, which
// Go reports as a fatal "concurrent map iteration and map write" and not as a
// recoverable panic.
//
// The test hammers both sides at once; the assertion is that the goroutines
// finish at all and that Persist still delivers snapshots.
func TestLos57bPersistSerializesAgainstWriters(t *testing.T) {
	ts := NewTimestampStore()
	var persisted atomic.Int64
	ts.SetPersistFunc(func(key, value string) {
		if len(value) == 0 {
			t.Errorf("persistFunc got an empty snapshot for %q", key)
		}
		persisted.Add(1)
	})

	const thread = "opencode:ses_los57b_persist"
	ts.Store(thread, 1, &TimestampMeta{Timestamp: "Mo 2026-10-20 10:00:00", PlanCheckpoint: "checkpoint"})

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			ts.Store(thread, i%500+1, &TimestampMeta{Timestamp: "x", LoopWarning: "w"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			ts.ScrubPerSessionContent(thread)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			ts.Persist(thread)
		}
	}()
	wg.Wait()

	if persisted.Load() == 0 {
		t.Fatal("Persist never reached persistFunc")
	}
}
