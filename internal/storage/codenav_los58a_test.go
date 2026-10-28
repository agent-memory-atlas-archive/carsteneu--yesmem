package storage

import "testing"

// YM-409: a recursive grep or a find names a directory. The plugin answers that
// through the directory index; the Go hook asked IsFileInCodeIndex and got false,
// so the normal call-site search slipped through.
func TestLos58aIsTreeInCodeIndexDirectory(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	scanJSON := `{"Packages":[{"Files":[
		{"Path":"internal/hooks/check.go"},
		{"Path":"cmd/server/main.go"},
		{"Path":".github/workflows/ci.yml"},
		{"Path":"main.go"}]}]}`
	if err := s.PersistScan("testproject", scanJSON, "abc123", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	// Anchored at the project root and without a trailing separator: the command
	// names "internal", the index stores "internal/...".
	for _, dir := range []string{"internal", "internal/", "internal/hooks", "internal/hooks/"} {
		if !s.IsTreeInCodeIndex("testproject", dir) {
			t.Errorf("directory operand %q holds an indexed file, so it must answer true", dir)
		}
	}
	// A command that runs in a subdirectory names its operand relative to it, the
	// same way the file lookup already answers by suffix.
	if !s.IsTreeInCodeIndex("testproject", "server") {
		t.Error("a directory named relative to a subdirectory must answer true")
	}
	// Dot directories and names with a dot are directories too: the extension rule
	// exempted exactly the directories a repository usually has.
	for _, dir := range []string{".github", ".github/workflows"} {
		if !s.IsTreeInCodeIndex("testproject", dir) {
			t.Errorf("dot directory %q must answer true", dir)
		}
	}
	// The root operand of "grep -rn x ." is the project itself.
	if !s.IsTreeInCodeIndex("testproject", ".") {
		t.Error("the project root operand must answer true")
	}

	if s.IsTreeInCodeIndex("testproject", "docs") {
		t.Error("a directory without indexed files must not answer true")
	}
	if s.IsTreeInCodeIndex("testproject", "intern") {
		t.Error("a prefix that is not a path segment must not match")
	}
	// The file answers are unchanged: exact, by suffix, and root level.
	if !s.IsTreeInCodeIndex("testproject", "main.go") || !s.IsTreeInCodeIndex("testproject", "internal/hooks/check.go") {
		t.Error("the file lookups must keep working")
	}
	// An empty project has no tree at all.
	empty := mustOpen(t)
	defer empty.Close()
	if empty.IsTreeInCodeIndex("nowhere", ".") {
		t.Error("a project without a scan has no root to match")
	}
}
