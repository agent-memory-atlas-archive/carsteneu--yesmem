package proxy

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRulesInjectAfterCollapse_OnlyOnce(t *testing.T) {
	s := &Server{}

	// First collapse injection should return rules
	s.rulesCache = map[string]rulesCacheEntry{"proj": {block: "test rules", fetchedAt: time.Now()}}
	result := s.rulesInjectAfterCollapse("thread1", "proj")
	if result == "" {
		t.Fatal("first collapse injection should return rules block")
	}

	// Second collapse injection (same thread) should be blocked
	result = s.rulesInjectAfterCollapse("thread1", "proj")
	if result != "" {
		t.Fatalf("second collapse injection should be empty, got: %s", result)
	}

	// Third collapse — still blocked
	result = s.rulesInjectAfterCollapse("thread1", "proj")
	if result != "" {
		t.Fatal("third collapse injection should still be blocked")
	}
}

func TestRulesInjectAfterCollapse_ResetByNormalInject(t *testing.T) {
	s := &Server{}
	s.rulesCache = map[string]rulesCacheEntry{"proj": {block: "test rules", fetchedAt: time.Now()}}

	// Collapse injection
	result := s.rulesInjectAfterCollapse("thread1", "proj")
	if result == "" {
		t.Fatal("first collapse should inject")
	}

	// Normal rulesInject call — sets baseline (first call path)
	result = s.rulesInject("thread1", 100000, "proj")
	// First normal call sets baseline, returns ""
	if result != "" {
		t.Fatal("first normal call should set baseline")
	}

	// Next collapse should work again (flag was reset by normal inject)
	result = s.rulesInjectAfterCollapse("thread1", "proj")
	if result == "" {
		t.Fatal("collapse after normal inject should inject again")
	}
}

func TestRulesInjectAfterCollapse_DifferentThreads(t *testing.T) {
	s := &Server{}
	s.rulesCache = map[string]rulesCacheEntry{"proj": {block: "test rules", fetchedAt: time.Now()}}

	// Collapse on thread1
	result := s.rulesInjectAfterCollapse("thread1", "proj")
	if result == "" {
		t.Fatal("thread1 first collapse should inject")
	}

	// Collapse on thread2 should still work (independent)
	result = s.rulesInjectAfterCollapse("thread2", "proj")
	if result == "" {
		t.Fatal("thread2 first collapse should inject independently")
	}

	// thread1 still blocked
	result = s.rulesInjectAfterCollapse("thread1", "proj")
	if result != "" {
		t.Fatal("thread1 second collapse should be blocked")
	}
}

func TestRulesInject_40kInterval(t *testing.T) {
	s := &Server{}
	s.rulesCache = map[string]rulesCacheEntry{"proj": {block: "test rules", fetchedAt: time.Now()}}

	// First call sets baseline
	result := s.rulesInject("thread1", 50000, "proj")
	if result != "" {
		t.Fatal("first call should set baseline, not inject")
	}

	// Under 40k delta — no injection
	result = s.rulesInject("thread1", 80000, "proj")
	if result != "" {
		t.Fatal("should not inject at 30k delta")
	}

	// Over 40k delta — inject
	result = s.rulesInject("thread1", 95000, "proj")
	if result == "" {
		t.Fatal("should inject at 45k delta")
	}
}

// rulesDaemonStub answers get_rules_block with a block that names the project,
// so a cross-project cache leak becomes visible in the returned content.
func rulesDaemonStub(t *testing.T) *Server {
	t.Helper()
	return fakeDaemon(t, func(method string, params map[string]any) string {
		if method != "get_rules_block" {
			return ""
		}
		project, _ := params["project"].(string)
		b, _ := json.Marshal(map[string]any{"content": "rules of " + project, "exists": true})
		return string(b)
	})
}

// YM-26: the rules cache lived in one string per proxy process, so the first
// project's block was served to every other project.
func TestRulesBlock_KeyedByProject(t *testing.T) {
	s := rulesDaemonStub(t)

	if got := s.getRulesBlock("/tmp/proj-a"); got != "rules of /tmp/proj-a" {
		t.Fatalf("getRulesBlock(proj-a) = %q, want its own block", got)
	}
	if got := s.getRulesBlock("/tmp/proj-b"); got != "rules of /tmp/proj-b" {
		t.Fatalf("getRulesBlock(proj-b) = %q — project A's rules leaked into project B", got)
	}
	if got := s.getRulesBlock("/tmp/proj-a"); got != "rules of /tmp/proj-a" {
		t.Fatalf("getRulesBlock(proj-a) after B = %q, want its own block again", got)
	}
}

// The cache exists to keep the daemon call out of the hot path.
func TestRulesBlock_CachedWithinTTL(t *testing.T) {
	var calls int
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method != "get_rules_block" {
			return ""
		}
		calls++
		b, _ := json.Marshal(map[string]any{"content": "rules", "exists": true})
		return string(b)
	})

	s.getRulesBlock("/tmp/proj-a")
	s.getRulesBlock("/tmp/proj-a")

	if calls != 1 {
		t.Fatalf("daemon calls = %d, want 1 (second read served from cache)", calls)
	}
}

