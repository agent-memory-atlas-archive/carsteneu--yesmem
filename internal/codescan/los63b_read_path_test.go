package codescan

import "testing"

// YM-453: a row written before the fix could hold another checkout's document
// under this key. Serving it hands this checkout another branch's file list —
// the very symptom the ticket removes on the write side — so the read path asks
// the same rule.
func TestLos63bForeignStoredDocumentIsNotServed(t *testing.T) {
	main, worktree := los63bWorktree(t)
	key := ScanCacheKey(main)
	head := ReadGitHead(main)
	if head == "" {
		t.Fatal("fixture broken: no git head in the main checkout")
	}

	store := &los04eRowStore{rows: map[string]los04eRow{}}
	foreign := `{"RootDir":"` + worktree + `","Packages":[{"Files":[{"Path":"internal/storage/store.go"}]}]}`
	store.rows[key] = los04eRow{scanJSON: foreign, gitHead: head}

	inner := &countingScanner{}
	result, err := NewCachedScanner(inner).WithStore(store).Scan(main)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.RootDir != main {
		t.Fatalf("the main checkout was served the document of %q", result.RootDir)
	}
	if inner.calls != 1 {
		t.Errorf("a document rooted elsewhere must not count as a cache hit; inner scans = %d", inner.calls)
	}
}
