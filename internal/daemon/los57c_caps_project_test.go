package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

const los57cReplScript = `[{"name":"run","kind":"tool","runtime":"repl","body":"async () => 1"}]`

// YM-402 (OPS-98): a save bound to one project must not retire the global cap of
// the same name. The supersede lookup ran through GetActiveLearnings("cap",
// project, ...), whose SQL filter admits rows whose canonical_project is empty —
// the shape of every global cap written before save_cap filled that column (live
// rows: rule_guard, test_llm). A project taking over such a name retired the cap
// for all other projects, silently, because the supersede path reports success.
func TestLos57cProjectSaveDoesNotRetireGlobalCap(t *testing.T) {
	h, s := mustHandler(t)
	proj := t.TempDir()

	// Legacy global row: written before canonical_project was populated.
	seedCap(t, h, "los57c_legacy_global", "", "echo global", nil)
	// Current global row: save_cap writes canonical_project "." for it.
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_saved_global", "description": "global", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}

	for _, name := range []string{"los57c_legacy_global", "los57c_saved_global"} {
		if resp := h.handleSaveCap(map[string]any{
			"name": name, "description": "project copy", "project": proj, "scripts": los57cReplScript,
		}); resp.Error != "" {
			t.Fatalf("save project cap %s: %s", name, resp.Error)
		}
	}

	active, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	bound := map[string]map[string]bool{}
	for _, l := range active {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		if bound[meta.Name] == nil {
			bound[meta.Name] = map[string]bool{}
		}
		bound[meta.Name][l.Project] = true
	}
	for _, name := range []string{"los57c_legacy_global", "los57c_saved_global"} {
		if !bound[name][""] {
			t.Errorf("%s: the global cap was retired by a project-bound save", name)
		}
		if !bound[name][proj] {
			t.Errorf("%s: the project's own cap was not stored alongside it", name)
		}
	}
}

// YM-403 (OPS-99): get_caps and register_caps answer "which caps exist here" —
// the question the capability catalog answers as well. Both filtered through
// GetActiveLearnings("cap", project, ...), whose SQL compares canonical_project,
// while save_cap stores "." for a global cap (models.CanonicalProject("")). Every
// global cap was therefore missing from their answer while the catalog showed it:
// two answers to one question. Live: 18 of 20 global cap rows carry ".".
func TestLos57cGetCapsFindsGlobalCapsForAProjectCaller(t *testing.T) {
	h, _ := mustHandler(t)
	proj := t.TempDir()
	const globalScript = `[{"name":"los57c_global_run","kind":"tool","runtime":"repl","body":"async () => 1"}]`

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_global_tool", "description": "global tool", "scripts": globalScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_own_tool", "description": "own tool", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save project cap: %s", resp.Error)
	}

	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": proj}))
	if !hasCap(caps, "los57c_global_tool") {
		t.Errorf("get_caps with a project hid the global cap, got %v", capNames(caps))
	}
	if !hasCap(caps, "los57c_own_tool") {
		t.Errorf("get_caps with a project hid the project's own cap, got %v", capNames(caps))
	}

	// register_caps answers the same question and must agree.
	reg := h.handleRegisterCaps(map[string]any{"project": proj})
	if reg.Error != "" {
		t.Fatalf("register_caps: %s", reg.Error)
	}
	if !strings.Contains(string(reg.Result), "los57c_global_run") {
		t.Errorf("register_caps did not hand out the global cap for a project caller: %s", reg.Result)
	}
}

// A project that takes over the name of a global cap shadows it (loadCapMeta
// picks the caller's own row first). Now that the global row survives such a
// save, the catalog-shaped answers must not list the same name twice.
func TestLos57cGetCapsListsShadowedNameOnce(t *testing.T) {
	h, _ := mustHandler(t)
	proj := t.TempDir()

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_shadowed", "description": "global", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_shadowed", "description": "project", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save project cap: %s", resp.Error)
	}

	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": proj}))
	seen := 0
	for _, c := range caps {
		if c.Meta.Name == "los57c_shadowed" {
			seen++
			if c.Project != proj {
				t.Errorf("the shadowed entry came from project %q, want the caller's own row", c.Project)
			}
		}
	}
	if seen != 1 {
		t.Errorf("get_caps listed %q %d times, want exactly the caller's own row", "los57c_shadowed", seen)
	}
}

