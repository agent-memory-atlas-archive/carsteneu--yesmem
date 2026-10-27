package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func TestHandle_Ping(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "ping"})
	if resp.Error != "" {
		t.Fatalf("ping error: %s", resp.Error)
	}
}

func TestHandle_UnknownMethod(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "nonexistent_method_xyz"})
	if resp.Error == "" {
		t.Fatal("expected error for unknown method")
	}
}

func TestHandle_UpdateFixationRatio(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.UpsertSession(&models.Session{ID: "sess-fix", Project: "/proj", StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	resp := h.Handle(Request{
		Method: "update_fixation_ratio",
		Params: map[string]any{"session_id": "sess-fix", "fixation_ratio": 0.5},
	})
	if resp.Error != "" {
		t.Fatalf("update fixation ratio: %s", resp.Error)
	}
	if m := resultMap(t, resp); m["status"] != "ok" {
		t.Errorf("status = %q, want ok", m["status"])
	}

	ratio, err := s.GetSessionFixationRatio("sess-fix")
	if err != nil {
		t.Fatalf("read fixation ratio: %v", err)
	}
	if ratio != 0.5 {
		t.Errorf("fixation ratio = %v, want 0.5", ratio)
	}
}

func TestHandle_UpdateFixationRatio_MissingSession(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "update_fixation_ratio",
		Params: map[string]any{"fixation_ratio": 0.5},
	})
	if resp.Error == "" {
		t.Fatal("expected error for missing session_id")
	}
}

func TestHandle_TrackProxyUsage(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "track_proxy_usage",
		Params: map[string]any{
			"day":                   "2026-04-01",
			"input_tokens":          float64(1000),
			"output_tokens":         float64(500),
			"cache_read_tokens":     float64(200),
			"cache_creation_tokens": float64(100),
		},
	})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

func TestHandle_TrackProxyUsage_AutoDay(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "track_proxy_usage",
		Params: map[string]any{"input_tokens": float64(100)},
	})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

func TestHandle_TrackForkUsage(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "track_fork_usage",
		Params: map[string]any{
			"day":           "2026-04-01",
			"input_tokens":  float64(500),
			"output_tokens": float64(200),
		},
	})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

func TestHandle_ReloadVectors_NoVectorStore(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "reload_vectors"})
	if resp.Error == "" {
		t.Fatal("expected error when vector store is nil")
	}
}

func TestHandle_IdleTickUpdatesTime(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "idle_tick",
		Params: map[string]any{"thread_id": "t-idle", "session_id": "s-idle"},
	})
	if resp.Error != "" {
		t.Fatalf("idle_tick error: %s", resp.Error)
	}
}

func TestHandle_IncrementCounters(t *testing.T) {
	h, _ := mustHandler(t)

	idMethods := []string{"increment_hits", "increment_noise", "increment_match", "increment_inject", "increment_use", "increment_fail", "increment_save"}
	for _, method := range idMethods {
		resp := h.Handle(Request{
			Method: method,
			Params: map[string]any{"ids": []any{float64(1), float64(2)}},
		})
		if resp.Error != "" {
			t.Errorf("%s error: %s", method, resp.Error)
		}
	}

	// increment_turn needs project, not ids
	resp := h.Handle(Request{
		Method: "increment_turn",
		Params: map[string]any{"project": "yesmem"},
	})
	if resp.Error != "" {
		t.Errorf("increment_turn error: %s", resp.Error)
	}
}

func TestHandle_SkipIndexing(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{
		Method: "skip_indexing",
		Params: map[string]any{"session_id": "s-skip"},
	})
	if resp.Error != "" {
		t.Fatalf("skip_indexing error: %s", resp.Error)
	}
}

func TestHandle_ListProjects(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "list_projects"})
	if resp.Error != "" {
		t.Fatalf("list_projects error: %s", resp.Error)
	}
}

// --- Helpers ---

