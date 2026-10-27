package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-181/YM-197 (Los 56i): SyncCapsFromDisk importiert jede CAP.md aus dem
// Cap-Verzeichnis. Ein ausgemustertes Cap, dessen Datei auf einer
// Bestandsinstallation liegen bleibt, kaeme damit bei jedem Daemon-Start als
// aktives Cap-Learning zurueck und stuende wieder im Katalog.
func TestLos56i_SyncCapsFromDiskSkipsARetiredCap(t *testing.T) {
	h, store := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "wiki_export", 1, "echo retired\n")
	los32bWriteDiskCapFile(t, dir, "still_shipped", 1, "echo live\n")

	SyncCapsFromDisk(h, dir, "")

	if meta := storedCapMeta(t, store, "wiki_export"); meta != nil {
		t.Errorf("the retired cap %q was imported as an active cap learning", meta.Name)
	}
	if meta := storedCapMeta(t, store, "still_shipped"); meta == nil {
		t.Error("a cap that is still shipped was not imported")
	}
}

// Das Gegenstueck fuer den Export: eine Zeile, die aus einer frueheren Version
// noch aktiv ist, darf die ausgemusterte CAP.md nicht wieder auf die Platte
// schreiben.
func TestLos56i_ExportAllCapsSkipsARetiredCap(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	// Both rows are seeded directly: save_cap refuses a retired name (that is the
	// rule this pair of tests guards), but a row stored by an older version is
	// exactly the state an existing installation is in.
	for _, name := range []string{"wiki_export", "still_shipped"} {
		seedCapMeta(t, s, CapMeta{
			Name: name, Description: "seeded", Version: 2,
			Scripts: []ScriptMeta{{
				Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
				Body: "echo hi", Schema: `{"type":"object"}`,
			}},
		}, "")
	}

	ExportAllCaps(h, dir)

	if _, err := os.Stat(filepath.Join(dir, "wiki_export", "CAP.md")); err == nil {
		t.Error("ExportAllCaps wrote the retired cap back to disk")
	}
	if _, err := os.Stat(filepath.Join(dir, "still_shipped", "CAP.md")); err != nil {
		t.Errorf("a live cap was not exported: %v", err)
	}
}

// Cold-Review-Nachpruefung (Los 56i): die Ausmusterung als Einmal-Migration
// reicht nicht. Ein Install, der nur ein neues Binary bekommt (make deploy,
// ohne "yesmem migrate"), behaelt die aktive Zeile, und damit stuende das Cap
// weiter im Katalog und waere ausfuehrbar. Die Leser filtern deshalb selbst.
func TestLos56i_ReadersHideARetiredCapEvenWithoutARetirementRun(t *testing.T) {
	h, s := mustHandler(t)
	seedCapMeta(t, s, CapMeta{
		Name: "wiki_export", Description: "still active from an older version", Version: 2,
		Scripts: []ScriptMeta{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "echo hi", Schema: `{"type":"object"}`,
		}},
	}, "")

	catalog := h.handleGetCaps(map[string]any{})
	if strings.Contains(string(catalog.Result), "wiki_export") {
		t.Errorf("get_caps still offers the retired cap: %s", catalog.Result)
	}

	if resp := h.handleActivateCap(map[string]any{"name": "wiki_export", "thread_id": "th_los56i"}); resp.Error == "" {
		t.Error("activate_cap still activates the retired cap")
	}

	exec := h.handleExecuteCap(map[string]any{"name": "wiki_export", "args": "{}"})
	if exec.Error == "" {
		t.Errorf("execute_cap still runs the retired cap: %s", exec.Result)
	}
}

// Consequence-Nachpruefung (Los 56i): register_caps inlined den Script-Body als
// registerTool()-Code. Ein ausgemustertes Cap waere damit ausfuehrbar, obwohl
// der Katalog es verschweigt.
func TestLos56i_RegisterCapsSkipsARetiredCap(t *testing.T) {
	h, s := mustHandler(t)
	seedCapMeta(t, s, CapMeta{
		Name: "wiki_export", Description: "retired but still stored", Version: 2,
		Scripts: []ScriptMeta{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "echo retired", Schema: `{"type":"object"}`,
		}},
	}, "")

	resp := h.handleRegisterCaps(map[string]any{})
	if resp.Error != "" {
		t.Fatalf("register_caps: %s", resp.Error)
	}
	// The cap name is not part of the generated code; the script body is.
	if strings.Contains(string(resp.Result), "echo retired") {
		t.Errorf("register_caps still hands out the retired cap's tool code: %s", resp.Result)
	}
}