// YM-404 (OPS-100): a row stored under a RELATIVE project names no repository, so
// the repository-root half of capProjectMatches cannot run, and the predicate
// fell back to the basename: that row served every checkout of that name. Pinned
// on both paths that ask it — reachability (loadCapMeta, execute_cap) and the
// catalog (get_caps) — against a repository that carries exactly that basename.
func TestLos57cRelativeRowProjectServesNoRepository(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "yesmem")
	if err := os.MkdirAll(filepath.Join(ownMain, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	seedCap(t, h, "los57c_relative_row", "yesmem/.worktrees/w1", "echo hi", nil)

	if _, _, err := h.loadCapMeta("los57c_relative_row", ownMain); err == nil {
		t.Errorf("a row stored under the relative project %q was reachable from %s",
			"yesmem/.worktrees/w1", ownMain)
	}
	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": ownMain}))
	if hasCap(caps, "los57c_relative_row") {
		t.Errorf("get_caps offered a relative-project row to %s, got %v", ownMain, capNames(caps))
	}

	// Positive control, so the test cannot pass by making every project match fail:
	// the same cap bound to an absolute worktree path of this repository stays
	// reachable.
	wt := makeWorktreeDir(t, ownMain, "los57c")
	seedCap(t, h, "los57c_absolute_row", wt, "echo hi", nil)
	if _, _, err := h.loadCapMeta("los57c_absolute_row", ownMain); err != nil {
		t.Errorf("a cap bound to this repository's worktree was not reachable from its main checkout: %v", err)
	}
}

// Review finding on YM-402: scope=user is the documented way OUT of a project
// binding. Now that a project row and the global row of the same name both stay
// alive, the save has to convert the project row. Retiring the global row instead
// leaves the project row in place, still shadowing the cap the caller just
// converted, and the save reports success.
func TestLos57cScopeUserConvertsTheProjectRowNotTheGlobalOne(t *testing.T) {
	h, s := mustHandler(t)
	proj := t.TempDir()

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_convert", "description": "project", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save project cap: %s", resp.Error)
	}
	// The global row is the newer one, so list order alone would pick it.
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_convert", "description": "global", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_convert", "description": "converted", "project": proj, "scope": "user",
	}); resp.Error != "" {
		t.Fatalf("scope=user must stay the way out: %s", resp.Error)
	}

	active, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	for _, l := range active {
		meta, err := ParseCapMeta(l.Context)
		if err != nil || meta.Name != "los57c_convert" {
			continue
		}
		if l.Project == proj {
			t.Errorf("the project row survived the conversion (description %q) and still shadows the cap", meta.Description)
		}
	}

	// What the caller sees after the conversion is its converted cap, and it runs
	// the converted body.
	if _, _, err := h.loadCapMeta("los57c_convert", proj); err != nil {
		t.Fatalf("the converted cap is not usable in the project it was converted from: %v", err)
	}
	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": proj}))
	for _, c := range caps {
		if c.Meta.Name == "los57c_convert" && c.Meta.Description != "converted" {
			t.Errorf("get_caps described the converted cap as %q", c.Meta.Description)
		}
	}
}

// Review finding on YM-402: the row that runs is the caller's own (loadCapMeta
// resolves own-first), so the row that is described in the catalog and the row
// whose code activate_cap hands to the REPL must be the same one. The list is
// ordered newest-first, and a project copy is normally NOT the newer row after a
// bundled cap was re-imported, so order alone decides wrong.
func TestLos57cOwnRowWinsOverANewerGlobalRow(t *testing.T) {
	h, _ := mustHandler(t)
	proj := t.TempDir()

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_order", "description": "OWN", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save own cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_order", "description": "GLOBAL", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}

	act := h.handleActivateCap(map[string]any{"name": "los57c_order", "thread_id": "t", "project": proj})
	if act.Error != "" {
		t.Fatalf("activate_cap: %s", act.Error)
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(act.Result, &payload); err != nil {
		t.Fatalf("unmarshal activate_cap: %v", err)
	}
	if !strings.Contains(payload.Code, `"OWN"`) {
		t.Errorf("activate_cap registered the global row instead of the caller's own: %s", payload.Code)
	}

	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": proj}))
	if !hasCap(caps, "los57c_order") {
		t.Fatalf("the activated cap is missing from the catalog, got %v", capNames(caps))
	}
	for _, c := range caps {
		if c.Meta.Name == "los57c_order" && c.Meta.Description != "OWN" {
			t.Errorf("the catalog described the activated cap as %q while execute_cap runs the caller's own row", c.Meta.Description)
		}
	}
}

