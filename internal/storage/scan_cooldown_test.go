package storage

import (
	"testing"
	"time"
)

func TestScanCooldown_RecordGetClear(t *testing.T) {
	s := newTestStore(t)

	failedAt, errMsg, err := s.GetScanFailure("someproject")
	if err != nil {
		t.Fatalf("GetScanFailure (empty): %v", err)
	}
	if !failedAt.IsZero() {
		t.Fatal("expected zero time when no failure recorded")
	}

	if err := s.RecordScanFailure("someproject", "cbm cli: signal: killed"); err != nil {
		t.Fatalf("RecordScanFailure: %v", err)
	}

	failedAt, errMsg, err = s.GetScanFailure("someproject")
	if err != nil {
		t.Fatalf("GetScanFailure: %v", err)
	}
	if failedAt.IsZero() {
		t.Fatal("expected failure timestamp after RecordScanFailure")
	}
	if age := time.Since(failedAt); age < 0 || age > time.Minute {
		t.Errorf("failed_at = %v, want roughly now", failedAt)
	}
	if errMsg != "cbm cli: signal: killed" {
		t.Errorf("error = %q, want %q", errMsg, "cbm cli: signal: killed")
	}
}

func TestScanCooldown_OverwritesPreviousFailure(t *testing.T) {
	s := newTestStore(t)

	if err := s.RecordScanFailure("proj", "first"); err != nil {
		t.Fatalf("RecordScanFailure 1: %v", err)
	}
	first, _, err := s.GetScanFailure("proj")
	if err != nil {
		t.Fatalf("GetScanFailure 1: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := s.RecordScanFailure("proj", "second"); err != nil {
		t.Fatalf("RecordScanFailure 2: %v", err)
	}
	second, msg, err := s.GetScanFailure("proj")
	if err != nil {
		t.Fatalf("GetScanFailure 2: %v", err)
	}

	if !second.After(first) {
		t.Errorf("expected failed_at to advance on re-record (first=%v second=%v)", first, second)
	}
	if msg != "second" {
		t.Errorf("error = %q, want %q", msg, "second")
	}
}

func TestScanCooldown_Clear(t *testing.T) {
	s := newTestStore(t)

	if err := s.RecordScanFailure("projectX", "boom"); err != nil {
		t.Fatalf("RecordScanFailure: %v", err)
	}
	if err := s.ClearScanFailure("projectX"); err != nil {
		t.Fatalf("ClearScanFailure: %v", err)
	}

	failedAt, _, err := s.GetScanFailure("projectX")
	if err != nil {
		t.Fatalf("GetScanFailure after clear: %v", err)
	}
	if !failedAt.IsZero() {
		t.Fatal("expected no failure record after ClearScanFailure")
	}
}
