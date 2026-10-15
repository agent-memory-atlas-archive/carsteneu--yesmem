package indexer

import (
	"log"
	"os"
	"runtime/debug"
	"sync"
)

// Indexing-pool defaults. Both are deliberately conservative: parsing a session
// is the memory-hungry part of indexing, so the pool trades throughput for a
// bounded footprint.
const (
	// DefaultIndexWorkers is how many sessions may be parsed concurrently.
	DefaultIndexWorkers = 2
	// LargeSessionBytes is the size above which a session is indexed on its own:
	// GB-class rollouts are serialized so two of them never expand at once.
	LargeSessionBytes = 100 << 20 // 100 MiB
)

// indexRunFunc indexes a single session file.
type indexRunFunc func(path string) error

// IndexPool bounds how many sessions are indexed at the same time and keeps the
// dispatch loop that feeds it responsive.
//
// At most maxWorkers files are parsed concurrently, and files larger than
// largeBytes are serialized to one at a time on top of that. Submitting a path
// that is already queued or running does not start a second concurrent run —
// that would insert its messages twice — but it is not lost either: the running
// job performs one more pass afterwards, which picks up writes that landed after
// it had already measured the file.
type IndexPool struct {
	run        indexRunFunc
	onDone     func(path string)
	maxWorkers int
	largeBytes int64

	sem     chan struct{}
	largeMu sync.Mutex

	mu       sync.Mutex
	inflight map[string]bool
	rerun    map[string]bool
	wg       sync.WaitGroup
}

// NewIndexPool creates a pool around run. maxWorkers < 1 falls back to a single
// worker; onDone (optional) runs after every pass, successful or not.
func NewIndexPool(run indexRunFunc, maxWorkers int, largeBytes int64, onDone func(path string)) *IndexPool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &IndexPool{
		run:        run,
		onDone:     onDone,
		maxWorkers: maxWorkers,
		largeBytes: largeBytes,
		sem:        make(chan struct{}, maxWorkers),
		inflight:   make(map[string]bool),
		rerun:      make(map[string]bool),
	}
}

// Submit schedules path for indexing and returns immediately, so the caller's
// loop never waits for a parse. Files that cannot be stat'ed are ignored.
func (p *IndexPool) Submit(path string) {
	if !p.claim(path) {
		return // already queued or running; it will do one more pass afterwards
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		for {
			p.runPass(path)
			if !p.releasePass(path) {
				return
			}
		}
	}()
}

// runPass indexes the path once under the pool's concurrency budget. The file is
// measured here — not at Submit time — so a re-run sees writes that arrived in
// the meantime.
func (p *IndexPool) runPass(path string) {
	// A panic while parsing one session must not take the daemon down with it. The
	// deferred release of the concurrency slot runs during the unwinding that
	// recovers here (defers are last in, first out), so the pool stays
	// consistent and the caller's loop can release the path normally. onDone still
	// runs, as the pool's contract promises for every pass, and the stack is logged
	// because the guard turns a crash into a silently skipped session.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: index %q: %v\n%s", path, r, debug.Stack())
			if p.onDone != nil {
				p.onDone(path)
			}
		}
	}()

	info, err := os.Stat(path)
	if err != nil {
		return
	}

	if info.Size() > p.largeBytes {
		p.largeMu.Lock()
		defer p.largeMu.Unlock()
	}

	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	if err := p.run(path); err != nil {
		log.Printf("warn: index %q: %v", path, err)
	}
	if p.onDone != nil {
		p.onDone(path)
	}
}

// Wait blocks until every submitted job has finished.
func (p *IndexPool) Wait() { p.wg.Wait() }

// claim registers path as in-flight, reporting false when it already is and
// recording that one more pass is needed.
func (p *IndexPool) claim(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[path] {
		p.rerun[path] = true
		return false
	}
	p.inflight[path] = true
	return true
}

// releasePass reports whether another pass is due, keeping the path claimed
// while it does so that a concurrent Submit cannot slip in between. When no
// further pass is due the path is released.
func (p *IndexPool) releasePass(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rerun[path] {
		delete(p.rerun, path)
		return true
	}
	delete(p.inflight, path)
	return false
}
