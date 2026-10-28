package mcp

import (
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ── YM-336 (Los 82): save_cap verliert die requires-Liste ──────────────────

// The daemon's save_cap reads a "requires" param (capfile adapter declarations,
// comma list or array) and stores it verbatim; only the MCP tool schema never
// declared it. A CAP.md imported through the file path keeps its requires list,
// an agent that saves the same cap through the tool lost it, and the stored
// metadata was then the only place the list had ever been.
func TestLos56fSaveCapToolDeclaresRequires(t *testing.T) {
	srv := &Server{}
	srv.srv = mcpserver.NewMCPServer("test", "0.0.0")
	srv.registerTools()

	for _, st := range srv.srv.ListTools() {
		if st.Tool.Name != "save_cap" {
			continue
		}
		prop, ok := st.Tool.InputSchema.Properties["requires"]
		if !ok {
			t.Fatalf("save_cap does not declare a requires property; properties = %v",
				los56fPropNames(st.Tool.InputSchema.Properties))
		}
		desc, _ := prop.(map[string]any)["description"].(string)
		if !strings.Contains(desc, "adapter") {
			t.Errorf("the requires description does not say what it holds: %q", desc)
		}
		return
	}
	t.Fatal("save_cap is not registered")
}

// los56fPropNames lists the property names of a tool schema for the failure
// message above.
func los56fPropNames(props map[string]any) []string {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	return names
}
