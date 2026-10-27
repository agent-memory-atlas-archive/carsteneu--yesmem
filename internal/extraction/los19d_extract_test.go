package extraction

import (
	"errors"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los19dSingleLearning is a valid extraction answer carrying one learning.
const los19dSingleLearning = `{"domain":"code","learnings":[{"category":"gotcha","content":"Der erfolgreiche Chunk liefert diesen Befund","context":"","entities":[],"actions":[],"keywords":[],"trigger":"","importance":3}]}`

// YM-362 Key 2: a partly failed pass returned (nil, nil). The chunks that did
// answer were invisible to the breaker and to the attempt budget, and the batch
// caller certified the session although whole chunks were never read.
func TestLos19d_PartialFailureIsReportedAndKeepsLearnings(t *testing.T) {
	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("first chunk timed out")
			}
			return los19dSingleLearning, nil
		},
	}
	ext := NewExtractor(client, nil)
	ext.SetChunkSize(100)

	learnings, err := ext.ExtractFromSession("los19d-partial", los19bConversation(4))
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("a partial pass must report ErrPartial, got %v", err)
	}
	if len(learnings) == 0 {
		t.Fatal("the partial pass dropped the learnings of the chunk that answered")
	}
	if calls < 2 {
		t.Fatalf("only %d chunk(s) attempted: the session was not chunked, the case is untested", calls)
	}
	if IsOutage(err) {
		t.Error("a pass in which a chunk answered is not a transport outage")
	}
}

// YM-362 Key 2, the storing side: the learnings of the answering chunks are
// kept although the pass reports a failure. Otherwise the retry would cost the
// good chunks their result as well.
func TestLos19d_ExtractAndStoreKeepsLearningsOfAPartialPass(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("first chunk timed out")
			}
			return los19dSingleLearning, nil
		},
	}
	ext := NewExtractor(client, store)
	ext.SetChunkSize(100)

	if err := ext.ExtractAndStore("los19d-store-partial", "los19d", los19bConversation(4), false); !errors.Is(err, ErrPartial) {
		t.Fatalf("ExtractAndStore must surface the partial pass, got %v", err)
	}

	active, err := store.GetActiveLearnings("", "", "", "", 0)
	if err != nil {
		t.Fatalf("read active learnings: %v", err)
	}
	if len(active) == 0 {
		t.Fatal("the answering chunk produced a learning but nothing was stored")
	}
}

// YM-362 Key 4: a pass can fail in both ways at once. The old classification
// collapsed that mixed case into ErrParse, so the transport failure disappeared
// behind the content label. Both classes have to stay distinguishable.
func TestLos19d_MixedFailuresAreNeitherParseNorOutage(t *testing.T) {
	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("upstream 500")
			}
			return "not json at all", nil
		},
	}
	ext := NewExtractor(client, nil)
	ext.SetChunkSize(100)

	_, err := ext.ExtractFromSession("los19d-mixed", los19bConversation(4))
	if err == nil {
		t.Fatal("every chunk failed but the pass reported success")
	}
	if errors.Is(err, ErrParse) {
		t.Fatalf("the mixed pass was collapsed into a pure content failure: %v", err)
	}
	if IsOutage(err) {
		t.Fatalf("an unparseable answer proves the API is reachable, so a mixed pass is no outage: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream 500") {
		t.Errorf("the transport failure is not named in the error: %v", err)
	}
	if calls < 2 {
		t.Fatalf("only %d chunk(s) attempted: the session was not chunked, the case is untested", calls)
	}
}

// YM-362 Key 2 for the two-pass pipeline: when only some summaries fail, the
// failed chunks are missing from the combined text. That pass cannot certify
// the session either.
func TestLos19d_TwoPassPartialSummaryIsReported(t *testing.T) {
	summarizeCalls := 0
	summarizeClient := &mockLLMClient{
		completeFunc: func(system, user string) (string, error) {
			summarizeCalls++
			if summarizeCalls == 1 {
				return "", errors.New("summarize timeout")
			}
			return "Zusammenfassung eines Chunks mit einem Befund", nil
		},
		model: "haiku",
	}
	extractClient := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return los19dSingleLearning, nil
		},
		model: "sonnet",
	}

	ext := NewTwoPassExtractor(summarizeClient, extractClient, nil)
	ext.SetChunkSize(100)

	learnings, err := ext.ExtractFromSession("los19d-twopass-partial", los19bConversation(4))
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("a partial summarize pass must report ErrPartial, got %v", err)
	}
	if len(learnings) == 0 {
		t.Fatal("pass 2 produced a learning but the partial pass dropped it")
	}
	if summarizeCalls < 2 {
		t.Fatalf("only %d summary/summaries attempted: the session was not chunked, the case is untested", summarizeCalls)
	}
}
