package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/bloom"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Synthetic-session benchmarks for the append fast path.
//
// The report and the benchmarks compare the same file state twice: once indexed
// incrementally (what IndexSession does today for a grown file) and once with
// the watermark cleared, which forces the pre-change behaviour of re-parsing the
// whole file on every write. Sizes are parameterised so CI runs a small case
// while a real GB-class rollout can be reproduced on demand:
//
//	YESMEM_BENCH_MB      total session size to grow to (default 16)
//	YESMEM_BENCH_STEPS   number of append steps (default 8)
//	YESMEM_BENCH_ARCHIVE set to 0 to index without the JSONL archiver
//
// Examples: YESMEM_BENCH_MB=200 go test ./internal/indexer -run Scaling -v
//           YESMEM_BENCH_MB=1024 YESMEM_BENCH_STEPS=8 go test ./internal/indexer -run Scaling -v -timeout 0
//
// A GB-class run needs -timeout 0: each step measures a full reparse of the
// current file, so the aggregate work is several times one full pass.

// benchLineBytes is the approximate size of one generated JSONL line.
const benchLineBytes = 1024

var benchBaseTime = time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)

// syntheticLine renders one Claude Code JSONL line of roughly benchLineBytes.
func syntheticLine(sessionID string, i int) string {
	role := "user"
	if i%2 == 1 {
		role = "assistant"
	}
	body := strings.Repeat("benchmark payload ", 50)
	ts := benchBaseTime.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":"/bench/project","gitBranch":"main",`+
		`"message":{"role":%q,"content":[{"type":"text","text":%q}]},"timestamp":%q}`+"\n",
		role, sessionID, role, fmt.Sprintf("%s line %d", body, i), ts)
}

// syntheticChunk returns JSONL text of at least bytes and the next line index.
func syntheticChunk(sessionID string, from int, bytes int64) (string, int) {
	var sb strings.Builder
	sb.Grow(int(bytes) + benchLineBytes)
	i := from
	for int64(sb.Len()) < bytes {
		sb.WriteString(syntheticLine(sessionID, i))
		i++
	}
	return sb.String(), i
}

// appendChunk grows the session file by at least bytes.
func appendChunk(tb testing.TB, path, sessionID string, from int, bytes int64) int {
	tb.Helper()
	chunk, next := syntheticChunk(sessionID, from, bytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		tb.Fatalf("open session file: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(chunk); err != nil {
		tb.Fatalf("append chunk: %v", err)
	}
	return next
}

// benchIndexer builds an indexer backed by an on-disk store in a temp dir, so
// the measurement is not distorted by holding messages.db in memory.
func benchIndexer(tb testing.TB, withArchive bool) (*Indexer, *storage.Store) {
	tb.Helper()
	dir := tb.TempDir()
	store, err := storage.Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { store.Close() })

	var arch *archive.Archiver
	if withArchive {
		arch = archive.New(filepath.Join(dir, "archive"))
	}
	return New(store, bloom.New(), arch, nil), store
}

// forceFullPath clears the append watermark so the next IndexSession re-parses
// the whole file — the "before" side of the comparison, expressed with the
// public API only.
func forceFullPath(tb testing.TB, store *storage.Store, path string) {
	tb.Helper()
	info, err := os.Stat(path)
	if err != nil {
		tb.Fatalf("stat %s: %v", path, err)
	}
	if err := store.UpsertIndexState(&models.IndexState{
		JSONLPath: path,
		FileSize:  info.Size() - 1, // differs from stat, so a reindex is due
		FileMtime: info.ModTime(),
		IndexedAt: time.Now(),
		// no offset/session/message watermark: full reparse
	}); err != nil {
		tb.Fatalf("clear watermark: %v", err)
	}
}

// measureResult reports what one indexing call cost.
type measureResult struct {
	elapsed   time.Duration
	allocated uint64 // bytes allocated during the call (TotalAlloc delta)
	peakHeap  uint64 // highest HeapAlloc observed while the call ran
}

// sampleInterval is how often HeapAlloc is sampled. ReadMemStats stops the
// world, so sampling too eagerly would distort the very timing we measure.
const sampleInterval = 10 * time.Millisecond

func measureCall(fn func()) measureResult {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var peak atomic.Uint64
	stop := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()

	start := time.Now()
	fn()
	elapsed := time.Since(start)
	close(stop)
	<-samplerDone

	runtime.ReadMemStats(&after)
	if after.TotalAlloc < before.TotalAlloc {
		// counter reset (rare, e.g. after FreeOSMemory) — report 0 rather than a wrap
		return measureResult{elapsed: elapsed, peakHeap: peak.Load()}
	}
	return measureResult{
		elapsed:   elapsed,
		allocated: after.TotalAlloc - before.TotalAlloc,
		peakHeap:  peak.Load(),
	}
}

func benchEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// TestReindexScalingReport grows one session in steps and reports, per append,
// the wall time and allocation cost of the incremental path against the full
// reparse over the identical file state. It doubles as a correctness guard: the
// stored message count must match the watermark after both passes.
func TestReindexScalingReport(t *testing.T) {
	if testing.Short() {
		t.Skip("scaling report is skipped with -short")
	}

	targetMB := benchEnvInt("YESMEM_BENCH_MB", 16)
	steps := benchEnvInt("YESMEM_BENCH_STEPS", 8)
	withArchive := os.Getenv("YESMEM_BENCH_ARCHIVE") != "0"

	idx, store := benchIndexer(t, withArchive)
	dir := t.TempDir()
	path := filepath.Join(dir, "bench-session.jsonl")
	const sessionID = "bench-scaling"

	totalBytes := int64(targetMB) << 20
	chunkBytes := totalBytes / int64(steps)
	if chunkBytes < benchLineBytes {
		chunkBytes = benchLineBytes
	}

	// Seed with the first chunk and index it fully, so the first measured append
	// already has a watermark to work from.
	next := appendChunk(t, path, sessionID, 0, chunkBytes)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	t.Logf("append reindex scaling — target %d MB in %d steps, archiver=%v, sample=%v",
		targetMB, steps, withArchive, sampleInterval)
	t.Logf("%-5s %8s %10s %10s %9s %13s %13s %11s %11s",
		"step", "file MB", "incr ms", "full ms", "speedup", "incr allocMB", "full allocMB", "incr peakMB", "full peakMB")

	var totalIncr, totalFull time.Duration
	var totalIncrAlloc, totalFullAlloc uint64
	var incrPeakMax, fullPeakMax uint64

	for step := 1; step <= steps; step++ {
		next = appendChunk(t, path, sessionID, next, chunkBytes)

		incr := measureCall(func() {
			if err := idx.IndexSession(path); err != nil {
				t.Fatalf("incremental index step %d: %v", step, err)
			}
		})
		assertWatermarkMatchesStored(t, store, path, sessionID, "incremental", step)

		forceFullPath(t, store, path)
		full := measureCall(func() {
			if err := idx.IndexSession(path); err != nil {
				t.Fatalf("full index step %d: %v", step, err)
			}
		})
		assertWatermarkMatchesStored(t, store, path, sessionID, "full", step)

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}

		speedup := float64(full.elapsed) / float64(incr.elapsed)
		t.Logf("%-5d %8.1f %10.1f %10.1f %8.1fx %13.1f %13.1f %11.1f %11.1f",
			step, float64(info.Size())/(1<<20),
			float64(incr.elapsed.Microseconds())/1000,
			float64(full.elapsed.Microseconds())/1000,
			speedup,
			float64(incr.allocated)/(1<<20),
			float64(full.allocated)/(1<<20),
			float64(incr.peakHeap)/(1<<20),
			float64(full.peakHeap)/(1<<20))

		totalIncr += incr.elapsed
		totalFull += full.elapsed
		totalIncrAlloc += incr.allocated
		totalFullAlloc += full.allocated
		if incr.peakHeap > incrPeakMax {
			incrPeakMax = incr.peakHeap
		}
		if full.peakHeap > fullPeakMax {
			fullPeakMax = full.peakHeap
		}
	}

	t.Logf("totals over %d appends: incremental %.1f ms / %.1f MB allocated — full %.1f ms / %.1f MB allocated",
		steps,
		float64(totalIncr.Microseconds())/1000, float64(totalIncrAlloc)/(1<<20),
		float64(totalFull.Microseconds())/1000, float64(totalFullAlloc)/(1<<20))
	t.Logf("peak heap high-water mark: incremental %.1f MB — full %.1f MB",
		float64(incrPeakMax)/(1<<20), float64(fullPeakMax)/(1<<20))
	if totalIncr > 0 {
		t.Logf("aggregate speedup: %.1fx wall, %.1fx allocated",
			float64(totalFull)/float64(totalIncr),
			float64(totalFullAlloc)/float64(totalIncrAlloc))
	}
}

// assertWatermarkMatchesStored guards the measurement: if the incremental pass
// silently fell back or dropped messages, the reported numbers would be
// meaningless.
func assertWatermarkMatchesStored(t *testing.T, store *storage.Store, path, sessionID, label string, step int) {
	t.Helper()
	msgs, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("%s step %d: get messages: %v", label, step, err)
	}
	st, err := store.GetIndexState(path)
	if err != nil {
		t.Fatalf("%s step %d: get index state: %v", label, step, err)
	}
	if st.IndexedMsgCount != len(msgs) {
		t.Fatalf("%s step %d: watermark says %d messages, store holds %d",
			label, step, st.IndexedMsgCount, len(msgs))
	}
	if st.IndexedOffset > st.FileSize {
		t.Fatalf("%s step %d: offset %d beyond file size %d", label, step, st.IndexedOffset, st.FileSize)
	}
}

// BenchmarkAppendReindexIncremental appends one chunk and reindexes the session
// per iteration. The file keeps growing across iterations, so the reported
// per-iteration cost is the average append cost as a session approaches its
// final size.
func BenchmarkAppendReindexIncremental(b *testing.B) {
	idx, _ := benchIndexer(b, true)
	path := filepath.Join(b.TempDir(), "bench.jsonl")
	const sessionID = "bench-incremental"
	const chunk = 256 << 10

	next := appendChunk(b, path, sessionID, 0, 4<<20)
	if err := idx.IndexSession(path); err != nil {
		b.Fatalf("seed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next = appendChunk(b, path, sessionID, next, chunk)
		if err := idx.IndexSession(path); err != nil {
			b.Fatalf("index: %v", err)
		}
	}
}

// BenchmarkAppendReindexFullReparse appends the same chunk but clears the
// watermark first, so every iteration re-parses the whole file — the behaviour
// before the append fast path existed.
func BenchmarkAppendReindexFullReparse(b *testing.B) {
	idx, store := benchIndexer(b, true)
	path := filepath.Join(b.TempDir(), "bench.jsonl")
	const sessionID = "bench-full"
	const chunk = 256 << 10

	next := appendChunk(b, path, sessionID, 0, 4<<20)
	if err := idx.IndexSession(path); err != nil {
		b.Fatalf("seed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next = appendChunk(b, path, sessionID, next, chunk)
		forceFullPath(b, store, path)
		if err := idx.IndexSession(path); err != nil {
			b.Fatalf("index: %v", err)
		}
	}
}
