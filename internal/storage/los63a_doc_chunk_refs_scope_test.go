package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos63a_SupersedeByDocChunkRefs_IsProjectScoped pins YM-452: the per-chunk
// supersede that runs on every re-ingest retired every active learning whose
// doc_chunk_ref pointed at a dropped chunk, without looking at the learning's
// project. The two re-ingest sweeps and DeleteDocSource were already scoped
// (YM-443, YM-445); this is the path that fires on every changed document, not
// only on cleanup. A re-ingest in one project could therefore retire the
// learnings of another one. The chunks are the source's own; the learnings are
// the rows that need the scope (docLearningProjectScope).
func TestLos63a_SupersedeByDocChunkRefs_IsProjectScoped(t *testing.T) {
	f := los62cNewSweepFixture(t, "los63a-chunk-ref-docs", "/home/tester/memory/los62c-sweep-owner/docs")
	retired, err := f.store.SupersedeByDocChunkRefs(f.sourceID, []int64{f.chunkID}, "re-ingest: chunk changed")
	f.check(t, retired, err)
}

// TestLos63a_SupersedeByDocChunkRefs_UnscopedSource pins the second shape of the
// scope: a global doc source (skills, coding styles) registers with an empty
// project, and the ingest of such a source stamps its learnings without one. That
// branch of docLearningProjectScope carries no bind arguments at all, so the
// statement's argument count differs from the sourced case above; a learning of
// another project must still survive.
func TestLos63a_SupersedeByDocChunkRefs_UnscopedSource(t *testing.T) {
	s := mustOpen(t)
	sourceID, err := s.UpsertDocSource(&DocSource{Name: "los63a-global-docs", Project: ""})
	if err != nil {
		t.Fatalf("upsert global doc source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:   sourceID,
		SourceFile: "style.md",
		SourceHash: "los63a-global-hash",
		Content:    "a documented convention",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	insert := func(label, project string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los63a-global-session", Category: "gotcha",
			Content:    "a fact extracted from the global docs, " + label,
			Project:    project,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
			Source: "docs_extracted", DocChunkRef: chunkID,
		})
		if err != nil {
			t.Fatalf("insert %s learning: %v", label, err)
		}
		return id
	}
	unstamped := insert("unstamped", "")
	foreign := insert("foreign", "/home/tester/memory/los63a-elsewhere")

	retired, err := s.SupersedeByDocChunkRefs(sourceID, []int64{chunkID}, "re-ingest: chunk changed")
	if err != nil {
		t.Fatalf("supersede from a global source: %v", err)
	}
	if len(retired) != 1 || retired[0] != unstamped {
		t.Fatalf("retired = %v, want exactly the unstamped learning %d", retired, unstamped)
	}
	var edge *int64
	if err := s.readerDB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, foreign).Scan(&edge); err != nil {
		t.Fatalf("read foreign edge: %v", err)
	}
	if edge != nil {
		t.Errorf("the learning of project /home/tester/memory/los63a-elsewhere was retired with %d", *edge)
	}
}

// TestLos63a_SupersedeByDocChunkRefs_UnknownSourceTouchesNothing pins the
// fallback: without a source row there is no project to bind the learnings to,
// so the call must retire nothing. Falling back to the unscoped UPDATE here
// would reopen exactly the cross-project retirement this lot closes.
func TestLos63a_SupersedeByDocChunkRefs_UnknownSourceTouchesNothing(t *testing.T) {
	f := los62cNewSweepFixture(t, "los63a-chunk-ref-unknown", "/home/tester/memory/los62c-sweep-owner/docs")
	retired, err := f.store.SupersedeByDocChunkRefs(f.sourceID+1000, []int64{f.chunkID}, "re-ingest: chunk changed")
	if err != nil {
		t.Fatalf("supersede for an unknown source: %v", err)
	}
	if len(retired) != 0 {
		t.Fatalf("an unknown source retired learnings %v, want none", retired)
	}
	for _, id := range []int64{f.own, f.checkout, f.foreign, f.stampless} {
		var edge *int64
		if err := f.store.readerDB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, id).Scan(&edge); err != nil {
			t.Fatalf("read edge of learning %d: %v", id, err)
		}
		if edge != nil {
			t.Errorf("learning %d retired with superseded_by = %d, want untouched", id, *edge)
		}
	}
}
