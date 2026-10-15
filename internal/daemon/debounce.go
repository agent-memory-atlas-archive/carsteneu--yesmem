package daemon

import (
	"sort"
	"time"
)

// fileDebouncer turns a stream of file-write events into at most one index
// action per quiet period per file, plus a separate settle action once a file
// has been quiet long enough to be considered finished. A session that is being
// written continuously therefore triggers a single index pass per pause instead
// of one per write event.
type fileDebouncer struct {
	indexAfter  time.Duration
	settleAfter time.Duration

	pending map[string]time.Time // path → last observed write
	indexed map[string]time.Time // path → time of the last index action
}

func newFileDebouncer(indexAfter, settleAfter time.Duration) *fileDebouncer {
	return &fileDebouncer{
		indexAfter:  indexAfter,
		settleAfter: settleAfter,
		pending:     make(map[string]time.Time),
		indexed:     make(map[string]time.Time),
	}
}

// mark records a write event for path, restarting its quiet window.
func (d *fileDebouncer) mark(path string, now time.Time) {
	d.pending[path] = now
}

// due reports which paths are ready to be indexed and which have settled.
// Paths are sorted so the caller's action order is deterministic. A path that is
// quiet for longer than settleAfter is dropped after being reported, so a later
// write starts a fresh window.
func (d *fileDebouncer) due(now time.Time) (toIndex, settled []string) {
	for path, lastChange := range d.pending {
		quiet := now.Sub(lastChange)

		if quiet > d.indexAfter {
			if lastIdx, ok := d.indexed[path]; !ok || lastChange.After(lastIdx) {
				d.indexed[path] = now
				toIndex = append(toIndex, path)
			}
		}

		if quiet > d.settleAfter {
			delete(d.pending, path)
			settled = append(settled, path)
		}
	}

	sort.Strings(toIndex)
	sort.Strings(settled)
	return toIndex, settled
}
