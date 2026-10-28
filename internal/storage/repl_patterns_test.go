package storage

import (
	"database/sql"
	"testing"
)

func TestRecordReplPattern_NewInsert(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "abc123", "sqlite3 db DELETE", ""); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 1)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p == nil {
		t.Fatal("expected pattern, got nil")
	}
	if p.Count != 1 {
		t.Errorf("count: got %d, want 1", p.Count)
	}
	if p.ShapeHash != "abc123" {
		t.Errorf("shape_hash: got %q", p.ShapeHash)
	}
	if p.FirstCmdExample != "sqlite3 db DELETE" {
		t.Errorf("first_cmd_example: got %q", p.FirstCmdExample)
	}
}

func TestRecordReplPattern_StoresMatchedCap(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h1", "sqlite3 caps.db SELECT FROM cap_reddit__posts", "reddit"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 1)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p == nil {
		t.Fatal("expected pattern, got nil")
	}
	if p.MatchedCap != "reddit" {
		t.Errorf("matched_cap: got %q, want %q", p.MatchedCap, "reddit")
	}
}

func TestRecordReplPattern_BackfillsMatchedCapOnConflict(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h1", "first", ""); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := s.RecordReplPattern("proj", "h1", "second", "reddit"); err != nil {
		t.Fatalf("second Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 1)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p == nil {
		t.Fatal("expected pattern, got nil")
	}
	if p.MatchedCap != "reddit" {
		t.Errorf("matched_cap should be backfilled on conflict: got %q, want %q", p.MatchedCap, "reddit")
	}
}

func TestGetReadyReplPatternSuggestionForActiveCaps_OnlyMatchingActive(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h-reddit", "cap_reddit__posts", "reddit"); err != nil {
		t.Fatalf("Record reddit: %v", err)
	}
	if err := s.RecordReplPattern("proj", "h-telegram", "cap_telegram__updates", "telegram"); err != nil {
		t.Fatalf("Record telegram: %v", err)
	}
	if err := s.RecordReplPattern("proj", "h-empty", "git status", ""); err != nil {
		t.Fatalf("Record empty: %v", err)
	}

	p, err := s.GetReadyReplPatternSuggestionForActiveCaps("proj", 1, []string{"reddit"})
	if err != nil {
		t.Fatalf("GetReady reddit-only: %v", err)
	}
	if p == nil {
		t.Fatal("expected reddit-matching pattern, got nil")
	}
	if p.MatchedCap != "reddit" {
		t.Errorf("matched_cap: got %q, want reddit", p.MatchedCap)
	}
}

func TestGetReadyReplPatternSuggestionForActiveCaps_SkipsInactive(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h-telegram", "cap_telegram__updates", "telegram"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestionForActiveCaps("proj", 1, []string{"reddit"})
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil (telegram not active), got %+v", p)
	}
}

func TestGetReadyReplPatternSuggestionForActiveCaps_SkipsEmptyMatchedCap(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h-empty", "git status", ""); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestionForActiveCaps("proj", 1, []string{"reddit"})
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil (empty matched_cap), got %+v", p)
	}
}

func TestGetReadyReplPatternSuggestionForActiveCaps_EmptyActiveListReturnsNil(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h-reddit", "cap_reddit__posts", "reddit"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestionForActiveCaps("proj", 1, nil)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil (no active caps), got %+v", p)
	}
}

func TestRecordReplPattern_IncrementExisting(t *testing.T) {
	s := mustOpen(t)
	for i := 0; i < 3; i++ {
		if err := s.RecordReplPattern("proj", "h1", "cmd", ""); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 3)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p == nil {
		t.Fatal("expected pattern, got nil")
	}
	if p.Count != 3 {
		t.Errorf("count after 3 records: got %d, want 3", p.Count)
	}
}

func TestGetReadyReplPatternSuggestion_UnderThreshold(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordReplPattern("proj", "h", "cmd", ""); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 5)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil (count=1 < threshold=5), got %+v", p)
	}
}

func TestGetReadyReplPatternSuggestion_MarksSuggestedOnce(t *testing.T) {
	s := mustOpen(t)
	for i := 0; i < 5; i++ {
		s.RecordReplPattern("proj", "h", "cmd", "")
	}
	p1, err := s.GetReadyReplPatternSuggestion("proj", 5)
	if err != nil {
		t.Fatalf("GetReady #1: %v", err)
	}
	if p1 == nil {
		t.Fatal("first call should return row")
	}
	p2, err := s.GetReadyReplPatternSuggestion("proj", 5)
	if err != nil {
		t.Fatalf("GetReady #2: %v", err)
	}
	if p2 != nil {
		t.Errorf("second call should return nil (already suggested), got %+v", p2)
	}
}

func TestGetReadyReplPatternSuggestion_RespectsProjectPartition(t *testing.T) {
	s := mustOpen(t)
	for i := 0; i < 5; i++ {
		s.RecordReplPattern("proj-a", "h", "cmd", "")
	}
	p, err := s.GetReadyReplPatternSuggestion("proj-b", 5)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("project partition leak: got pattern from proj-a when querying proj-b: %+v", p)
	}
}

func TestDismissReplPattern_ResetsCountAndClearsSuggested(t *testing.T) {
	s := mustOpen(t)
	for i := 0; i < 5; i++ {
		s.RecordReplPattern("proj", "h", "cmd", "")
	}
	if _, err := s.GetReadyReplPatternSuggestion("proj", 5); err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if err := s.DismissReplPattern("proj", "h"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if err := s.RecordReplPattern("proj", "h", "cmd", ""); err != nil {
		t.Fatalf("Record after dismiss: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 1)
	if err != nil {
		t.Fatalf("GetReady post-dismiss: %v", err)
	}
	if p == nil {
		t.Fatal("expected pattern available after dismiss+record (suggested_at cleared)")
	}
	if p.Count != 1 {
		t.Errorf("count after dismiss+1-record: got %d, want 1", p.Count)
	}
	if p.DismissCount != 1 {
		t.Errorf("dismiss_count: got %d, want 1", p.DismissCount)
	}
}

func TestDismissReplPattern_ThreeTimesSetsPermanent(t *testing.T) {
	s := mustOpen(t)
	for cycle := 0; cycle < 3; cycle++ {
		if err := s.RecordReplPattern("proj", "h", "cmd", ""); err != nil {
			t.Fatalf("Record cycle %d: %v", cycle, err)
		}
		if err := s.DismissReplPattern("proj", "h"); err != nil {
			t.Fatalf("Dismiss cycle %d: %v", cycle, err)
		}
	}
	if err := s.RecordReplPattern("proj", "h", "cmd", ""); err != nil {
		t.Fatalf("Record after 3x dismiss: %v", err)
	}
	p, err := s.GetReadyReplPatternSuggestion("proj", 1)
	if err != nil {
		t.Fatalf("GetReady: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil after 3x dismiss (permanent), got %+v", p)
	}
	var dp sql.NullString
	row := s.db.QueryRow(`SELECT dismissed_permanent_at FROM repl_pattern_observations WHERE project=? AND shape_hash=?`, "proj", "h")
	if err := row.Scan(&dp); err != nil {
		t.Fatalf("scan permanent flag: %v", err)
	}
	if !dp.Valid {
		t.Error("dismissed_permanent_at should be set after 3 dismissals")
	}
}