// Review finding on YM-403: the shadowing rule must not swallow a global row that
// answers the query when the shadowing row does not. A tag filter is part of the
// question.
func TestLos57cRegisterCapsTagReachesTheGlobalRow(t *testing.T) {
	h, _ := mustHandler(t)
	proj := t.TempDir()
	const globalScript = `[{"name":"los57c_tagged_run","kind":"tool","runtime":"repl","body":"async () => 1"}]`

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_tagged", "description": "global", "scripts": globalScript, "tags": "web",
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_tagged", "description": "own", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save own cap: %s", resp.Error)
	}

	reg := h.handleRegisterCaps(map[string]any{"project": proj, "tag": "web"})
	if reg.Error != "" {
		t.Fatalf("register_caps: %s", reg.Error)
	}
	if !strings.Contains(string(reg.Result), "los57c_tagged_run") {
		t.Errorf("the untagged shadowing row hid the tagged global row: %s", reg.Result)
	}
}

// Review finding on YM-403: a caller that names only a basename is not tied to a
// repository, so it reaches the global caps and no bound row — the same
// fail-closed rule execute_cap applies (YM-359). Pinned so the narrowing is not
// silent.
func TestLos57cGetCapsShortNameReachesOnlyGlobalCaps(t *testing.T) {
	h, _ := mustHandler(t)
	proj := t.TempDir()

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_short_global", "description": "global", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_short_own", "description": "own", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save own cap: %s", resp.Error)
	}

	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": filepath.Base(proj)}))
	if !hasCap(caps, "los57c_short_global") {
		t.Errorf("a short-name caller lost the global cap, got %v", capNames(caps))
	}
	if hasCap(caps, "los57c_short_own") {
		t.Errorf("a short-name caller reached a path-bound cap, got %v", capNames(caps))
	}
}

// Review finding (round 2) on YM-403: the catalog must resolve a name to the same
// row execute_cap runs. A row of ANOTHER project is no candidate for the caller,
// and the list order (newest-first) must not let it win: the global row is the
// answer when the caller has no row of its own.
func TestLos57cCatalogPrefersGlobalOverAForeignRow(t *testing.T) {
	h, _ := mustHandler(t)
	ours, foreign := t.TempDir(), t.TempDir()

	seedCapAt(t, h, "los57c_foreign", "", "GLOBAL", "2026-01-01T00:00:00+00:00")
	seedCapAt(t, h, "los57c_foreign", foreign, "FOREIGN", "2026-02-01T00:00:00+00:00")

	if meta, _, err := h.loadCapMeta("los57c_foreign", ours); err != nil || meta.Description != "GLOBAL" {
		t.Fatalf("loadCapMeta = %q, %v; the setup should run the global row", meta.Description, err)
	}
	act := h.handleActivateCap(map[string]any{"name": "los57c_foreign", "thread_id": "t", "project": ours})
	if act.Error != "" {
		t.Fatalf("activate_cap: %s", act.Error)
	}
	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": ours}))
	for _, c := range caps {
		if c.Meta.Name == "los57c_foreign" && c.Meta.Description != "GLOBAL" {
			t.Errorf("the catalog described the cap as %q while execute_cap runs the global row", c.Meta.Description)
		}
	}
}

// Review finding (round 2) on YM-402: a scope=user save that names no project is
// the documented way out of a binding, but it names no row either. With two
// projects carrying the name, converting one of them is an arbitrary, silent
// loss.
func TestLos57cProjectLessScopeUserLeavesAmbiguousRowsAlone(t *testing.T) {
	h, s := mustHandler(t)
	projA, projB := t.TempDir(), t.TempDir()

	for _, proj := range []string{projA, projB} {
		if resp := h.handleSaveCap(map[string]any{
			"name": "los57c_ambiguous", "description": "copy", "project": proj, "scripts": los57cReplScript,
		}); resp.Error != "" {
			t.Fatalf("save cap for %s: %s", proj, resp.Error)
		}
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_ambiguous", "description": "global", "scope": "user", "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}

	active, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	survived := map[string]bool{}
	for _, l := range active {
		if meta, err := ParseCapMeta(l.Context); err == nil && meta.Name == "los57c_ambiguous" {
			survived[l.Project] = true
		}
	}
	if !survived[projA] || !survived[projB] {
		t.Errorf("a project-bound copy was retired by a save that named no project: %v", survived)
	}
}

