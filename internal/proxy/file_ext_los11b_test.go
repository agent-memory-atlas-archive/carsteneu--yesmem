package proxy

import "testing"

// YM-252 (PRX-P29): fileToolNames knew only the Claude Code spelling
// (PascalCase name + file_path), so for opencode (read/edit/write with "path")
// the extension detection behind trigger_extensions returned nothing. The
// function's only caller (findDocsByFileExtensions) is currently disabled, so
// this is correctness of the helper itself, not a live behaviour change.
func TestExtractFileExtensions_Los11b_OpencodeDialect(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_los11b",
				"name":  "read",
				"input": map[string]any{"path": "/src/app.py"},
			},
		}},
	}

	got := extractFileExtensionsFromMessages(msgs, []string{".py"})
	if len(got) != 1 || got[0] != ".py" {
		t.Fatalf("opencode read + path = %v, want [.py]", got)
	}
}

// The write side of the same dialect: opencode's edit carries "path".
func TestExtractFileExtensions_Los11b_OpencodeEditDialect(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_los11b",
				"name":  "edit",
				"input": map[string]any{"path": "/src/main.rs", "oldString": "a", "newString": "b"},
			},
		}},
	}

	got := extractFileExtensionsFromMessages(msgs, []string{".rs"})
	if len(got) != 1 || got[0] != ".rs" {
		t.Fatalf("opencode edit + path = %v, want [.rs]", got)
	}
}

// A transport that spells the key camelCase must resolve as well.
func TestExtractFileExtensions_Los11b_FilePathCamelCase(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_los11b",
				"name":  "Write",
				"input": map[string]any{"filePath": "/src/notes.md"},
			},
		}},
	}

	got := extractFileExtensionsFromMessages(msgs, []string{".md"})
	if len(got) != 1 || got[0] != ".md" {
		t.Fatalf("Write + filePath = %v, want [.md]", got)
	}
}

// A tool that carries no file at all must not resolve a path by accident.
func TestExtractFileExtensions_Los11b_ShellHasNoPath(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_los11b",
				"name":  "shell",
				"input": map[string]any{"command": "go test ./...", "path": "cleanup.py"},
			},
		}},
	}

	if got := extractFileExtensionsFromMessages(msgs, []string{".py"}); len(got) != 0 {
		t.Fatalf("shell resolved an extension from a non-file tool: %v", got)
	}
}
