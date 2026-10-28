package indexer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// seedBackfillSession indexes one session the way the opencode scanner leaves it
// behind: a row without a JSONL file, so the archive document is its only
// archived form.
func seedBackfillSession(t *testing.T, store *storage.Store, rawID, projectDir string, texts ...string) string {
	t.Helper()
	id := models.NormalizeSessionID(models.SourceAgentOpencode, rawID)
	sess := &models.Session{
		ID:           id,
		Project:      projectDir,
		MessageCount: len(texts),
		StartedAt:    time.Now().Add(-time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}
	if err := store.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	msgs := make([]models.Message, 0, len(texts))
	for i, text := range texts {
		msgs = append(msgs, models.Message{
			SessionID:   id,
			SourceAgent: models.SourceAgentOpencode,
			Role:        "user",
			MessageType: "text",
			Content:     text,
			Timestamp:   sess.StartedAt.Add(time.Duration(i) * time.Second),
			Sequence:    i,
		})
	}
	if err := store.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	return id
}

func openBackfillStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func readBackfillDoc(t *testing.T, path string) sessionExport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive document %s: %v", path, err)
	}
	var doc sessionExport
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("archive document is not valid JSON: %v", err)
	}
	return doc
}

// A session indexed before the scanner archived anything keeps its place in the
// index but has no document, and the watermark never looks at it again. The
// catch-up path is what turns an indexed backlog into archived sessions.
func TestArchiveMissingSessions_WritesDocumentForLegacySession(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/legacy"
	id := seedBackfillSession(t, store, "ses_legacy", projectDir, "archived late", "second turn")

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if res.Considered != 1 || res.Present != 0 {
		t.Errorf("counts: considered=%d present=%d, want 1/0", res.Considered, res.Present)
	}
	if len(res.Written) != 1 || res.Written[0] != id {
		t.Errorf("written: got %+v, want [%s]", res.Written, id)
	}

	path := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	doc := readBackfillDoc(t, path)
	if doc.Format != sessionExportFormat {
		t.Errorf("format marker: got %q, want %q", doc.Format, sessionExportFormat)
	}
	if doc.Session == nil || doc.Session.ID != id {
		t.Errorf("archived session: got %+v, want id %s", doc.Session, id)
	}
	if len(doc.Messages) != 2 || doc.Messages[0].Content != "archived late" {
		t.Errorf("archived messages: got %+v, want both indexed turns", doc.Messages)
	}
}

// The catch-up must be cheap to repeat: a document that is already there is not
// rewritten, which is what keeps a full run from copying the whole archive again.
func TestArchiveMissingSessions_KeepsExistingDocument(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/present"
	id := seedBackfillSession(t, store, "ses_present", projectDir, "already archived")

	archiveDir := filepath.Join(t.TempDir(), "archive")
	if _, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	const sentinel = "sentinel"
	if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Present != 1 || len(res.Written) != 0 {
		t.Errorf("second run: present=%d written=%+v, want 1/none", res.Present, res.Written)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != sentinel {
		t.Errorf("existing document was rewritten: %q (%v)", raw, err)
	}
}

func TestArchiveMissingSessions_DryRunWritesNothing(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/dryrun"
	id := seedBackfillSession(t, store, "ses_dryrun", projectDir, "only listed")

	var progress bytes.Buffer
	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{DryRun: true, Progress: &progress})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(res.Written) != 1 || res.Written[0] != id {
		t.Errorf("dry run list: got %+v, want [%s]", res.Written, id)
	}
	if !bytes.Contains(progress.Bytes(), []byte("would archive "+id)) {
		t.Errorf("progress output: got %q, want a would-archive line", progress.String())
	}
	path := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dry run wrote %s: %v", path, err)
	}
}

// Sessions that do have a JSONL file are archived by ArchiveFile; an extra export
// document would be a second copy of the same conversation.
func TestArchiveMissingSessions_SkipsSessionsWithJSONLFile(t *testing.T) {
	store := openBackfillStore(t)

	jsonl := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &models.Session{
		ID:           "claude_file-backed",
		Project:      "/home/test/file-backed",
		JSONLPath:    jsonl,
		JSONLSize:    3,
		StartedAt:    time.Now().Add(-time.Hour),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentClaude,
		MessageCount: 1,
	}
	if err := store.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}

	res, err := ArchiveMissingSessions(store, archive.New(filepath.Join(t.TempDir(), "archive")), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if res.Considered != 0 || len(res.Written) != 0 {
		t.Errorf("file-backed session was considered: considered=%d written=%+v", res.Considered, res.Written)
	}
}

