package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/briefing"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los16EchoLLMClient returns the prompt unchanged, so the stored refined
// briefing mirrors what the background path handed to the refiner.
type los16EchoLLMClient struct{}

func (los16EchoLLMClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	return userMsg, nil
}

func (los16EchoLLMClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return userMsg, nil
}

func (los16EchoLLMClient) Name() string  { return "los16-echo" }
func (los16EchoLLMClient) Model() string { return "los16-echo" }

// YM-104: the background briefing built its generator with briefing.New(store, 0)
// and only SetUserProfile, so briefing.max_per_category, the dedup threshold and
// the unfinished TTL were silently dropped. The raw briefing is what the refiner
// receives, and its hash is what RefineBriefing later validates against, so a
// config knob that never reaches the generator leaves the hash unchanged.
func TestLos16BackgroundBriefingHonoursConfiguredBriefingConfig(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	project := t.TempDir()
	if err := store.UpsertSession(&models.Session{
		ID: "los16-bg-s1", Project: project, ProjectShort: project,
		FirstMessage: "los16 background fixture", MessageCount: 3,
		StartedAt: time.Now().Add(-2 * time.Hour),
		JSONLPath: "/los16-bg-s1.jsonl",
		IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	for _, content := range []string{
		"los16 adamant harbour clause",
		"los16 bitter meadows requiem",
		"los16 crimson valley noise",
	} {
		if _, err := store.InsertLearning(&models.Learning{
			Category: "gotcha", Content: content, Project: project,
			Confidence: 1.0, CreatedAt: time.Now(), Source: "llm_extracted",
		}); err != nil {
			t.Fatalf("insert learning: %v", err)
		}
	}

	cfg := &config.Config{}
	cfg.Paths.DB = filepath.Join(t.TempDir(), "yesmem.db")
	// regenerateBriefingsForTargets sets the package-global strings path; leaving
	// it pointing into a deleted temp dir makes later briefings load zero strings.
	t.Cleanup(func() { briefing.SetStringsPath("") })
	targets := []briefingRefreshTarget{{
		Project: storage.ProjectSummary{Project: project, ProjectShort: project},
	}}

	rawHashFor := func(maxPerCategory int) string {
		cfg.Briefing.MaxPerCategory = maxPerCategory
		regenerateBriefingsForTargets(store, cfg, los16EchoLLMClient{}, targets)
		hash, err := store.GetRefinedBriefingHash(project)
		if err != nil {
			t.Fatalf("get refined briefing hash: %v", err)
		}
		return hash
	}

	wide := rawHashFor(3)
	narrow := rawHashFor(1)
	if wide == narrow {
		t.Errorf("briefing.max_per_category 3 and 1 produced the same raw briefing (hash %q): the background path ignores the configured selection", narrow)
	}
}
