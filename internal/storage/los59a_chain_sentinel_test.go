package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos59a_SupersedeLearningBatchPropagatesWriteErrors pins the second part of
// YM-426. SupersedeLearningBatch discarded the errors of its mark, backlink and
// FTS statements, so a failing statement still ended in a successful COMMIT and
// the caller believed the rows were retired. A write that cannot be recorded must
// surface as an error and leave every row of the batch untouched.
func TestLos59a_SupersedeLearningBatchPropagatesWriteErrors(t *testing.T) {
	mustExec := func(t *testing.T, s *Store, stmt string) {
		t.Helper()
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("set up failing write: %v", err)
		}
	}

	// A trigger is the only way to make a statement fail deterministically
	// without touching the schema or the disk. Each case breaks one of the three
	// writes of the batch: the retire itself, the backlink onto the successor,
	// and the anticipated-query cleanup.
	cases := []struct {
		name    string
		breakFn func(t *testing.T, s *Store)
	}{
		{
			name: "mark_aborted",
			breakFn: func(t *testing.T, s *Store) {
				t.Helper()
				mustExec(t, s, `CREATE TRIGGER los59a_deny_mark BEFORE UPDATE ON learnings
					WHEN NEW.superseded_by IS NOT NULL AND NEW.superseded_by > 0
					BEGIN SELECT RAISE(ABORT, 'los59a: mark denied'); END`)
			},
		},
		{
			name: "link_aborted",
			breakFn: func(t *testing.T, s *Store) {
				t.Helper()
				mustExec(t, s, `CREATE TRIGGER los59a_deny_link BEFORE UPDATE OF supersedes ON learnings
					WHEN NEW.supersedes IS NOT NULL
					BEGIN SELECT RAISE(ABORT, 'los59a: link denied'); END`)
			},
		},
		{
			name: "fts_cleanup_aborted",
			breakFn: func(t *testing.T, s *Store) {
				t.Helper()
				mustExec(t, s, `DROP TABLE anticipated_queries_fts`)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpen(t)
			insert := func(content string) int64 {
				id, err := store.InsertLearning(&models.Learning{
					Category: "decision", Content: content, Confidence: 1.0,
					CreatedAt: time.Now(), ModelUsed: "test",
				})
				if err != nil {
					t.Fatalf("insert %q: %v", content, err)
				}
				return id
			}
			// Two losers: the first statement of the batch fails, so the second
			// row proves that nothing of the batch was left retired.
			losers := []int64{insert("write error loser 1"), insert("write error loser 2")}
			winner := insert("write error winner")

			tc.breakFn(t, store)

			if err := store.SupersedeLearningBatch(losers, winner, "must fail"); err == nil {
				t.Fatalf("SupersedeLearningBatch reported success although %s aborted the write", tc.name)
			}

			for _, id := range losers {
				got, err := store.GetLearning(id)
				if err != nil {
					t.Fatalf("get loser #%d: %v", id, err)
				}
				if got.SupersededBy != nil {
					t.Errorf("loser #%d was retired despite the failed write: superseded_by=%d", id, *got.SupersededBy)
				}
			}
		})
	}
}