// A failed archive write leaves the session indexed but unarchived, and the
// watermark moves past it, so nothing else ever visits it again. The catch-up has
// to pick it up once the obstacle is gone.
func TestArchiveMissingSessions_RetriesFailedArchiveWrite(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/blocked"
	id := seedBackfillSession(t, store, "ses_blocked", projectDir, "written on the second try")

	archiveDir := filepath.Join(t.TempDir(), "archive")
	// The destination name is occupied by a directory, so the atomic rename into
	// place fails and no document is written.
	blocker := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{}); err == nil {
		t.Fatal("blocked write reported success")
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(res.Written) != 1 || res.Written[0] != id {
		t.Errorf("retry written: got %+v, want [%s]", res.Written, id)
	}
	if doc := readBackfillDoc(t, blocker); doc.Session == nil || doc.Session.ID != id {
		t.Errorf("retried document: got %+v", doc.Session)
	}
}

func TestArchiveMissingSessions_FiltersByProject(t *testing.T) {
	store := openBackfillStore(t)

	keep := seedBackfillSession(t, store, "ses_keep", "/home/test/keep", "wanted")
	seedBackfillSession(t, store, "ses_drop", "/home/test/drop", "not wanted")

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{Project: "/home/test/keep"})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if len(res.Written) != 1 || res.Written[0] != keep {
		t.Errorf("written: got %+v, want [%s]", res.Written, keep)
	}
	dropped := filepath.Join(archiveDir, archive.ProjectDirName("/home/test/drop"), archiveDocumentName("opencode:ses_drop"))
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Errorf("filtered project was archived: %v", err)
	}
}

// A session whose stored message count disagrees with the readable messages is a
// read that raced the re-indexing scanner or lost rows. Writing it would freeze a
// truncated conversation as "archived" for good.
func TestArchiveMissingSessions_LeavesIncompleteSessionUnwritten(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/partial"
	id := seedBackfillSession(t, store, "ses_partial", projectDir, "only one of three")
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	sess.MessageCount = 3
	if err := store.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if len(res.Written) != 0 {
		t.Errorf("incomplete session was archived: %+v", res.Written)
	}
	if len(res.Incomplete) != 1 || res.Incomplete[0] != id {
		t.Errorf("incomplete list: got %+v, want [%s]", res.Incomplete, id)
	}
	path := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("incomplete session was written: %v", err)
	}
}

// A document without messages is never the goal either: the count matches here,
// because the row was indexed while it had no messages, yet the conversation was
// written in between.
func TestArchiveMissingSessions_LeavesEmptySessionUnwritten(t *testing.T) {
	store := openBackfillStore(t)

	const projectDir = "/home/test/empty"
	id := seedBackfillSession(t, store, "ses_empty", projectDir)

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if len(res.Written) != 0 {
		t.Errorf("session without messages was archived: %+v", res.Written)
	}
	if len(res.Incomplete) != 1 || res.Incomplete[0] != id {
		t.Errorf("incomplete list: got %+v, want [%s]", res.Incomplete, id)
	}
	path := filepath.Join(archiveDir, archive.ProjectDirName(projectDir), archiveDocumentName(id))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty session was written: %v", err)
	}
}

// Copying rows of an excluded project into the permanent archive would overrule
// the exclusion for every session that predates it.
func TestArchiveMissingSessions_HonorsExcludedProjects(t *testing.T) {
	store := openBackfillStore(t)

	const excludedDir = "/home/test/secret"
	seedBackfillSession(t, store, "ses_secret", excludedDir, "must not be archived")
	open := seedBackfillSession(t, store, "ses_open", "/home/test/open", "archived")

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{
		ExcludeProjects: []string{excludedDir},
	})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if res.Excluded != 1 {
		t.Errorf("excluded count: got %d, want 1", res.Excluded)
	}
	if len(res.Written) != 1 || res.Written[0] != open {
		t.Errorf("written: got %+v, want [%s]", res.Written, open)
	}
	path := filepath.Join(archiveDir, archive.ProjectDirName(excludedDir), archiveDocumentName("opencode:ses_secret"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("excluded project was archived: %v", err)
	}
}

func TestArchiveMissingSessions_StopsAtLimit(t *testing.T) {
	store := openBackfillStore(t)

	for _, id := range []string{"ses_a", "ses_b", "ses_c"} {
		seedBackfillSession(t, store, id, "/home/test/limited", id)
	}

	archiveDir := filepath.Join(t.TempDir(), "archive")
	res, err := ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{Limit: 2})
	if err != nil {
		t.Fatalf("archive missing sessions: %v", err)
	}
	if len(res.Written) != 2 {
		t.Errorf("written: got %d (%+v), want 2", len(res.Written), res.Written)
	}

	// The remaining session is picked up by the next run.
	res, err = ArchiveMissingSessions(store, archive.New(archiveDir), ArchiveBackfillOptions{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Present != 2 || len(res.Written) != 1 {
		t.Errorf("second run: present=%d written=%+v, want 2/one more", res.Present, res.Written)
	}
}
