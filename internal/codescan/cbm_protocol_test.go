package codescan

import (
	"fmt"
	"strings"
	"testing"
)

// These fixtures are verbatim outputs captured from
// `codebase-memory-mcp cli <tool> ... --json` v0.10.8.

func TestParseQueryTable_MultiCol(t *testing.T) {
	text := "rows: 3  (cols: name path)\n  .PHONY Makefile\n  build Makefile\n  benchmark Makefile\ntotal: 3\n"
	rows, err := parseQueryTable(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	if rows[0][0] != ".PHONY" || rows[0][1] != "Makefile" {
		t.Errorf("unexpected first row: %v", rows[0])
	}
	if rows[2][0] != "benchmark" || rows[2][1] != "Makefile" {
		t.Errorf("unexpected last row: %v", rows[2])
	}
}

func TestParseQueryTable_SingleColQuoted(t *testing.T) {
	text := "rows: 1  (cols: COUNT(n))\n  \"120708\"\ntotal: 1\n"
	rows, err := parseQueryTable(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0] != "120708" {
		t.Errorf("expected 120708, got %v", rows[0][0])
	}
}

func TestParseQueryTable_ZeroRows(t *testing.T) {
	text := "rows: 0  (cols: n.name)\ntotal: 0\nhint: \"Query returned no results.\"\n"
	rows, err := parseQueryTable(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

func TestParseQueryTable_UnexpectedFormat(t *testing.T) {
	if _, err := parseQueryTable("expected token type 85, got 5 at pos 17"); err == nil {
		t.Fatal("expected error for non-table output")
	}
}

func TestParseCBMResponse(t *testing.T) {
	out := []byte(`{"content":[{"type":"text","text":"rows: 1  (cols: COUNT(n))\n  \"120708\"\ntotal: 1\n"}],"isError":false}`)
	text, err := parseCBMResponse(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "rows: 1  (cols: COUNT(n))\n  \"120708\"\ntotal: 1\n" {
		t.Errorf("unexpected inner text: %q", text)
	}
}

func TestParseCBMResponse_IsError(t *testing.T) {
	out := []byte(`{"content":[{"type":"text","text":"expected token type 85, got 5 at pos 17"}],"structuredContent":{"error":"expected token type 85, got 5 at pos 17"},"isError":true}`)
	if _, err := parseCBMResponse(out); err == nil {
		t.Fatal("expected error for isError envelope")
	}
}

func TestParseCBMResponse_Empty(t *testing.T) {
	if _, err := parseCBMResponse([]byte(`{"content":[],"isError":false}`)); err == nil {
		t.Fatal("expected error for empty content")
	}
}

// The error text of the CBM CLI is client data. It must reach the error
// message bounded and quoted: raw line breaks forge log lines, and an unbounded
// body floods the daemon log (YM-277 / DEV-DT45).
func TestParseCBMResponse_ErrorTextIsBoundedAndQuoted(t *testing.T) {
	forged := "forged: everything fine"
	payload := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":true}`,
		"expected token type 85\n"+forged+strings.Repeat("x", 4096))

	_, err := parseCBMResponse([]byte(payload))
	if err == nil {
		t.Fatal("expected error for isError envelope")
	}
	msg := err.Error()

	if strings.Contains(msg, "\n") {
		t.Fatalf("error message carries a raw line break: %q", msg)
	}
	if len(msg) > 400 {
		t.Fatalf("error message is unbounded (%d chars): %q", len(msg), msg[:80])
	}
	if !strings.Contains(msg, "expected token type 85") {
		t.Fatalf("the reason did not survive: %q", msg)
	}
}