// YM-26: the block was cached for the whole process lifetime and
// invalidateRulesCache was never called, so a CLAUDE.md change never reached
// the next injection. An entry older than rulesCacheTTL is refetched.
func TestRulesBlock_RefetchedAfterTTL(t *testing.T) {
	s := rulesDaemonStub(t)
	if got := s.getRulesBlock("/tmp/proj-a"); got != "rules of /tmp/proj-a" {
		t.Fatalf("initial fetch = %q", got)
	}

	s.rulesMu.Lock()
	s.rulesCache["/tmp/proj-a"] = rulesCacheEntry{
		block:     "stale",
		fetchedAt: time.Now().Add(-2 * rulesCacheTTL),
	}
	s.rulesMu.Unlock()

	if got := s.getRulesBlock("/tmp/proj-a"); got != "rules of /tmp/proj-a" {
		t.Fatalf("expired entry served %q, want a refetch", got)
	}
}

// switchingDaemonStub answers get_rules_block until broken is set, so a test
// can simulate a daemon outage inside the injection interval.
func switchingDaemonStub(t *testing.T) (*Server, *bool) {
	t.Helper()
	broken := false
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method != "get_rules_block" || broken {
			return ""
		}
		b, _ := json.Marshal(map[string]any{"content": "rules", "exists": true})
		return string(b)
	})
	return s, &broken
}

// A daemon outage at the injection moment must not swallow the whole interval:
// the mark only moves forward after a successful fetch.
func TestRulesInject_RetriesAfterFetchFailure(t *testing.T) {
	s, broken := switchingDaemonStub(t)
	const threadID = "tid-rules-outage"

	if got := s.rulesInject(threadID, 5000, "/tmp/proj"); got != "" {
		t.Fatalf("first call should only set the baseline, got %q", got)
	}

	*broken = true
	if got := s.rulesInject(threadID, 50000, "/tmp/proj"); got != "" {
		t.Fatalf("a failed fetch must not inject, got %q", got)
	}

	*broken = false
	if got := s.rulesInject(threadID, 50000, "/tmp/proj"); got == "" {
		t.Fatal("interval was consumed by the failed fetch — next request stayed silent")
	}
}

// Same guarantee on the collapse path: the once-per-collapse claim is released
// when the fetch fails, so the reminder is not muted until the next collapse.
func TestRulesInjectAfterCollapse_RetriesAfterFetchFailure(t *testing.T) {
	s, broken := switchingDaemonStub(t)
	const threadID = "tid-rules-collapse-outage"

	*broken = true
	if got := s.rulesInjectAfterCollapse(threadID, "/tmp/proj"); got != "" {
		t.Fatalf("a failed fetch must not inject, got %q", got)
	}

	*broken = false
	if got := s.rulesInjectAfterCollapse(threadID, "/tmp/proj"); got == "" {
		t.Fatal("collapse claim was not released after the failed fetch")
	}
}

// The prune on write is what keeps the map bounded by one TTL window.
func TestRulesBlock_ExpiredEntriesPrunedOnWrite(t *testing.T) {
	s := rulesDaemonStub(t)

	s.rulesMu.Lock()
	s.rulesCache = map[string]rulesCacheEntry{
		"/tmp/gone": {block: "old rules", fetchedAt: time.Now().Add(-2 * rulesCacheTTL)},
	}
	s.rulesMu.Unlock()

	s.getRulesBlock("/tmp/fresh")

	s.rulesMu.RLock()
	_, survived := s.rulesCache["/tmp/gone"]
	s.rulesMu.RUnlock()
	if survived {
		t.Fatal("expired entry survived a cache write")
	}
}

// The mark is re-checked after the fetch, so a second request that passed the
// gate while the first was fetching must not inject the block again. The stub
// plays the role of the concurrent winner by advancing the mark during the
// fetch.
func TestRulesInject_SkipsSecondInjectionWhenMarkMovedDuringFetch(t *testing.T) {
	const threadID = "tid-rules-single-flight"
	var srv *Server
	srv = fakeDaemon(t, func(method string, params map[string]any) string {
		if method != "get_rules_block" {
			return ""
		}
		srv.rulesMu.Lock()
		srv.rulesTokenCount[threadID] = 99999
		srv.rulesMu.Unlock()
		b, _ := json.Marshal(map[string]any{"content": "rules", "exists": true})
		return string(b)
	})

	if got := srv.rulesInject(threadID, 5000, "/tmp/proj"); got != "" {
		t.Fatalf("first call should only set the baseline, got %q", got)
	}
	if got := srv.rulesInject(threadID, 50000, "/tmp/proj"); got != "" {
		t.Fatalf("mark moved during the fetch, so this request must stay silent, got %q", got)
	}
}
