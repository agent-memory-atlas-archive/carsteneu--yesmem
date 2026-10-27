
package locomo

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestRunnerIngestAndQuery(t *testing.T) {
	store := mustStore(t)

	samples, err := ParseDataset(strings.NewReader(miniDataset))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}

	answerLLM := &mockLLM{response: "test answer"}
	judgeLLM := &mockLLM{response: `{"reasoning": "ok", "label": "CORRECT"}`}
	runner := NewRunner(store, answerLLM, judgeLLM)

	// Ingest
	stats, err := runner.Ingest(samples)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stats entry, got %d", len(stats))
	}
	if stats[0].Sessions != 2 {
		t.Errorf("stats[0].Sessions = %d, want 2", stats[0].Sessions)
	}

	// Query with UseMessages=true (skip extract)
	cfg := DefaultQueryConfig()
	cfg.UseMessages = true

	results, err := runner.Query(samples, cfg)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	// miniDataset has 3 scored QA pairs (categories 1, 2, 1 — category 5 excluded)
	if len(results) != 3 {
		t.Fatalf("expected 3 query results, got %d", len(results))
	}

	for i, r := range results {
		if r.Generated != "test answer" {
			t.Errorf("result[%d].Generated = %q, want %q", i, r.Generated, "test answer")
		}
		if r.SampleID != "1" {
			t.Errorf("result[%d].SampleID = %q, want %q", i, r.SampleID, "1")
		}
	}
}

func TestRunnerJudge(t *testing.T) {
	store := mustStore(t)

	judgeLLM := &mockLLM{response: `{"reasoning": "Match", "label": "CORRECT"}`}
	answerLLM := &mockLLM{response: "unused"}
	runner := NewRunner(store, answerLLM, judgeLLM)

	queries := []QueryResult{
		{SampleID: "1", Question: "Q1", Gold: "A1", Generated: "A1", Category: 1},
		{SampleID: "1", Question: "Q2", Gold: "A2", Generated: "A2", Category: 2},
	}

	results, err := runner.Judge(queries)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 judge results, got %d", len(results))
	}

	for i, r := range results {
		if r.Label != "CORRECT" {
			t.Errorf("result[%d].Label = %q, want %q", i, r.Label, "CORRECT")
		}
		if r.Score != 1 {
			t.Errorf("result[%d].Score = %d, want 1", i, r.Score)
		}
	}
}

func TestRunnerRunFull(t *testing.T) {
	store := mustStore(t)

	samples, err := ParseDataset(strings.NewReader(miniDataset))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}

	answerLLM := &mockLLM{response: "Paris"}
	judgeLLM := &mockLLM{response: `{"reasoning": "Match", "label": "CORRECT"}`}
	runner := NewRunner(store, answerLLM, judgeLLM)

	// skipExtract=true means UseMessages=true as fallback
	report, err := runner.RunFull(samples, DefaultQueryConfig(), true)
	if err != nil {
		t.Fatalf("RunFull: %v", err)
	}

	// 3 scored QA pairs, all judged CORRECT by mock
	if report.TotalCount != 3 {
		t.Errorf("report.TotalCount = %d, want 3", report.TotalCount)
	}
	if report.TotalCorrect != 3 {
		t.Errorf("report.TotalCorrect = %d, want 3", report.TotalCorrect)
	}
	if report.OverallMean != 1.0 {
		t.Errorf("report.OverallMean = %f, want 1.0", report.OverallMean)
	}

	// Should have category scores
	if len(report.Categories) == 0 {
		t.Error("report.Categories is empty")
	}
}

func TestRunnerRunMultiple(t *testing.T) {
	store := mustStore(t)

	samples, err := ParseDataset(strings.NewReader(miniDataset))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}

	answerLLM := &mockLLM{response: "Paris"}
	judgeLLM := &mockLLM{response: `{"reasoning": "Match", "label": "CORRECT"}`}
	runner := NewRunner(store, answerLLM, judgeLLM)

	stats, err := runner.RunMultiple(samples, DefaultQueryConfig(), true, 3)
	if err != nil {
		t.Fatalf("RunMultiple: %v", err)
	}

	if stats.Runs != 3 {
		t.Errorf("stats.Runs = %d, want 3", stats.Runs)
	}
	// All runs should score 1.0 with mock returning CORRECT
	if stats.Mean != 1.0 {
		t.Errorf("stats.Mean = %f, want 1.0", stats.Mean)
	}
	if stats.Min != 1.0 {
		t.Errorf("stats.Min = %f, want 1.0", stats.Min)
	}
	if stats.Max != 1.0 {
		t.Errorf("stats.Max = %f, want 1.0", stats.Max)
	}
}

func TestRunnerExtractNilError(t *testing.T) {
	store := mustStore(t)
	answerLLM := &mockLLM{response: "unused"}
	judgeLLM := &mockLLM{response: "unused"}
	runner := NewRunner(store, answerLLM, judgeLLM)

	samples, err := ParseDataset(strings.NewReader(miniDataset))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}

	// Extract without setting extractor should return error
	_, err = runner.Extract(samples, 1)
	if err == nil {
		t.Fatal("expected error when extractor is nil")
	}
}

// los19cFailingExtractor fails every extraction, the way a provider outage or a
// broken model would.
type los19cFailingExtractor struct {
	calls int32
}

func (e *los19cFailingExtractor) ExtractFromSession(string, []models.Message) ([]models.Learning, error) {
	atomic.AddInt32(&e.calls, 1)
	return nil, errors.New("upstream 500")
}

func (e *los19cFailingExtractor) ExtractAndStore(string, string, []models.Message, bool) error {
	atomic.AddInt32(&e.calls, 1)
	return errors.New("upstream 500")
}

func (e *los19cFailingExtractor) SetChunkSize(int) {}

// YM-317 Key 4: a run whose extraction failed still reported "Extracted N
// sessions total", so a broken extraction looked like a complete one. The count
// must follow the outcome.
func TestLos19c_ExtractCountsFailuresAsFailures(t *testing.T) {
	store := mustStore(t)
	runner := NewRunner(store, &mockLLM{response: "unused"}, &mockLLM{response: "unused"})
	ext := &los19cFailingExtractor{}
	runner.SetExtractor(ext)

	samples, err := ParseDataset(strings.NewReader(miniDataset))
	if err != nil {
		t.Fatalf("ParseDataset: %v", err)
	}
	if _, err := runner.Ingest(samples); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	stats, err := runner.Extract(samples, 1)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := atomic.LoadInt32(&ext.calls); got == 0 {
		t.Fatal("extractor was never called, the test proves nothing")
	}
	if stats.Extracted != 0 {
		t.Errorf("stats.Extracted = %d, want 0 for a run in which every extraction failed", stats.Extracted)
	}
	if stats.Failed != int(ext.calls) {
		t.Errorf("stats.Failed = %d, want %d (every attempted session failed)", stats.Failed, ext.calls)
	}
}