func TestResolveProjectParam(t *testing.T) {
	h, _ := mustHandler(t)

	// With project set
	params := map[string]any{"project": "yesmem"}
	resolved := h.resolveProjectParam(params)
	if resolved["project"] != "yesmem" {
		t.Errorf("expected 'yesmem', got %q", resolved["project"])
	}

	// Without project — should not crash
	params = map[string]any{}
	resolved = h.resolveProjectParam(params)
	_ = resolved // just ensure no panic
}

// The session behind a short project name must come from the caller. Under
// opencode v2 neither the shared service PID (which stands for whichever session
// registered last) nor a leaked process environment id (which names another
// session) may decide it. The basename is deliberately ambiguous: the name
// cannot be resolved on its own, so only a foreign session could pick one.
func TestResolveProjectParam_ForeignSessionDoesNotSupplyTheProject(t *testing.T) {
	h := mustHandlerWithAmbiguousProbeProjects(t)

	cases := []struct {
		name   string
		params map[string]any
	}{
		{"shared service PID", map[string]any{
			"_source_agent": "opencode", "_caller_pid": float64(opencodeSharedServicePID),
		}},
		{"leaked environment id", map[string]any{
			"_session_id": "opencode:ses_ENVLEAK", "_session_id_source": "env",
			"_source_agent": "opencode", "_caller_pid": float64(opencodeSharedServicePID),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params := map[string]any{"project": "probe"}
			for k, v := range c.params {
				params[k] = v
			}
			resolved := h.resolveProjectParam(params)
			if got := resolved["project"]; got != "probe" {
				t.Fatalf("project = %v, want the ambiguous name to stay unresolved instead of taking another session's project", got)
			}
		})
	}
}

// A caller whose own session is known keeps its project resolution: the
// guards above must not disable the session-derived path in general.
func TestResolveProjectParam_PerCallSessionSuppliesTheProject(t *testing.T) {
	h := mustHandlerWithAmbiguousProbeProjects(t)

	resolved := h.resolveProjectParam(map[string]any{
		"project": "probe", "_session_id": "opencode:ses_LASTREG", "_source_agent": "opencode",
	})
	if got := resolved["project"]; got != "/elsewhere/probe" {
		t.Fatalf("project = %v, want the caller's own session project", got)
	}
}

// mustHandlerWithAmbiguousProbeProjects seeds two sessions whose projects share
// the basename "probe" — a short name that cannot be resolved without knowing
// which session asks — and registers the shared service PID for the first.
func mustHandlerWithAmbiguousProbeProjects(t *testing.T) *Handler {
	t.Helper()
	h, s := mustHandler(t)
	isolateHome(t)
	for _, sess := range []struct{ id, project string }{
		{"opencode:ses_LASTREG", "/elsewhere/probe"},
		{"opencode:ses_OTHER", "/other/probe"},
	} {
		if err := s.UpsertSession(&models.Session{ID: sess.id, Project: sess.project, StartedAt: time.Now()}); err != nil {
			t.Fatalf("seed session %s: %v", sess.id, err)
		}
	}
	if resp := h.handleRegisterPID(map[string]any{
		"session_id": "opencode:ses_LASTREG", "pid": float64(opencodeSharedServicePID), "source_agent": "opencode",
	}); resp.Error != "" {
		t.Fatalf("register pid: %s", resp.Error)
	}
	return h
}

func TestInjectExcludeSession(t *testing.T) {
	h, _ := mustHandler(t)
	params := map[string]any{"_session_id": "my-session"}
	h.injectExcludeSession(params)
	if params["exclude_session"] != "my-session" {
		t.Errorf("expected _session_id propagated to exclude_session")
	}
}

