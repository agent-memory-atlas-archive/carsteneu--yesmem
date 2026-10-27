package proxy

import (
	"encoding/json"
	"testing"
)

func TestDocsHintInject_FirstRequest(t *testing.T) {
	// First request should set baseline, not inject
	s := &Server{}
	result := s.docsHintInject("thread-1", 5000, "test-project")
	if result != "" {
		t.Errorf("first request should not inject, got: %s", result)
	}
}

func TestDocsHintInject_Before10kThreshold(t *testing.T) {
	s := &Server{}
	// Set baseline
	s.docsHintInject("thread-2", 5000, "test-project")
	// 8k tokens later — under 10k threshold
	result := s.docsHintInject("thread-2", 13000, "test-project")
	if result != "" {
		t.Errorf("under 10k threshold should not inject, got: %s", result)
	}
}

func TestDocsHintInject_After10kThreshold(t *testing.T) {
	s := &Server{}
	// Set baseline
	s.docsHintInject("thread-3", 5000, "test-project")
	// 15k tokens later — over 10k threshold
	result := s.docsHintInject("thread-3", 20000, "test-project")
	// Will return "" because no daemon to query, but the mechanism should trigger
	// We test the token tracking, not the daemon call
	docsCp.mu.Lock()
	lastTokens := docsCp.lastTokenCount["thread-3"]
	docsCp.mu.Unlock()
	if lastTokens != 20000 {
		t.Errorf("expected token count updated to 20000, got %d", lastTokens)
	}
	_ = result // can't test content without daemon
}

func TestDocsHintInject_IndependentOfPlan(t *testing.T) {
	// Docs hint should work even without active plan
	// (plan checkpoint already handles docs hint when plan is active)
	s := &Server{}
	s.docsHintInject("thread-4", 1000, "test-project")
	// This tests that the function exists and tracks independently
	docsCp.mu.Lock()
	_, exists := docsCp.lastTokenCount["thread-4"]
	docsCp.mu.Unlock()
	if !exists {
		t.Error("docs hint should track tokens independently")
	}
}

// YM-99: the docs-hint mark stored the absolute token count, so after a
// collapse the delta turned negative and the hint paused until the context
// climbed back over the pre-collapse high-water mark.
func TestDocsHintInject_ResetsAfterContextShrink(t *testing.T) {
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method != "get_docs_hint" {
			return ""
		}
		b, _ := json.Marshal(map[string]any{"docs_hint": "[Docs available] reference docs indexed"})
		return string(b)
	})
	const threadID = "thread-shrink"
	t.Cleanup(func() {
		docsCp.mu.Lock()
		delete(docsCp.lastTokenCount, threadID)
		docsCp.mu.Unlock()
	})

	if got := s.docsHintInject(threadID, 100000, "/tmp/proj"); got != "" {
		t.Fatalf("first call should only set the baseline, got %q", got)
	}
	if got := s.docsHintInject(threadID, 5000, "/tmp/proj"); got != "" {
		t.Fatalf("the collapsed context must not fire a hint, got %q", got)
	}
	if got := s.docsHintInject(threadID, 15000, "/tmp/proj"); got == "" {
		t.Fatal("docs hint stayed silent after the context shrank — counter still pinned to the pre-collapse mark")
	}
}
