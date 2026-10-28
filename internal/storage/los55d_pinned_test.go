package storage

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

const (
	los55dProject = "/home/testuser/projects/myapp"
	los55dOther   = "/home/testuser/projects/other"
)

// YM-325 (3): a short name carries no path, so the view identity stayed empty and
// every path-bound pin was invisible to a caller that asked with the project name
// the CLI and the hooks use.
func TestLos55d_GetPinnedLearningsResolvesAShortNameView(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "los55dsess-app", los55dProject)

	seedPinNow(t, s, "permanent", los55dProject+"/internal", "subdirectory pin")
	seedPinNow(t, s, "permanent", "", "global pin")
	seedPinNow(t, s, "permanent", los55dOther, "sibling project pin")

	got := pinContents(t, s, "permanent", "myapp")
	for _, want := range []string{"subdirectory pin", "global pin"} {
		if !pinHasContent(got, want) {
			t.Errorf("%q not visible in the short-name view %q, got: %v", want, "myapp", got)
		}
	}
	if pinHasContent(got, "sibling project pin") {
		t.Errorf("sibling project pin leaked into the short-name view, got: %v", got)
	}
}

// YM-325 (2): a directory whose only session row is a subagent was no project home
// at all, so a pin written below it belonged to the ancestor and disappeared from
// the directory's own view.
func TestLos55d_OwningProjectCountsSubagentSessions(t *testing.T) {
	s := mustOpenFiles(t)
	const sub = "/home/testuser/projects/subagent-only"
	if err := s.UpsertSession(&models.Session{
		ID: "los55dsess-sub", Project: sub, ParentSessionID: "los55dsess-parent",
		AgentType: "general",
	}); err != nil {
		t.Fatalf("seed subagent session: %v", err)
	}

	cases := []struct{ dir, want string }{
		{sub, sub},
		{sub + "/deep", sub},
		{sub + "/deep/deeper", sub},
	}
	for _, c := range cases {
		if got := s.OwningProject(c.dir); got != c.want {
			t.Errorf("OwningProject(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// The same gap through the pin lookup: a pin written in a subdirectory of a
// subagent-only project must be visible from that project.
func TestLos55d_GetPinnedLearningsSeesPinBelowASubagentOnlyProject(t *testing.T) {
	s := mustOpenFiles(t)
	const sub = "/home/testuser/projects/subagent-only"
	if err := s.UpsertSession(&models.Session{
		ID: "los55dsess-sub", Project: sub, ParentSessionID: "los55dsess-parent",
		AgentType: "general",
	}); err != nil {
		t.Fatalf("seed subagent session: %v", err)
	}
	seedPinNow(t, s, "permanent", sub+"/deep", "pin below a subagent-only project")

	if !pinHasContent(pinContents(t, s, "permanent", sub), "pin below a subagent-only project") {
		t.Error("pin of a subdirectory is invisible in the project that owns it")
	}
}

// A worktree that ran nothing but subagents still belongs to the checkout above it:
// counting subagent rows as a home must not take the checkout's pins away from it.
func TestLos55d_SubagentOnlyWorktreeStillBelongsToItsCheckout(t *testing.T) {
	s := mustOpenFiles(t)
	const checkout = "/home/testuser/projects/repo"
	const worktree = checkout + "/.worktrees/feature/agent-supervision"

	seedSession(t, s, "los55dsess-checkout", checkout)
	if err := s.UpsertSession(&models.Session{
		ID: "los55dsess-agent", Project: worktree, ParentSessionID: "los55dsess-checkout",
		AgentType: "general",
	}); err != nil {
		t.Fatalf("seed subagent session: %v", err)
	}
	seedPinNow(t, s, "permanent", checkout, "checkout pin")
	seedPinNow(t, s, "permanent", worktree, "worktree pin")

	if got := s.OwningProject(worktree); got != checkout {
		t.Errorf("OwningProject(%q) = %q, want the checkout %q", worktree, got, checkout)
	}
	got := pinContents(t, s, "permanent", worktree)
	for _, want := range []string{"checkout pin", "worktree pin"} {
		if !pinHasContent(got, want) {
			t.Errorf("%q not visible in the worktree view, got: %v", want, got)
		}
	}
}

// YM-325 (1): a row that cannot be scanned was dropped without a trace, so a pin
// could vanish from every briefing and nothing said why.
func TestLos55d_GetPinnedLearningsLogsScanFailures(t *testing.T) {
	s := mustOpenFiles(t)
	if _, err := s.pinnedDB("permanent").Exec(
		`INSERT INTO pinned_learnings (project, session_id, content, source, created_at)
		 VALUES ('', '', 'unreadable pin', 'test', 'not-a-timestamp')`); err != nil {
		t.Fatalf("seed unreadable pin: %v", err)
	}
	seedPinNow(t, s, "permanent", "", "readable pin")

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	got := pinContents(t, s, "permanent", los55dProject)
	if !pinHasContent(got, "readable pin") {
		t.Errorf("the readable pin must still be returned, got: %v", got)
	}
	if !strings.Contains(buf.String(), "pinned_learnings") {
		t.Errorf("a skipped pin row was not logged, log output: %q", buf.String())
	}
}

// YM-325 (4): every legacy short name was resolved with its own scan of the
// sessions table on every turn. All names of one lookup resolve in one query.
func TestLos55d_LegacyProjectPathsResolveInOneBatch(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "los55dsess-app", los55dProject)
	seedSession(t, s, "los55dsess-other", los55dOther)
	seedSession(t, s, "los55dsess-second", "/home/other/myapp")
	seedSession(t, s, "los55dsess-tools", "/home/testuser/projects/tools/b")

	got := s.legacyProjectPaths([]string{"other", "myapp", "tools/b", "unknown", ""})

	if got["other"] != los55dOther {
		t.Errorf("unique short name resolved to %q, want %q", got["other"], los55dOther)
	}
	if got["tools/b"] != "/home/testuser/projects/tools/b" {
		t.Errorf("a stored name with a slash resolved to %q, want its path", got["tools/b"])
	}
	if _, ok := got["myapp"]; ok {
		t.Errorf("ambiguous short name resolved to %q, want no entry", got["myapp"])
	}
	if _, ok := got["unknown"]; ok {
		t.Errorf("unknown short name resolved to %q, want no entry", got["unknown"])
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty name resolved to %q, want no entry", got[""])
	}
}

// A store without its own runtime.db keeps session and permanent pins in one table.
// The widened purge clause must not reach the permanent rows there: they are not
// session pins and nothing would ever bring them back.
func TestLos55d_PurgeKeepsPermanentPinsOnACollapsedStore(t *testing.T) {
	s := mustOpen(t) // :memory:, one handle for both scopes
	old := time.Now().Add(-30 * 24 * time.Hour)
	seedPin(t, s, "permanent", "", "", "old permanent pin", old)
	seedPin(t, s, "session", "", "sess-a", "old attributed session pin", old)

	if _, err := s.PurgeSessionPinsBefore(time.Now().Add(-SessionPinMaxAge)); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var permanent, session int
	if err := s.pinnedDB("permanent").QueryRow(
		`SELECT COUNT(*) FROM pinned_learnings WHERE content = 'old permanent pin'`).Scan(&permanent); err != nil {
		t.Fatalf("count permanent pins: %v", err)
	}
	if err := s.pinnedDB("session").QueryRow(
		`SELECT COUNT(*) FROM pinned_learnings WHERE content = 'old attributed session pin'`).Scan(&session); err != nil {
		t.Fatalf("count session pins: %v", err)
	}
	if permanent != 1 {
		t.Errorf("permanent pin was purged on a collapsed store (%d rows left)", permanent)
	}
	if session != 0 {
		t.Errorf("attributed session pin must be purged, %d rows left", session)
	}
}

// YM-325 (5): the lookup read the whole pin table on every turn. The read scope has
// to be the set of rows that can belong to the view, and nothing outside it.
func TestLos55d_PinReadScopeSelectsOnlyReachableRows(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "los55dsess-app", los55dProject)
	seedSession(t, s, "los55dsess-other", los55dOther)

	seedPinNow(t, s, "permanent", los55dProject, "exact project pin")
	seedPinNow(t, s, "permanent", los55dProject+"/internal", "subdirectory pin")
	seedPinNow(t, s, "permanent", "myapp", "legacy short-name pin")
	seedPinNow(t, s, "permanent", "", "global pin")
	seedPinNow(t, s, "permanent", los55dOther+"/vendor/deep", "vendored clone pin")
	seedPinNow(t, s, "permanent", los55dOther, "sibling project pin")
	seedPinNow(t, s, "permanent", "/home/testuser/projects", "parent directory pin")

	scope := s.pinVisibilityFor(los55dProject)
	clause, args := pinReadScope(scope)
	rows, err := s.pinnedDB("permanent").Query(
		`SELECT content FROM pinned_learnings WHERE `+clause+` ORDER BY id`, args...)
	if err != nil {
		t.Fatalf("run read scope: %v", err)
	}
	defer rows.Close()
	var selected []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan selected pin: %v", err)
		}
		selected = append(selected, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate selected pins: %v", err)
	}

	for _, want := range []string{"exact project pin", "subdirectory pin", "legacy short-name pin", "global pin"} {
		if !pinHasContent(selected, want) {
			t.Errorf("read scope misses %q, selected: %v", want, selected)
		}
	}
	for _, unwanted := range []string{"vendored clone pin", "sibling project pin", "parent directory pin"} {
		if pinHasContent(selected, unwanted) {
			t.Errorf("read scope reads %q, which cannot be visible in this view", unwanted)
		}
	}

	// A global view asks for the global pins only.
	globalClause, globalArgs := pinReadScope(s.pinVisibilityFor(""))
	rows2, err := s.pinnedDB("permanent").Query(
		`SELECT content FROM pinned_learnings WHERE `+globalClause+` ORDER BY id`, globalArgs...)
	if err != nil {
		t.Fatalf("run global read scope: %v", err)
	}
	defer rows2.Close()
	var globalSelected []string
	for rows2.Next() {
		var content string
		if err := rows2.Scan(&content); err != nil {
			t.Fatalf("scan global pin: %v", err)
		}
		globalSelected = append(globalSelected, content)
	}
	if len(globalSelected) != 1 || globalSelected[0] != "global pin" {
		t.Errorf("global read scope selected %v, want only the global pin", globalSelected)
	}
}
