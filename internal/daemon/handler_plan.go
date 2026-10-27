package daemon

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/repo"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Plan represents an active plan for a thread/session.
type Plan struct {
	Content   string    `json:"content"`
	Status    string    `json:"status"` // "active" or "completed"
	Scope     string    `json:"scope"`  // "session" or "permanent"
	Project   string    `json:"project"`
	ThreadID  string    `json:"thread_id"`
	DocsHint  string    `json:"docs_hint"` // formatted reminder for plan checkpoints
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// planStore holds plans in memory (cache), backed by SQLite for persistence.
var planStore = struct {
	sync.RWMutex
	plans map[string]*Plan // threadID → plan
}{plans: make(map[string]*Plan)}

// LoadPlansFromDB restores active plans from DB into memory cache.
// Called once at daemon startup.
func LoadPlansFromDB(store *storage.Store) {
	rows, err := store.GetActivePlans()
	if err != nil {
		return
	}

	// Build before taking the lock: the docs hint needs a store query per plan.
	// Memoized per project — every plan of the same project yields the same hint,
	// and a restart can restore hundreds of them.
	hints := make(map[string]string)
	restored := make(map[string]*Plan, len(rows))
	for _, r := range rows {
		hint, ok := hints[r.Project]
		if !ok {
			hint = buildDocsHint(store, r.Project)
			hints[r.Project] = hint
		}
		restored[r.ThreadID] = &Plan{
			Content:  r.Content,
			Status:   r.Status,
			Scope:    r.Scope,
			Project:  r.Project,
			ThreadID: r.ThreadID,
			// DocsHint is derived from the project's doc sources and never
			// persisted, so a reloaded plan has to rebuild it or its
			// checkpoints lose the docs reminder until the next set_plan.
			DocsHint:  hint,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		}
	}

	planStore.Lock()
	for threadID, p := range restored {
		planStore.plans[threadID] = p
	}
	planStore.Unlock()
}

// planPersistMu serializes [snapshot, write] pairs of one plan.
//
// Without it two parallel update_plan calls can invert: caller A reads the plan,
// caller B reads the newer one, B writes, A writes — and the DB keeps the older
// content while memory holds the newer one. Under this mutex every writer reads
// the state as it is when it gets its turn, so the last write is the current
// state.
var planPersistMu sync.Mutex

// persistPlan writes the plan of threadID to the DB. The snapshot is taken
// under planStore's read lock: reading the *Plan after Unlock raced against a
// concurrent update_plan that was mutating the same struct.
func (h *Handler) persistPlan(threadID string) {
	if h.store == nil {
		return
	}
	planPersistMu.Lock()
	defer planPersistMu.Unlock()

	planStore.RLock()
	p, ok := planStore.plans[threadID]
	if !ok || p == nil {
		planStore.RUnlock()
		return
	}
	row := storage.PlanRow{
		ThreadID:  p.ThreadID,
		Content:   p.Content,
		Status:    p.Status,
		Scope:     p.Scope,
		Project:   p.Project,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	planStore.RUnlock()

	h.store.UpsertPlan(&row)
}

func (h *Handler) handleSetPlan(params map[string]any) Response {
	plan, _ := params["plan"].(string)
	if plan == "" {
		return errorResponse("plan is required")
	}
	scope, _ := params["scope"].(string)
	if scope == "" {
		scope = "session"
	}
	threadID, _ := params["thread_id"].(string)
	// Store the resolved project, not the raw param: the MCP schema for
	// set_plan carries only plan/scope, so the param is empty on the normal
	// path while the docs hint is built from the cwd fallback. A plan restored
	// after a daemon restart rebuilds its hint from this value and would
	// otherwise fall back to globals-only.
	project := docsHintProject(params)

	now := time.Now()
	p := &Plan{
		Content:   plan,
		Status:    "active",
		Scope:     scope,
		Project:   project,
		ThreadID:  threadID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Build docs hint from indexed reference sources, scoped to the session's
	// project. set_plan carries no `project` param (the MCP schema exposes only
	// plan/scope), so fall back to the caller's working directory — otherwise
	// the hint would degrade to globals-only and drop the session's own docs.
	if h.store != nil {
		p.DocsHint = buildDocsHint(h.store, project)
	}

	planStore.Lock()
	planStore.plans[threadID] = p
	planStore.Unlock()

	h.persistPlan(threadID)

	return jsonResponse(map[string]any{
		"message": fmt.Sprintf("Plan set (%s scope, %d chars)", scope, len(plan)),
		"status":  "active",
		"plan":    plan,
	})
}

func (h *Handler) handleUpdatePlan(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)

	// A plan written before set_plan persisted the resolved project carries an
	// empty one. Resolve it here as well, and outside planStore's lock: the
	// lookup walks the filesystem for a .git root and buildDocsHint runs a store
	// query, while GetActivePlan sits on the request path.
	needsBackfill := false
	planStore.RLock()
	if existing, ok := planStore.plans[threadID]; ok && existing != nil && existing.Project == "" {
		needsBackfill = true
	}
	planStore.RUnlock()

	backfillProject := ""
	backfillHint := ""
	if needsBackfill {
		backfillProject = docsHintProject(params)
		if backfillProject != "" && h.store != nil {
			backfillHint = buildDocsHint(h.store, backfillProject)
		}
	}

	planStore.Lock()
	p, ok := planStore.plans[threadID]
	if !ok || p == nil {
		planStore.Unlock()
		return errorResponse("no active plan — use set_plan() first")
	}
	if p.Status == "completed" {
		planStore.Unlock()
		return errorResponse("plan already completed — use set_plan() for a new plan")
	}

	if backfillProject != "" && p.Project == "" {
		p.Project = backfillProject
		if h.store != nil {
			p.DocsHint = backfillHint
		}
		log.Printf("[plan] backfilled project %q on thread %q", backfillProject, threadID)
	}

	// Apply updates
	if completed, ok := params["completed"].([]any); ok {
		for _, item := range completed {
			if s, ok := item.(string); ok {
				p.Content = markCompleted(p.Content, s)
			}
		}
	}
	if add, ok := params["add"].([]any); ok {
		for _, item := range add {
			if s, ok := item.(string); ok {
				p.Content += "\n" + s
			}
		}
	}
	if remove, ok := params["remove"].([]any); ok {
		for _, item := range remove {
			if s, ok := item.(string); ok {
				p.Content = removeLine(p.Content, s)
			}
		}
	}
	// Allow direct content replacement
	if newContent, ok := params["plan"].(string); ok && newContent != "" {
		p.Content = newContent
	}

	p.UpdatedAt = time.Now()
	content := p.Content
	planStore.Unlock()

	h.persistPlan(threadID)

	return jsonResponse(map[string]any{
		"message": "Plan updated",
		"status":  "active",
		"plan":    content,
	})
}

func (h *Handler) handleGetPlan(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)

	// Read every field under the lock: another goroutine mutates this plan in
	// place under the write lock (update_plan), so a field read after RUnlock
	// raced against that write.
	planStore.RLock()
	defer planStore.RUnlock()
	p, ok := planStore.plans[threadID]
	if !ok || p == nil {
		return jsonResponse(map[string]any{"exists": false, "plan": "", "status": ""})
	}
	return jsonResponse(map[string]any{
		"exists":    true,
		"plan":      p.Content,
		"status":    p.Status,
		"scope":     p.Scope,
		"project":   p.Project,
		"docs_hint": p.DocsHint,
	})
}

func (h *Handler) handleCompletePlan(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)

	planStore.Lock()
	p, ok := planStore.plans[threadID]
	if !ok || p == nil {
		planStore.Unlock()
		return errorResponse("no active plan to complete")
	}
	p.Status = "completed"
	p.UpdatedAt = time.Now()
	content := p.Content
	planStore.Unlock()

	h.persistPlan(threadID)

	return jsonResponse(map[string]any{
		"message": "Plan completed — checkpoints stopped",
		"plan":    content,
		"status":  "completed",
	})
}

