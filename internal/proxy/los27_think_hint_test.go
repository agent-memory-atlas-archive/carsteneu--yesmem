package proxy

import (
	"strings"
	"testing"
)

// YM-110: buildThinkReminder appended hints.NextTimestampHint() twice in the
// non-Claude branch — once inside the branch and once unconditionally at the
// end of the block. The duplicate also advances the rotating hint counter by
// two per request, so every second formulation was skipped.
//
// The assertion counts the trailer that NextTimestampHint appends to every
// formulation, which is stable across the rotating pool.
func TestLos27_BuildThinkReminder_NonClaudeSingleTimestampHint(t *testing.T) {
	const hintTrailer = "Use them as your time sense"

	tests := []struct {
		name      string
		nonClaude bool
	}{
		{name: "non-claude branch", nonClaude: true},
		{name: "claude branch", nonClaude: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{thinkCounters: make(map[string]int)}

			reminder := s.buildThinkReminder("thread-los27", "", tt.nonClaude)

			if got := strings.Count(reminder, hintTrailer); got != 1 {
				t.Errorf("timestamp hint appears %d times, want exactly 1\nreminder:\n%s", got, reminder)
			}
		})
	}
}
