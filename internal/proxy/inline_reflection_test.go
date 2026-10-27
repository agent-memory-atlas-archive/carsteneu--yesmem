package proxy

import (
	"testing"
)

func TestScanAssistantSignals_ThinkingWithIDs(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":     "thinking",
					"thinking": "I see [ID:123] mentions the deploy order and [ID:456] covers the retry logic.",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 2 {
		t.Fatalf("expected 2 UsedIDs, got %d: %v", len(got.UsedIDs), got.UsedIDs)
	}
	wantIDs := map[int64]bool{123: true, 456: true}
	for _, id := range got.UsedIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected ID %d", id)
		}
	}
}

func TestScanAssistantSignals_TextWithHTMLComments(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "Here's my answer.\n<!-- [IDs: 789, 101] -->",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 2 {
		t.Fatalf("expected 2 UsedIDs, got %d: %v", len(got.UsedIDs), got.UsedIDs)
	}
	wantIDs := map[int64]bool{789: true, 101: true}
	for _, id := range got.UsedIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected ID %d", id)
		}
	}
}

func TestScanAssistantSignals_GapDetection(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "Done.\n<!-- [gap: SQLite WAL mode] -->",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.GapTopics) != 1 {
		t.Fatalf("expected 1 GapTopic, got %d: %v", len(got.GapTopics), got.GapTopics)
	}
	if got.GapTopics[0] != "SQLite WAL mode" {
		t.Errorf("expected 'SQLite WAL mode', got %q", got.GapTopics[0])
	}
}

func TestScanAssistantSignals_ContradictionDetection(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "Done.\n<!-- [contradiction: 123 vs 456: deploy order wrong] -->",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.Contradictions) != 1 {
		t.Fatalf("expected 1 Contradiction, got %d: %v", len(got.Contradictions), got.Contradictions)
	}
	if got.Contradictions[0] != "123 vs 456: deploy order wrong" {
		t.Errorf("expected '123 vs 456: deploy order wrong', got %q", got.Contradictions[0])
	}
}

func TestScanAssistantSignals_Empty(t *testing.T) {
	// No messages at all.
	got := scanAssistantSignals([]any{})
	if len(got.UsedIDs) != 0 || len(got.GapTopics) != 0 || len(got.Contradictions) != 0 {
		t.Errorf("expected empty signals, got %+v", got)
	}

	// Only user messages — no assistant.
	messages := []any{
		map[string]any{
			"role":    "user",
			"content": "Hello",
		},
	}
	got = scanAssistantSignals(messages)
	if len(got.UsedIDs) != 0 || len(got.GapTopics) != 0 || len(got.Contradictions) != 0 {
		t.Errorf("expected empty signals for user-only messages, got %+v", got)
	}
}

func TestScanAssistantSignals_Dedup(t *testing.T) {
	// Same ID appears in both thinking and text blocks.
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":     "thinking",
					"thinking": "Referencing [ID:42] for context.",
				},
				map[string]any{
					"type": "text",
					"text": "Answer here.\n<!-- [IDs: 42] -->",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 1 {
		t.Fatalf("expected 1 UsedID after dedup, got %d: %v", len(got.UsedIDs), got.UsedIDs)
	}
	if got.UsedIDs[0] != 42 {
		t.Errorf("expected ID 42, got %d", got.UsedIDs[0])
	}
}

func TestScanAssistantSignals_StopsAtUserMessage(t *testing.T) {
	// Assistant message before the last user turn should NOT be scanned.
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "Old answer with [ID:999].",
				},
			},
		},
		map[string]any{
			"role":    "user",
			"content": "New question",
		},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "New answer.\n<!-- [IDs: 7] -->",
				},
			},
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 1 {
		t.Fatalf("expected 1 UsedID (only from last turn), got %d: %v", len(got.UsedIDs), got.UsedIDs)
	}
	if got.UsedIDs[0] != 7 {
		t.Errorf("expected ID 7, got %d", got.UsedIDs[0])
	}
	// ID 999 must not be present.
	for _, id := range got.UsedIDs {
		if id == 999 {
			t.Error("ID 999 from pre-user-turn assistant message should not be included")
		}
	}
}

