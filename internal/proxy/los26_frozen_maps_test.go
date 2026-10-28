package proxy

import (
	"testing"
	"time"
)

func los26FrozenStoreFixture(t *testing.T) (*FrozenStubs, []any, *[]string) {
	t.Helper()
	f := NewFrozenStubs()
	persisted := &[]string{}
	f.SetPersistFunc(func(key, value string) { *persisted = append(*persisted, value) })
	msgs := []any{map[string]any{"role": "user", "content": "stub"}}
	f.Store("t1", msgs, 1, msgs[0], 100, 200)
	*persisted = (*persisted)[:0]
	return f, msgs, persisted
}

func los26MapHas(f *FrozenStubs, name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch name {
	case "rawTokens":
		_, ok := f.rawTokens["t1"]
		return ok
	case "loadedFromDB":
		_, ok := f.loadedFromDB["t1"]
		return ok
	}
	panic("unknown map " + name)
}

func TestLos26_InvalidateClearsPerThreadMaps(t *testing.T) {
	f, _, persisted := los26FrozenStoreFixture(t)

	f.Invalidate("t1")

	// rawTokens is display state for the frozen prefix and must not outlive it.
	if los26MapHas(f, "rawTokens") {
		t.Error("Invalidate left a rawTokens entry behind")
	}
	// loadedFromDB deliberately stays: it is the in-memory tombstone that keeps
	// the invalidated prefix from coming back through the lazy DB load.
	if !los26MapHas(f, "loadedFromDB") {
		t.Error("Invalidate dropped the loadedFromDB tombstone, an invalidated prefix can now be reloaded")
	}
	if len(*persisted) == 0 {
		t.Fatal("Invalidate did not touch the persisted prefix")
	}
	if last := (*persisted)[len(*persisted)-1]; last != "" {
		t.Errorf("last persisted value keeps %d bytes, want the empty tombstone (the row carries several MB)", len(last))
	}
}

// The tombstone write is best effort (the persist callback cannot report
// failure). The guarantee that an invalidated prefix stays gone must therefore
// come from the in-memory tombstone, not from the row.
func TestLos26_InvalidateKeepsPrefixGoneWhenTombstoneWriteFails(t *testing.T) {
	rows := map[string]string{}
	daemonUp := true
	f := NewFrozenStubs()
	f.SetPersistFunc(func(key, value string) {
		if daemonUp {
			rows[key] = value
		}
	})
	f.SetLoadFunc(func(key string) (string, bool) {
		v, ok := rows[key]
		return v, ok && v != ""
	})

	msgs := []any{map[string]any{"role": "user", "content": "stub"}}
	f.Store("t1", msgs, 1, msgs[0], 100, 200)
	if len(rows) == 0 {
		t.Fatal("fixture did not persist the prefix")
	}

	daemonUp = false
	f.Invalidate("t1")

	if got := f.Get("t1", msgs); got != nil {
		t.Error("invalidated prefix was resurrected from the persisted row after the tombstone write failed")
	}
}

func TestLos26_EvictClearsPerThreadMaps(t *testing.T) {
	f, _, persisted := los26FrozenStoreFixture(t)

	f.mu.Lock()
	f.lastAccess["t1"] = time.Now().Add(-2 * time.Hour)
	f.mu.Unlock()

	if n := f.Evict(); n != 1 {
		t.Fatalf("Evict removed %d entries, want 1", n)
	}

	if los26MapHas(f, "rawTokens") {
		t.Error("Evict left a rawTokens entry behind")
	}
	if los26MapHas(f, "loadedFromDB") {
		t.Error("Evict left a loadedFromDB entry behind")
	}
	// Evict runs inside handleMessages: it must stay pure memory work.
	if len(*persisted) != 0 {
		t.Errorf("Evict issued %d daemon writes, want 0 (no I/O on the request path)", len(*persisted))
	}
}

// Guard against the regression the map cleanup could cause: once a prefix is
// invalidated, the lazy DB load must not bring it back. A refreeze depends on
// it staying gone.
func TestLos26_InvalidateThenGetDoesNotResurrect(t *testing.T) {
	rows := map[string]string{}
	f := NewFrozenStubs()
	f.SetPersistFunc(func(key, value string) { rows[key] = value })
	f.SetLoadFunc(func(key string) (string, bool) {
		v, ok := rows[key]
		return v, ok && v != ""
	})

	msgs := []any{map[string]any{"role": "user", "content": "stub"}}
	f.Store("t1", msgs, 1, msgs[0], 100, 200)
	f.Invalidate("t1")

	if got := f.Get("t1", msgs); got != nil {
		t.Error("invalidated prefix was resurrected from the persisted row")
	}
}
