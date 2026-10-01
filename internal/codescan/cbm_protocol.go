package codescan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseCBMResponse extracts the text payload from the MCP content wrapper
// returned by `cbm cli <tool> ... --json`: {"content":[{"type":"text","text":"..."}]}.
func parseCBMResponse(out []byte) (string, error) {
	var mcpResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(out, &mcpResp); err != nil {
		return "", fmt.Errorf("parse mcp response: %w", err)
	}
	if len(mcpResp.Content) == 0 {
		return "", fmt.Errorf("empty mcp response")
	}
	text := mcpResp.Content[0].Text
	if mcpResp.IsError {
		return "", fmt.Errorf("cbm tool error: %s", strings.TrimSpace(text))
	}
	return text, nil
}

// parseQueryTable parses v0.10.x query_graph table text:
//
//	rows: 3  (cols: name path)
//	  .PHONY Makefile
//	  build Makefile
//	total: 3
//
// Single-column values may be wrap-quoted ("120708"). Multi-column values are
// space-separated; the scanner's queries select paths, identifiers and counts,
// none of which contain spaces.
func parseQueryTable(text string) ([][]interface{}, error) {
	colCount := 0
	var rows [][]interface{}
	seenHeader := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "rows: "):
			seenHeader = true
			rest := strings.TrimPrefix(line, "rows: ")
			if i := strings.Index(rest, "cols: "); i >= 0 {
				spec := strings.TrimSuffix(strings.TrimPrefix(rest[i+len("cols: "):], ")"), ")")
				colCount = len(strings.Fields(spec))
			}
		case strings.HasPrefix(line, "  "):
			if colCount == 0 {
				return nil, fmt.Errorf("unexpected data line: %q", line)
			}
			value := strings.TrimSpace(line)
			row := make([]interface{}, colCount)
			if colCount == 1 {
				row[0] = strings.Trim(value, "\"")
			} else {
				fields := strings.Fields(value)
				for i := 0; i < colCount; i++ {
					if i < len(fields) {
						row[i] = fields[i]
					}
				}
			}
			rows = append(rows, row)
		}
	}
	if !seenHeader {
		return nil, fmt.Errorf("no query table in output: %q", strings.TrimSpace(text))
	}
	return rows, nil
}
