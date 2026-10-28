package extraction

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-362 review finding I-1: sparing a user-owned row in the reextract path is
// only half the protection. The dedup step that follows an extraction could
// still rewrite that row's content through PreAdmissionUpdate, so the agreement
// survived as a row but not as the user's wording.
func TestLos19d_PreAdmissionNeverRewritesUserOwnedRows(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const sessionID = "los19d-user-owned-dedup"
	existingContent := "Port 8080 war belegt wir wechselten auf 8081"
	existingID, err := store.InsertLearning(&models.Learning{
		SessionID: sessionID, Category: "gotcha", Content: existingContent,
		Source: "agreed_upon", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert user-owned learning: %v", err)
	}

	// Similar topic, all tokens of the existing row, long enough for the
	// enrichment branch (newLen > existLen*3/2, token similarity >= 0.5).
	paraphrase := "Port 8080 war belegt wir wechselten auf 8081 und passten danach die Firewall Regeln an weil der neue Port sonst nicht erreichbar gewesen waere"
	result := CheckPreAdmission(store, &models.Learning{
		SessionID: sessionID, Category: "gotcha", Content: paraphrase,
		Source: "llm_extracted", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})
	if result.Action != PreAdmissionSkip {
		t.Fatalf("pre-admission action = %v (reason %q), want Skip for a user-owned row", result.Action, result.Reason)
	}

	stored, err := store.GetLearning(existingID)
	if err != nil {
		t.Fatalf("read back user-owned learning: %v", err)
	}
	if stored.Content != existingContent {
		t.Fatalf("user-owned content was rewritten to %q", stored.Content)
	}
}

// YM-362 review finding M1: ErrPartial claims that the pass produced learnings.
// A whitespace-only answer is neither a success nor a failure, so a pass made of
// one transport failure and one empty answer produced nothing and must not be
// reported as a partial success.
func TestLos19d_EmptyAnswersAreNoPartialSuccess(t *testing.T) {
	calls := 0
	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("first chunk timed out")
			}
			return "   ", nil
		},
	}
	ext := NewExtractor(client, nil)
	ext.SetChunkSize(100)

	learnings, err := ext.ExtractFromSession("los19d-empty-answer", los19bConversation(4))
	if err == nil {
		t.Fatal("a pass that produced no result reported success")
	}
	if errors.Is(err, ErrPartial) {
		t.Fatalf("no chunk produced a result, so this is no partial success: %v", err)
	}
	if len(learnings) != 0 {
		t.Fatalf("expected no learnings, got %d", len(learnings))
	}
}

// YM-362 review finding (consequence, cycle 3): the pass-2 call is the decisive
// call of the two-pass pipeline. Before this loop existed, its transport failure
// counted towards the apiDown breaker, and it still has to: a pass that produces
// nothing is not a partial success.
func TestLos19d_PassTwoTransportFailureIsAnOutageAndNoPartial(t *testing.T) {
	summarizeClient := &mockLLMClient{
		completeFunc: func(system, user string) (string, error) {
			return "Zusammenfassung eines Chunks", nil
		},
		model: "haiku",
	}
	extractClient := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return "", errors.New("pass 2 upstream 500")
		},
		model: "sonnet",
	}
	ext := NewTwoPassExtractor(summarizeClient, extractClient, nil)
	ext.SetChunkSize(100)

	learnings, err := ext.ExtractFromSession("los19d-pass2-outage", los19bConversation(4))
	if err == nil {
		t.Fatal("a failed pass 2 reported success")
	}
	if !IsOutage(err) {
		t.Fatalf("the decisive extraction call failed, so this is an outage: %v", err)
	}
	if errors.Is(err, ErrPartial) {
		t.Fatalf("nothing was extracted, so this is no partial success: %v", err)
	}
	if len(learnings) != 0 {
		t.Fatalf("expected no learnings, got %d", len(learnings))
	}
}

// The same distinction for an unreadable pass 2: nothing was extracted, so the
// error is a content failure and not a partial success.
func TestLos19d_PassTwoParseFailureIsNoPartial(t *testing.T) {
	summarizeClient := &mockLLMClient{
		completeFunc: func(system, user string) (string, error) {
			return "Zusammenfassung eines Chunks", nil
		},
		model: "haiku",
	}
	extractClient := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return "not json at all", nil
		},
		model: "sonnet",
	}
	ext := NewTwoPassExtractor(summarizeClient, extractClient, nil)
	ext.SetChunkSize(100)

	if _, err := ext.ExtractFromSession("los19d-pass2-parse", los19bConversation(4)); err == nil {
		t.Fatal("an unreadable pass 2 reported success")
	} else if errors.Is(err, ErrPartial) {
		t.Fatalf("nothing was extracted, so this is no partial success: %v", err)
	} else if IsOutage(err) {
		t.Fatalf("an unreadable answer proves the API is reachable: %v", err)
	}
}

// Security review NEW-1: the error text embeds the model's raw answer. A newline
// in it must not survive into the log line, otherwise the answer can forge a
// second, fake log entry.
func TestLos19d_ErrorTextCannotForgeALogLine(t *testing.T) {
	failure := &chunkFailures{chunks: 2, parse: 1, parseErr: errors.New(
		"parse json: boom\n2026/10/20 12:00:00   [1/1] ok deadbeef (proj) extraction ok")}
	msg := failure.Error()
	if strings.ContainsAny(msg, "\r\n") {
		t.Fatalf("error text carries a line break: %q", msg)
	}
}

// The real sink is the per-chunk warning line, not the aggregated error. The
// raw answer reaches it through parseExtractionResponse, so the log writer is
// what the test has to watch.
func TestLos19d_ParseWarningCannotForgeALogLine(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	client := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return "kaputt\n2026/10/20 12:00:00   [9/9] ok forge0001 (proj) extraction ok", nil
		},
	}
	ext := NewExtractor(client, nil)
	if _, err := ext.ExtractFromSession("los19d-log-forge", los19bConversation(2)); err == nil {
		t.Fatal("an unparseable answer reported success")
	}

	out := buf.String()
	if strings.Contains(out, "\n2026/10/20 12:00:00") {
		t.Fatalf("the model answer forged a second log line: %q", out)
	}
	if !strings.Contains(out, "kaputt 2026/10/20 12:00:00") {
		t.Fatalf("the warning line is missing or was not flattened: %q", out)
	}
}
