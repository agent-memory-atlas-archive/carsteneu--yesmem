package ivf

import "testing"

// TestBuildExcludesQuarantinedVectorsLos10b (YM-245 [MEM-F28]): Build loaded
// every vector that was merely non-superseded. A quarantined vector still got a
// cluster slot, shaped the k-means centroids and displaced active neighbours in
// the probe, even though the read path then dropped it.
func TestBuildExcludesQuarantinedVectorsLos10b(t *testing.T) {
	db := testDBWithVectors(t, 4, map[uint64][]float32{
		1: makeTestVec(4, 0.1),
		2: makeTestVec(4, 0.4),
		3: makeTestVec(4, 0.7),
	})
	if _, err := db.Exec(`UPDATE learnings SET quarantined_at = '2026-10-17 20:00:00' WHERE id = 2`); err != nil {
		t.Fatalf("quarantine row: %v", err)
	}

	idx, err := Build(db, 4, 1, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	total := 0
	for _, cluster := range idx.Clusters {
		for _, id := range cluster {
			total++
			if id == 2 {
				t.Errorf("quarantined vector 2 was placed in a cluster")
			}
		}
	}
	if total != 2 {
		t.Errorf("index holds %d vectors, want 2 (active only)", total)
	}
}
