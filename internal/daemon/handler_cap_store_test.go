package daemon

import (
	"encoding/json"
	"strings"
	"testing"

)

func TestHandleCapStore_CreateTable(t *testing.T) {
	h := mustHandlerWithCapStore(t)
	resp := h.handleCapStore(map[string]any{
		"capability": "reddit",
		"action":     "create_table",
		"table":      "posts",
		"columns": []any{
			map[string]any{"name": "title", "type": "TEXT"},
			map[string]any{"name": "score", "type": "INTEGER"},
		},
	})
	if resp.Error != "" {
		t.Fatalf("create_table error: %s", resp.Error)
	}
	var result map[string]any
	json.Unmarshal(resp.Result, &result)
	if result["status"] != "created" {
		t.Errorf("expected status=created, got %v", result["status"])
	}
}

func TestHandleCapStore_FullRoundTrip(t *testing.T) {
	h := mustHandlerWithCapStore(t)

	// 1. Create table
	resp := h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "create_table",
		"table":      "results",
		"columns":    []any{map[string]any{"name": "url", "type": "TEXT"}, map[string]any{"name": "status", "type": "INTEGER"}},
	})
	if resp.Error != "" {
		t.Fatalf("create: %s", resp.Error)
	}

	// 2. Upsert rows
	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "upsert",
		"table":      "results",
		"data":       map[string]any{"url": "https://example.com", "status": 200},
	})
	if resp.Error != "" {
		t.Fatalf("upsert: %s", resp.Error)
	}
	var upsertResult map[string]any
	json.Unmarshal(resp.Result, &upsertResult)
	if upsertResult["id"].(float64) <= 0 {
		t.Error("expected positive ID from upsert")
	}

	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "upsert",
		"table":      "results",
		"data":       map[string]any{"url": "https://fail.com", "status": 500},
	})
	if resp.Error != "" {
		t.Fatalf("upsert2: %s", resp.Error)
	}

	// 3. Query all
	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "query",
		"table":      "results",
	})
	if resp.Error != "" {
		t.Fatalf("query: %s", resp.Error)
	}
	var queryResult map[string]any
	json.Unmarshal(resp.Result, &queryResult)
	if queryResult["count"].(float64) != 2 {
		t.Errorf("expected 2 rows, got %v", queryResult["count"])
	}

	// 4. Query with WHERE
	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "query",
		"table":      "results",
		"where":      "status = ?",
		"args":       []any{500},
	})
	var filterResult map[string]any
	json.Unmarshal(resp.Result, &filterResult)
	if filterResult["count"].(float64) != 1 {
		t.Errorf("expected 1 row with status=500, got %v", filterResult["count"])
	}

	// 5. Delete
	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "delete",
		"table":      "results",
		"where":      "status = ?",
		"args":       []any{500},
	})
	if resp.Error != "" {
		t.Fatalf("delete: %s", resp.Error)
	}
	var delResult map[string]any
	json.Unmarshal(resp.Result, &delResult)
	if delResult["affected"].(float64) != 1 {
		t.Errorf("expected 1 deleted, got %v", delResult["affected"])
	}

	// 6. List tables
	resp = h.handleCapStore(map[string]any{
		"capability": "scanner",
		"action":     "list_tables",
	})
	var listResult map[string]any
	json.Unmarshal(resp.Result, &listResult)
	if listResult["count"].(float64) != 1 {
		t.Errorf("expected 1 table, got %v", listResult["count"])
	}
}

func TestHandleCapStore_ValidationErrors(t *testing.T) {
	h := mustHandlerWithCapStore(t)

	tests := []struct {
		name   string
		params map[string]any
	}{
		{"missing capability", map[string]any{"action": "create_table"}},
		{"missing action", map[string]any{"capability": "test"}},
		{"bad action", map[string]any{"capability": "test", "action": "drop_database"}},
		{"create without table", map[string]any{"capability": "test", "action": "create_table"}},
		{"create without columns", map[string]any{"capability": "test", "action": "create_table", "table": "t"}},
		{"upsert without data", map[string]any{"capability": "test", "action": "upsert", "table": "t"}},
		{"delete without where", map[string]any{"capability": "test", "action": "delete", "table": "t"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handleCapStore(tt.params)
			if resp.Error == "" {
				t.Errorf("%s: expected error, got success", tt.name)
			}
		})
	}
}

func TestHandleCapStore_Dispatch(t *testing.T) {
	h := mustHandlerWithCapStore(t)
	resp := h.Handle(Request{Method: "cap_store", Params: map[string]any{
		"capability": "test",
		"action":     "list_tables",
	}})
	if resp.Error != "" {
		t.Fatalf("dispatch error: %s", resp.Error)
	}
}

func TestHandleCapStore_MalformedArgsJSON(t *testing.T) {
	h := mustHandlerWithCapStore(t)
	if resp := h.handleCapStore(map[string]any{
		"capability": "test", "action": "create_table", "table": "items",
		"columns": []any{map[string]any{"name": "name", "type": "TEXT"}},
	}); resp.Error != "" {
		t.Fatalf("create: %s", resp.Error)
	}

	cases := []struct {
		action string
		args   string
	}{
		{"query", "not-json"},
		{"query", "{broken"},
		{"delete", "not-json"},
		{"delete", "{broken"},
	}
	for _, tc := range cases {
		t.Run(tc.action+"_"+tc.args, func(t *testing.T) {
			resp := h.handleCapStore(map[string]any{
				"capability": "test", "action": tc.action, "table": "items",
				"where": "name = ?", "args": tc.args,
			})
			if resp.Error == "" {
				t.Fatalf("expected error for malformed args %q, got success", tc.args)
			}
			// Must surface JSON parse failure, not downstream SQL bind error.
			if !strings.Contains(strings.ToLower(resp.Error), "args") {
				t.Errorf("expected error to mention 'args', got: %s", resp.Error)
			}
		})
	}
}
