package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TestDocIngest_RemovingADocumentIsAllOrNothingLos56h covers YM-395: the tree sweep
// retired the learnings of a removed document and then deleted the chunk rows in
// separate statements. A failure in the second half left the learnings retired while
// the document was still in the index, which is the one state neither the reader nor
// the next sweep can repair.
func TestDocIngest_RemovingADocumentIsAllOrNothingLos56h(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yesmem.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srcID, err := store.UpsertDocSource(&storage.DocSource{Name: "all-or-nothing", Project: "proj"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	chunkID, err := store.InsertDocChunk(&storage.DocChunk{
		SourceID:    srcID,
		SourceFile:  "gone.md",
		SourceHash:  "hash-gone",
		Content:     "Prose of the removed document.",
		ContentHash: "content-gone",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	learningID, err := store.InsertLearning(&models.Learning{
		Content:     "A learning distilled from the removed document.",
		Category:    "gotcha",
		Project:     "proj",
		Source:      "llm_extracted",
		DocChunkRef: chunkID,
	})
	if err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	watermark, err := store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	// The failure the removal has to survive: without the full-text table the delete of
	// the chunk row errors out after the learnings step has already run.
	other, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open the failing writer: %v", err)
	}
	t.Cleanup(func() { other.Close() })
	if _, err := other.Exec(`DROP TABLE doc_chunks_fts`); err != nil {
		t.Fatalf("drop the full-text table: %v", err)
	}

	result := &Result{}
	if err := removeDocumentChunks(store, srcID, "gone.md", watermark, result); err == nil {
		t.Fatal("a removal whose delete failed must report the error")
	}

	chunks, err := store.GetDocChunksByFile("gone.md", srcID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("a failed removal must leave the chunk in place, got %d chunk(s)", len(chunks))
	}
	learning, err := store.GetLearning(learningID)
	if err != nil || learning == nil {
		t.Fatalf("read learning: %v", err)
	}
	if learning.SupersededBy != nil {
		t.Errorf("a failed removal must not retire the learning (superseded_by = %v): the document is still indexed", *learning.SupersededBy)
	}
	if result.LearningsSuperseded != 0 || result.FilesRemoved != 0 {
		t.Errorf("a failed removal must count nothing, got %d learnings, %d files",
			result.LearningsSuperseded, result.FilesRemoved)
	}
}

// TestDocIngest_RemovingADocumentClearsEveryLayerLos56h is the other side: a removal that
// succeeds leaves no chunk row, no full-text row and no active learning behind.
func TestDocIngest_RemovingADocumentClearsEveryLayerLos56h(t *testing.T) {
	store := mustOpenStore(t)

	srcID, err := store.UpsertDocSource(&storage.DocSource{Name: "every-layer", Project: "proj"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	chunkID, err := store.InsertDocChunk(&storage.DocChunk{
		SourceID:    srcID,
		SourceFile:  "gone.md",
		SourceHash:  "hash-gone",
		Content:     "Prose of the removed document.",
		ContentHash: "content-gone",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	learningID, err := store.InsertLearning(&models.Learning{
		Content:     "A learning distilled from the removed document.",
		Category:    "gotcha",
		Project:     "proj",
		Source:      "llm_extracted",
		DocChunkRef: chunkID,
	})
	if err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	watermark, err := store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	result := &Result{}
	if err := removeDocumentChunks(store, srcID, "gone.md", watermark, result); err != nil {
		t.Fatalf("remove document: %v", err)
	}

	chunks, err := store.GetDocChunksByFile("gone.md", srcID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("the removal must delete the chunk, got %d", len(chunks))
	}
	if hits, err := store.SearchDocChunksFTS("removed", "", "", "", "", 10, nil); err != nil {
		t.Fatalf("search: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("the removal must clear the full-text row, got %d hit(s)", len(hits))
	}
	learning, err := store.GetLearning(learningID)
	if err != nil || learning == nil {
		t.Fatalf("read learning: %v", err)
	}
	if learning.SupersededBy == nil {
		t.Error("the removal must retire the learning of a removed document")
	}
	if result.FilesRemoved != 1 || result.LearningsSuperseded != 1 {
		t.Errorf("counts must report the removal, got %d file(s), %d learning(s)",
			result.FilesRemoved, result.LearningsSuperseded)
	}
}
