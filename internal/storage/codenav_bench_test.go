package storage

import (
	"encoding/json"
	"fmt"
	"testing"
)

// benchScanJSON builds a scan document of roughly the size yesmem's own index
// reaches, so the per-call cost of IsFileInCodeIndex is visible.
func benchScanJSON(packages, filesPerPkg int) string {
	type file struct{ Path string }
	type pkg struct {
		Name  string
		Files []file
	}
	pkgs := make([]pkg, 0, packages)
	for i := 0; i < packages; i++ {
		p := pkg{Name: fmt.Sprintf("internal/pkg%03d", i)}
		for j := 0; j < filesPerPkg; j++ {
			p.Files = append(p.Files, file{Path: fmt.Sprintf("internal/pkg%03d/file%03d.go", i, j)})
		}
		pkgs = append(pkgs, p)
	}
	data, _ := json.Marshal(map[string]any{"Packages": pkgs})
	return string(data)
}

// The code-nav check runs this lookup on every shell tool call, so its cost is
// on the hook hot path (DT31 / YM-7).
func BenchmarkIsFileInCodeIndex(b *testing.B) {
	s, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	scan := benchScanJSON(400, 100) // 40k files
	if err := s.PersistScan("bench", scan, "abc123", 0); err != nil {
		b.Fatal(err)
	}
	b.Logf("scan json: %.0f KB", float64(len(scan))/1024)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.IsFileInCodeIndex("bench", "internal/pkg399/file099.go") {
			b.Fatal("lookup must find an indexed path")
		}
	}
}

// A lookup that names only the file (no directory) has to fall back to the
// suffix rule, which compares every same-named candidate in the project.
func BenchmarkIsFileInCodeIndexByBaseName(b *testing.B) {
	s, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	scan := benchScanJSON(400, 100)
	if err := s.PersistScan("bench", scan, "abc123", 0); err != nil {
		b.Fatal(err)
	}
	b.Logf("same-named candidates for the probe: 400")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.IsFileInCodeIndex("bench", "file099.go") {
			b.Fatal("suffix lookup must find an indexed path")
		}
	}
}

// A miss is the case the reversed question adds a query to: exact and narrow suffix
// answer nothing, so the shorter spellings are probed. This benchmark measures the
// whole miss path against the widest candidate set the base index admits (400
// same-named rows), so its number is an upper bound for that added cost and is
// dominated by the pre-existing non-matching suffix scan (YM-436).
func BenchmarkIsFileInCodeIndexMiss(b *testing.B) {
	s, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	scan := benchScanJSON(400, 100)
	if err := s.PersistScan("bench", scan, "abc123", 0); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.IsFileInCodeIndex("bench", "internal/other/file099.go") {
			b.Fatal("lookup must miss")
		}
	}
}

// The added cost alone: a bare name that no row carries, so the exact probe and the
// suffix scan return without candidates and no shorter spelling exists. Subtracting
// this from BenchmarkIsFileInCodeIndexTailProbe leaves the tail probes.
func BenchmarkIsFileInCodeIndexMissBareName(b *testing.B) {
	s, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	scan := benchScanJSON(400, 100)
	if err := s.PersistScan("bench", scan, "abc123", 0); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.IsFileInCodeIndex("bench", "zzz_absent.go") {
			b.Fatal("lookup must miss")
		}
	}
}

// The miss with the shorter spellings: the same bare name, but under a directory, so
// two tail probes run. The difference to the bare-name miss is the part this rule
// adds to the hook hot path.
func BenchmarkIsFileInCodeIndexTailProbe(b *testing.B) {
	s, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	scan := benchScanJSON(400, 100)
	if err := s.PersistScan("bench", scan, "abc123", 0); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.IsFileInCodeIndex("bench", "internal/other/zzz_absent.go") {
			b.Fatal("lookup must miss")
		}
	}
}
