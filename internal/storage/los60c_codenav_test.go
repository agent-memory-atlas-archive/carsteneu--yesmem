package storage

import (
	"path/filepath"
	"testing"
)

// YM-436 finding 4: the scan cache is keyed under the checkout the scanner resolved
// (codescan.ScanCacheKey), while the rows carry the paths relative to the directory
// the scanner walked. A scan rooted below the checkout therefore stores
// "daemon/wiki_tick.go" for a file a command on the checkout names as
// "internal/daemon/wiki_tick.go". The read side only asked whether a stored path
// ends with the requested one, never the other way round, so such a row stayed
// unreachable for exactly the commands the key was built for.
func TestLos60cLookupReachesASubtreeRootedScan(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	project := filepath.Join(t.TempDir(), "checkout")
	// The rows of a scan rooted at <checkout>/internal.
	scanJSON := `{"Packages":[{"Files":[{"Path":"daemon/wiki_tick.go"}]}]}`
	if err := s.PersistScan(project, scanJSON, "head1234", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	if !s.IsFileInCodeIndex(project, "internal/daemon/wiki_tick.go") {
		t.Error("a checkout-relative spelling must reach a row written below the checkout")
	}
	if !s.IsTreeInCodeIndex(project, "internal/daemon/wiki_tick.go") {
		t.Error("the tree lookup goes through the same file answer")
	}
	if s.IsFileInCodeIndex(project, "internal/daemon/other.go") {
		t.Error("another file of the same directory must not match")
	}
	// The fixture keeps the row below a directory, so the shorter spellings of
	// another subtree's path do not reach it. A row stored as a bare name (a scan
	// rooted one level down) is reachable from any subtree with that tail; that
	// looseness is the accepted side of the rule.
	if s.IsFileInCodeIndex(project, "internal/other/wiki_tick.go") {
		t.Error("a same-named file of another directory is not the stored row")
	}
}

// A scan rooted at <checkout>/internal writes the files directly below that root
// under their bare name ("version.go"), which is the shortest tail of the checkout
// spelling. Round one matched it, and the shorter spelling must not get lost.
func TestLos60cLookupReachesABareNameOfASubtreeRootedScan(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	project := filepath.Join(t.TempDir(), "checkout")
	scanJSON := `{"Packages":[{"Files":[{"Path":"version.go"}]}]}`
	if err := s.PersistScan(project, scanJSON, "head1234", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}
	if !s.IsFileInCodeIndex(project, "internal/version.go") {
		t.Error("a file at the scan root must be reachable by its checkout spelling")
	}
}

// The shorter spellings run from the first segment boundary down to the bare file
// name. The whole path is not listed, it is probed before.
func TestLos60cPathTails(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"internal/daemon/wiki_tick.go", []string{"daemon/wiki_tick.go", "wiki_tick.go"}},
		{"a/b/c/d.go", []string{"b/c/d.go", "c/d.go", "d.go"}},
		{"main.go", nil},
		{"internal/proxy.go", []string{"proxy.go"}},
	}
	for _, tc := range cases {
		got := pathTails(tc.path)
		if len(got) != len(tc.want) {
			t.Errorf("pathTails(%q) = %v, want %v", tc.path, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("pathTails(%q) = %v, want %v", tc.path, got, tc.want)
				break
			}
		}
	}
}

// A deep path is trimmed at the long end: the bare file name and the spellings of a
// shallow scan root survive, and the number of probes stays bounded.
func TestLos60cPathTailsIsBounded(t *testing.T) {
	deep := "a/b/c/d/e/f/g/h/i/j/k.go"
	tails := pathTails(deep)
	if len(tails) != maxIndexTails {
		t.Fatalf("pathTails(%q) has %d spellings, want %d: %v", deep, len(tails), maxIndexTails, tails)
	}
	if last := tails[len(tails)-1]; last != "k.go" {
		t.Errorf("the bare file name must survive the trimming, got %q", last)
	}
	if first := tails[0]; first != "d/e/f/g/h/i/j/k.go" {
		t.Errorf("the trimming must drop the longest spellings, got %q first", first)
	}
}
