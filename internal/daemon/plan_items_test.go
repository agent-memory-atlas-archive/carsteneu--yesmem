package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// itemList is a shorthand for the wire shape an MCP client sends: an array of
// objects. Values arrive as float64 for numbers through JSON.
func itemList(t *testing.T, raw string) []any {
	t.Helper()
	var items []any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return items
}

func planFromStore(t *testing.T, threadID string) *Plan {
	t.Helper()
	planStore.RLock()
	defer planStore.RUnlock()
	p := planStore.plans[threadID]
	if p == nil {
		t.Fatalf("no plan for %q", threadID)
	}
	return p
}

func TestSetPlanWithItemsDerivesContent(t *testing.T) {
	h, store := mustHandler(t)
	defer cleanupPlans()

	resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-derive",
		"items": itemList(t, `[
			{"content":"read the design doc","status":"completed"},
			{"content":"wire the ledger","status":"in_progress","reason":"doc risk 2","depth":1}
		]`),
	})
	if resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-derive")
	if len(p.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(p.Items))
	}
	if p.Items[0].Status != "completed" || p.Items[1].Depth != 1 || p.Items[1].Reason != "doc risk 2" {
		t.Errorf("items not stored verbatim: %+v", p.Items)
	}
	if !strings.Contains(p.Content, "[x] read the design doc") {
		t.Errorf("derived content missing completed marker: %q", p.Content)
	}
	if !strings.Contains(p.Content, "\n  - [>] wire the ledger (doc risk 2)") {
		t.Errorf("derived content missing nested in-progress line: %q", p.Content)
	}

	row, err := store.GetPlan("t-items-derive")
	if err != nil || row == nil {
		t.Fatalf("plan not persisted: %v", err)
	}
	if !strings.Contains(row.Items, "wire the ledger") {
		t.Errorf("items not persisted: %q", row.Items)
	}
}

// Both together: the outline drives the display, the supplied full text stays
// exactly as written (overwriting caller input would be worse than a snapshot
// that lags).
func TestSetPlanItemsWithTextKeepsText(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-text",
		"plan":      "Hand-written plan text.",
		"items":     itemList(t, `[{"content":"a"}]`),
	})
	if resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-text")
	if p.Content != "Hand-written plan text." {
		t.Errorf("content must stay untouched, got %q", p.Content)
	}
	if len(p.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(p.Items))
	}
}

func TestSetPlanWithoutItemsKeepsLegacyBehaviour(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-legacy", "plan": "⬜ Step 1\n⬜ Step 2"})
	if resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	p := planFromStore(t, "t-items-legacy")
	if p.Content != "⬜ Step 1\n⬜ Step 2" {
		t.Errorf("content changed: %q", p.Content)
	}
	if len(p.Items) != 0 {
		t.Errorf("expected no items, got %+v", p.Items)
	}

	m := resultMap(t, h.handleGetPlan(map[string]any{"thread_id": "t-items-legacy"}))
	if _, ok := m["items"]; ok {
		t.Error("get_plan must not grow an items key for a plan without items")
	}
	if m["plan"] != "⬜ Step 1\n⬜ Step 2" {
		t.Errorf("legacy plan text changed: %q", m["plan"])
	}
}

func TestSetPlanRequiresPlanOrItems(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-empty"})
	if resp.Error == "" {
		t.Fatal("expected error without plan and without items")
	}
}

// An empty items array with no text is not a plan: it would replace the active
// one with nothing and silently remove the context-loss anchor.
func TestSetPlanRejectsEmptyItemListWithoutText(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-vanish", "plan": "the real plan"}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-vanish", "items": []any{}})
	if resp.Error == "" {
		t.Fatal("expected error for an empty item list with no text")
	}
	if got := planFromStore(t, "t-items-vanish"); got.Content != "the real plan" {
		t.Errorf("the existing plan was overwritten: %q", got.Content)
	}
}

// The item array is the one validation boundary of the feature; every renderer
// caps its output, so the stored outline must not be able to grow without bound.
func TestSetPlanCapsItemCountAndFieldLength(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	raw := make([]any, 0, maxPlanItems+5)
	for i := 0; i < maxPlanItems+5; i++ {
		raw = append(raw, map[string]any{"content": "step", "reason": "why"})
	}
	raw[0] = map[string]any{"content": strings.Repeat("c", maxPlanItemRunes+50), "reason": strings.Repeat("r", maxPlanReasonRunes+50), "status": strings.Repeat("s", maxPlanStatusRunes+50)}

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-cap", "items": raw}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-cap")
	if len(p.Items) != maxPlanItems {
		t.Errorf("expected at most %d items, got %d", maxPlanItems, len(p.Items))
	}
	if got := len([]rune(p.Items[0].Content)); got > maxPlanItemRunes+1 {
		t.Errorf("content not capped: %d runes", got)
	}
	if got := len([]rune(p.Items[0].Reason)); got > maxPlanReasonRunes+1 {
		t.Errorf("reason not capped: %d runes", got)
	}
	if got := len([]rune(p.Items[0].Status)); got > maxPlanStatusRunes+1 {
		t.Errorf("status not capped: %d runes", got)
	}
}

