package daemon

import (
	"testing"
	"time"
)

const (
	testIndexAfter  = 3 * time.Second
	testSettleAfter = 5 * time.Minute
)

func newTestDebouncer() *fileDebouncer {
	return newFileDebouncer(testIndexAfter, testSettleAfter)
}

// A session that keeps being written must not be indexed on every write — only
// once it has been quiet for the index window.
func TestFileDebouncerWaitsForQuiet(t *testing.T) {
	d := newTestDebouncer()
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		d.mark("sess.jsonl", start.Add(time.Duration(i)*time.Second))
		toIndex, _ := d.due(start.Add(time.Duration(i)*time.Second + 100*time.Millisecond))
		if len(toIndex) != 0 {
			t.Fatalf("indexed mid-write at tick %d: %v", i, toIndex)
		}
	}
}

// One quiet period produces exactly one index action, even when several ticks
// observe the same quiet file.
func TestFileDebouncerFiresOncePerQuietPeriod(t *testing.T) {
	d := newTestDebouncer()
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d.mark("sess.jsonl", start)

	quiet := start.Add(testIndexAfter + time.Second)
	toIndex, _ := d.due(quiet)
	if len(toIndex) != 1 || toIndex[0] != "sess.jsonl" {
		t.Fatalf("first quiet tick: got %v, want [sess.jsonl]", toIndex)
	}

	toIndex, _ = d.due(quiet.Add(10 * time.Second))
	if len(toIndex) != 0 {
		t.Fatalf("re-fired without a new write: %v", toIndex)
	}
}

// After the index action, a further write starts a new window.
func TestFileDebouncerFiresAgainAfterNewWrite(t *testing.T) {
	d := newTestDebouncer()
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d.mark("sess.jsonl", start)
	if _, _, ok := mustDue(t, d, start.Add(testIndexAfter+time.Second), 1, 0); !ok {
		t.Fatal("initial index action missing")
	}

	write2 := start.Add(30 * time.Second)
	d.mark("sess.jsonl", write2)
	toIndex, _ := d.due(write2.Add(time.Second))
	if len(toIndex) != 0 {
		t.Fatalf("indexed too early after the second write: %v", toIndex)
	}
	toIndex, _ = d.due(write2.Add(testIndexAfter + time.Second))
	if len(toIndex) != 1 {
		t.Fatalf("second quiet period did not fire: %v", toIndex)
	}
}

// A long-quiet file is reported as settled and dropped, so a later write starts
// a fresh window instead of inheriting the old timestamp.
func TestFileDebouncerSettlesAndForgets(t *testing.T) {
	d := newTestDebouncer()
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d.mark("sess.jsonl", start)

	// Quiet for longer than the settle window: index + settle in the same tick.
	toIndex, settled := d.due(start.Add(testSettleAfter + time.Second))
	if len(toIndex) != 1 {
		t.Errorf("index action: got %v, want the path", toIndex)
	}
	if len(settled) != 1 || settled[0] != "sess.jsonl" {
		t.Fatalf("settled: got %v, want [sess.jsonl]", settled)
	}

	// Dropped: the stale timestamp must not resurrect an action.
	toIndex, settled = d.due(start.Add(testSettleAfter + time.Hour))
	if len(toIndex) != 0 || len(settled) != 0 {
		t.Fatalf("settled path produced more work: index=%v settled=%v", toIndex, settled)
	}
}

// Multiple files are reported deterministically (sorted), so the watcher's
// action order does not depend on Go's map iteration order.
func TestFileDebouncerOrderIsDeterministic(t *testing.T) {
	d := newTestDebouncer()
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	for _, p := range []string{"c.jsonl", "a.jsonl", "b.jsonl"} {
		d.mark(p, start)
	}

	toIndex, _ := d.due(start.Add(testIndexAfter + time.Second))
	want := []string{"a.jsonl", "b.jsonl", "c.jsonl"}
	if len(toIndex) != len(want) {
		t.Fatalf("got %v, want %v", toIndex, want)
	}
	for i := range want {
		if toIndex[i] != want[i] {
			t.Fatalf("got %v, want %v", toIndex, want)
		}
	}
}

// A file quiet for longer than the settle window never settles (it was never
// marked) and must not appear out of nowhere.
func TestFileDebouncerUnknownPathIsSilent(t *testing.T) {
	d := newTestDebouncer()
	toIndex, settled := d.due(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	if len(toIndex) != 0 || len(settled) != 0 {
		t.Fatalf("unknown path produced work: index=%v settled=%v", toIndex, settled)
	}
}

func mustDue(t *testing.T, d *fileDebouncer, now time.Time, wantIndex, wantSettled int) ([]string, []string, bool) {
	t.Helper()
	toIndex, settled := d.due(now)
	if len(toIndex) != wantIndex || len(settled) != wantSettled {
		return toIndex, settled, false
	}
	return toIndex, settled, true
}
