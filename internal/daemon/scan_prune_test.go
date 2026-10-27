package daemon

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

func TestPruneScanState_RemovesOrphanedRows(t *testing.T) {
	s := newWikiTickTestStore(t)

	// The sweep needs at least one resolvable session; otherwise it treats the
	// session table as untrustworthy and skips pruning on purpose.
	live := t.TempDir()
	if _, err := s.DB().Exec(
		`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at)
		 VALUES ('sess-live', ?, ?, '2026-10-01T10:00:00Z', '', '2026-10-01T10:00:00Z')`,
		live, live,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := s.SaveProjectScan(&storage.ProjectScanRow{Project: "ghostproj", ScanJSON: `{"Files":[]}`, GitHead: "h"}); err != nil {
		t.Fatalf("save scan: %v", err)
	}

	pruneScanState(s)

	if row, err := s.GetProjectScan("ghostproj"); err != nil || row != nil {
		t.Errorf("orphaned scan row survived pruneScanState: row=%v err=%v", row, err)
	}
}
