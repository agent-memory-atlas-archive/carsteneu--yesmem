package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeSizedFile creates a file of exactly size bytes.
func writeSizedFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// concurrencyTracker records the peak number of in-flight runs, optionally
// separated by "large" runs.
type concurrencyTracker struct {
	mu        sync.Mutex
	cur       int
	peak      int
	curLarge  int
	peakLarge int
	total     atomic.Int32
}

func (c *concurrencyTracker) enter(large bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur++
	if c.cur > c.peak {
		c.peak = c.cur
	}
	if large {
		c.curLarge++
		if c.curLarge > c.peakLarge {
			c.peakLarge = c.curLarge
		}
	}
	c.total.Add(1)
}

func (c *concurrencyTracker) leave(large bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur--
	if large {
		c.curLarge--
	}
}

// The pool must never run more than maxWorkers sessions at once: parsing is the
// memory-hungry part of indexing.
func TestIndexPoolCapsConcurrency(t *testing.T) {
	dir := t.TempDir()
	var tr concurrencyTracker
	run := func(path string) error {
		tr.enter(false)
		defer tr.leave(false)
		time.Sleep(20 * time.Millisecond)
		return nil
	}

	const maxWorkers = 2
	p := NewIndexPool(run, maxWorkers, 1<<30, nil)
	for i := 0; i < 8; i++ {
		p.Submit(writeSizedFile(t, dir, fmt.Sprintf("f%d.jsonl", i), 16))
	}
	p.Wait()

	if tr.peak > maxWorkers {
		t.Errorf("peak concurrency: got %d, want <= %d", tr.peak, maxWorkers)
	}
	if got := tr.total.Load(); got != 8 {
		t.Errorf("runs: got %d, want 8", got)
	}
	t.Logf("peak concurrency %d (cap %d)", tr.peak, maxWorkers)
}

// Files above the size threshold are serialized on top of the worker cap: a
// GB-class session must not be parsed twice in parallel.
func TestIndexPoolSerializesLargeFiles(t *testing.T) {
	dir := t.TempDir()
	var tr concurrencyTracker
	run := func(path string) error {
		large := strings.HasPrefix(filepath.Base(path), "large")
		tr.enter(large)
		defer tr.leave(large)
		time.Sleep(20 * time.Millisecond)
		return nil
	}

	const threshold = 1024
	p := NewIndexPool(run, 4, threshold, nil)
	for i := 0; i < 4; i++ {
		p.Submit(writeSizedFile(t, dir, fmt.Sprintf("large%d.jsonl", i), 4096))
	}
	p.Submit(writeSizedFile(t, dir, "small.jsonl", 16))
	p.Wait()

	if tr.peakLarge > 1 {
		t.Errorf("large files ran in parallel: peak %d, want 1", tr.peakLarge)
	}
	if got := tr.total.Load(); got != 5 {
		t.Errorf("runs: got %d, want 5", got)
	}
}