// Review finding (round 2) on YM-404: loadCapMeta refuses a relative row, and the
// catalog must not offer it either. The basename fallback in capProjectVisible
// handed that row to every checkout of the name.
func TestLos57cCatalogHidesRelativeRows(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "yesmem")
	if err := os.MkdirAll(filepath.Join(ownMain, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	seedCapAutoActive(t, h, "los57c_relative_auto", "yesmem/.worktrees/w1")

	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": ownMain}))
	if hasCap(caps, "los57c_relative_auto") {
		t.Errorf("the catalog offered a relative-project row to %s, got %v", ownMain, capNames(caps))
	}
}

// Review finding (round 2) on YM-403: without a project the question is the plain
// inventory, so the collapse must not swallow the row the caller would actually
// run. Before the fix the newest row won by list order, which could be a foreign
// project's copy.
func TestLos57cInventoryListsEveryRowOfAName(t *testing.T) {
	h, _ := mustHandler(t)
	own := t.TempDir()

	seedCapAt(t, h, "los57c_inventory", "", "GLOBAL", "2026-01-01T00:00:00+00:00")
	seedCapAt(t, h, "los57c_inventory", own, "OWN", "2026-02-01T00:00:00+00:00")

	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"name": "los57c_inventory"}))
	if len(caps) != 2 {
		t.Fatalf("the inventory listed %d rows of the name, want both: %v", len(caps), capNames(caps))
	}
	descriptions := map[string]bool{}
	for _, c := range caps {
		descriptions[c.Meta.Description] = true
	}
	if !descriptions["GLOBAL"] || !descriptions["OWN"] {
		t.Errorf("the inventory lost a row, got %v", descriptions)
	}
}

// Consequence finding on YM-402: every bundled CAP.md declares scope: user, and
// SyncCapsFromDisk imports such a file with its scripts and without a project. That
// is a re-import, not a conversion: it must not retire a project's copy of the
// name, or a routine cap update silently takes one project's cap away.
func TestLos57cScopeUserImportKeepsTheProjectCopy(t *testing.T) {
	h, s := mustHandler(t)
	proj := t.TempDir()

	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_import", "description": "project copy", "project": proj, "scripts": los57cReplScript,
	}); resp.Error != "" {
		t.Fatalf("save project cap: %s", resp.Error)
	}
	// The shape CapFileToParams produces for a CAP.md with scope: user.
	if resp := h.handleSaveCap(map[string]any{
		"name": "los57c_import", "description": "bundled v2", "scope": "user",
		"scripts": los57cReplScript, "version": float64(2),
	}); resp.Error != "" {
		t.Fatalf("import global cap: %s", resp.Error)
	}

	active, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	seen := map[string]string{}
	for _, l := range active {
		if meta, err := ParseCapMeta(l.Context); err == nil && meta.Name == "los57c_import" {
			seen[l.Project] = meta.Description
		}
	}
	if seen[proj] != "project copy" {
		t.Errorf("the project's copy was retired by a scope=user import: %v", seen)
	}
	if seen[""] != "bundled v2" {
		t.Errorf("the imported global row is missing: %v", seen)
	}
}

// Security finding on YM-403: the catalog folds a caller that names a
// subdirectory of a checkout to the checkout root. Reachability must answer with
// the same folding, otherwise the catalog offers a cap that execute_cap refuses.
func TestLos57cSubdirectoryCallerIsFoldedOnBothPaths(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "yesmem")
	sub := filepath.Join(ownMain, "internal", "daemon")
	if err := os.MkdirAll(filepath.Join(ownMain, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	seedCap(t, h, "los57c_folded", ownMain, "echo hi", nil)

	if _, _, err := h.loadCapMeta("los57c_folded", sub); err != nil {
		t.Errorf("execute_cap refused a cap of the caller's own checkout: %v", err)
	}
	caps := unmarshalCaps(t, h.handleGetCaps(map[string]any{"project": sub}))
	if !hasCap(caps, "los57c_folded") {
		t.Errorf("get_caps hid a cap of the caller's own checkout, got %v", capNames(caps))
	}
}

// seedCapAt writes a cap row with an explicit created_at, so a test can pin the// newest-first order GetActiveLearnings returns. The rows carry no scripts: they
// answer only the description and the visibility question.
func seedCapAt(t *testing.T, h *Handler, name, project, description, createdAt string) int64 {
	t.Helper()
	meta := CapMeta{Name: name, Description: description, Version: 1, Tested: true,
		Scripts: []ScriptMeta{{Name: "run", Kind: "tool", Runtime: "repl", Body: "async () => 1", Schema: "{}"}}}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", createdAt, err)
	}
	id, err := h.store.InsertLearning(&models.Learning{
		Content:          name + " — " + description,
		Category:         "cap",
		Source:           "user_stated",
		Project:          project,
		CanonicalProject: canonicalProjectFor(project),
		Context:          ctx,
		TriggerRule:      "cap:" + name,
		CreatedAt:        created,
	})
	if err != nil {
		t.Fatalf("insert cap: %v", err)
	}
	return id
}
