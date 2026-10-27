package proxy

import (
	"hash/fnv"
	"regexp"
	"strings"
)

// The fail counter answers one question per injected learning: did an action
// this learning was supposed to help with fail anyway? Its natural trigger, a
// tool error in the turn, fires on every request of an error loop. Without an
// identity for the failure a single repeated build error raised fail_count once
// per turn, and from fail_count 5 the PreToolUse hook hard-blocks on the
// learning — one stuck build punished the learning, not the loop.
//
// The fingerprint is the error text itself: the same failure counts once, a new
// failure counts again.

// Volatile parts of a failure message. A failing `go test` reports the same
// failure with a new duration, a new build directory and new panic pointers on
// every run; without normalizing them every repetition looks like a new error
// and the counter is back to one increment per turn, which is what this file
// exists to prevent.
var (
	// Durations come in ns/µs/ms/s — a failing go test prints "(0.03s)" but a
	// slow one prints "(1.2s)" and a benchmark "(12.34ms)".
	failureDurationRe = regexp.MustCompile(`\([0-9]+(\.[0-9]+)?(ns|µs|ms|s)\)`)
	// Build directories and test temp dirs: /tmp/go-build123/... and
	// /tmp/TestFoo123/001 (t.TempDir) are new on every run.
	failureBuildDirRe  = regexp.MustCompile(`/tmp/(go-build|Test)[A-Za-z0-9_]*[0-9]+`)
	failureAddrRe      = regexp.MustCompile(`\b0x[0-9a-f]{6,}\b`)
	failureGoroutineRe = regexp.MustCompile(`goroutine [0-9]+`)
)

// errorFingerprint hashes the normalized failure text. Two failures that differ
// in the message stay two failures; everything a re-run changes by itself does
// not.
func errorFingerprint(text string) uint64 {
	normalized := failureDurationRe.ReplaceAllString(text, "(#s)")
	normalized = failureBuildDirRe.ReplaceAllString(normalized, "/tmp/go-build#")
	normalized = failureAddrRe.ReplaceAllString(normalized, "0x#")
	normalized = failureGoroutineRe.ReplaceAllString(normalized, "goroutine #")

	h := fnv.New64a()
	h.Write([]byte(strings.TrimSpace(normalized)))
	return h.Sum64()
}

// freshFailIDs returns the ids whose current failure differs from the one that
// was already counted for this thread. Recording happens here, so a second call
// with the same error returns nothing.
func (s *Server) freshFailIDs(threadID string, ids []int64, errorText string) []int64 {
	if threadID == "" || len(ids) == 0 || strings.TrimSpace(errorText) == "" {
		// No identity available: keep the historic behaviour rather than
		// dropping the signal entirely.
		return ids
	}
	print := errorFingerprint(errorText)

	s.failStateMu.Lock()
	defer s.failStateMu.Unlock()
	if s.failFingerprints == nil {
		s.failFingerprints = make(map[string]map[int64]uint64)
	}
	seen := s.failFingerprints[threadID]
	if seen == nil {
		seen = make(map[int64]uint64)
		s.failFingerprints[threadID] = seen
	}

	fresh := make([]int64, 0, len(ids))
	for _, id := range ids {
		if prev, ok := seen[id]; ok && prev == print {
			continue
		}
		seen[id] = print
		fresh = append(fresh, id)
	}
	return fresh
}
