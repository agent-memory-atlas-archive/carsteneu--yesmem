package extraction

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func los19InsertLearning(t *testing.T, store *storage.Store, content, source string) int64 {
	t.Helper()
	id, err := store.InsertLearning(&models.Learning{
		Category: "preference", Content: content, Source: source,
		Confidence: 1.0, CreatedAt: time.Now(), Project: "los19-consolidate",
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

func los19Cluster(t *testing.T, label string, ids []int64) models.LearningCluster {
	t.Helper()
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshal cluster ids: %v", err)
	}
	return models.LearningCluster{Label: label, LearningIDs: string(raw), LearningCount: len(ids)}
}

// YM-228: cluster distillation superseded every source learning, including the
// ones the user stated themselves. A cluster that contains a user-owned learning
// is skipped as a whole: no distilled row, no partial supersede, and no LLM call,
// so the active set stays unchanged and the next run does not re-distill it.
func TestLos19_DistillClusterSkipsClustersWithUserOwnedLearnings(t *testing.T) {
	store := mustOpenStore(t)

	userID := los19InsertLearning(t, store, "User will Antworten auf Deutsch, ohne Anglizismen und ohne Striche", "user_stated")
	firstID := los19InsertLearning(t, store, "Extrahierte Praeferenz: deutsche Antworten, keine Anglizismen", "llm_extracted")
	secondID := los19InsertLearning(t, store, "Extrahierte Praeferenz: Deutsch als Antwortsprache, kein Denglisch", "llm_extracted")

	called := false
	client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
		called = true
		return fmt.Sprintf(`{"actions":[{"distilled_text":"Antworten auf Deutsch","category":"preference","supersedes_ids":[%d,%d,%d],"reason":"merge"}]}`,
			userID, firstID, secondID), nil
	}}

	result := distillCluster(store, client, los19Cluster(t, "deutsche antworten", []int64{userID, firstID, secondID}))

	if called {
		t.Fatalf("cluster with a user-owned learning was sent to the LLM")
	}
	if result.Skipped != 1 || result.Distilled != 0 || result.Superseded != 0 {
		t.Fatalf("result = %+v, want Skipped=1 and nothing distilled", result)
	}

	for _, id := range []int64{userID, firstID, secondID} {
		l, err := store.GetLearning(id)
		if err != nil {
			t.Fatalf("get learning %d: %v", id, err)
		}
		if l.SupersededBy != nil {
			t.Fatalf("learning %d (%s) was superseded, want it active", id, l.Source)
		}
	}
}

// YM-228: the guard must not stop ordinary clusters from being distilled.
func TestLos19_DistillClusterStillDistillsAutoExtractedLearnings(t *testing.T) {
	store := mustOpenStore(t)

	firstID := los19InsertLearning(t, store, "Extrahierte Praeferenz: deutsche Antworten, keine Anglizismen", "llm_extracted")
	secondID := los19InsertLearning(t, store, "Extrahierte Praeferenz: Deutsch als Antwortsprache, kein Denglisch", "llm_extracted")

	client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
		return fmt.Sprintf(`{"actions":[{"distilled_text":"Antworten auf Deutsch ohne Anglizismen","category":"preference","supersedes_ids":[%d,%d],"reason":"merge"}]}`,
			firstID, secondID), nil
	}}

	result := distillCluster(store, client, los19Cluster(t, "deutsche antworten", []int64{firstID, secondID}))

	if result.Distilled != 1 || result.Superseded != 2 {
		t.Fatalf("result = %+v, want Distilled=1 Superseded=2", result)
	}
	for _, id := range []int64{firstID, secondID} {
		l, err := store.GetLearning(id)
		if err != nil {
			t.Fatalf("get learning %d: %v", id, err)
		}
		if l.SupersededBy == nil {
			t.Fatalf("learning %d stayed active, want it superseded by the distilled text", id)
		}
	}
}
