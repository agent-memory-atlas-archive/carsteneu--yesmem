package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestProjectSourceColumnExists(t *testing.T) {
	s := mustOpen(t)
	var src string
	err := s.db.QueryRow(`SELECT COALESCE(project_source,'') FROM learnings LIMIT 1`).Scan(&src)
	if err != nil && err.Error() != "sql: no rows in result set" {
		t.Fatalf("project_source column missing: %v", err)
	}
}

func TestProjectSourceRoundtrip(t *testing.T) {
	s := mustOpen(t)

	id, err := s.InsertLearning(&models.Learning{
		Category:      "gotcha",
		Content:       "attributed by content signal",
		Project:       "/home/test/memory/yesmem",
		ProjectSource: "content",
		Source:        "test",
		CreatedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectSource != "content" {
		t.Errorf("ProjectSource = %q, want %q", got.ProjectSource, "content")
	}
}

func TestProjectSourceDefaultEmpty(t *testing.T) {
	s := mustOpen(t)

	id, err := s.InsertLearning(&models.Learning{
		Category:  "gotcha",
		Content:   "no project source given",
		Project:   "/home/test/projects/opencode",
		Source:    "test",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectSource != "" {
		t.Errorf("ProjectSource = %q, want empty", got.ProjectSource)
	}
}
