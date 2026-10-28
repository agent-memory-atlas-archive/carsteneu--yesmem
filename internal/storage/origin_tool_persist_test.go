package storage

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestInsertLearning_PersistsOriginTool(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	l := &models.Learning{
		Category:   "preference",
		Content:    "test origin_tool persistence",
		Project:    "test",
		Source:     "user_stated",
		OriginTool: "user",
	}
	id, err := s.InsertLearning(l)
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}
	if id == 0 {
		t.Fatalf("InsertLearning returned id=0")
	}

	got, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if got.OriginTool != "user" {
		t.Fatalf("OriginTool round-trip lost value: got %q want %q", got.OriginTool, "user")
	}
}

func TestInsertLearning_DefaultsToEmptyOrigin(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	l := &models.Learning{
		Category: "preference",
		Content:  "no origin set",
		Project:  "test",
	}
	id, err := s.InsertLearning(l)
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}
	got, err := s.GetLearning(id)
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if got.OriginTool != "" {
		t.Fatalf("expected empty OriginTool, got %q", got.OriginTool)
	}
}