// A queued large file must not stall small ones — that is the point of moving
// indexing off the watcher's dispatch loop.
func TestIndexPoolLargeFileDoesNotBlockSmallOnes(t *testing.T) {
	dir := t.TempDir()

	release := make(chan struct{})
	ran := make(chan string, 8)
	run := func(path string) error {
		ran <- filepath.Base(path)
		if filepath.Base(path) == "large.jsonl" {
			<-release
		}
		return nil
	}

	const threshold = 1024
	p := NewIndexPool(run, 2, threshold, nil)
	p.Submit(writeSizedFile(t, dir, "large.jsonl", 4096))

	select {
	case got := <-ran:
		if got != "large.jsonl" {
			t.Fatalf("first run was %q, want large.jsonl", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("large file never started")
	}

	p.Submit(writeSizedFile(t, dir, "small.jsonl", 16))
	select {
	case got := <-ran:
		if got != "small.jsonl" {
			t.Fatalf("second run was %q, want small.jsonl", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("small file was blocked behind the large one")
	}

	close(release)
	p.Wait()
}

// Submitting the same path while it is running must not start a second
// concurrent run (that would insert its messages twice), but the extra submits
// must not be lost either: exactly one follow-up pass picks up whatever landed
// after the first pass had measured the file.
func TestIndexPoolCoalescesInFlightPath(t *testing.T) {
	dir := t.TempDir()
	path := writeSizedFile(t, dir, "sess.jsonl", 16)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	var peak, cur int32
	var mu sync.Mutex
	run := func(string) error {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		defer func() { mu.Lock(); cur--; mu.Unlock() }()

		n := calls.Add(1)
		if n == 1 {
			started <- struct{}{}
			<-release
		}
		return nil
	}

	p := NewIndexPool(run, 2, 1<<30, nil)
	p.Submit(path)
	<-started
	p.Submit(path)
	p.Submit(path)
	close(release)
	p.Wait()

	if got := calls.Load(); got != 2 {
		t.Errorf("runs: got %d, want 2 (one pass plus one coalesced follow-up)", got)
	}
	if peak > 1 {
		t.Errorf("peak concurrency for one path: got %d, want 1", peak)
	}
}

// A write that lands during a pass must still be picked up by the follow-up.
func TestIndexPoolRerunObservesGrowth(t *testing.T) {
	dir := t.TempDir()
	path := writeSizedFile(t, dir, "sess.jsonl", 16)

	firstPassStarted := make(chan struct{})
	firstPassDone := make(chan struct{})
	sizes := make(chan int64, 8)

	run := func(p string) error {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		sizes <- info.Size()
		select {
		case <-firstPassStarted:
		default:
			close(firstPassStarted)
			<-firstPassDone
		}
		return nil
	}

	pool := NewIndexPool(run, 1, 1<<30, nil)
	pool.Submit(path)
	<-firstPassStarted
	// Grow the file while the first pass is still running.
	writeSizedFile(t, dir, "sess.jsonl", 64)
	pool.Submit(path)
	close(firstPassDone)
	pool.Wait()

	if len(sizes) != 2 {
		t.Fatalf("measured %d passes, want 2", len(sizes))
	}
	first, second := <-sizes, <-sizes
	if first != 16 {
		t.Errorf("first pass saw size %d, want 16", first)
	}
	if second != 64 {
		t.Errorf("follow-up pass saw size %d, want 64 (growth missed)", second)
	}
}

// Once the in-flight run finishes, the path can be submitted again.
func TestIndexPoolAllowsResubmitAfterCompletion(t *testing.T) {
	dir := t.TempDir()
	path := writeSizedFile(t, dir, "sess.jsonl", 16)

	var calls atomic.Int32
	p := NewIndexPool(func(string) error { calls.Add(1); return nil }, 1, 1<<30, nil)
	p.Submit(path)
	p.Wait()
	p.Submit(path)
	p.Wait()

	if got := calls.Load(); got != 2 {
		t.Errorf("runs: got %d, want 2", got)
	}
}

// A failing run must be contained: the pool keeps working and onDone still runs.
func TestIndexPoolSurvivesRunError(t *testing.T) {
	dir := t.TempDir()

	var done atomic.Int32
	p := NewIndexPool(
		func(path string) error {
			if strings.HasPrefix(filepath.Base(path), "bad") {
				return fmt.Errorf("boom")
			}
			return nil
		}, 1, 1<<30, func(string) { done.Add(1) })

	p.Submit(writeSizedFile(t, dir, "bad.jsonl", 16))
	p.Submit(writeSizedFile(t, dir, "good.jsonl", 16))
	p.Wait()

	if got := done.Load(); got != 2 {
		t.Errorf("onDone calls: got %d, want 2", got)
	}
}

// A path that cannot be stat'ed must be ignored without panicking.
func TestIndexPoolIgnoresUnreadablePath(t *testing.T) {
	var calls atomic.Int32
	p := NewIndexPool(func(string) error { calls.Add(1); return nil }, 1, 1<<30, nil)
	p.Submit(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	p.Wait()

	if got := calls.Load(); got != 0 {
		t.Errorf("runs: got %d, want 0", got)
	}
}

// Indexing runs on the pool's own goroutines, so a panic in one of them would
// otherwise take the whole daemon down. It must be contained without leaving the
// path claimed: a path that stays in the pool's in-flight set is never indexed
// again, silently and for the rest of the process's life.
func TestIndexPoolContainsIndexerPanic(t *testing.T) {
	dir := t.TempDir()
	path := writeSizedFile(t, dir, "panics.jsonl", 128)

	var runs atomic.Int32
	var cleanups atomic.Int32
	p := NewIndexPool(func(string) error {
		runs.Add(1)
		panic("indexer blew up")
	}, 1, 1<<30, func(string) { cleanups.Add(1) })

	p.Submit(path)
	p.Wait()

	// The path has to be free again, or this session is never reindexed.
	p.Submit(path)
	p.Wait()

	if got := runs.Load(); got != 2 {
		t.Errorf("runs: got %d, want 2 — a panicking pass must not claim the path forever", got)
	}
	// onDone is documented to run after every pass, successful or not.
	if got := cleanups.Load(); got != 2 {
		t.Errorf("onDone calls: got %d, want 2", got)
	}
}
