package extraction

import (
	"errors"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los19bConversation builds a conversation the pre-filter keeps. 100 characters
// per message, so SetChunkSize(100) splits it into several chunks while the
// default chunk size keeps it in one.
func los19bConversation(n int) []models.Message {
	msgs := make([]models.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, models.Message{
			Role: role, MessageType: "text",
			Content: strings.Repeat("wort ", 25),
		})
	}
	return msgs
}

// YM-265 (finding F1 from the Los 19 cold review): every chunk of a session
// could fail and ExtractFromSession still returned a nil error. The batch
// caller then marked the session as extracted and certified its message range,
// so the session was never retried and its knowledge was lost silently. A pass
// in which no chunk produced a result is not proof of extraction.
func TestLos19b_ExtractReportsFailureWhenEveryChunkFails(t *testing.T) {
	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			return "", errors.New("upstream 500")
		},
	}
	ext := NewExtractor(client, nil)
	ext.SetChunkSize(100)

	_, err := ext.ExtractFromSession("los19b-all-chunks-failed", los19bConversation(4))
	if err == nil {
		t.Fatal("extraction reported success although every chunk failed: the session would be certified without extraction evidence")
	}
	if !strings.Contains(err.Error(), "upstream 500") {
		t.Errorf("error does not name the chunk failure: %v", err)
	}
	if calls < 2 {
		t.Fatalf("only %d chunk(s) attempted, the session was not chunked: the case 'all of several chunks failed' is untested", calls)
	}
}

// The counterpart: a partly failed pass still extracts what it saw. Since
// YM-362 the failure is no longer reported as full success (the older assertion
// here said "must not fail the session"), because a silent partial pass let the
// caller certify a session whose chunks were never read.
func TestLos19b_ExtractReportsPartialFailureWhenOnlyOneChunkFails(t *testing.T) {
	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("first chunk timed out")
			}
			return `{"learnings":[]}`, nil
		},
	}
	ext := NewExtractor(client, nil)
	ext.SetChunkSize(100)

	if _, err := ext.ExtractFromSession("los19b-one-chunk-failed", los19bConversation(4)); !errors.Is(err, ErrPartial) {
		t.Fatalf("a single failed chunk must be reported as a partial pass, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("only %d chunk(s) attempted: the session was not chunked, the test proved nothing", calls)
	}
}

// A valid answer without learnings is not a failure. Most sessions produce a
// handful of rows at best, and a session with nothing worth keeping must stay
// certifiable rather than be retried forever.
func TestLos19b_ExtractAcceptsAnEmptyExtraction(t *testing.T) {
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return `{"learnings":[]}`, nil
		},
	}
	ext := NewExtractor(client, nil)

	learnings, err := ext.ExtractFromSession("los19b-empty-extraction", los19bConversation(3))
	if err != nil {
		t.Fatalf("an empty extraction must not be a failure: %v", err)
	}
	if len(learnings) != 0 {
		t.Errorf("expected 0 learnings, got %d", len(learnings))
	}
}

// The two-pass extractor has the same hole in pass 1: when every summary fails
// it returns (nil, nil) and the session gets certified with nothing behind it.
func TestLos19b_TwoPassReportsFailureWhenEverySummaryFails(t *testing.T) {
	summarizeCalls := 0
	summarizeClient := &mockLLMClient{
		completeFunc: func(system, user string) (string, error) {
			summarizeCalls++
			return "", errors.New("summarize upstream 503")
		},
		model: "haiku",
	}
	extractClient := &mockLLMClient{model: "sonnet"}

	ext := NewTwoPassExtractor(summarizeClient, extractClient, nil)
	ext.SetChunkSize(100)

	_, err := ext.ExtractFromSession("los19b-all-summaries-failed", los19bConversation(4))
	if err == nil {
		t.Fatal("two-pass extraction reported success although every summary failed")
	}
	if !strings.Contains(err.Error(), "summarize upstream 503") {
		t.Errorf("error does not name the summarize failure: %v", err)
	}
	if summarizeCalls < 2 {
		t.Fatalf("only %d summary/summaries attempted, the session was not chunked: the case 'all of several chunks failed' is untested", summarizeCalls)
	}
}
