package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestGetPendingConfirmationLearnings lists only active learnings that still
// carry the retired status, matches legacy rows without a canonical_project,
// and reports the total independently of the returned page.
func TestGetPendingConfirmationLearnings(t *testing.T) {
	store := mustOpen(t)
	now := time.Now()

	parked, err := store.InsertLearning(&models.Learning{
		Category: "decision", Content: "parked conflict side", Project: "yesmem",
		Confidence: 1.0, CreatedAt: now, ModelUsed: "test", Source: "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Legacy shape: parked before the canonical_project backfill, so the project
	// column holds a full path and canonical_project is empty. Live rows look
	// like this; an exact canonical comparison would hide them.
	legacy, err := store.InsertLearning(&models.Learning{
		Category: "decision", Content: "legacy parked row", Project: "/work/yesmem",
		Confidence: 1.0, CreatedAt: now, ModelUsed: "test", Source: "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("UPDATE learnings SET canonical_project = '' WHERE id = ?", legacy); err != nil {
		t.Fatal(err)
	}
	other, err := store.InsertLearning(&models.Learning{
		Category: "decision", Content: "parked other project", Project: "otherproj",
		Confidence: 1.0, CreatedAt: now, ModelUsed: "test", Source: "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertLearning(&models.Learning{
		Category: "decision", Content: "unrelated active decision", Project: "yesmem",
		Confidence: 1.0, CreatedAt: now, ModelUsed: "test", Source: "user_stated",
	}); err != nil {
		t.Fatal(err)
	}
	alreadySuperseded, err := store.InsertLearning(&models.Learning{
		Category: "decision", Content: "parked but already superseded", Project: "yesmem",
		Confidence: 1.0, CreatedAt: now, ModelUsed: "test", Source: "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{parked, legacy, other, alreadySuperseded} {
		if err := store.SetSupersedeStatus(id, "pending_confirmation"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SupersedeLearning(alreadySuperseded, parked, "already handled"); err != nil {
		t.Fatal(err)
	}

	got, total, err := store.GetPendingConfirmationLearnings("yesmem", 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 matching rows (modern + legacy), got %d", total)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 listed rows, got %d", len(got))
	}
	ids := map[int64]bool{got[0].ID: true, got[1].ID: true}
	if !ids[parked] || !ids[legacy] {
		t.Fatalf("expected #%d and legacy #%d, got %v", parked, legacy, ids)
	}
	if ids[other] {
		t.Fatalf("other project #%d must not match", other)
	}
	if ids[alreadySuperseded] {
		t.Fatalf("superseded learning #%d must not be listed", alreadySuperseded)
	}
	if got[0].SupersedeStatus != "pending_confirmation" {
		t.Fatalf("expected status pending_confirmation, got %q", got[0].SupersedeStatus)
	}

	// The page is capped, but the total must stay the true match count.
	page, total, err := store.GetPendingConfirmationLearnings("yesmem", 1)
	if err != nil {
		t.Fatalf("paged query: %v", err)
	}
	if len(page) != 1 || total != 2 {
		t.Fatalf("expected 1 of 2, got %d of %d", len(page), total)
	}

	filtered, total, err := store.GetPendingConfirmationLearnings("otherproj", 0)
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if total != 1 || len(filtered) != 1 || filtered[0].ID != other {
		t.Fatalf("expected only #%d for otherproj, got %d rows (total %d)", other, len(filtered), total)
	}
}