// TestLos59a_GetLearningChain_SurvivesNonPositiveSentinels pins YM-426.
//
// A retired learning does not always point at a real successor. Cleanup paths
// write a non-positive sentinel into superseded_by instead: -1 for junk
// cleanup, -2 for resolved, -3 for re-extract, -4 for a successor that a delete
// path removed (YM-435), and 0 for a rule-based substance-free retire. None of
// those values is a learning ID, so the forward walk has to stop at that row
// instead of querying it and failing with sql.ErrNoRows. Every row that carries
// such a sentinel is a readable chain member.
func TestLos59a_GetLearningChain_SurvivesNonPositiveSentinels(t *testing.T) {
	sentinels := []struct {
		name  string
		value int64
	}{
		{"junk_cleanup", -1},
		{"resolved", SupersededByResolved},
		{"reextract", SupersededByReextract},
		{"successor_deleted", SupersededByDeleted},
		// -6 has no writer left in this tree but exists on 182 live rows, which
		// is the reason the guard tests the sign and not a list of known values.
		{"unlisted_live_value", -6},
		{"substance_free", 0},
	}

	ids := func(chain []models.Learning) []int64 {
		out := make([]int64, 0, len(chain))
		for _, l := range chain {
			out = append(out, l.ID)
		}
		return out
	}
	equal := func(got, want []int64) bool {
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

	for _, tc := range sentinels {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpen(t)

			// Mixed strand: v1 -> v2 (real IDs) and v2 ends on a sentinel.
			v1, err := store.InsertLearning(&models.Learning{
				Category: "decision", Content: "sentinel " + tc.name + " v1", Confidence: 1.0,
				CreatedAt: time.Now(), ModelUsed: "test",
			})
			if err != nil {
				t.Fatalf("insert v1: %v", err)
			}
			v2, err := store.InsertLearning(&models.Learning{
				Category: "decision", Content: "sentinel " + tc.name + " v2", Confidence: 1.0,
				CreatedAt: time.Now(), ModelUsed: "test",
			})
			if err != nil {
				t.Fatalf("insert v2: %v", err)
			}
			if err := store.SupersedeLearning(v1, v2, "real supersede"); err != nil {
				t.Fatalf("supersede v1 by v2: %v", err)
			}
			if _, err := store.DB().Exec(
				`UPDATE learnings SET superseded_by = ?, supersede_reason = ? WHERE id = ?`,
				tc.value, "sentinel "+tc.name, v2); err != nil {
				t.Fatalf("seed sentinel %d: %v", tc.value, err)
			}

			want := []int64{v1, v2}
			for _, start := range []int64{v1, v2} {
				chain, err := store.GetLearningChain(start)
				if err != nil {
					t.Fatalf("chain from #%d (sentinel %d) returned an error instead of the readable history: %v",
						start, tc.value, err)
				}
				if got := ids(chain); !equal(got, want) {
					t.Errorf("chain from #%d (sentinel %d) = %v, want %v", start, tc.value, got, want)
				}
			}
		})
	}

	// A single retired row whose only edge is a sentinel: history = itself.
	t.Run("single_row", func(t *testing.T) {
		store := mustOpen(t)
		row, err := store.InsertLearning(&models.Learning{
			Category: "decision", Content: "sentinel only row", Confidence: 1.0,
			CreatedAt: time.Now(), ModelUsed: "test",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := store.ResolveLearning(row, "resolved without successor"); err != nil {
			t.Fatalf("resolve: %v", err)
		}

		chain, err := store.GetLearningChain(row)
		if err != nil {
			t.Fatalf("chain from resolved #%d: %v", row, err)
		}
		if got := ids(chain); !equal(got, []int64{row}) {
			t.Errorf("chain = %v, want [%d]", got, row)
		}
	})

	// Control: a chain with real IDs only and an active end keeps its full
	// oldest-first order; the sentinel guard must not truncate it.
	t.Run("positive_chain_unchanged", func(t *testing.T) {
		store := mustOpen(t)
		first, err := store.InsertLearning(&models.Learning{Category: "decision", Content: "pos v1", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test"})
		if err != nil {
			t.Fatalf("insert v1: %v", err)
		}
		second, err := store.InsertLearning(&models.Learning{Category: "decision", Content: "pos v2", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test"})
		if err != nil {
			t.Fatalf("insert v2: %v", err)
		}
		third, err := store.InsertLearning(&models.Learning{Category: "decision", Content: "pos v3", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test"})
		if err != nil {
			t.Fatalf("insert v3: %v", err)
		}
		if err := store.SupersedeLearning(first, second, "step 1"); err != nil {
			t.Fatalf("supersede 1->2: %v", err)
		}
		if err := store.SupersedeLearning(second, third, "step 2"); err != nil {
			t.Fatalf("supersede 2->3: %v", err)
		}

		chain, err := store.GetLearningChain(first)
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		if got := ids(chain); !equal(got, []int64{first, second, third}) {
			t.Errorf("chain = %v, want [%d %d %d]", got, first, second, third)
		}
	})
}