// An id the MCP layer took from the shared service environment names another
// session: excluding it would hide a foreign session's learnings and keep the
// caller's own — so the caller is identified from its working directory instead.
func TestInjectExcludeSession_UsesCallerDirectoryForLeakedEnvironmentID(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const worktree = "/worktrees/exclude-cwd"
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-exclude", Project: "/repo/exclude", Section: "exclude-sec",
		SessionID: "opencode:ses_mine", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_mine",
	})
	seedRootSession(t, worktree, "ses_mine")

	params := map[string]any{
		"_session_id": "opencode:ses_FOREIGN", "_session_id_source": "env",
		"_source_agent": "opencode", "_caller_pid": float64(opencodeSharedServicePID),
		"_cwd": worktree,
	}
	h.injectExcludeSession(params)
	if params["exclude_session"] != "opencode:ses_mine" {
		t.Errorf("exclude_session = %v, want the caller's own session from its directory", params["exclude_session"])
	}
}

// Without a directory that identifies the caller, a leaked id still excludes
// nothing rather than somebody else's session.
func TestInjectExcludeSession_LeakedIDWithoutDirectoryExcludesNothing(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	params := map[string]any{
		"_session_id": "opencode:ses_FOREIGN", "_session_id_source": "env",
		"_source_agent": "opencode", "_caller_pid": float64(opencodeSharedServicePID),
	}
	h.injectExcludeSession(params)
	if got, exists := params["exclude_session"]; exists {
		t.Errorf("exclude_session = %v, want it unset for a leaked environment id", got)
	}
}

// A session the caller named itself keeps the exclusion, even with the shared
// service PID in play.
func TestInjectExcludeSession_KeepsInjectedSession(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	params := map[string]any{
		"_session_id": "opencode:ses_mine", "_source_agent": "opencode",
		"_caller_pid": float64(opencodeSharedServicePID),
	}
	h.injectExcludeSession(params)
	if params["exclude_session"] != "opencode:ses_mine" {
		t.Errorf("exclude_session = %v, want the caller's own session", params["exclude_session"])
	}
}

func TestInjectExcludeSession_NoSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	params := map[string]any{"query": "test"}
	h.injectExcludeSession(params)
	if _, ok := params["exclude_session"]; ok {
		t.Error("should not set exclude_session when no _session_id")
	}
}

func TestIntOr(t *testing.T) {
	params := map[string]any{"count": float64(42)}
	if intOr(params, "count", 0) != 42 {
		t.Error("expected 42")
	}
	if intOr(params, "missing", 10) != 10 {
		t.Error("expected default 10")
	}
}

