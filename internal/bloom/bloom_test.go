package bloom

import (
	"testing"
)

func TestAddAndQuery(t *testing.T) {
	mgr := New()

	mgr.AddSession("s1", []string{"nginx", "config", "port", "8080", "deployment"})
	mgr.AddSession("s2", []string{"docker", "compose", "redis", "deployment"})
	mgr.AddSession("s3", []string{"php", "migration", "database"})

	// "nginx" should match s1 only
	matches := mgr.MayContain("nginx")
	if !contains(matches, "s1") {
		t.Error("expected s1 to match 'nginx'")
	}
	if contains(matches, "s3") {
		t.Error("s3 should not match 'nginx'")
	}

	// "deployment" should match s1 and s2
	matches = mgr.MayContain("deployment")
	if len(matches) < 2 {
		t.Errorf("expected at least 2 matches for 'deployment', got %d", len(matches))
	}

	// "nonexistent" should match nothing
	matches = mgr.MayContain("zzz_nonexistent_zzz")
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for nonexistent term, got %d", len(matches))
	}
}

func TestMayContainMultiple(t *testing.T) {
	mgr := New()

	mgr.AddSession("s1", []string{"nginx", "config", "port"})
	mgr.AddSession("s2", []string{"nginx", "docker", "deploy"})
	mgr.AddSession("s3", []string{"php", "artisan", "migrate"})

	// "nginx" + "docker" should narrow to s2
	matches := mgr.MayContainAll([]string{"nginx", "docker"})
	if len(matches) != 1 || matches[0] != "s2" {
		t.Errorf("expected [s2], got %v", matches)
	}
}

func TestSessionCount(t *testing.T) {
	mgr := New()
	if mgr.SessionCount() != 0 {
		t.Error("empty manager should have 0 sessions")
	}

	mgr.AddSession("s1", []string{"a", "b"})
	mgr.AddSession("s2", []string{"c"})
	if mgr.SessionCount() != 2 {
		t.Errorf("expected 2, got %d", mgr.SessionCount())
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// Incremental indexing appends terms to a session that is already tracked.
// AddTerms must merge into the existing filter instead of replacing it,
// otherwise terms from the already-indexed part of the session disappear.
func TestAddTermsMergesIntoExistingFilter(t *testing.T) {
	m := New()
	m.AddSession("sess-1", []string{"alpha term"})

	m.AddTerms("sess-1", []string{"beta term"})

	if got := m.MayContain("alpha"); len(got) != 1 || got[0] != "sess-1" {
		t.Errorf("earlier term lost after AddTerms: %v", got)
	}
	if got := m.MayContain("beta"); len(got) != 1 || got[0] != "sess-1" {
		t.Errorf("new term not registered: %v", got)
	}
}

func TestAddTermsCreatesFilterWhenAbsent(t *testing.T) {
	m := New()

	m.AddTerms("sess-new", []string{"gamma term"})

	if got := m.MayContain("gamma"); len(got) != 1 || got[0] != "sess-new" {
		t.Errorf("AddTerms did not create a filter: %v", got)
	}
	if m.SessionCount() != 1 {
		t.Errorf("session count: got %d, want 1", m.SessionCount())
	}
}

// AddSession stays a full replace — reindexing a session must not accumulate
// stale terms from a previous version of the file.
func TestAddSessionStillReplaces(t *testing.T) {
	m := New()
	m.AddSession("sess-2", []string{"older token"})

	m.AddSession("sess-2", []string{"newer token"})

	if got := m.MayContain("older"); len(got) != 0 {
		t.Errorf("AddSession did not replace: %v", got)
	}
	if got := m.MayContain("newer"); len(got) != 1 {
		t.Errorf("new terms missing: %v", got)
	}
}

// Batch-level equivalence: repeated AddTerms calls must cover the same terms
// as a single AddSession with the concatenated term list.
func TestAddTermsEqualsBatchAddSession(t *testing.T) {
	parts := [][]string{
		{"first chunk"},
		{"second chunk"},
		{"third chunk"},
	}

	incremental := New()
	incremental.AddSession("s", parts[0])
	for _, p := range parts[1:] {
		incremental.AddTerms("s", p)
	}

	baseline := New()
	baseline.AddSession("s", []string{"first chunk", "second chunk", "third chunk"})

	for _, term := range []string{"first", "chunk", "second", "third"} {
		inc := incremental.MayContain(term)
		base := baseline.MayContain(term)
		if len(inc) != len(base) {
			t.Errorf("term %q: incremental %v vs baseline %v", term, inc, base)
		}
	}
}

// Pins the documented seeding contract: a filter that AddTerms has to create
// itself covers only the terms it was given. This matters after a daemon restart,
// where the in-memory filters are gone and only the appended terms arrive — a
// session can then be a false negative for terms indexed before the restart.
// Callers that need exact coverage must seed the session with its full term set.
func TestAddTermsOnUnseededFilterOnlyCoversGivenTerms(t *testing.T) {
	m := New()

	m.AddTerms("sess-restart", []string{"appended-only term"})

	if got := m.MayContain("appended-only"); len(got) != 1 {
		t.Errorf("given term missing: %v", got)
	}
	if got := m.MayContain("term-indexed-before-the-restart"); len(got) != 0 {
		t.Errorf("unseeded filter unexpectedly matches a term it never saw: %v", got)
	}
}
