package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TestDocReingest_KeepsForeignProjectLearningsLos63a pins YM-452 on the production
// path. processFile drops the chunks a changed document no longer produces and
// retires the learnings that referenced them. The scope of that retirement comes
// from the doc source's project, so the learning of another project must survive
// the re-ingest, while the source's own learning is retired and counted.
func TestDocReingest_KeepsForeignProjectLearningsLos63a(t *testing.T) {
	const (
		owner   = "/home/tester/memory/los63a-owner"
		foreign = "/home/tester/memory/los63a-other"
	)
	store := mustOpenStore(t)
	dir := t.TempDir()
	docPath := filepath.Join(dir, "guide.md")
	if err := os.WriteFile(docPath, []byte("# Guide\n\nThe second revision of the prose.\n"), 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}

	sourceID, err := store.UpsertDocSource(&storage.DocSource{Name: "los63a-reingest-docs", Project: owner, Path: dir})
	if err != nil {
		t.Fatalf("upsert doc source: %v", err)
	}
	// The chunk of the first revision: its content hash matches nothing the
	// current document produces, so the re-ingest removes it.
	oldChunkID, err := store.InsertDocChunk(&storage.DocChunk{
		SourceID:    sourceID,
		SourceFile:  "guide.md",
		SourceHash:  "los63a-first-revision",
		Content:     "The first revision of the prose.",
		ContentHash: "los63a-first-content",
	})
	if err != nil {
		t.Fatalf("insert chunk of the first revision: %v", err)
	}
	insert := func(project string) int64 {
		t.Helper()
		id, err := store.InsertLearning(&models.Learning{
			Content:     "A fact distilled from the first revision.",
			Category:    "gotcha",
			Project:     project,
			Source:      "docs_extracted",
			DocChunkRef: oldChunkID,
		})
		if err != nil {
			t.Fatalf("insert learning of %q: %v", project, err)
		}
		return id
	}
	own := insert(owner)
	other := insert(foreign)

	result := &Result{}
	if _, _, err := processFile(Config{Project: owner}, sourceFile{root: dir, path: docPath}, dir, sourceID, store, result); err != nil {
		t.Fatalf("process the changed document: %v", err)
	}

	ownLearning, err := store.GetLearning(own)
	if err != nil || ownLearning == nil {
		t.Fatalf("read the own learning: %v", err)
	}
	if ownLearning.SupersededBy == nil {
		t.Error("the re-ingest must retire the learning distilled from the replaced chunk")
	}
	foreignLearning, err := store.GetLearning(other)
	if err != nil || foreignLearning == nil {
		t.Fatalf("read the foreign learning: %v", err)
	}
	if foreignLearning.SupersededBy != nil {
		t.Errorf("the re-ingest retired the learning of project %q (superseded_by = %d)", foreign, *foreignLearning.SupersededBy)
	}
	if result.LearningsSuperseded != 1 {
		t.Errorf("superseded learnings = %d, want 1: the count follows the scoped set", result.LearningsSuperseded)
	}
}
