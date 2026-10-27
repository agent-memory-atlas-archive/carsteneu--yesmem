package daemon

import (
	"errors"
	"strings"
	"testing"
)

// TestLos56eSkipReasonSeparatesUpToDateFromAFailedQuery is the regression test
// for YM-367 (3): both pending queries in the extraction cycle logged a warning
// and then fell through to "skipped (up to date)". A run whose gate query failed
// therefore read like a run with nothing to do, so a broken query looked like a
// finished cycle on every start.
func TestLos56eSkipReasonSeparatesUpToDateFromAFailedQuery(t *testing.T) {
	if got := pendingSkipReason(nil); got != "up to date" {
		t.Fatalf("without an error the reason = %q, want %q", got, "up to date")
	}
	got := pendingSkipReason(errors.New("database is locked"))
	if !strings.Contains(got, "pending query failed") || !strings.Contains(got, "database is locked") {
		t.Fatalf("a failed query must be named as such, got %q", got)
	}
	if strings.Contains(got, "up to date") {
		t.Fatalf("a failed query must never read as up to date, got %q", got)
	}
}
