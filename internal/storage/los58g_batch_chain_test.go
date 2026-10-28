package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos58g_GetLearningChain_BatchSupersedeIncludesEveryRetiredRow pins the
// version history of a batch supersede (YM-418).
//
// SupersedeLearningBatch retires several rows into one successor. The successor
// can record only one predecessor in the scalar learnings.supersedes column,
// while every retired row carries its own forward edge in superseded_by. A
// history built from the scalar edge alone therefore loses the rows that did
// not win that edge: a cap upgrade that retires the project-bound and the
// global row shows only one of them afterwards.
func TestLos58g_GetLearningChain_BatchSupersedeIncludesEveryRetiredRow(t *testing.T) {
	s := mustOpen(t)
	insert := func(content string) int64 {
		id, err := s.InsertLearning(&models.Learning{
			Category: "cap", Content: content, Confidence: 1.0,
			CreatedAt: time.Now(), ModelUsed: "test",
		})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return id
	}
	ids := func(chain []models.Learning) []int64 {
		out := make([]int64, 0, len(chain))
		for _, l := range chain {
			out = append(out, l.ID)
		}
		return out
	}
	equal := func(a, b []int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	bound := insert("cap bound row")
	global := insert("cap global row")
	stale := insert("cap stale row")
	winner := insert("cap new row")
	retired := []int64{bound, global, stale}

	if err := s.SupersedeLearningBatch(retired, winner, "cap version upgrade"); err != nil {
		t.Fatalf("batch supersede: %v", err)
	}
	want := append(append([]int64{}, retired...), winner)

	// History of the successor: every retired row of the batch, oldest first.
	chain, err := s.GetLearningChain(winner)
	if err != nil {
		t.Fatalf("chain of successor: %v", err)
	}
	if got := ids(chain); !equal(got, want) {
		t.Errorf("history of successor #%d = %v, want %v (every retired row of the batch, oldest first)",
			winner, got, want)
	}

	// The same history must be reachable from each retired row, including the
	// ones that do not hold the scalar backward edge.
	for _, retiredID := range retired {
		chain, err := s.GetLearningChain(retiredID)
		if err != nil {
			t.Fatalf("chain of retired row #%d: %v", retiredID, err)
		}
		if got := ids(chain); !equal(got, want) {
			t.Errorf("history from retired row #%d = %v, want %v (every retired row of the batch, oldest first)",
				retiredID, got, want)
		}
	}
}

// TestLos58g_Schema_IndexesSupersededBy pins the index that keeps the batch
// history affordable: the read filters on superseded_by, and the other indexes
// on that column cannot serve it (one is partial, one leads with
// canonical_project). Without the index the read scans every row of learnings.
func TestLos58g_Schema_IndexesSupersededBy(t *testing.T) {
	s := mustOpen(t)
	var ddl string
	if err := s.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_learnings_superseded_by'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("idx_learnings_superseded_by missing: %v", err)
	}
	if !strings.Contains(ddl, "learnings(superseded_by)") {
		t.Errorf("index does not cover superseded_by: %s", ddl)
	}

	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT id FROM learnings WHERE superseded_by IN (?, ?)`, 1, 2)
	if err != nil {
		t.Fatalf("query plan: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "idx_learnings_superseded_by") {
		t.Errorf("predecessor lookup does not use the index, plan:\n%s", plan.String())
	}
}

// TestLos58g_GetLearningChain_BatchMemberKeepsItsOwnPredecessor pins the second
// half of the batch history: a row that was already a version head when the
// batch retired it. A → B, then B and C are retired into W. B is only reachable
// through the forward edge of W, so in the order where C holds the successor's
// scalar backward edge, B's own predecessor can only be found by reading the
// predecessors of a batch member.
func TestLos58g_GetLearningChain_BatchMemberKeepsItsOwnPredecessor(t *testing.T) {
	orders := []struct {
		name  string
		batch func(b, c int64) []int64
	}{
		{"member holds the scalar edge", func(b, c int64) []int64 { return []int64{b, c} }},
		{"member is only reachable through superseded_by", func(b, c int64) []int64 { return []int64{c, b} }},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			s := mustOpen(t)
			insert := func(content string) int64 {
				id, err := s.InsertLearning(&models.Learning{
					Category: "cap", Content: content, Confidence: 1.0,
					CreatedAt: time.Now(), ModelUsed: "test",
				})
				if err != nil {
					t.Fatalf("insert %q: %v", content, err)
				}
				return id
			}

			a := insert("cap v1")
			b := insert("cap v2")
			c := insert("cap stale row")
			winner := insert("cap v3")

			if err := s.SupersedeLearning(a, b, "cap version upgrade"); err != nil {
				t.Fatalf("single supersede: %v", err)
			}
			if err := s.SupersedeLearningBatch(tc.batch(b, c), winner, "cap version upgrade"); err != nil {
				t.Fatalf("batch supersede: %v", err)
			}

			chain, err := s.GetLearningChain(winner)
			if err != nil {
				t.Fatalf("chain of successor: %v", err)
			}
			got := make([]int64, 0, len(chain))
			for _, l := range chain {
				got = append(got, l.ID)
			}
			want := []int64{a, b, c, winner}
			if len(got) != len(want) {
				t.Fatalf("history = %v, want %v (predecessor of a batch member stays part of the versions)", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("history = %v, want %v (oldest first)", got, want)
				}
			}
		})
	}
}
