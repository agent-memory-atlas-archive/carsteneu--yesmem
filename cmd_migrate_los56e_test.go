package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TestLos56eMigrateProjectMovesTheArchiveDirectory is the regression test for
// YM-320 (1): MigrateProject rewrote sessions.project but left the archive
// directory under the old encoded name. The session documents archived there
// were then written a second time under the new name by the next archiving pass,
// so every session of a renamed project existed twice in the archive.
func TestLos56eMigrateProjectMovesTheArchiveDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	fromPath := filepath.Join(home, "old-project")
	toPath := filepath.Join(home, "new-project")

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertSession(&models.Session{
		ID:           "ses-los56e",
		Project:      fromPath,
		ProjectShort: filepath.Base(fromPath),
		MessageCount: 40,
		StartedAt:    time.Now().Add(-2 * time.Hour),
		EndedAt:      time.Now().Add(-time.Hour),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// A session document archived under the old project's encoded name.
	oldDir := filepath.Join(dataDir, "archive", archive.ProjectDirName(fromPath))
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const docName = "ses-los56e.jsonld"
	if err := os.WriteFile(filepath.Join(oldDir, docName), []byte(`{"id":"ses-los56e"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	prevArgs := os.Args
	t.Cleanup(func() { os.Args = prevArgs })
	os.Args = []string{"yesmem", "migrate-project", fromPath, toPath, "--no-backup"}
	runMigrateProject()

	newDir := filepath.Join(dataDir, "archive", archive.ProjectDirName(toPath))
	body, err := os.ReadFile(filepath.Join(newDir, docName))
	if err != nil {
		t.Fatalf("session document must be under the new project's archive dir: %v", err)
	}
	if string(body) != `{"id":"ses-los56e"}` {
		t.Fatalf("moved document content = %q", string(body))
	}
	if _, err := os.Stat(filepath.Join(oldDir, docName)); err == nil {
		t.Error("the document must not stay behind under the old project's dir")
	}

	// The database rename itself must still have happened.
	store2, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	sess, err := store2.GetSession("ses-los56e")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Project != toPath {
		t.Errorf("sessions.project = %q, want %q", sess.Project, toPath)
	}
}

// TestLos56eMigrateProjectMergesIntoAnExistingArchiveDir covers the second half
// of YM-320 (1): a target project that already owns archived sessions must not
// lose them, and the moved copies must not linger beside it.
func TestLos56eMigrateProjectMergesIntoAnExistingArchiveDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	fromPath := filepath.Join(home, "old-project")
	toPath := filepath.Join(home, "new-project")

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertSession(&models.Session{
		ID:           "ses-los56e",
		Project:      fromPath,
		ProjectShort: filepath.Base(fromPath),
		MessageCount: 40,
		StartedAt:    time.Now().Add(-2 * time.Hour),
		EndedAt:      time.Now().Add(-time.Hour),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	oldDir := filepath.Join(dataDir, "archive", archive.ProjectDirName(fromPath))
	newDir := filepath.Join(dataDir, "archive", archive.ProjectDirName(toPath))
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, "moved.jsonld"), []byte("from-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "kept.jsonld"), []byte("already-there"), 0o600); err != nil {
		t.Fatal(err)
	}

	prevArgs := os.Args
	t.Cleanup(func() { os.Args = prevArgs })
	os.Args = []string{"yesmem", "migrate-project", fromPath, toPath, "--no-backup"}
	runMigrateProject()

	if body, err := os.ReadFile(filepath.Join(newDir, "kept.jsonld")); err != nil || string(body) != "already-there" {
		t.Fatalf("existing document must survive the merge: %v %q", err, string(body))
	}
	if body, err := os.ReadFile(filepath.Join(newDir, "moved.jsonld")); err != nil || string(body) != "from-old" {
		t.Fatalf("moved document must land next to the existing ones: %v %q", err, string(body))
	}
	if _, err := os.Stat(filepath.Join(oldDir, "moved.jsonld")); err == nil {
		t.Error("a merged document must not stay behind under the old dir")
	}
}

// TestLos56eMigrateProjectUsesTheConfiguredArchiveDir covers the review finding on
// YM-320: the command derived the archive path from the data directory, while the
// daemon derives it from paths.archive. With a configured archive the move was a
// silent no-op, because the folder it looked for did not exist there.
func TestLos56eMigrateProjectUsesTheConfiguredArchiveDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customArchive := filepath.Join(home, "custom-archive")
	if err := os.MkdirAll(customArchive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"),
		[]byte("paths:\n  archive: "+customArchive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fromPath := filepath.Join(home, "old-project")
	toPath := filepath.Join(home, "new-project")

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.UpsertSession(&models.Session{
		ID:           "ses-los56e",
		Project:      fromPath,
		ProjectShort: filepath.Base(fromPath),
		MessageCount: 40,
		StartedAt:    time.Now().Add(-2 * time.Hour),
		EndedAt:      time.Now().Add(-time.Hour),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	oldDir := filepath.Join(customArchive, archive.ProjectDirName(fromPath))
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const docName = "ses-los56e.jsonld"
	if err := os.WriteFile(filepath.Join(oldDir, docName), []byte(`{"id":"ses-los56e"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	prevArgs := os.Args
	t.Cleanup(func() { os.Args = prevArgs })
	os.Args = []string{"yesmem", "migrate-project", fromPath, toPath, "--no-backup"}
	runMigrateProject()

	newDir := filepath.Join(customArchive, archive.ProjectDirName(toPath))
	if _, err := os.Stat(filepath.Join(newDir, docName)); err != nil {
		t.Fatalf("the configured archive dir must be the one that moves: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archive")); err == nil {
		t.Error("a hardcoded dataDir/archive must not be created beside a configured one")
	}
}