// An empty item list is an explicit statement: the plan has no outline, so it
// renders from the text again.
func TestSetPlanEmptyItemListFallsBackToText(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-clear", "plan": "text", "items": itemList(t, `[{"content":"a"}]`)}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-clear", "plan": "text again", "items": []any{}}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-clear")
	if len(p.Items) != 0 {
		t.Errorf("expected cleared items, got %+v", p.Items)
	}
	if p.Content != "text again" {
		t.Errorf("expected supplied text, got %q", p.Content)
	}
}

// An unknown status and an out-of-range depth are stored as given. Clamping is
// the renderer's job, and rewriting the caller's data here would hide the value
// the display is supposed to survive.
func TestSetPlanKeepsUnknownStatusAndDepth(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-odd",
		"items":     itemList(t, `[{"content":"odd","status":"blocked-ish","depth":9}]`),
	})
	if resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	p := planFromStore(t, "t-items-odd")
	if p.Items[0].Status != "blocked-ish" || p.Items[0].Depth != 9 {
		t.Errorf("values were rewritten: %+v", p.Items[0])
	}
}

func TestUpdatePlanCompletedMarksItemAndRenumbersContent(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-update",
		"items":     itemList(t, `[{"content":"first step"},{"content":"second step"}]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-update", "completed": []any{"second"}})
	if resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-update")
	if p.Items[0].Status != "" || p.Items[1].Status != "completed" {
		t.Errorf("wrong item statuses: %+v", p.Items)
	}
	if !strings.Contains(p.Content, "[x] second step") {
		t.Errorf("content not re-derived after an item mutation: %q", p.Content)
	}
}

func TestUpdatePlanAddAndRemoveItems(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-ar", "items": itemList(t, `[{"content":"keep me"},{"content":"drop me"}]`)}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-ar", "add": []any{"new step"}, "remove": []any{"drop"}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-ar")
	if len(p.Items) != 2 {
		t.Fatalf("expected 2 items, got %+v", p.Items)
	}
	if p.Items[0].Content != "keep me" || p.Items[1].Content != "new step" {
		t.Errorf("wrong items after add/remove: %+v", p.Items)
	}
}

// A legacy plan stays editable by the old string operations.
func TestUpdatePlanLegacyContentStillWorks(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-legacy-upd", "plan": "⬜ one\n⬜ two"}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-legacy-upd", "completed": []any{"one"}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-legacy-upd")
	if len(p.Items) != 0 {
		t.Errorf("legacy update must not invent items: %+v", p.Items)
	}
	if !strings.Contains(p.Content, "✅ one") {
		t.Errorf("legacy marker not applied: %q", p.Content)
	}
}

// Clearing the outline must not blank the plan: an active plan with neither
// items nor text would leave get_plan empty after a collapse, which is exactly
// the context the ledger shortening is allowed to rely on.
func TestUpdatePlanClearItemsKeepsText(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-clear-update",
		"items":     itemList(t, `[{"content":"first step"}]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	before := planFromStore(t, "t-items-clear-update").Content

	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-clear-update", "items": []any{}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-clear-update")
	if len(p.Items) != 0 {
		t.Errorf("expected cleared items, got %+v", p.Items)
	}
	if p.Content != before || p.Content == "" {
		t.Errorf("clearing the outline must keep the last text: got %q, want %q", p.Content, before)
	}
}

// A remove that empties the outline must not leave the removed steps as the
// plan: the ledger would fall back to the full text and inject deleted work as
// open steps. An explicitly empty item list keeps the text (that is a deliberate
// "no outline, keep the plan as text"); an emptied-by-removal outline does not.
func TestUpdatePlanRemoveAllClearsStaleText(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-remove-all",
		"items":     itemList(t, `[{"content":"gone step"},{"content":"other gone"}]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-remove-all", "remove": []any{"gone"}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-remove-all")
	if len(p.Items) != 0 {
		t.Errorf("expected no items left, got %+v", p.Items)
	}
	if strings.Contains(p.Content, "gone") {
		t.Errorf("removed steps still in the plan text: %q", p.Content)
	}
}

// The legacy text path marks every matching line, so the item path marks every
// matching item: the same call must not behave differently depending on whether
// the plan has an outline.
func TestUpdatePlanCompletedMarksEveryMatch(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-multi",
		"items":     itemList(t, `[{"content":"write test a"},{"content":"write test b"},{"content":"other"}]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-multi", "completed": []any{"write test"}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-multi")
	if p.Items[0].Status != "completed" || p.Items[1].Status != "completed" {
		t.Errorf("every matching item must be completed: %+v", p.Items)
	}
	if p.Items[2].Status != "" {
		t.Errorf("non-matching item changed: %+v", p.Items[2])
	}
}

