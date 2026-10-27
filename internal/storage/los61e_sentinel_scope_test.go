package storage

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos61e_DeleteDocSource_KeepsForeignProjectLearnings pins the first finding
// of YM-443. DeleteDocSource cascaded into every docs_extracted learning whose
// doc_chunk_ref pointed into the source, without looking at the learning's
// project: a delete meant for one project could take rows of another with it.
// The chunks themselves are bound to the source id and are always its own; the
// learnings are the rows that need the project scope.
func TestLos61e_DeleteDocSource_KeepsForeignProjectLearnings(t *testing.T) {
	s := mustOpen(t)
	const (
		owner   = "/home/tester/memory/los61e-owner"
		foreign = "/home/tester/memory/los61e-owner-sibling"
	)
	// The foreign project shares the owner's string prefix but is not below it:
	// a scope that matched on the bare prefix would take its rows with it.
	// An ingest started in a checkout stamps the raw checkout path while the
	// source sits at the repository root, so that row is the source's own and
	// has to go. Both cases are one predicate.
	mustUpsertDocSource(t, s, &DocSource{Name: "los61e-docs", Project: owner})
	ds, err := s.GetDocSource("los61e-docs", owner)
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:   ds.ID,
		SourceFile: "guide.md",
		SourceHash: "los61e-hash",
		Content:    "a documented fact",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	insert := func(label, project string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los61e-session", Category: "gotcha",
			Content:    "an extracted fact from the " + label + " project",
			Project:    project,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
			Source: "docs_extracted", DocChunkRef: chunkID,
		})
		if err != nil {
			t.Fatalf("insert %s learning: %v", label, err)
		}
		return id
	}
	own := insert("own", owner)
	other := insert("foreign", foreign)
	checkout := insert("checkout", owner+"/.worktrees/wt")
	stampless := insert("stampless", "")

	// An inbound edge onto a removed learning forces the two cleanup statements
	// to run with the project arguments in place. Their bind order is the one
	// thing here that a mistake would not make fail: a shifted argument writes a
	// path string into the integer superseded_by column.
	heir := insert("heir", foreign)
	pred := insert("predecessor", foreign)
	if _, err := s.DB().Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, own, heir); err != nil {
		t.Fatalf("point the heir at the removed learning: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE learnings SET supersedes = ? WHERE id = ?`, own, pred); err != nil {
		t.Fatalf("let the predecessor name the removed learning: %v", err)
	}

	if _, err := s.DeleteDocSource("los61e-docs", owner); err != nil {
		t.Fatalf("delete doc source: %v", err)
	}

	var heirEdge int64
	if err := s.readerDB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, heir).Scan(&heirEdge); err != nil {
		t.Fatalf("read the heir's edge: %v", err)
	}
	if heirEdge != SupersededByDeleted {
		t.Errorf("the heir's superseded_by = %d, want the deleted-successor sentinel %d", heirEdge, SupersededByDeleted)
	}
	var predEdge sql.NullInt64
	if err := s.readerDB().QueryRow(`SELECT supersedes FROM learnings WHERE id = ?`, pred).Scan(&predEdge); err != nil {
		t.Fatalf("read the predecessor's edge: %v", err)
	}
	if predEdge.Valid && predEdge.Int64 != 0 {
		t.Errorf("the predecessor still names the removed learning: supersedes = %d, want NULL", predEdge.Int64)
	}

	exists := func(id int64) bool {
		t.Helper()
		var n int
		if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count learning %d: %v", id, err)
		}
		return n == 1
	}
	if exists(own) {
		t.Error("the source's own learning survived the delete")
	}
	if exists(checkout) {
		t.Error("a learning stamped with a checkout path of the same repository survived; it belongs to the source")
	}
	if !exists(other) {
		t.Error("the delete took a learning of another project with it")
	}
	// A learning without a project is not evidence of a foreign repository; it
	// keeps the old behaviour and goes with the source.
	if exists(stampless) {
		t.Error("the unstamped learning survived the delete; it belongs to the source")
	}

	var chunks int
	if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE source_id = ?`, ds.ID).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 0 {
		t.Errorf("%d chunk(s) of the deleted source survived", chunks)
	}
}

// TestLos61e_DeleteDocSource_GlobalSourceSparesStampedRows covers the branch
// without a project: a source registered without one has no path to scope on,
// so its unstamped rows go with it and a stamped row stays. That branch takes no
// project arguments at all, and on the live DB it is the only one that fires:
// every docs_extracted row there carries an empty project.
func TestLos61e_DeleteDocSource_GlobalSourceSparesStampedRows(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los61e-global"})
	ds, err := s.GetDocSource("los61e-global", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:   ds.ID,
		SourceFile: "guide.md",
		SourceHash: "los61e-global-hash",
		Content:    "a documented fact",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	insert := func(label, project string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los61e-global-session", Category: "gotcha",
			Content:    "an extracted fact, " + label,
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
	stamped := insert("stamped", "/home/tester/memory/los61e-elsewhere")

	if _, err := s.DeleteDocSource("los61e-global", ""); err != nil {
		t.Fatalf("delete doc source: %v", err)
	}

	for _, tc := range []struct {
		name    string
		id      int64
		survive bool
	}{
		{"unstamped", unstamped, false},
		{"stamped", stamped, true},
	} {
		var n int
		if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE id = ?`, tc.id).Scan(&n); err != nil {
			t.Fatalf("count %s learning: %v", tc.name, err)
		}
		if got := n == 1; got != tc.survive {
			t.Errorf("%s learning: survived = %v, want %v", tc.name, got, tc.survive)
		}
	}
}