// A real request always ends with the user message of the current turn. The
// signals must therefore come from the assistant response before it.
func TestScanAssistantSignals_RealisticTranscriptEndsWithUserMessage(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "first question"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Older answer with [ID:999]."},
			},
		},
		map[string]any{"role": "user", "content": "second question"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "I leaned on [ID:123] here."},
				map[string]any{"type": "text", "text": "Here is the answer.\n<!-- [IDs: 456] -->"},
			},
		},
		map[string]any{"role": "user", "content": "third question"},
	}

	got := scanAssistantSignals(messages)
	if !got.Scanned {
		t.Error("Scanned = false, want true — an assistant response was available")
	}
	wantIDs := map[int64]bool{123: true, 456: true}
	if len(got.UsedIDs) != 2 {
		t.Fatalf("UsedIDs = %v, want the two IDs of the previous assistant answer", got.UsedIDs)
	}
	for _, id := range got.UsedIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected ID %d", id)
		}
	}
}

// Tool loop: the previous turn's final text sits behind a tool_result message.
func TestScanAssistantSignals_ToolLoopTranscript(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "run the build"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{}},
			},
		},
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
			},
		},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Build is green.\n<!-- [IDs: 321] -->"},
			},
		},
		map[string]any{"role": "user", "content": "next thing"},
	}

	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 1 || got.UsedIDs[0] != 321 {
		t.Errorf("UsedIDs = %v, want [321] from the final answer of the previous turn", got.UsedIDs)
	}
}

// Without a previous assistant response there is nothing to scan — the caller
// must not treat every injected learning as noise in that case.
func TestScanAssistantSignals_NoAssistantResponseNotScanned(t *testing.T) {
	got := scanAssistantSignals([]any{
		map[string]any{"role": "user", "content": "kickoff"},
	})
	if got.Scanned {
		t.Error("Scanned = true without any assistant message")
	}
	if len(got.UsedIDs) != 0 {
		t.Errorf("UsedIDs = %v, want none", got.UsedIDs)
	}
}

// reflectionFixture is a request transcript as the proxy sees it: the user
// message of the current turn comes last, the signals sit in the assistant
// answer before it.
func reflectionFixture() []any {
	return []any{
		map[string]any{"role": "user", "content": "how do we count a use?"},
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Per [ID:77].\n<!-- [gap: cluster columns] -->\n<!-- [contradiction: 1 vs 2: stale doc] -->"},
			},
		},
		map[string]any{"role": "user", "content": "and now?"},
	}
}

// signals.enabled off must switch the inline reflection off entirely.
func TestScanInlineSignals_GateOff(t *testing.T) {
	s := &Server{cfg: Config{InlineReflectionEnabled: false}}
	got := s.scanInlineSignals(reflectionFixture())
	if got.Scanned || len(got.UsedIDs) != 0 || len(got.GapTopics) != 0 || len(got.Contradictions) != 0 {
		t.Errorf("gated off: signals = %+v, want none and Scanned=false", got)
	}
}

func TestScanInlineSignals_GateOn(t *testing.T) {
	s := &Server{cfg: Config{InlineReflectionEnabled: true}}
	got := s.scanInlineSignals(reflectionFixture())
	if !got.Scanned {
		t.Error("gated on: Scanned = false")
	}
	if len(got.UsedIDs) != 1 || got.UsedIDs[0] != 77 {
		t.Errorf("gated on: UsedIDs = %v, want [77]", got.UsedIDs)
	}
	if len(got.GapTopics) != 1 {
		t.Errorf("gated on: GapTopics = %v, want 1 entry", got.GapTopics)
	}
	if len(got.Contradictions) != 1 {
		t.Errorf("gated on: Contradictions = %v, want 1 entry", got.Contradictions)
	}
}

func TestScanAssistantSignals_PlainStringContent(t *testing.T) {
	// content is a plain string, not a []any block list.
	messages := []any{
		map[string]any{
			"role":    "assistant",
			"content": "Plain text with [ID:55] and <!-- [gap: WAL checkpointing] -->.",
		},
	}
	got := scanAssistantSignals(messages)
	if len(got.UsedIDs) != 1 || got.UsedIDs[0] != 55 {
		t.Errorf("expected UsedIDs=[55], got %v", got.UsedIDs)
	}
	if len(got.GapTopics) != 1 || got.GapTopics[0] != "WAL checkpointing" {
		t.Errorf("expected GapTopics=['WAL checkpointing'], got %v", got.GapTopics)
	}
}
