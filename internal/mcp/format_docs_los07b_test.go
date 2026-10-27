package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// YM-231: the daemon reports path_missing per source, but the MCP text output
// only forwarded the summary line, so an agent could not tell which source is
// dead. Rules and skill sources stay usable when their path is gone; the others
// are simply never synced again.
func TestFormatDocSources_MarksDeadAndRelativePaths(t *testing.T) {
	raw := json.RawMessage(`{"message":"3 documentation source(s) indexed.","sources":[
		{"name":"dead-docs","chunks":3,"project":"p","path":"/gone/docs","path_missing":true},
		{"name":"relative-docs","chunks":1,"project":"p","path":"docs","path_relative":true},
		{"name":"live-docs","chunks":7,"project":"p","path":"/live/docs"}
	]}`)

	out := formatDocSources(raw)
	line := func(name string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, name) {
				return l
			}
		}
		return ""
	}

	if got := line("dead-docs"); !strings.Contains(got, "path missing") {
		t.Errorf("dead source not marked: %q", got)
	}
	if got := line("relative-docs"); !strings.Contains(got, "relative") {
		t.Errorf("relative path not marked: %q", got)
	}
	if got := line("live-docs"); strings.Contains(got, "path missing") || strings.Contains(got, "relative") {
		t.Errorf("healthy source wrongly marked: %q", got)
	}
}