// TestLos61e_LearningStats_SeparatesDeletedPredecessor pins the second finding
// of YM-443 for the statistics. SupersededByDeleted (-4) says the successor was
// hard-deleted, not that the row was replaced. Counting it under "superseded"
// reports a replacement where the data shows a loss.
func TestLos61e_LearningStats_SeparatesDeletedPredecessor(t *testing.T) {
	s := mustOpen(t)
	const project = "/home/tester/memory/los61e-stats"

	insert := func(content string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los61e-stats-session", Category: "gotcha",
			Content: content, Project: project,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
			Source: "llm_extracted",
		})
		if err != nil {
			t.Fatalf("insert learning: %v", err)
		}
		return id
	}
	successor := insert("the fact that replaced an older note")
	replaced := insert("a note replaced by a later fact")
	if err := s.SupersedeLearning(replaced, successor, "replaced"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	detached := insert("a note whose successor was hard-deleted")
	if _, err := s.DB().Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, SupersededByDeleted, detached); err != nil {
		t.Fatalf("mark detached row: %v", err)
	}
	insert("an active note")

	st, err := s.GetLearningStats(project)
	if err != nil {
		t.Fatalf("get learning stats: %v", err)
	}
	// Read the numbers through the JSON shape the CLI and MCP surface them with:
	// the split has to be visible in the published stats, not only in the struct.
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	num := func(key string) int {
		t.Helper()
		v, ok := fields[key]
		if !ok {
			t.Errorf("stats carry no %q: the deleted predecessor is not reported apart", key)
			return -1
		}
		return int(v.(float64))
	}
	superseded := num("superseded_count")
	deleted := num("deleted_predecessor_count")
	if superseded != 1 {
		t.Errorf("superseded_count = %d, want 1 (only the replaced row)", superseded)
	}
	if deleted != 1 {
		t.Errorf("deleted_predecessor_count = %d, want 1 (the row whose successor was deleted)", deleted)
	}
	if superseded+deleted != 2 {
		t.Errorf("the two counts add up to %d, want 2: the split must not lose rows", superseded+deleted)
	}
}

// TestLos61e_Fingerprints_DistinguishDeletedPredecessor pins the second finding
// of YM-443 for the two change-detection hashes. Both fed every non-NULL
// superseded_by into one counter, so a row moving from "replaced" to "successor
// deleted" (RepairDanglingSupersedeEdges rewrites the edge) left the hash
// untouched and no cluster or briefing was rebuilt. Two counts, both in the
// hash, make that transition visible again.
func TestLos61e_Fingerprints_DistinguishDeletedPredecessor(t *testing.T) {
	s := mustOpen(t)
	const project = "/home/tester/memory/los61e-fp"

	insert := func(content string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			SessionID: "los61e-fp-session", Category: "gotcha",
			Content: content, Project: project,
			Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
			Source: "llm_extracted",
		})
		if err != nil {
			t.Fatalf("insert learning: %v", err)
		}
		return id
	}
	successor := insert("the fact that will replace an older note")
	id := insert("a note whose predecessor edge changes meaning")

	fingerprints := func() (cluster, briefing string) {
		t.Helper()
		cluster, err := s.ClusterChangeFingerprint(project)
		if err != nil {
			t.Fatalf("cluster fingerprint: %v", err)
		}
		briefing, err = s.ProjectChangeFingerprint(project)
		if err != nil {
			t.Fatalf("project fingerprint: %v", err)
		}
		return cluster, briefing
	}

	setEdge := func(value int64) {
		t.Helper()
		if _, err := s.DB().Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, value, id); err != nil {
			t.Fatalf("set superseded_by = %d: %v", value, err)
		}
	}

	setEdge(successor) // replaced by a learning that exists
	clusterReplaced, briefingReplaced := fingerprints()
	setEdge(SupersededByDeleted) // same row, successor was hard-deleted
	clusterDetached, briefingDetached := fingerprints()

	if clusterReplaced == clusterDetached {
		t.Error("cluster fingerprint did not change when the edge turned from replaced into deleted")
	}
	if briefingReplaced == briefingDetached {
		t.Error("project fingerprint did not change when the edge turned from replaced into deleted")
	}
}
