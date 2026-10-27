package storage

import "testing"

func TestIsFileInCodeIndex_EmptyProject(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if s.IsFileInCodeIndex("nonexistent", "foo.go") {
		t.Error("should return false for nonexistent project")
	}
}

// The dismiss counters are read on every hook call, so their table must come
// from the schema instead of being created in the hook hot path (DT31 / YM-7).
func TestCodeNavDismissalsTableExistsAfterOpen(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if _, err := s.DB().Query("SELECT 1 FROM code_nav_dismissals LIMIT 1"); err != nil {
		t.Fatalf("code_nav_dismissals must exist after Open: %v", err)
	}
}

func TestIsFileInCodeIndex_ExactMatch(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	scanJSON := `{"Packages":[{"Files":[{"Path":"internal/proxy/proxy.go"},{"Path":"main.go"}]}]}`
	if err := s.PersistScan("testproject", scanJSON, "abc123", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	if !s.IsFileInCodeIndex("testproject", "internal/proxy/proxy.go") {
		t.Error("should find exact path match")
	}
	if !s.IsFileInCodeIndex("testproject", "main.go") {
		t.Error("should find root-level file")
	}
	if s.IsFileInCodeIndex("testproject", "nonexistent.go") {
		t.Error("should not find nonexistent file")
	}
}

func TestIsFileInCodeIndex_SuffixMatch(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	scanJSON := `{"Packages":[{"Files":[{"Path":"internal/proxy/proxy.go"}]}]}`
	if err := s.PersistScan("testproject", scanJSON, "abc123", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	if !s.IsFileInCodeIndex("testproject", "proxy.go") {
		t.Error("should match by filename suffix when unambiguous")
	}
}

// The code-nav lookup runs on every shell tool call, so the indexed paths are
// materialized with the scan instead of being parsed per call (DT31 / YM-7).
func TestPersistScan_MaterializesPathIndex(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	scanJSON := `{"Packages":[{"Files":[{"Path":"internal/proxy/proxy.go"},{"Path":"main.go"}]}]}`
	if err := s.PersistScan("testproject", scanJSON, "abc123", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path <> ''`, "testproject").Scan(&n); err != nil {
		t.Fatalf("per-path index must exist after a scan: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed paths = %d, want 2", n)
	}

	// The marker row records that this project is materialized even when a
	// document contributes no paths at all.
	var marker int
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path = ''`, "testproject").Scan(&marker); err != nil {
		t.Fatalf("count marker: %v", err)
	}
	if marker != 1 {
		t.Fatalf("materialization markers = %d, want 1", marker)
	}
}

// A new scan must replace the path index, not extend it — otherwise removed
// files keep blocking long after they are gone (DT31 / YM-7).
func TestPersistScan_ReplacesPathIndex(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := s.PersistScan("testproject", `{"Packages":[{"Files":[{"Path":"old.go"}]}]}`, "head1", 0); err != nil {
		t.Fatalf("persist first scan: %v", err)
	}
	if err := s.PersistScan("testproject", `{"Packages":[{"Files":[{"Path":"new.go"}]}]}`, "head2", 0); err != nil {
		t.Fatalf("persist second scan: %v", err)
	}

	if s.IsFileInCodeIndex("testproject", "old.go") {
		t.Error("a path from the previous scan must not stay indexed")
	}
	if !s.IsFileInCodeIndex("testproject", "new.go") {
		t.Error("the current scan's path must be indexed")
	}
}

// A document that yields no paths must still count as materialized: otherwise
// every hook call re-parses the document, the cost this index removes (DT31 / YM-7).
func TestPersistScan_EmptyDocumentIsMaterialized(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if err := s.PersistScan("testproject", `{"Packages":[]}`, "abc123", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	if !s.scanFilesIndexed("testproject") {
		t.Fatal("a persisted scan must count as materialized even without paths")
	}
	if s.IsFileInCodeIndex("testproject", "anything.go") {
		t.Error("an empty index must not match")
	}
}

// Scan rows written before the per-path index existed are still answerable:
// the index is rebuilt once from the stored document (DT31 / YM-7).
func TestIsFileInCodeIndex_BackfillsLegacyScan(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	scanJSON := `{"Packages":[{"Files":[{"Path":"internal/proxy/proxy.go"}]}]}`
	// Direct insert: this is what a row from an older binary looks like.
	if _, err := s.DB().Exec(`INSERT INTO project_scan (project, scan_json, git_head, cbm_mtime) VALUES (?, ?, ?, 0)`,
		"legacy", scanJSON, "abc123"); err != nil {
		t.Fatalf("insert legacy scan: %v", err)
	}

	if !s.IsFileInCodeIndex("legacy", "internal/proxy/proxy.go") {
		t.Fatal("a legacy scan row must still be answerable")
	}
	if s.IsFileInCodeIndex("legacy", "not-indexed.go") {
		t.Error("legacy row must not match a path it does not contain")
	}

	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path <> ''`, "legacy").Scan(&n); err != nil {
		t.Fatalf("count backfilled paths: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled paths = %d, want 1", n)
	}
}

func TestDismissCodeNav_NotDismissedInitially(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	if s.IsCodeNavDismissed("session-123", 5) {
		t.Error("should not be dismissed initially")
	}
}

func TestDismissCodeNav_IncrementBelowThreshold(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	for i := 0; i < 4; i++ {
		if err := s.DismissCodeNav("session-123"); err != nil {
			t.Fatalf("dismiss %d: %v", i+1, err)
		}
		if s.IsCodeNavDismissed("session-123", 5) {
			t.Errorf("should not be dismissed after %d dismissals", i+1)
		}
	}
}

func TestDismissCodeNav_AtThreshold(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	for i := 0; i < 5; i++ {
		if err := s.DismissCodeNav("session-123"); err != nil {
			t.Fatalf("dismiss %d: %v", i+1, err)
		}
	}
	if !s.IsCodeNavDismissed("session-123", 5) {
		t.Error("should be dismissed after 5 dismissals")
	}
}

func TestDismissCodeNav_SessionIsolation(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	for i := 0; i < 5; i++ {
		s.DismissCodeNav("session-A")
	}
	if s.IsCodeNavDismissed("session-B", 5) {
		t.Error("dismissal of session A should not affect session B")
	}
}