// A cwd-dependent ambiguity resolution must not poison the short-name cache:
// a later caller whose cwd matches one candidate must still get the
// tiebreaker resolution, not the first caller's cached error.
func TestResolveProjectParam_CwdErrorNotCached(t *testing.T) {
	h, store := mustHandler(t)

	// Two projects sharing the basename "dupname" — real fullpath data shape.
	for _, p := range []string{"/tmp/a/dupname", "/tmp/b/dupname"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// First caller stands outside both candidates → ambiguous error.
	// Error is cached, but cwd-bearing callers bypass the cache.
	params := map[string]any{"project": "dupname", "_cwd": "/somewhere/else"}
	resolved := h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; !ok {
		t.Fatalf("expected _project_error for cwd-unresolved ambiguity, got project=%v", resolved["project"])
	} else if !strings.Contains(errMsg.(string), "ambiguous") {
		t.Fatalf("expected ambiguous error, got: %v", errMsg)
	}

	// Second caller stands inside one candidate → cwd tiebreaker must win.
	params = map[string]any{"project": "dupname", "_cwd": "/tmp/a/dupname"}
	resolved = h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; ok {
		t.Fatalf("expected no error for cwd-matching caller, got: %v", errMsg)
	}
	if resolved["project"] != "/tmp/a/dupname" {
		t.Errorf("expected cwd tiebreaker to resolve to /tmp/a/dupname, got %q", resolved["project"])
	}
}

// TestResolveProjectParam_AmbiguousFallbackNotCachedWhenCwdMissing covers the
// cache-poisoning regression: a caller with no cwd that hits ambiguity must
// NOT cache the error, because a subsequent caller with a matching cwd must
// still get the tiebreaker resolution.
func TestResolveProjectParam_AmbiguousFallbackNotCachedWhenCwdMissing(t *testing.T) {
	h, store := mustHandler(t)

	for _, p := range []string{"/tmp/a/dupname", "/tmp/b/dupname"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// Caller 1: cwd missing → ambiguity error. Must NOT cache; the error is
	// cwd-dependent (a cwd-bearing caller may resolve it).
	params := map[string]any{"project": "dupname"} // no _cwd
	resolved := h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; !ok {
		t.Fatalf("caller 1: expected _project_error for ambiguity without cwd, got project=%v", resolved["project"])
	} else if !strings.Contains(errMsg.(string), "ambiguous") {
		t.Fatalf("caller 1: expected ambiguous error, got: %v", errMsg)
	}

	// Caller 2: cwd inside /tmp/b/dupname → tiebreaker must win, NOT serve cached error.
	params = map[string]any{"project": "dupname", "_cwd": "/tmp/b/dupname"}
	resolved = h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; ok {
		t.Fatalf("caller 2: expected no error, got: %v", errMsg)
	}
	if resolved["project"] != "/tmp/b/dupname" {
		t.Errorf("caller 2: cache-poisoning regression — expected /tmp/b/dupname via cwd tiebreaker, got %q", resolved["project"])
	}
}

// TestResolveProjectParam_UniqueShortWithCwdIsCached verifies that a unique
// short name (one DB candidate) with cwd present is cached. Before the fix,
// cwd-present results were never cached, causing repeated DB lookups on every
// MCP call even for deterministic resolutions.
func TestResolveProjectParam_UniqueShortWithCwdIsCached(t *testing.T) {
	h, store := mustHandler(t)

	// Seed one session — unique basename "uniqueproj"
	if err := store.UpsertSession(&models.Session{
		ID: "sess-unique", Project: "/tmp/uniqueproj",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Call 1: short name with cwd → should resolve to full path
	params := map[string]any{"project": "uniqueproj", "_cwd": "/tmp/somewhere"}
	resolved := h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; ok {
		t.Fatalf("call 1: unexpected error: %v", errMsg)
	}
	if resolved["project"] != "/tmp/uniqueproj" {
		t.Fatalf("call 1: expected /tmp/uniqueproj, got %q", resolved["project"])
	}

	// Delete the session — if cache works, call 2 returns cached result.
	if _, err := store.DB().Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}

	// Call 2: same params → must return CACHED full path, not passthrough
	params = map[string]any{"project": "uniqueproj", "_cwd": "/tmp/somewhere"}
	resolved = h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; ok {
		t.Fatalf("call 2: unexpected error after DB delete (cache miss): %v", errMsg)
	}
	if resolved["project"] != "/tmp/uniqueproj" {
		t.Errorf("call 2: cache miss — expected /tmp/uniqueproj from cache, got %q (was DB hit, passthrough)", resolved["project"])
	}
}

// TestResolveProjectParam_AmbiguousNoCwdErrorIsCached verifies that an
// ambiguous short name without cwd caches the error to prevent repeated
// DB lookups and WARNING logs.
func TestResolveProjectParam_AmbiguousNoCwdErrorIsCached(t *testing.T) {
	h, store := mustHandler(t)

	for _, p := range []string{"/tmp/a/dupname", "/tmp/b/dupname"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// Call 1: ambiguous short name without cwd → error
	params := map[string]any{"project": "dupname"} // no _cwd
	resolved := h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; !ok {
		t.Fatalf("call 1: expected _project_error, got project=%v", resolved["project"])
	} else if !strings.Contains(errMsg.(string), "ambiguous") {
		t.Fatalf("call 1: expected ambiguous error, got: %v", errMsg)
	}

	// Delete sessions — if error is cached, call 2 returns same error.
	if _, err := store.DB().Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}

	// Call 2: same params (no cwd) → must return CACHED ambiguous error
	params = map[string]any{"project": "dupname"} // no _cwd
	resolved = h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; !ok {
		t.Errorf("call 2: cache miss — expected cached ambiguous error, got project=%v (was DB hit, passthrough)", resolved["project"])
	} else if !strings.Contains(errMsg.(string), "ambiguous") {
		t.Errorf("call 2: cache miss — expected ambiguous error, got: %v", errMsg)
	}
}

// TestResolveProjectParam_AmbiguousWithCwdScopedCached verifies that
// ambiguous errors with cwd present are cached under the cwd-specific
// composite key. A caller with a DIFFERENT cwd is not affected (the
// scoped key isolates results). After cache invalidation, the stale
// error is evicted and a fresh DB lookup produces the current result.
func TestResolveProjectParam_AmbiguousWithCwdScopedCached(t *testing.T) {
	h, store := mustHandler(t)

	for _, p := range []string{"/tmp/a/dupname", "/tmp/b/dupname"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// Call 1: cwd doesn't match any candidate → ambiguous error, cached scoped.
	params := map[string]any{"project": "dupname", "_cwd": "/unrelated"}
	resolved := h.resolveProjectParam(params)
	if _, ok := resolved["_project_error"]; !ok {
		t.Fatalf("call 1: expected _project_error, got project=%v", resolved["project"])
	}

	// Delete sessions — scoped cache still holds the old ambiguous error.
	if _, err := store.DB().Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}

	// Call 2: same cwd → gets CACHED ambiguous error (scoped, correct).
	params = map[string]any{"project": "dupname", "_cwd": "/unrelated"}
	resolved = h.resolveProjectParam(params)
	if _, ok := resolved["_project_error"]; !ok {
		t.Errorf("call 2: scoped cache miss — expected cached ambiguous error, got project=%v", resolved["project"])
	}

	// Call 3: DIFFERENT cwd → misses the scoped cache, hits DB.
	params = map[string]any{"project": "dupname", "_cwd": "/other"}
	resolved = h.resolveProjectParam(params)
	if _, ok := resolved["_project_error"]; ok {
		t.Errorf("call 3: scoped cache leaked to different cwd — expected passthrough, got error")
	}

	// Invalidate — both the scoped error and any other entries are cleared.
	h.InvalidateProjectCache()

	// Call 4: after invalidation, same cwd → fresh DB lookup, 0 candidates. The name
	// resolves to nothing and the cwd does not carry it, so the value is empty and
	// marked unresolved: nothing may be written under a basename (YM-300).
	params = map[string]any{"project": "dupname", "_cwd": "/unrelated"}
	resolved = h.resolveProjectParam(params)
	if _, ok := resolved["_project_error"]; ok {
		t.Errorf("call 4: stale error survived invalidation")
	}
	if resolved["project"] != "" {
		t.Errorf("call 4: expected the unresolved empty value after invalidation, got %q", resolved["project"])
	}
	if resolved["_project_unresolved"] != "dupname" {
		t.Errorf("call 4: expected the unresolved marker to name %q, got %v", "dupname", resolved["_project_unresolved"])
	}
}

// TestInvalidateProjectCache verifies that after cache invalidation,
// a previously cached result is re-queried from the DB.
func TestInvalidateProjectCache(t *testing.T) {
	h, store := mustHandler(t)

	// Cache a unique short name resolution
	if err := store.UpsertSession(&models.Session{
		ID: "sess-inv", Project: "/tmp/cachedproj",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	params := map[string]any{"project": "cachedproj", "_cwd": "/tmp/anywhere"}
	resolved := h.resolveProjectParam(params)
	if resolved["project"] != "/tmp/cachedproj" {
		t.Fatalf("expected /tmp/cachedproj, got %q", resolved["project"])
	}

	// Invalidate the cache
	h.InvalidateProjectCache()

	// Delete the session → next query must hit DB, not cache
	if _, err := store.DB().Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}

	// After invalidation + DB change: the fresh lookup finds nothing, and the cwd
	// does not carry the name, so the value is empty and marked unresolved.
	params = map[string]any{"project": "cachedproj", "_cwd": "/tmp/anywhere"}
	resolved = h.resolveProjectParam(params)
	if resolved["project"] == "/tmp/cachedproj" {
		t.Errorf("cache was not invalidated — returned stale /tmp/cachedproj after DB delete")
	}
	if resolved["project"] != "" {
		t.Errorf("expected the unresolved empty value after invalidation + DB delete, got %q", resolved["project"])
	}
	if resolved["_project_unresolved"] != "cachedproj" {
		t.Errorf("expected the unresolved marker to name %q, got %v", "cachedproj", resolved["_project_unresolved"])
	}
}

// TestResolveProjectParam_TwoCwdSameBasename_CachePoisoning is the
// orchestrator-specified regression test. It proves that a cwd-tiebreaker
// resolution must NOT poison the global bare-basename cache, leaking project
// A to caller B.
func TestResolveProjectParam_TwoCwdSameBasename_CachePoisoning(t *testing.T) {
	h, store := mustHandler(t)

	for _, p := range []string{"/tmp/a/dupname", "/tmp/b/dupname"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// Caller A: resolves dupname with cwd /tmp/a/dupname — cwd tiebreaker yields a.
	paramsA := map[string]any{"project": "dupname", "_cwd": "/tmp/a/dupname"}
	resolvedA := h.resolveProjectParam(paramsA)
	if resolvedA["project"] != "/tmp/a/dupname" {
		t.Fatalf("caller A: expected /tmp/a/dupname, got %q", resolvedA["project"])
	}

	// Caller B: different cwd /tmp/b/dupname — must get b, not a.
	paramsB := map[string]any{"project": "dupname", "_cwd": "/tmp/b/dupname"}
	resolvedB := h.resolveProjectParam(paramsB)
	if errMsg, ok := resolvedB["_project_error"]; ok {
		t.Fatalf("caller B: unexpected error: %v", errMsg)
	}
	if resolvedB["project"] != "/tmp/b/dupname" {
		t.Errorf("cache-poisoning: caller B got %q, expected /tmp/b/dupname (value was leaked from caller A's cwd-specific resolution via bare-basename cache key)", resolvedB["project"])
	}
}

// TestResolveProjectParam_TransientDBErrorNotCached verifies that a
// non-ambiguous DB error (e.g. I/O failure, closed connection) is never
// cached. Only AmbiguousProjectError with cwd=="" is cacheable among
// error results; everything else must hit the DB on retry.
func TestResolveProjectParam_TransientDBErrorNotCached(t *testing.T) {
	h, store := mustHandler(t)

	// Seed a session so we have a resolvable project.
	if err := store.UpsertSession(&models.Session{
		ID: "sess-tmp", Project: "/tmp/knownproj",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Resolve once to populate cache for the known project.
	params := map[string]any{"project": "knownproj", "_cwd": "/tmp/knownproj"}
	resolved := h.resolveProjectParam(params)
	if resolved["project"] != "/tmp/knownproj" {
		t.Fatalf("expected /tmp/knownproj, got %q", resolved["project"])
	}

	// Close the underlying DB to simulate a transient failure.
	store.Close()

	// Resolve a DIFFERENT project — cache miss → DB hit → transient error.
	params2 := map[string]any{"project": "unknownproj", "_cwd": "/tmp/elsewhere"}
	resolved2 := h.resolveProjectParam(params2)
	if _, ok := resolved2["_project_error"]; !ok {
		t.Fatalf("expected transient DB error after store close, got project=%v", resolved2["project"])
	}

	// The transient error must NOT have been cached.
	h.projectCacheMu.RLock()
	_, globalCached := h.projectCache["unknownproj"]
	_, cwdCached := h.projectCache["unknownproj\x00/tmp/elsewhere"]
	h.projectCacheMu.RUnlock()
	if globalCached {
		t.Error("transient DB error was cached under global key — shouldCache logic incorrectly cached a non-ambiguous error")
	}
	if cwdCached {
		t.Error("transient DB error was cached under cwd-specific key — shouldCache logic incorrectly cached a non-ambiguous error")
	}
}

// TestResolveProjectParam_GenerationRaceGuard verifies that invalidation
// advances the cache generation and evicts previously resolved entries.
func TestResolveProjectParam_GenerationRaceGuard(t *testing.T) {
	h, store := mustHandler(t)

	if err := store.UpsertSession(&models.Session{
		ID: "sess-race", Project: "/tmp/raceproj",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Pre-seed the cache with a resolved value.
	params := map[string]any{"project": "raceproj", "_cwd": "/tmp/raceproj"}
	h.resolveProjectParam(params)

	// Verify cache is populated.
	h.projectCacheMu.RLock()
	_, hasEntry := h.projectCache["raceproj\x00/tmp/raceproj"]
	genBeforeInvalidate := h.projectCacheGen
	h.projectCacheMu.RUnlock()
	if !hasEntry {
		t.Fatal("cache not populated after first resolution")
	}

	// Invalidate — this must advance the generation counter.
	h.InvalidateProjectCache()

	// The cache must be cleared and generation must have advanced.
	h.projectCacheMu.RLock()
	_, stillHas := h.projectCache["raceproj\x00/tmp/raceproj"]
	genAfterInvalidate := h.projectCacheGen
	h.projectCacheMu.RUnlock()
	if stillHas {
		t.Error("cache entry survived invalidation")
	}
	if genAfterInvalidate <= genBeforeInvalidate {
		t.Errorf("generation did not advance: before=%d, after=%d", genBeforeInvalidate, genAfterInvalidate)
	}

	// After invalidation, a fresh lookup must hit the DB (not cache).
	params = map[string]any{"project": "raceproj", "_cwd": "/tmp/raceproj"}
	resolved := h.resolveProjectParam(params)
	if resolved["project"] != "/tmp/raceproj" {
		t.Errorf("after invalidation: expected /tmp/raceproj, got %q", resolved["project"])
	}
}

func TestResolveProjectParam_GenerationConcurrentAccess(t *testing.T) {
	h, store := mustHandler(t)

	if err := store.UpsertSession(&models.Session{
		ID: "sess-race-concurrent", Project: "/tmp/race-concurrent",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 250 {
				resolved := h.resolveProjectParam(map[string]any{
					"project": "race-concurrent",
					"_cwd":    "/tmp/race-concurrent",
				})
				if resolved["project"] != "/tmp/race-concurrent" {
					t.Errorf("unexpected project: %v", resolved["project"])
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for range 500 {
			h.InvalidateProjectCache()
		}
	}()

	close(start)
	workers.Wait()
}

// TestResolveProjectParam_SessionContextDisambiguation verifies that
// when a caller PID maps to a known session whose canonical project
// basename matches the requested short name, the canonical path is used
// directly — bypassing the ambiguous DB lookup entirely.
func TestResolveProjectParam_SessionContextDisambiguation(t *testing.T) {
	h, store := mustHandler(t)

	// Seed TWO sessions with same basename "shared" — ambiguous.
	for _, p := range []string{"/proj/a/shared", "/proj/b/shared"} {
		if err := store.UpsertSession(&models.Session{ID: "sess-" + p, Project: p}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	// Register a caller PID that maps to session /proj/a/shared.
	h.Handle(Request{Method: "register_pid", Params: map[string]any{
		"session_id": "sess-/proj/a/shared", "pid": float64(77777),
	}})

	// Request with short name "shared" + caller PID → must resolve
	// to /proj/a/shared (the session's canonical project), not ambiguous.
	params := map[string]any{
		"project":     "shared",
		"_caller_pid": float64(77777),
	}
	resolved := h.resolveProjectParam(params)
	if errMsg, ok := resolved["_project_error"]; ok {
		t.Fatalf("session context disambiguation failed: %v", errMsg)
	}
	if resolved["project"] != "/proj/a/shared" {
		t.Errorf("expected /proj/a/shared from session context, got %q", resolved["project"])
	}

	// Explicit full path must NOT be overridden by session context.
	params = map[string]any{
		"project":     "/proj/b/shared",
		"_caller_pid": float64(77777),
	}
	resolved = h.resolveProjectParam(params)
	if resolved["project"] != "/proj/b/shared" {
		t.Errorf("explicit full path %q was overridden by session context", resolved["project"])
	}

	// Different basename must NOT be overridden by session context.
	params = map[string]any{
		"project":     "unrelated",
		"_caller_pid": float64(77777),
	}
	resolved = h.resolveProjectParam(params)
	// "unrelated" doesn't match any candidate → passthrough or ambiguous
	// but must NOT return the session's /proj/a/shared.
	if resolved["project"] == "/proj/a/shared" {
		t.Error("session context leaked to unrelated basename")
	}
}

func TestResolveProjectParam_ExplicitSessionCacheIsolation(t *testing.T) {
	h, store := mustHandler(t)

	for _, session := range []models.Session{
		{ID: "opencode:session-a", Project: "/proj/a/shared"},
		{ID: "opencode:session-b", Project: "/proj/b/shared"},
	} {
		if err := store.UpsertSession(&session); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	first := h.resolveProjectParam(map[string]any{
		"project":     "shared",
		"_cwd":        "/stale/shared",
		"_session_id": "opencode:session-a",
	})
	if first["project"] != "/proj/a/shared" {
		t.Fatalf("first session: got %v, want /proj/a/shared", first["project"])
	}

	second := h.resolveProjectParam(map[string]any{
		"project":     "shared",
		"_cwd":        "/stale/shared",
		"_session_id": "opencode:session-b",
	})
	if second["project"] != "/proj/b/shared" {
		t.Fatalf("second session reused first session cache: got %v, want /proj/b/shared", second["project"])
	}
}

// TestResolveProjectParam_GlobalActiveSessionNotTrusted verifies that
// the global active-session fallback (activeSessionID, active_session_opencode)
// is NEVER used for project disambiguation. A caller with no PID mapping
// and no explicit _session_id must receive AmbiguousProjectError even if
// the global active session happens to have a matching basename.
func TestResolveProjectParam_GlobalActiveSessionNotTrusted(t *testing.T) {
	h, store := mustHandler(t)

	// Seed a session with basename "shared" — the global active session.
	if err := store.UpsertSession(&models.Session{
		ID: "sess-/proj/b/shared", Project: "/proj/b/shared",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Seed ANOTHER session with same basename "shared" — ambiguous.
	if err := store.UpsertSession(&models.Session{
		ID: "sess-/proj/a/shared", Project: "/proj/a/shared",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Set the global active session to /proj/b/shared (simulates a
	// different session having been active recently).
	h.activeSessionMu.Lock()
	h.activeSessionID = "sess-/proj/b/shared"
	h.activeSessionMu.Unlock()

	// Caller has no PID mapping and no explicit _session_id.
	// The request for short "shared" MUST remain ambiguous —
	// NOT silently route to /proj/b/shared via global active-session.
	params := map[string]any{"project": "shared"}
	resolved := h.resolveProjectParam(params)
	if _, ok := resolved["_project_error"]; !ok {
		t.Errorf("global active-session was trusted for disambiguation: silently routed to %q", resolved["project"])
	}
}