// GetActivePlan returns the active plan for a thread (used by proxy for injection).
func GetActivePlan(threadID string) (string, bool) {
	planStore.RLock()
	defer planStore.RUnlock()
	p, ok := planStore.plans[threadID]
	if !ok || p == nil || p.Status != "active" {
		return "", false
	}
	return p.Content, true
}

// HasActivePlan returns true if there is an active plan for a thread.
func HasActivePlan(threadID string) bool {
	planStore.RLock()
	defer planStore.RUnlock()
	p, ok := planStore.plans[threadID]
	return ok && p != nil && p.Status == "active"
}

// buildDocsHint queries reference doc sources and builds a formatted reminder
// for plan checkpoint injection. The hint is scoped to `project` (plus global
// sources); a foreign project's reference docs must never appear here.
// Returns "" if no reference sources exist for the project.
func buildDocsHint(store *storage.Store, project string) string {
	sources, err := store.GetReferenceSources(project)
	if err != nil || len(sources) == 0 {
		return ""
	}

	var sourceList []string
	var examples []string
	for _, s := range sources {
		label := s.Name
		if s.Version != "" {
			label += " " + s.Version
		}
		sourceList = append(sourceList, label)
		if s.ExampleQuery != "" {
			examples = append(examples, fmt.Sprintf("- %s → docs_search(\"%s\")", label, s.ExampleQuery))
		}
	}

	var b strings.Builder
	b.WriteString("[Docs available] Reference docs indexed:\n")
	b.WriteString("  " + strings.Join(sourceList, ", ") + "\n\n")
	b.WriteString("BEFORE writing code that uses framework APIs, external libraries,\n")
	b.WriteString("or language stdlib beyond basic control flow:\n")
	b.WriteString("1. Identify which functions/methods/tags you will use\n")
	b.WriteString("2. Call docs_search(query=\"<function or concept>\", exact=true)\n")
	b.WriteString("3. Read the result BEFORE writing the Edit/Write\n")
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, ex := range examples {
			b.WriteString(ex + "\n")
		}
	}
	b.WriteString("\nSkip for: variable assignments, if/else, loops, string operations,\n")
	b.WriteString("error handling patterns you already know.\n")
	b.WriteString("[/Docs available]")
	return b.String()
}

