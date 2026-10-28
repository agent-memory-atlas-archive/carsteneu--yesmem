package storage

import (
	"encoding/json"
	"testing"
)

func TestRecordTurnHash_NewThread(t *testing.T) {
	s := mustOpen(t)
	if err := s.RecordTurnHash("thread-1", "proj", "a1b2", "sh→cap_store→haiku"); err != nil {
		t.Fatalf("RecordTurnHash: %v", err)
	}
	seq, err := s.GetThreadSequence("thread-1")
	if err != nil {
		t.Fatalf("GetThreadSequence: %v", err)
	}
	if seq == nil {
		t.Fatal("expected sequence, got nil")
	}
	if seq.Project != "proj" {
		t.Errorf("project: got %q, want %q", seq.Project, "proj")
	}
	var hashes []string
	if err := json.Unmarshal([]byte(seq.TurnHashes), &hashes); err != nil {
		t.Fatalf("unmarshal turn_hashes: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != "a1b2" {
		t.Errorf("turn_hashes: got %v, want [a1b2]", hashes)
	}
}

func TestRecordTurnHash_AppendsToExisting(t *testing.T) {
	s := mustOpen(t)
	for i, h := range []string{"h1", "h2", "h3"} {
		if err := s.RecordTurnHash("t1", "proj", h, "example"); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	seq, err := s.GetThreadSequence("t1")
	if err != nil {
		t.Fatalf("GetThreadSequence: %v", err)
	}
	var hashes []string
	if err := json.Unmarshal([]byte(seq.TurnHashes), &hashes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(hashes) != 3 {
		t.Errorf("expected 3 hashes, got %d", len(hashes))
	}
	if hashes[0] != "h1" || hashes[2] != "h3" {
		t.Errorf("order wrong: got %v", hashes)
	}
}

func TestRecordTurnHash_FIFOat20(t *testing.T) {
	s := mustOpen(t)
	for i := 0; i < 25; i++ {
		h := "h" + string(rune('A'+i))
		if err := s.RecordTurnHash("t1", "proj", h, "ex"); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	seq, err := s.GetThreadSequence("t1")
	if err != nil {
		t.Fatalf("GetThreadSequence: %v", err)
	}
	var hashes []string
	if err := json.Unmarshal([]byte(seq.TurnHashes), &hashes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(hashes) != 20 {
		t.Fatalf("expected 20 hashes (FIFO cap), got %d", len(hashes))
	}
	if hashes[0] != "hF" {
		t.Errorf("FIFO: first hash should be hF (6th inserted), got %q", hashes[0])
	}
	if hashes[19] != "hY" {
		t.Errorf("FIFO: last hash should be hY (25th inserted), got %q", hashes[19])
	}
}

func TestRecordTurnHash_ProjectScope(t *testing.T) {
	s := mustOpen(t)
	s.RecordTurnHash("t1", "proj-a", "ha", "ex")
	s.RecordTurnHash("t1", "proj-a", "hb", "ex")

	seq, err := s.GetThreadSequence("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if seq.Project != "proj-a" {
		t.Errorf("project: got %q", seq.Project)
	}
}

func TestGetWorkflowSuggestions_FindsRepeatedSubsequences(t *testing.T) {
	s := mustOpen(t)
	pattern := []string{"aa", "bb", "cc"}
	for _, tid := range []string{"t1", "t2", "t3"} {
		for _, h := range pattern {
			s.RecordTurnHash(tid, "proj", h, "ex")
		}
	}
	suggestions, err := s.GetWorkflowSuggestions("proj", 3)
	if err != nil {
		t.Fatalf("GetWorkflowSuggestions: %v", err)
	}
	if len(suggestions) == 0 {
		t.Fatal("expected at least one workflow suggestion")
	}
	found := false
	for _, sg := range suggestions {
		if sg.Count >= 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("no suggestion with count >= 3, got %+v", suggestions)
	}
}

func TestGetWorkflowSuggestions_RespectsProjectScope(t *testing.T) {
	s := mustOpen(t)
	pattern := []string{"aa", "bb", "cc"}
	for _, tid := range []string{"t1", "t2", "t3"} {
		for _, h := range pattern {
			s.RecordTurnHash(tid, "proj-a", h, "ex")
		}
	}
	suggestions, err := s.GetWorkflowSuggestions("proj-b", 3)
	if err != nil {
		t.Fatalf("GetWorkflowSuggestions: %v", err)
	}
	if len(suggestions) != 0 {
		t.Errorf("expected 0 suggestions for proj-b, got %d", len(suggestions))
	}
}

func TestGetWorkflowSuggestions_NoFalsePositiveFromSingleThread(t *testing.T) {
	s := mustOpen(t)
	for _, h := range []string{"aa", "bb", "cc"} {
		s.RecordTurnHash("t1", "proj", h, "ex")
	}
	suggestions, err := s.GetWorkflowSuggestions("proj", 3)
	if err != nil {
		t.Fatalf("GetWorkflowSuggestions: %v", err)
	}
	if len(suggestions) != 0 {
		t.Errorf("expected 0 (only 1 thread has the subsequence, need 3), got %d", len(suggestions))
	}
}
