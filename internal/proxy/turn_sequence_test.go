package proxy

import (
	"testing"
)

func TestExtractToolTypes_Basic(t *testing.T) {
	msg := map[string]interface{}{
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "hello"},
			map[string]interface{}{"type": "tool_use", "name": "Bash", "id": "t1"},
			map[string]interface{}{"type": "tool_use", "name": "Read", "id": "t2"},
			map[string]interface{}{"type": "tool_use", "name": "Bash", "id": "t3"},
		},
	}
	tools := ExtractToolTypes(msg)
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d: %v", len(tools), tools)
	}
	if tools[0] != "Bash" || tools[1] != "Read" || tools[2] != "Bash" {
		t.Errorf("got %v", tools)
	}
}

func TestExtractToolTypes_NoTools(t *testing.T) {
	msg := map[string]interface{}{
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "just text"},
		},
	}
	tools := ExtractToolTypes(msg)
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %v", tools)
	}
}

func TestExtractToolTypes_UserMessage(t *testing.T) {
	msg := map[string]interface{}{
		"role":    "user",
		"content": "hello",
	}
	tools := ExtractToolTypes(msg)
	if len(tools) != 0 {
		t.Errorf("expected 0 for user msg, got %v", tools)
	}
}

func TestComputeTurnHash_DeduplicatesConsecutive(t *testing.T) {
	h1 := ComputeTurnHash([]string{"Bash", "Bash", "Bash", "Read", "Bash"})
	h2 := ComputeTurnHash([]string{"Bash", "Read", "Bash"})
	if h1 != h2 {
		t.Errorf("consecutive dedup failed: %q != %q", h1, h2)
	}
}

func TestComputeTurnHash_DifferentSequencesDifferentHash(t *testing.T) {
	h1 := ComputeTurnHash([]string{"Bash", "Read", "Write"})
	h2 := ComputeTurnHash([]string{"Read", "Bash", "Write"})
	if h1 == h2 {
		t.Error("different sequences should produce different hashes")
	}
}

func TestComputeTurnHash_Empty(t *testing.T) {
	h := ComputeTurnHash(nil)
	if h != "" {
		t.Errorf("expected empty for nil, got %q", h)
	}
	h = ComputeTurnHash([]string{})
	if h != "" {
		t.Errorf("expected empty for empty slice, got %q", h)
	}
}

func TestComputeTurnHash_Length(t *testing.T) {
	h := ComputeTurnHash([]string{"Bash", "Read"})
	if len(h) != 16 {
		t.Errorf("expected 16-char hash, got %d: %q", len(h), h)
	}
}