// docsHintTTL bounds how long a cached docs hint is served.
const docsHintTTL = 5 * time.Minute

// docsHintProject resolves the project to scope a docs hint to: the explicit
// `project` param, else the caller's working directory. resolveProjectParam
// only canonicalizes an already-present `project`, so without this fallback
// callers that never send one (the MCP set_plan path) would scope to "" and
// lose their own reference docs.
func docsHintProject(params map[string]any) string {
	if p := stringOr(params, "project", ""); p != "" {
		return repo.RootOrSelf(p)
	}
	if cwd, _ := params["_cwd"].(string); cwd != "" {
		return repo.RootOrSelf(models.ProjectShortFromPath(cwd))
	}
	return ""
}

// docsHintEntry is one cached docs hint for a project.
type docsHintEntry struct {
	hint    string
	builtAt time.Time
}

// docsHintCache caches the formatted docs hint per project with a TTL.
// Keying by project matters: a single global cache served one project's hint
// to every other project for the whole TTL window.
var docsHintCache = struct {
	sync.RWMutex
	entries map[string]docsHintEntry
}{entries: make(map[string]docsHintEntry)}

// handleGetDocsHint returns the cached docs hint for the requested project,
// rebuilding if stale (>docsHintTTL). Used by proxy for subagent injection.
func (h *Handler) handleGetDocsHint(params map[string]any) Response {
	project := docsHintProject(params)

	docsHintCache.RLock()
	entry, ok := docsHintCache.entries[project]
	docsHintCache.RUnlock()

	if ok && time.Since(entry.builtAt) < docsHintTTL && entry.hint != "" {
		return jsonResponse(map[string]any{"docs_hint": entry.hint})
	}

	// Rebuild
	if h.store == nil {
		return jsonResponse(map[string]any{"docs_hint": ""})
	}
	hint := buildDocsHint(h.store, project)

	docsHintCache.Lock()
	if docsHintCache.entries == nil {
		docsHintCache.entries = make(map[string]docsHintEntry)
	}
	// Drop expired entries so the map stays bounded by the projects seen within
	// one TTL window rather than growing with every project string ever sent.
	for k, e := range docsHintCache.entries {
		if time.Since(e.builtAt) >= docsHintTTL {
			delete(docsHintCache.entries, k)
		}
	}
	docsHintCache.entries[project] = docsHintEntry{hint: hint, builtAt: time.Now()}
	docsHintCache.Unlock()

	return jsonResponse(map[string]any{"docs_hint": hint})
}

// markCompleted marks a line as completed (prepends ✅ or replaces ⬜/🔄 with ✅).
func markCompleted(content, item string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, item) {
			line = strings.Replace(line, "⬜", "✅", 1)
			line = strings.Replace(line, "🔄", "✅", 1)
			if !strings.Contains(line, "✅") {
				line = "✅ " + line
			}
			lines[i] = line
		}
	}
	return strings.Join(lines, "\n")
}

// removeLine removes lines containing the given text.
func removeLine(content, item string) string {
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if !strings.Contains(line, item) {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}
