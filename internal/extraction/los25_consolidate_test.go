package extraction

import (
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func los25InsertPair(t *testing.T, store interface {
	InsertLearning(*models.Learning) (int64, error)
}) []int64 {
	t.Helper()
	var ids []int64
	for i, content := range []string{
		"los25 distilled input one",
		"los25 distilled input two",
	} {
		id, err := store.InsertLearning(&models.Learning{
			Category:   "preference",
			Content:    content,
			Source:     "llm_extracted",
			Confidence: 1.0,
			Project:    "los25-distill",
			CreatedAt:  time.Now().Add(-time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("insert learning %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// YM-117 review round 1: the distillable set is narrower than the valid set. A
// distilled row must never become an unfinished task, a strategic note, or a cap.
func TestLos25_DistillClusterRejectsNonDistillableCategories(t *testing.T) {
	for _, proposed := range []string{"unfinished", "strategic", "cap"} {
		t.Run(proposed, func(t *testing.T) {
			store := mustOpenStore(t)
			ids := los25InsertPair(t, store)

			client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
				return fmt.Sprintf(`{"actions":[{"distilled_text":"los25 distilled result","category":%q,"supersedes_ids":[%d,%d],"reason":"merge"}]}`,
					proposed, ids[0], ids[1]), nil
			}}

			result := distillCluster(store, client, los25Cluster(t, "los25", ids))
			if result.Distilled != 1 {
				t.Fatalf("result = %+v, want one distilled learning", result)
			}

			all, err := store.GetActiveLearnings("", "los25-distill", "", "", 0)
			if err != nil {
				t.Fatalf("get active learnings: %v", err)
			}
			for _, l := range all {
				if l.Content != "los25 distilled result" {
					continue
				}
				if l.Category == proposed {
					t.Fatalf("distilled row carries non-distillable category %q", proposed)
				}
				if l.Category != "preference" {
					t.Fatalf("distilled category = %q, want the dominant input category", l.Category)
				}
			}
		})
	}
}

// YM-116: distillation inserted its consolidated learning without created_at, so
// the row carried an empty timestamp and "latest alerts" had no order to sort by.
func TestLos25_DistillClusterStampsCreatedAt(t *testing.T) {
	store := mustOpenStore(t)
	ids := los25InsertPair(t, store)

	client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
		return fmt.Sprintf(`{"actions":[{"distilled_text":"los25 distilled result","category":"preference","supersedes_ids":[%d,%d],"reason":"merge"}]}`,
			ids[0], ids[1]), nil
	}}

	result := distillCluster(store, client, los25Cluster(t, "los25", ids))
	if result.Distilled != 1 {
		t.Fatalf("result = %+v, want one distilled learning", result)
	}

	distilled, err := store.GetLearningsByCategory("preference", "los25-distill", 10)
	if err != nil {
		t.Fatalf("get distilled learnings: %v", err)
	}
	var found *models.Learning
	for i := range distilled {
		if distilled[i].Content == "los25 distilled result" {
			found = &distilled[i]
		}
	}
	if found == nil {
		t.Fatalf("distilled learning not found among %d preference rows", len(distilled))
	}
	if found.CreatedAt.IsZero() {
		t.Fatalf("distilled learning has no created_at")
	}
}

// los25Cluster builds a single-cluster fixture without depending on the los19 helper.
func los25Cluster(t *testing.T, label string, ids []int64) models.LearningCluster {
	t.Helper()
	raw := "["
	for i, id := range ids {
		if i > 0 {
			raw += ","
		}
		raw += fmt.Sprintf("%d", id)
	}
	raw += "]"
	return models.LearningCluster{Label: label, LearningIDs: raw, LearningCount: len(ids)}
}

// YM-117: distillation accepted whatever category the model proposed. Categories
// outside the valid set ("Build", "code", ...) drop out of every view that groups
// by category. The distilled row must carry a valid category.
func TestLos25_DistillClusterRejectsUnknownCategory(t *testing.T) {
	store := mustOpenStore(t)
	ids := los25InsertPair(t, store)

	client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
		return fmt.Sprintf(`{"actions":[{"distilled_text":"los25 distilled result","category":"Build","supersedes_ids":[%d,%d],"reason":"merge"}]}`,
			ids[0], ids[1]), nil
	}}

	result := distillCluster(store, client, los25Cluster(t, "los25", ids))
	if result.Distilled != 1 {
		t.Fatalf("result = %+v, want the cluster to be distilled", result)
	}

	all, err := store.GetActiveLearnings("", "los25-distill", "", "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	var distilled *models.Learning
	for i := range all {
		if all[i].Content == "los25 distilled result" {
			distilled = &all[i]
		}
	}
	if distilled == nil {
		t.Fatalf("distilled learning missing from the active set")
	}
	if !models.IsValidCategory(distilled.Category) {
		t.Fatalf("distilled category = %q, want a valid category", distilled.Category)
	}
	if distilled.Category != "preference" {
		t.Fatalf("distilled category = %q, want the dominant input category %q", distilled.Category, "preference")
	}
}
