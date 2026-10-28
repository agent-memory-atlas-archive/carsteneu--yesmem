package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/clustering"
)

func TestFindProjectDir_Exact(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-project")
	os.MkdirAll(dir, 0755)

	result := findProjectDir(root, "/home/user/project")
	if result != dir {
		t.Errorf("expected %q, got %q", dir, result)
	}
}

// The scan fallback that this test used to pin matched on the trailing name
// segment only; YM-279 (Los 11f) removed it because it wrote into foreign
// project directories. See memorymd_los11f_test.go for the replacement.
func TestFindProjectDir_NotFound(t *testing.T) {
	root := t.TempDir()
	result := findProjectDir(root, "/nonexistent/path")
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestLabelFromDocs_Empty(t *testing.T) {
	result := labelFromDocs(nil)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestLabelFromDocs_Short(t *testing.T) {
	docs := []clustering.Document{{Content: "short label"}}
	result := labelFromDocs(docs)
	if result != "short label" {
		t.Errorf("expected 'short label', got %q", result)
	}
}

func TestLabelFromDocs_Truncation(t *testing.T) {
	long := make([]rune, 100)
	for i := range long {
		long[i] = 'x'
	}
	docs := []clustering.Document{{Content: string(long)}}
	result := labelFromDocs(docs)
	if len([]rune(result)) != 60 {
		t.Errorf("expected 60 runes, got %d", len([]rune(result)))
	}
}
