package proxy

import "testing"

func TestExtractStatsFromMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/app/main.go"}},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "content": "file contents..."},
			},
		},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": "/app/main.go"}},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "content": "edited"},
			},
		},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/app/config.go"}},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "content": "config"},
			},
		},
	}

	stats := extractStatsFromMessages(messages, 0, 6)

	// Tool stats: Read:2, Edit:1
	if stats.ToolStats == "" {
		t.Error("expected non-empty tool stats")
	}

	// File stats: main.go(2), config.go(1)
	if stats.FileStats == "" {
		t.Error("expected non-empty file stats")
	}
}
