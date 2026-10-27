package daemon

import (
	"fmt"
	"sync"
	"testing"
)

// YM-331 (2): persistPlan read *Plan after planStore.Unlock(). Two parallel
// update_plan calls therefore raced on p.Content: the writer still held the
// plan pointer while the reader copied its fields into the storage row. This
// test is the RED proof — run it with -race.
func TestLos56dPlanPersistReadsASnapshotUnderTheLock(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"plan": "Step 1", "thread_id": "t-los56d-race", "project": "yesmem"}); resp.Error != "" {
		t.Fatalf("set plan: %s", resp.Error)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				h.handleUpdatePlan(map[string]any{
					"thread_id": "t-los56d-race",
					"add":       []any{fmt.Sprintf("step %d-%d", i, j)},
				})
			}
		}(i)
	}
	wg.Wait()

	// The persisted row must match what the in-memory plan holds: the snapshot
	// is taken under the lock, so the last write is not a torn read.
	rows, err := h.store.GetActivePlans()
	if err != nil {
		t.Fatalf("get active plans: %v", err)
	}
	planStore.RLock()
	inMemory := planStore.plans["t-los56d-race"]
	planStore.RUnlock()
	if inMemory == nil {
		t.Fatal("plan missing in memory")
	}
	var persisted string
	var found bool
	for _, row := range rows {
		if row.ThreadID != "t-los56d-race" {
			continue
		}
		found = true
		persisted = row.Content
	}
	if !found {
		t.Fatal("plan row not persisted")
	}
	if got := inMemory.Content; got == "" {
		t.Fatal("in-memory plan content is empty")
	}
	// The persisted row must be the state the last writer saw. Without the
	// serialized [snapshot, write] pair two writers invert and the DB keeps an
	// older content than memory.
	if persisted != inMemory.Content {
		t.Errorf("persisted plan diverged from memory:\npersisted: %.120s\nmemory:    %.120s", persisted, inMemory.Content)
	}
}
