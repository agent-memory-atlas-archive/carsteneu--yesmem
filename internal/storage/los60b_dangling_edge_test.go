package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los60bInsert writes a plain active learning and returns its id.
func los60bInsert(t *testing.T, s *Store, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "decision", Content: content, Confidence: 1.0,
		CreatedAt: time.Now(), ModelUsed: "test",
	})
	if err != nil {
		t.Fatalf("insert %q: %v", content, err)
	}
	return id
}

// los60bHardDelete removes a row the way a delete path does: the row is gone,
// nothing else is touched. Any edge that pointed at it now points into nothing.
func los60bHardDelete(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.DB().Exec(`DELETE FROM learnings WHERE id = ?`, id); err != nil {
		t.Fatalf("hard delete %d: %v", id, err)
	}
}

func los60bIDs(chain []models.Learning) []int64 {
	out := make([]int64, 0, len(chain))
	for _, l := range chain {
		out = append(out, l.ID)
	}
	return out
}

func los60bEqual(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLos60b_GetLearningChain_SurvivesDanglingEdge pins YM-435 (a).
//
// A hard delete removes a row but leaves the edges that pointed at it. A row
// whose superseded_by names a deleted successor keeps a positive value, and the
// forward walk queries that id, gets sql.ErrNoRows and aborts the whole history.
// The same happens backwards when supersedes names a deleted predecessor. The
// live database carries 53 such forward edges and 31 backward ones.
//
// A positive edge without a target row is a chain boundary, not an error: the
// branch that was found must still be readable. Only the start id is required to
// exist.
func TestLos60b_GetLearningChain_SurvivesDanglingEdge(t *testing.T) {
	t.Run("forward_target_deleted", func(t *testing.T) {
		s := mustOpen(t)
		v1 := los60bInsert(t, s, "dangling forward v1")
		v2 := los60bInsert(t, s, "dangling forward v2")
		ghost := los60bInsert(t, s, "dangling forward successor")
		if err := s.SupersedeLearning(v1, v2, "step 1"); err != nil {
			t.Fatalf("supersede v1 by v2: %v", err)
		}
		if err := s.SupersedeLearning(v2, ghost, "step 2"); err != nil {
			t.Fatalf("supersede v2 by ghost: %v", err)
		}
		los60bHardDelete(t, s, ghost)

		chain, err := s.GetLearningChain(v1)
		if err != nil {
			t.Fatalf("chain from v1 broke on a deleted successor: %v", err)
		}
		if got, want := los60bIDs(chain), []int64{v1, v2}; !los60bEqual(got, want) {
			t.Errorf("chain from v1 = %v, want %v (the readable branch, ending at the boundary)", got, want)
		}

		// The row that carries the dangling edge is itself a readable chain.
		chain, err = s.GetLearningChain(v2)
		if err != nil {
			t.Fatalf("chain from the dangling row v2: %v", err)
		}
		if got, want := los60bIDs(chain), []int64{v1, v2}; !los60bEqual(got, want) {
			t.Errorf("chain from v2 = %v, want %v", got, want)
		}
	})

	t.Run("backward_target_deleted", func(t *testing.T) {
		s := mustOpen(t)
		v1 := los60bInsert(t, s, "dangling backward predecessor")
		v2 := los60bInsert(t, s, "dangling backward v2")
		if err := s.SupersedeLearning(v1, v2, "step 1"); err != nil {
			t.Fatalf("supersede v1 by v2: %v", err)
		}
		los60bHardDelete(t, s, v1)

		chain, err := s.GetLearningChain(v2)
		if err != nil {
			t.Fatalf("chain from v2 broke on a deleted predecessor: %v", err)
		}
		if got, want := los60bIDs(chain), []int64{v2}; !los60bEqual(got, want) {
			t.Errorf("chain from v2 = %v, want %v", got, want)
		}
	})

	t.Run("batch_predecessor_after_boundary", func(t *testing.T) {
		s := mustOpen(t)
		a1 := los60bInsert(t, s, "batch loser 1")
		a2 := los60bInsert(t, s, "batch loser 2")
		winner := los60bInsert(t, s, "batch winner")
		if err := s.SupersedeLearningBatch([]int64{a1, a2}, winner, "batch"); err != nil {
			t.Fatalf("batch supersede: %v", err)
		}
		// The scalar backward edge names the first loser; the second one is only
		// reachable through the forward column. Deleting the first makes that edge
		// dangle, and the second must still appear in front of the winner.
		los60bHardDelete(t, s, a1)

		chain, err := s.GetLearningChain(winner)
		if err != nil {
			t.Fatalf("chain from the batch winner broke on the dangling predecessor: %v", err)
		}
		if got, want := los60bIDs(chain), []int64{a2, winner}; !los60bEqual(got, want) {
			t.Errorf("chain = %v, want %v (the surviving batch predecessor stays in front of the winner)", got, want)
		}
	})

	t.Run("unknown_start_still_errors", func(t *testing.T) {
		s := mustOpen(t)
		if _, err := s.GetLearningChain(999999); err == nil {
			t.Fatal("chain from a non-existent id must stay an error, not an empty history")
		}
	})

	t.Run("intact_chain_unchanged", func(t *testing.T) {
		s := mustOpen(t)
		v1 := los60bInsert(t, s, "intact v1")
		v2 := los60bInsert(t, s, "intact v2")
		v3 := los60bInsert(t, s, "intact v3")
		if err := s.SupersedeLearning(v1, v2, "step 1"); err != nil {
			t.Fatalf("supersede 1->2: %v", err)
		}
		if err := s.SupersedeLearning(v2, v3, "step 2"); err != nil {
			t.Fatalf("supersede 2->3: %v", err)
		}

		chain, err := s.GetLearningChain(v1)
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		if got, want := los60bIDs(chain), []int64{v1, v2, v3}; !los60bEqual(got, want) {
			t.Errorf("chain = %v, want %v", got, want)
		}
	})
}

// los60bAssertNoDanglingReference fails when row still carries a positive edge
// to targetID. Tolerating the edge in the walk is not enough: after the row is
// gone, the reference itself has to be cleared.
func los60bAssertNoDanglingReference(t *testing.T, s *Store, row, targetID int64, column string) {
	t.Helper()
	got, err := s.GetLearning(row)
	if err != nil {
		t.Fatalf("get row %d: %v", row, err)
	}
	var edge *int64
	switch column {
	case "superseded_by":
		edge = got.SupersededBy
	case "supersedes":
		// supersedes is not part of the models.Learning scan; read it directly.
		if err := s.readerDB().QueryRow(`SELECT supersedes FROM learnings WHERE id = ?`, row).Scan(&edge); err != nil {
			t.Fatalf("read supersedes of %d: %v", row, err)
		}
	default:
		t.Fatalf("unknown column %q", column)
	}
	if edge != nil && *edge == targetID {
		t.Errorf("row %d still points at the hard-deleted learning %d via %s: the delete path has to clear the reference",
			row, targetID, column)
	}
}

// TestLos60b_DeleteDocSource_ClearsInboundReferences pins YM-435 (b) for the
// document path. DeleteDocSource hard-deletes the learnings extracted from a
// document, and a learning that was retired by one of them keeps its positive
// superseded_by afterwards: the edge points into nothing and the version history
// of that strand ends at a hole.
func TestLos60b_DeleteDocSource_ClearsInboundReferences(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los60b-docs", Project: ""})
	ds, err := s.GetDocSource("los60b-docs", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:   ds.ID,
		SourceFile: "guide.md",
		SourceHash: "los60b-hash",
		Content:    "a documented fact",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	successor, err := s.InsertLearning(&models.Learning{
		SessionID: "los60b-docs-session", Category: "gotcha",
		Content:    "the extracted fact that retires an older note",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
		Source: "docs_extracted", DocChunkRef: chunkID,
	})
	if err != nil {
		t.Fatalf("insert docs learning: %v", err)
	}
	secondDocs, err := s.InsertLearning(&models.Learning{
		SessionID: "los60b-docs-session", Category: "gotcha",
		Content:    "an extracted fact that a later note replaced",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
		Source: "docs_extracted", DocChunkRef: chunkID,
	})
	if err != nil {
		t.Fatalf("insert second docs learning: %v", err)
	}
	retired := los60bInsert(t, s, "note retired by the extracted fact")
	if err := s.SupersedeLearning(retired, successor, "replaced by the doc fact"); err != nil {
		t.Fatalf("supersede retired by successor: %v", err)
	}
	// The backward direction on the same path: the successor of a removed doc
	// learning keeps its pointer in supersedes.
	successorOfDocs := los60bInsert(t, s, "later note that replaced an extracted fact")
	if err := s.SupersedeLearning(secondDocs, successorOfDocs, "replaced by a later note"); err != nil {
		t.Fatalf("supersede second docs learning: %v", err)
	}

	if _, err := s.DeleteDocSource("los60b-docs", ""); err != nil {
		t.Fatalf("delete doc source: %v", err)
	}

	los60bAssertNoDanglingReference(t, s, retired, successor, "superseded_by")
	los60bAssertNoDanglingReference(t, s, successorOfDocs, secondDocs, "supersedes")

	// The row stays retired: the successor is gone, but the note must not
	// reappear as an active learning. The sentinel names the reason the chain
	// ends here.
	got, err := s.GetLearning(retired)
	if err != nil {
		t.Fatalf("get retired row: %v", err)
	}
	if got.SupersededBy == nil {
		t.Error("the retire was lifted by the delete: the row would answer active searches again")
	} else if *got.SupersededBy != SupersededByDeleted {
		t.Errorf("superseded_by = %d after the delete, want the deleted-successor sentinel %d",
			*got.SupersededBy, SupersededByDeleted)
	}
	if chain, err := s.GetLearningChain(retired); err != nil {
		t.Errorf("chain from the retired row: %v", err)
	} else if ids := los60bIDs(chain); !los60bEqual(ids, []int64{retired}) {
		t.Errorf("chain = %v, want just the retired row", ids)
	}
}

// TestLos60b_DeleteOldNarratives_ClearsInboundReferences pins YM-435 (b) for the
// narrative retention. Both directions have to be cleared: a retired row that
// pointed at the dropped narrative, and a surviving narrative whose supersedes
// pointed at it.
func TestLos60b_DeleteOldNarratives_ClearsInboundReferences(t *testing.T) {
	s := mustOpen(t)
	project := "/home/tester/memory/yesmem"
	base := time.Now().Truncate(time.Second).Add(-4 * time.Hour)
	dropped := los13dInsertNarrative(t, s, "sess-old", project, "old story", base)
	kept := los13dInsertNarrative(t, s, "sess-new", project, "new story", base.Add(time.Minute))

	// dropped is retired by kept (backward edge of kept), and a plain learning
	// was retired by dropped (forward edge onto the dropped row).
	if err := s.SupersedeLearning(dropped, kept, "newer story"); err != nil {
		t.Fatalf("supersede dropped by kept: %v", err)
	}
	retired := los60bInsert(t, s, "note retired by the dropped narrative")
	if err := s.SupersedeLearning(retired, dropped, "replaced by the story"); err != nil {
		t.Fatalf("supersede retired by dropped: %v", err)
	}

	deleted, err := s.DeleteOldNarratives(project, 1, 0)
	if err != nil {
		t.Fatalf("delete old narratives: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d rows, want 1 (the older narrative)", deleted)
	}

	los60bAssertNoDanglingReference(t, s, retired, dropped, "superseded_by")
	los60bAssertNoDanglingReference(t, s, kept, dropped, "supersedes")

	if got, err := s.GetLearning(retired); err != nil {
		t.Fatalf("get retired row: %v", err)
	} else if got.SupersededBy == nil || *got.SupersededBy != SupersededByDeleted {
		t.Errorf("superseded_by after the delete = %v, want the deleted-successor sentinel %d",
			got.SupersededBy, SupersededByDeleted)
	}
}

// TestLos60b_RepairDanglingSupersedeEdges pins YM-435 (c). The live database
// carries 53 forward and 31 backward edges that point at rows which no longer
// exist. The code paths no longer produce them, but the existing ones need a
// report to be cleaned up once, and the cleanup must keep the retired rows
// retired.
func TestLos60b_RepairDanglingSupersedeEdges(t *testing.T) {
	s := mustOpen(t)

	// Forward dangler: a retired row whose successor was deleted.
	owner := los60bInsert(t, s, "retired until the successor vanished")
	goneSuccessor := los60bInsert(t, s, "successor about to be deleted")
	if err := s.SupersedeLearning(owner, goneSuccessor, "then deleted"); err != nil {
		t.Fatalf("supersede owner: %v", err)
	}
	los60bHardDelete(t, s, goneSuccessor)

	// Backward dangler: a successor whose predecessor was deleted.
	gonePredecessor := los60bInsert(t, s, "predecessor about to be deleted")
	child := los60bInsert(t, s, "successor that stays")
	if err := s.SupersedeLearning(gonePredecessor, child, "then deleted"); err != nil {
		t.Fatalf("supersede child: %v", err)
	}
	los60bHardDelete(t, s, gonePredecessor)

	// Intact strand: must stay untouched by both the report and the repair.
	first := los60bInsert(t, s, "intact strand v1")
	second := los60bInsert(t, s, "intact strand v2")
	if err := s.SupersedeLearning(first, second, "intact"); err != nil {
		t.Fatalf("supersede intact: %v", err)
	}

	report, cleared, err := s.RepairDanglingSupersedeEdges(false)
	if err != nil {
		t.Fatalf("report dangling edges: %v", err)
	}
	if cleared != 0 {
		t.Errorf("dry run reported %d cleared edges, want 0", cleared)
	}
	want := []DanglingSupersedeEdge{
		{ID: owner, Column: "superseded_by", Target: goneSuccessor},
		{ID: child, Column: "supersedes", Target: gonePredecessor},
	}
	if len(report) != len(want) {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	for i := range want {
		if report[i] != want[i] {
			t.Errorf("report[%d] = %+v, want %+v", i, report[i], want[i])
		}
	}

	// Reporting must not change anything.
	if got, err := s.GetLearning(owner); err != nil {
		t.Fatalf("get owner: %v", err)
	} else if got.SupersededBy == nil || *got.SupersededBy != goneSuccessor {
		t.Fatalf("report already changed the row: superseded_by = %v", got.SupersededBy)
	}

	applied, clearedApply, err := s.RepairDanglingSupersedeEdges(true)
	if err != nil {
		t.Fatalf("repair dangling edges: %v", err)
	}
	if len(applied) != len(want) {
		t.Errorf("repair reported %d edges, want %d", len(applied), len(want))
	}
	if clearedApply != int64(len(want)) {
		t.Errorf("repair cleared %d edges, want %d", clearedApply, len(want))
	}

	// The retired row stays retired, the surviving successor loses the pointer,
	// and the intact strand keeps both of its edges.
	if got, err := s.GetLearning(owner); err != nil {
		t.Fatalf("get owner after repair: %v", err)
	} else if got.SupersededBy == nil || *got.SupersededBy != SupersededByDeleted {
		t.Errorf("owner.superseded_by = %v, want the deleted-successor sentinel %d", got.SupersededBy, SupersededByDeleted)
	}
	var childSupersedes *int64
	if err := s.readerDB().QueryRow(`SELECT supersedes FROM learnings WHERE id = ?`, child).Scan(&childSupersedes); err != nil {
		t.Fatalf("read child supersedes: %v", err)
	}
	if childSupersedes != nil {
		t.Errorf("child.supersedes = %d after repair, want NULL", *childSupersedes)
	}
	if got, err := s.GetLearning(first); err != nil {
		t.Fatalf("get intact v1: %v", err)
	} else if got.SupersededBy == nil || *got.SupersededBy != second {
		t.Errorf("intact strand was touched: superseded_by = %v, want %d", got.SupersededBy, second)
	}

	// Second pass: nothing left to report, and an apply over an empty report
	// still runs its UPDATEs without touching anything.
	rest, restCleared, err := s.RepairDanglingSupersedeEdges(false)
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("dangling edges survived the repair: %+v", rest)
	}
	if _, restClearedApply, err := s.RepairDanglingSupersedeEdges(true); err != nil {
		t.Fatalf("second repair: %v", err)
	} else if restClearedApply != 0 || restCleared != 0 {
		t.Errorf("second repair cleared %d/%d edges on a clean store, want 0/0", restClearedApply, restCleared)
	}
}
