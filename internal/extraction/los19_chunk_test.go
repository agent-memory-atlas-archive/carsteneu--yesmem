package extraction

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-54: the chunk formatter cut at byte offsets, so the 100/200/1500/3000 byte
// limits could end mid-rune and put invalid UTF-8 into the extraction prompt.
func TestLos19_ChunkContentStaysValidUTF8(t *testing.T) {
	// Euro signs are three bytes each, so every byte limit lands mid-rune.
	body := strings.Repeat("€", 3000)
	msgs := []models.Message{
		{Role: "assistant", MessageType: "text", Content: body},
		{Role: "user", MessageType: "tool_result", Content: body, ToolName: "bash"},
		{Role: "user", MessageType: "tool_use", Content: body, ToolName: "read"},
		{Role: "user", MessageType: "bash_output", Content: body},
	}

	chunks := ChunkMessages(msgs, 25000)
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Content) {
			runes := []rune(c.Content)
			tail := c.Content
			if len(runes) > 12 {
				tail = string(runes[len(runes)-12:])
			}
			t.Fatalf("chunk %d carries invalid UTF-8, tail %q", c.Index, tail)
		}
	}
}

// YM-54: extraction.chunk_size was accepted by the config and ignored by the
// extractor, which always chunked at 25000 tokens.
func TestLos19_SetChunkSizeReachesTheChunker(t *testing.T) {
	msgs := make([]models.Message, 0, 10)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, models.Message{
			Role: "assistant", MessageType: "text",
			Content: strings.Repeat("wort ", 25), // 100 characters
		})
	}

	countChunks := func(setChunkSize bool) int {
		chunks := 0
		client := &mockLLMClient{completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			chunks++
			return `{"learnings":[]}`, nil
		}}
		ext := NewExtractor(client, nil)
		if setChunkSize {
			ext.SetChunkSize(100)
		}
		if _, err := ext.ExtractFromSession("los19-chunk-size", msgs); err != nil {
			t.Fatalf("ExtractFromSession: %v", err)
		}
		return chunks
	}

	if got := countChunks(false); got != 1 {
		t.Fatalf("default chunk size produced %d chunks, want 1 (25000 tokens)", got)
	}
	if got := countChunks(true); got <= 1 {
		t.Fatalf("chunk size 100 tokens produced %d chunks, want more than 1: the configured value is ignored", got)
	}
}
