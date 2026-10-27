package daemon

import (
	"sync"
	"testing"
)

// YM-413: the daemon publishes the LLM clients from its async startup goroutine
// while handlers and the periodic jobs (rules refresh, staleness tick,
// llm_complete) already read them. Unlocked, a reader catches an interface value
// mid-write: the probe that motivated this test made the race detector report
// three DATA RACE warnings on the same fields. Every access now goes through the
// guarded wiring, so the same concurrent pattern is clean.
func TestLLMWiringIsRaceFreeAndVisible(t *testing.T) {
	h := &Handler{}
	client := &spySummarizeClient{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.SetQualityClient(client)
			h.SetCommitEvalClient(client)
			h.SetSummarizeClient(nil)
			h.SetLLMRouting("api", "", "http://example.test", nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			wiring := h.llmSnapshot()
			_ = wiring.quality
			_ = wiring.provider
			_ = h.commitEvalLLM()
			_ = h.summarizeLLM()
		}
	}()
	wg.Wait()

	if got := h.qualityLLMClient(); got == nil {
		t.Fatal("quality client not visible after wiring")
	}
	if got := h.commitEvalLLM(); got == nil {
		t.Fatal("commit-eval client not visible after wiring")
	}
	if got := h.llmSnapshot().provider; got != "api" {
		t.Fatalf("provider = %q, want api", got)
	}
}
