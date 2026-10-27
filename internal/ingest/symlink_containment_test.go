package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

const symlinkMarker = "OUTSIDE-ONLY-MARKER-9f31"

// A document tree is read for its content, not for whatever its entries point
// at: a symlink leaving the tree must not pull a foreign file into the index
// (YM-277 / DEV-DT45).
func TestDocIngest_DoesNotFollowSymlinksOutOfTheSourceTree(t *testing.T) {
	store := mustOpenStore(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("# Secret\n\n"+symlinkMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "inside.md"), []byte("# Inside\n\ninside text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(docs, "leak.md")); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Project: "test-proj", Name: "symlink-docs"}
	if _, err := Run(cfg, []string{docs}, store); err != nil {
		t.Fatalf("ingest run: %v", err)
	}

	var leaked int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE content LIKE '%' || ? || '%'`, symlinkMarker).Scan(&leaked); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d chunk(s) carry content reached through a symlink", leaked)
	}

	var inside int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE content LIKE '%inside text%'`).Scan(&inside); err != nil {
		t.Fatalf("count inside chunks: %v", err)
	}
	if inside == 0 {
		t.Fatal("the regular document was not ingested")
	}
}

// A source path that is itself a symlink is the same escape one level up. The
// containment must hold at the read, not only in the directory walk: a path
// accepted by a check can be replaced before the read (YM-277).
func TestDocIngest_RefusesASourcePathThatIsASymlink(t *testing.T) {
	store := mustOpenStore(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("# Secret\n\n"+symlinkMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	docs := t.TempDir()
	link := filepath.Join(docs, "leak.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Project: "test-proj", Name: "symlink-source"}
	if _, err := Run(cfg, []string{link}, store); err != nil {
		t.Fatalf("ingest run: %v", err)
	}

	var leaked int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE content LIKE '%' || ? || '%'`, symlinkMarker).Scan(&leaked); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d chunk(s) were read through a symlinked source path", leaked)
	}
}
