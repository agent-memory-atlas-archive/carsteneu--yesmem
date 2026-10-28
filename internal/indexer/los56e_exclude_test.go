package indexer

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los56eExclusions builds the exclusion map exactly the way the production
// constructors do, so the test fails when their normalization regresses.
func los56eExclusions(t *testing.T, configured []string) map[string]bool {
	t.Helper()
	return New(nil, nil, nil, configured).excludeProject
}

// TestLos56eExclusionListIgnoresSpelling is the regression test for YM-320 (3):
// the exclusion list was stored with strings.ToLower alone, in two constructors
// that each built their own map. A path written with a trailing separator
// ("/home/tester/private/") therefore never matched the stored project
// ("/home/tester/private"), and the project was indexed although the operator
// meant to keep it out. The same list feeds the daemon's opencode scanner, so a
// silently dead entry leaks private transcripts into memory.
func TestLos56eExclusionListIgnoresSpelling(t *testing.T) {
	configured := []string{
		"  /home/tester/private/  ",
		"/home/tester/Other//",
		"memory",
		"",
	}
	exclude := los56eExclusions(t, configured)

	cases := []struct {
		name           string
		project, short string
		want           bool
	}{
		{"trailing sep in the list, plain path in the row", "/home/tester/private", "private", true},
		{"plain path in the list, trailing sep in the row", "/home/tester/other/", "other", true},
		{"whitespace and case in the list", "/HOME/Tester/PRIVATE", "", true},
		{"double separator in the list", "/home/tester/other", "", true},
		{"short name entry", "/home/tester/elsewhere/x", "MEMORY", true},
		{"short name row with a trailing sep", "/home/tester/elsewhere/y", "memory/", true},
		{"unrelated path", "/home/tester/public", "public", false},
		{"empty project never matches", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectExcluded(exclude, tc.project, tc.short); got != tc.want {
				t.Errorf("projectExcluded(%q, %q) = %v, want %v", tc.project, tc.short, got, tc.want)
			}
		})
	}
}

// TestLos56eBothIndexersUseTheSameNormalization pins that the two constructors
// cannot drift apart again: both must exclude the same project for the same
// configured spelling.
func TestLos56eBothIndexersUseTheSameNormalization(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	configured := []string{"/home/tester/private/"}

	idx := New(store, nil, nil, configured)
	if !projectExcluded(idx.excludeProject, "/home/tester/private", "private") {
		t.Error("the JSONL indexer must exclude a list entry written with a trailing separator")
	}

	scanner := NewOpencodeScanner(":memory:", store, nil, nil, configured, nil)
	if !projectExcluded(scanner.excludeProject, "/home/tester/private", "private") {
		t.Error("the opencode scanner must exclude a list entry written with a trailing separator")
	}
}
