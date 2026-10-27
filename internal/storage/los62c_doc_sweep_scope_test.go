package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los62cSweepFixture registers one doc source with one chunk and four learnings
// that reference it: the source's own project, a checkout of the same
// repository, a foreign project, and an unstamped row. The first, second and
// fourth belong to the source; the third does not.
type los62cSweepFixture struct {
	store     *Store
	sourceID  int64
	chunkID   int64
	own       int64
	checkout  int64
	foreign   int64
	stampless int64
}

func los62cNewSweepFixture(t *testing.T, name, sourcePath string) *los62cSweepFixture {
	t.Helper()
	s := mustOpen(t)
	const (
		owner   = "/home/tester/memory/los62c-sweep-owner"
		foreign = "/home/tester/memory/los62c-sweep-other"
	)
	// The foreign project shares the owner's prefix but is not below it: a scope
	// that matched on the bare prefix would sweep its rows too.
	f := &los62cSweepFixture{store: s}
	mustUpsertDocSource(t, s, &DocSource{Name: name, Project: owner, Path: sourcePath})
	ds, err := s.GetDocSource(name, owner)
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	f.sourceID = ds.ID
	f.chunkID, err = s.InsertDocChunk(&DocChunk{
		SourceID:   ds.ID,
		SourceFile: "guide.md",
		SourceHash: name + "-hash",
		Content:    "a documented fact",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	insert := func(label, project string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los62c-sweep-session", Category: "gotcha",
			Content:    "a fact extracted from the docs, " + label,
			Project:    project,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
			Source: "docs_extracted", DocChunkRef: f.chunkID,
		})
		if err != nil {
			t.Fatalf("insert %s learning: %v", label, err)
		}
		return id
	}
	f.own = insert("own", owner)
	f.checkout = insert("checkout", owner+"/.worktrees/wt")
	f.foreign = insert("foreign", foreign)
	f.stampless = insert("stampless", "")
	return f
}

// check asserts the retirement state the two sweeps owe: the source's own rows
// are retired with the junk sentinel, the foreign row is untouched, and the
// returned ids name exactly the retired rows. The second part matters because
// the caller drops embeddings for the returned ids: a row reported but still
// active would lose its vector.
func (f *los62cSweepFixture) check(t *testing.T, retired []int64, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	retiredSet := map[int64]bool{}
	for _, id := range retired {
		retiredSet[id] = true
	}
	for _, tc := range []struct {
		name    string
		id      int64
		retired bool
	}{
		{"own", f.own, true},
		{"checkout", f.checkout, true},
		{"unstamped", f.stampless, true},
		{"foreign", f.foreign, false},
	} {
		var edge *int64
		if err := f.store.readerDB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, tc.id).Scan(&edge); err != nil {
			t.Fatalf("read %s edge: %v", tc.name, err)
		}
		isRetired := edge != nil
		if isRetired != tc.retired {
			t.Errorf("%s learning retired = %v, want %v (superseded_by = %v)", tc.name, isRetired, tc.retired, edge)
		}
		if tc.retired {
			if edge == nil || *edge != -1 {
				t.Errorf("%s learning superseded_by = %v, want the re-ingest sentinel -1", tc.name, edge)
			}
			if !retiredSet[tc.id] {
				t.Errorf("%s learning was retired but is not in the returned ids: its embedding would survive the sweep", tc.name)
			}
		} else if retiredSet[tc.id] {
			t.Errorf("the sweep reported the %s learning as retired; the caller would drop the embedding of an active row", tc.name)
		}
	}
}

// TestLos62c_SweepDocChunksByFile_IsProjectScoped pins finding 2 of YM-445 for
// the per-document re-ingest path. The sweep retired every active learning whose
// doc_chunk_ref pointed into the swept chunks, without looking at the learning's
// project, while DeleteDocSource had already been scoped (YM-443). The chunks are
// the source's own; the learnings are the rows that need the scope.
func TestLos62c_SweepDocChunksByFile_IsProjectScoped(t *testing.T) {
	f := los62cNewSweepFixture(t, "los62c-file-docs", "/home/tester/memory/los62c-sweep-owner/docs")
	watermark, err := f.store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("max chunk id: %v", err)
	}
	chunks, retired, err := f.store.SweepDocChunksByFile(f.sourceID, "guide.md", watermark, "re-ingest: document removed")
	if err != nil {
		t.Fatalf("sweep by file: %v", err)
	}
	if chunks != 1 {
		t.Errorf("swept chunks = %d, want 1", chunks)
	}
	f.check(t, retired, nil)
}

// TestLos62c_SweepPathlessSourceChunks_IsProjectScoped is the same pin for the
// pathless-source sweep: it carried the identical unscoped UPDATE.
func TestLos62c_SweepPathlessSourceChunks_IsProjectScoped(t *testing.T) {
	f := los62cNewSweepFixture(t, "los62c-pathless-docs", "")
	watermark, err := f.store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("max chunk id: %v", err)
	}
	chunks, _, retired, err := f.store.SweepPathlessSourceChunks(f.sourceID, watermark)
	if err != nil {
		t.Fatalf("sweep pathless: %v", err)
	}
	if chunks != 1 {
		t.Errorf("swept chunks = %d, want 1", chunks)
	}
	f.check(t, retired, nil)
}