// The derived snapshot is the text a reader without the outline sees, so it must
// not carry control characters into the terminal either.
func TestDerivePlanContentStripsControlCharacters(t *testing.T) {
	got := derivePlanContent([]PlanItem{{Content: "ok\x1b[31mred\x00tail", Reason: "why\x07bell"}})
	for _, r := range got {
		if r < 0x20 && r != '\n' {
			t.Fatalf("derived content kept control character %q in %q", r, got)
		}
	}
	if !strings.Contains(got, "ok") || !strings.Contains(got, "tail") {
		t.Errorf("printable text lost: %q", got)
	}
}

// Items must survive the persistence roundtrip the daemon performs on startup;
// the handler tests read the in-memory cache, which hides a broken column.
func TestPlanItemsSurviveDaemonRestart(t *testing.T) {
	h, store := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-restart",
		"items": itemList(t, `[
			{"content":"first","status":"completed"},
			{"content":"second","reason":"why","depth":1}
		]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	cleanupPlans()
	LoadPlansFromDB(store)

	p := planFromStore(t, "t-items-restart")
	if len(p.Items) != 2 {
		t.Fatalf("items lost on restore: %+v", p.Items)
	}
	if p.Items[0].Status != "completed" || p.Items[1].Reason != "why" || p.Items[1].Depth != 1 {
		t.Errorf("items changed on restore: %+v", p.Items)
	}
	if !strings.Contains(p.Content, "first") {
		t.Errorf("derived text lost on restore: %q", p.Content)
	}
}

// update_plan's add path is the second way into the outline, so the caps have to
// hold there too: a client can grow the stored outline through repeated adds.
func TestUpdatePlanAddRespectsCapsAndSkipsEmpty(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-add-cap", "items": itemList(t, `[{"content":"seed"}]`)}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}
	raw := make([]any, 0, maxPlanItems+5)
	for i := 0; i < maxPlanItems+5; i++ {
		raw = append(raw, "filler")
	}
	raw = append(raw, "   ", "", strings.Repeat("c", maxPlanItemRunes+50))

	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-add-cap", "add": raw}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-add-cap")
	if len(p.Items) != maxPlanItems {
		t.Errorf("expected the cap to hold at %d, got %d", maxPlanItems, len(p.Items))
	}
	for _, it := range p.Items {
		if strings.TrimSpace(it.Content) == "" {
			t.Errorf("empty item was added: %+v", p.Items)
		}
		if len([]rune(it.Content)) > maxPlanItemRunes+1 {
			t.Errorf("added item not capped: %d runes", len([]rune(it.Content)))
		}
	}
}

// An empty needle matches everything. The legacy text path had the same hole;
// with an outline the mistake also wipes the derived text.
func TestUpdatePlanIgnoresEmptyNeedles(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{
		"thread_id": "t-items-needle",
		"items":     itemList(t, `[{"content":"one"},{"content":"two"}]`),
	}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-needle", "completed": []any{""}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}
	if resp := h.handleUpdatePlan(map[string]any{"thread_id": "t-items-needle", "remove": []any{""}}); resp.Error != "" {
		t.Fatalf("update plan error: %s", resp.Error)
	}

	p := planFromStore(t, "t-items-needle")
	if len(p.Items) != 2 {
		t.Fatalf("an empty needle removed items: %+v", p.Items)
	}
	if p.Items[0].Status == "completed" || p.Items[1].Status == "completed" {
		t.Errorf("an empty needle completed items: %+v", p.Items)
	}
}

func TestGetPlanReturnsItems(t *testing.T) {
	h, _ := mustHandler(t)
	defer cleanupPlans()

	if resp := h.handleSetPlan(map[string]any{"thread_id": "t-items-get", "items": itemList(t, `[{"content":"a","status":"pending"},{"content":"b","depth":1}]`)}); resp.Error != "" {
		t.Fatalf("set plan error: %s", resp.Error)
	}

	m := resultMap(t, h.handleGetPlan(map[string]any{"thread_id": "t-items-get"}))
	raw, ok := m["items"]
	if !ok {
		t.Fatal("get_plan must return items when the plan has them")
	}
	items, ok := raw.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items, got %#v", raw)
	}
	first, _ := items[0].(map[string]any)
	if first["content"] != "a" || first["status"] != "pending" {
		t.Errorf("item not returned verbatim: %#v", first)
	}
}
