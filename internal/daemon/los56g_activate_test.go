package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// The catalog, execute_cap and activation all answer the same question: may this
// caller reach this cap row. Activation was the one that still compared project
// strings exactly, against a project the handler had already folded to the main
// checkout — so a cap saved from a worktree session was listed in the catalog and
// runnable, and refused here (YM-373, consequence review).
func TestLos56gActivateCapAcceptsAWorktreeBoundCap(t *testing.T) {
	h, _ := mustHandler(t)
	main := t.TempDir()
	wt := makeWorktreeDir(t, main, "los56g")
	seedCapFull(t, h, "los56g_activate", wt, "", "async ({ x }) => x", "", nil)

	resp := h.handleActivateCap(map[string]any{
		"name":      "los56g_activate",
		"thread_id": "thread-los56g",
		"project":   wt,
	})

	if resp.Error != "" {
		t.Fatalf("activate_cap refused a cap of the caller's own worktree: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "registerTool") {
		t.Fatalf("activation returned no tool code: %s", resp.Result)
	}
}

// The same refusal for a foreign project must survive. It no longer names that
// project's path: the caller has no access to it, and the text was a map of
// somebody else's disk (YM-427).
func TestLos56gActivateCapStillRefusesAForeignProject(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "los56g-own")
	otherMain := filepath.Join(t.TempDir(), "b", "los56g-own")
	seedCapFull(t, h, "los56g_foreign_activate", otherMain, "", "async ({ x }) => x", "", nil)

	resp := h.handleActivateCap(map[string]any{
		"name":      "los56g_foreign_activate",
		"thread_id": "thread-los56g",
		"project":   ownMain,
	})

	if !strings.Contains(resp.Error, "is scoped to another project") {
		t.Fatalf("a cap of another project was activated, got %q", resp.Error)
	}
	if strings.Contains(resp.Error, otherMain) {
		t.Errorf("the refusal names the foreign project's path: %q", resp.Error)
	}
}

// The caller's own row wins over a foreign row of the same name, whatever order
// the store returns them in.
func TestLos56gActivateCapPrefersTheCallersOwnRow(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "los56g-dupe")
	otherMain := filepath.Join(t.TempDir(), "b", "los56g-dupe")
	seedCapFull(t, h, "los56g_dupe", ownMain, "", "async ({ x }) => x", "", nil)
	seedCapFull(t, h, "los56g_dupe", otherMain, "", "async ({ x }) => x", "", nil)

	resp := h.handleActivateCap(map[string]any{
		"name":      "los56g_dupe",
		"thread_id": "thread-los56g",
		"project":   ownMain,
	})

	if resp.Error != "" {
		t.Fatalf("the caller's own row was not reachable: %s", resp.Error)
	}
}
