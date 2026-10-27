package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// YM-244 [AGT-MA33] key 1: max_turns was a spawn parameter that only reached the
// backend's argv. The agent row carried no column for it, so nothing survived a
// crash: respawnAgent handed spawn_agent no max_turns, and the replacement came
// back on the daemon's config default (or on the CLI's own default).
func TestAgentsTableHasMaxTurnsColumnLos55a(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query(`PRAGMA table_info(agents)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(agents): %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "max_turns" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if !found {
		t.Fatal("agents has no max_turns column — a respawn cannot inherit the spawn-time turn budget")
	}
}

// The value has to survive the store round trip, not just the in-memory struct:
// respawnAgent reads the crashed row back from the store and hands its MaxTurns
// to spawn_agent.
func TestAgentMaxTurnsRoundTripLos55a(t *testing.T) {
	s := newTestStore(t)

	if err := s.AgentCreate(Agent{
		ID: "agent-20261020-01", Project: "los55a-proj", Section: "los55a-sec",
		Status: "running", MaxTurns: 42,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	got, err := s.AgentGet("agent-20261020-01")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if got.MaxTurns != 42 {
		t.Errorf("AgentGet max_turns = %d, want 42", got.MaxTurns)
	}

	bySection, err := s.AgentGetBySection("los55a-proj", "los55a-sec")
	if err != nil {
		t.Fatalf("AgentGetBySection: %v", err)
	}
	if bySection.MaxTurns != 42 {
		t.Errorf("AgentGetBySection max_turns = %d, want 42", bySection.MaxTurns)
	}

	list, err := s.AgentList("los55a-proj")
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(list) != 1 || list[0].MaxTurns != 42 {
		t.Errorf("AgentList = %+v, want one row with max_turns 42", list)
	}
}

// --- YM-244 [AGT-MA33] keys 2, 4 and 5 ---

// key 2: a cascade closed the children's rows but left pid and sock_path
// standing. The closed row then advertised a process nobody owned (AgentGetByPID,
// the stop path and the paused-row guards all read the pair), and a recycled pid
// could be signalled as if it were the child's.
func TestAgentCascadeStop_ClearsProcessBindingLos55a(t *testing.T) {
	s := newTestStore(t)

	if err := s.AgentCreate(Agent{
		ID: "agent-20261020-10", Project: "los55a-proj", Section: "root",
		SessionID: "parent-session", Status: "running", PID: 4001,
	}); err != nil {
		t.Fatalf("AgentCreate parent: %v", err)
	}
	if err := s.AgentCreate(Agent{
		ID: "agent-20261020-11", Project: "los55a-proj", Section: "child",
		SessionID: "child-session", CallerSession: "parent-session",
		Status: "running", PID: 4002, SockPath: "/run/child.sock",
	}); err != nil {
		t.Fatalf("AgentCreate child: %v", err)
	}

	stopped, err := s.AgentCascadeStop("parent-session")
	if err != nil {
		t.Fatalf("AgentCascadeStop: %v", err)
	}
	if len(stopped) != 1 {
		t.Fatalf("cascade returned %d rows, want 1", len(stopped))
	}
	// The returned snapshot is the caller's signal target, so it has to keep the
	// pid the process held when the row was closed.
	if stopped[0].PID != 4002 {
		t.Errorf("returned snapshot pid = %d, want 4002 (the caller signals this pid)", stopped[0].PID)
	}

	child, err := s.AgentGet("agent-20261020-11")
	if err != nil {
		t.Fatalf("AgentGet child: %v", err)
	}
	if child.Status != "stopped" {
		t.Errorf("child status = %q, want stopped", child.Status)
	}
	if child.PID != 0 {
		t.Errorf("child pid = %d, want 0 — a closed row must not name a process", child.PID)
	}
	if child.SockPath != "" {
		t.Errorf("child sock_path = %q, want empty — the socket belongs to a process that is ending", child.SockPath)
	}
	if child.StoppedAt == "" {
		t.Error("child stopped_at is empty, want a timestamp")
	}
}

// key 5: stop_agent of a row without a session id cascaded with "" as the root,
// and AgentCascadeStop matched every agent whose caller_session is empty — every
// root agent of the installation.
func TestAgentCascadeStop_EmptyRootStopsNothingLos55a(t *testing.T) {
	s := newTestStore(t)

	for _, row := range []Agent{
		{ID: "agent-20261020-20", Project: "los55a-proj", Section: "root-a", SessionID: "root-a-session", Status: "running", PID: 5001},
		{ID: "agent-20261020-21", Project: "los55a-proj", Section: "root-b", SessionID: "root-b-session", Status: "running", PID: 5002},
	} {
		if err := s.AgentCreate(row); err != nil {
			t.Fatalf("AgentCreate %s: %v", row.ID, err)
		}
	}

	stopped, err := s.AgentCascadeStop("")
	if err != nil {
		t.Fatalf("AgentCascadeStop(empty): %v", err)
	}
	if len(stopped) != 0 {
		t.Errorf("cascade with an empty root stopped %d agents (%v), want 0", len(stopped), stopped)
	}
	for _, id := range []string{"agent-20261020-20", "agent-20261020-21"} {
		a, err := s.AgentGet(id)
		if err != nil {
			t.Fatalf("AgentGet %s: %v", id, err)
		}
		if a.Status != "running" {
			t.Errorf("%s status = %q after an empty-root cascade, want running", id, a.Status)
		}
	}
}

// key 4: the exit tail's second disjunct (pid 0) exists for a spawn whose bind was
// refused. It also matched a row a stop had deliberately closed, so a late exit
// flipped "stopped" to "failed" and wrote an error onto a finished agent.
func TestAgentFinalizeExit_DoesNotOverwriteStoppedRowLos55a(t *testing.T) {
	s := newTestStore(t)

	if err := s.AgentCreate(Agent{
		ID: "agent-20261020-30", Project: "los55a-proj", Section: "stopped-by-operator",
		Status: "stopped", PID: 0, StoppedAt: "2026-10-20T10:00:00+02:00",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	written, err := s.AgentFinalizeExit("agent-20261020-30", 6001, map[string]any{
		"status": "failed",
		"error":  "exit tail arrived late",
	})
	if err != nil {
		t.Fatalf("AgentFinalizeExit: %v", err)
	}
	if written {
		t.Error("AgentFinalizeExit wrote over a stopped row, want it refused")
	}

	a, err := s.AgentGet("agent-20261020-30")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if a.Status != "stopped" {
		t.Errorf("status = %q, want stopped — the operator's stop was overwritten", a.Status)
	}
	if a.Error != "" {
		t.Errorf("error = %q, want empty — a closed row got a reason it never had", a.Error)
	}
}

// --- YM-244 [AGT-MA33] key 6 ---

// key 6: QuarantineSession matched on the session id alone. Crash recovery
// quarantines a failed agent's learnings by address, and a session id that also
// appears in another project quarantined that project's rows with it — the
// quarantine is a recall-wide write, so the scope has to travel with it.
func TestQuarantineSessionProjectScopeLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-shared"
	inAlpha := insertQuarantineLos10bLearning(t, s, "alpha rule", session, "/home/tester/memory/alpha", "")
	inBeta := insertQuarantineLos10bLearning(t, s, "beta rule", session, "/home/tester/memory/beta", "")

	n, err := s.QuarantineSessionInProject("/home/tester/memory/alpha", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Errorf("quarantined %d rows, want 1 (only the named project)", n)
	}

	alpha, err := s.GetLearning(inAlpha)
	if err != nil {
		t.Fatalf("GetLearning alpha: %v", err)
	}
	if quarantineStampLos55a(t, s, alpha.ID) == "" {
		t.Error("the named project's learning was not quarantined")
	}
	if stamp := quarantineStampLos55a(t, s, inBeta); stamp != "" {
		t.Errorf("a learning of the other project was quarantined with it (quarantined_at = %q)", stamp)
	}
}

// quarantineStampLos55a reads the quarantine marker of one row. The Learning model
// does not expose it, and the write is a bare column — reading the row is the only
// direct proof.
func quarantineStampLos55a(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var stamp sql.NullString
	if err := s.db.QueryRow(`SELECT quarantined_at FROM learnings WHERE id = ?`, id).Scan(&stamp); err != nil {
		t.Fatalf("read quarantined_at of learning %d: %v", id, err)
	}
	return stamp.String
}

// A learning row that carries no project at all stays reachable through its
// session id: crash recovery must not turn into a silent no-op because the ingest
// wrote no project.
func TestQuarantineSessionInProject_KeepsUnscopedRowsLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-noproject"
	id := insertQuarantineLos10bLearning(t, s, "row without a project", session, "", "")

	n, err := s.QuarantineSessionInProject("/home/tester/memory/alpha", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Fatalf("quarantined %d rows, want 1", n)
	}
	if quarantineStampLos55a(t, s, id) == "" {
		t.Error("a learning without a project claim was left unquarantined")
	}
}

// A legacy row can name a project without a canonical form (measured: 1120 such
// rows). The tolerance branch was written for rows that declare no project at all,
// so it must not widen to a row that names another project — that is exactly the
// cross-project write YM-244 is about.
func TestQuarantineSessionInProject_DoesNotSpareAProjectWithoutCanonicalLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-legacy"
	inAlpha := insertQuarantineLos10bLearning(t, s, "alpha rule", session, "/home/tester/memory/alpha", "")
	inBeta := insertQuarantineLos10bLearning(t, s, "legacy beta rule", session, "/home/tester/memory/beta", "")
	// The legacy shape: a project path, no canonical form.
	if _, err := s.db.Exec(`UPDATE learnings SET canonical_project = '' WHERE id = ?`, inBeta); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	n, err := s.QuarantineSessionInProject("/home/tester/memory/alpha", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Errorf("quarantined %d rows, want 1 — the row of the other project was taken along", n)
	}
	if quarantineStampLos55a(t, s, inAlpha) == "" {
		t.Error("the named project's learning was not quarantined")
	}
	if stamp := quarantineStampLos55a(t, s, inBeta); stamp != "" {
		t.Errorf("a legacy row naming another project was quarantined (quarantined_at = %q)", stamp)
	}
}

// A legacy row can name its project as a full path without a canonical form
// (measured: 592 rows for one repository). The caller knows only the name it was
// given, so the directory basename has to match too — otherwise crash recovery
// quarantines nothing for exactly those rows.
func TestQuarantineSessionInProject_MatchesAPathOnlyRowByBasenameLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-pathonly"
	inAlpha := insertQuarantineLos10bLearning(t, s, "path-only alpha rule", session, "/home/tester/memory/alpha_x", "")
	// The legacy shape: full path, no canonical form.
	if _, err := s.db.Exec(`UPDATE learnings SET canonical_project = '' WHERE id = ?`, inAlpha); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	n, err := s.QuarantineSessionInProject("alpha_x", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Fatalf("quarantined %d rows, want 1 — the path-only row was missed", n)
	}
}

// The basename match must not become a wildcard: a project name with "_" would
// otherwise match more paths than its own.
func TestQuarantineSessionInProject_BasenameIsNotAWildcardLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-wildcard"
	other := insertQuarantineLos10bLearning(t, s, "other project", session, "/home/tester/memory/alphaXx", "")
	if _, err := s.db.Exec(`UPDATE learnings SET canonical_project = '' WHERE id = ?`, other); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	n, err := s.QuarantineSessionInProject("alpha_x", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 0 {
		t.Errorf("quarantined %d rows: the '_' in the project name acted as a LIKE wildcard", n)
	}
}

// YM-244 (orch-Probe 2): zwei REPOSITORIES mit gleichem Basename sind zwei
// Projekte. Der Scope darf den expliziten absoluten Pfad nicht über den
// Basename umgehen — genau diese Fuzzy-Gleichheit schließt models.ProjectMatches
// aus ("suffix/basename caused collisions between distinct repos").
func TestQuarantineSessionInProject_SameBasenameIsNotTheSameProjectLos55a(t *testing.T) {
	for _, tc := range []struct {
		name           string
		clearCanonical bool
	}{
		{"canonical set", false},
		{"path only, canonical cleared", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			base := t.TempDir()
			first := filepath.Join(base, "first", "repo")
			second := filepath.Join(base, "second", "repo")
			const session = "sess-los55a-samename"

			inFirst := insertQuarantineLos10bLearning(t, s, "first repo rule", session, first, "")
			inSecond := insertQuarantineLos10bLearning(t, s, "second repo rule", session, second, "")
			if tc.clearCanonical {
				for _, id := range []int64{inFirst, inSecond} {
					if _, err := s.db.Exec(`UPDATE learnings SET canonical_project = '' WHERE id = ?`, id); err != nil {
						t.Fatalf("fixture: %v", err)
					}
				}
			}

			n, err := s.QuarantineSessionInProject(first, session)
			if err != nil {
				t.Fatalf("quarantine: %v", err)
			}
			if n != 1 {
				t.Errorf("quarantined %d rows, want 1 (only first/repo)", n)
			}
			if quarantineStampLos55a(t, s, inFirst) == "" {
				t.Error("the named repository's row was not quarantined")
			}
			if stamp := quarantineStampLos55a(t, s, inSecond); stamp != "" {
				t.Errorf("a row of the other repository with the same basename was quarantined (quarantined_at = %q)", stamp)
			}
		})
	}
}

// Eine Zeile aus einem Worktree desselben Repositories gehört zum Scope, ohne dass
// dafür der Basename über Repository-Grenzen hinweg greift.
func TestQuarantineSessionInProject_MatchesItsOwnWorktreesLos55a(t *testing.T) {
	s := newTestStore(t)

	base := t.TempDir()
	root := filepath.Join(base, "repo")
	worktree := filepath.Join(root, ".worktrees", "feature")
	otherRoot := filepath.Join(base, "other")
	const session = "sess-los55a-worktree"

	inRoot := insertQuarantineLos10bLearning(t, s, "root row", session, root, "")
	inWorktree := insertQuarantineLos10bLearning(t, s, "worktree row", session, worktree, "")
	inOther := insertQuarantineLos10bLearning(t, s, "other project", session, otherRoot, "")

	n, err := s.QuarantineSessionInProject(root, session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 2 {
		t.Errorf("quarantined %d rows, want 2 (root and its worktree)", n)
	}
	for _, id := range []int64{inRoot, inWorktree} {
		if quarantineStampLos55a(t, s, id) == "" {
			t.Errorf("learning %d of the repository was not quarantined", id)
		}
	}
	if stamp := quarantineStampLos55a(t, s, inOther); stamp != "" {
		t.Errorf("a row of another project was quarantined (quarantined_at = %q)", stamp)
	}
}

// Ein Aufrufer mit Kurznamen (kein Pfad) muss auch Zeilen erreichen, deren project
// die Kurzform SELBST ist — sonst ist die Quarantäne für genau diese Klasse ein
// stiller No-Op (Review-Befund zum Projektgrenzen-Fix).
func TestQuarantineSessionInProject_ShortNameMatchesAShortNameRowLos55a(t *testing.T) {
	s := newTestStore(t)

	const session = "sess-los55a-shortrow"
	short := insertQuarantineLos10bLearning(t, s, "row keyed by the short name", session, "alpha_x", "")

	n, err := s.QuarantineSessionInProject("alpha_x", session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Fatalf("quarantined %d rows, want 1 — a row keyed by the short name itself was missed", n)
	}
	if quarantineStampLos55a(t, s, short) == "" {
		t.Error("the row keyed by the short name was not quarantined")
	}
}

// Beide Worktree-Layouts gehören zum Repository (models.worktreeMarkers): yesmem
// spawnt unter .worktrees/, anderes Tooling unter .claude/worktrees/.
func TestQuarantineSessionInProject_MatchesTheSecondWorktreeLayoutLos55a(t *testing.T) {
	s := newTestStore(t)

	base := t.TempDir()
	root := filepath.Join(base, "repo")
	nested := filepath.Join(root, ".claude", "worktrees", "wt")
	const session = "sess-los55a-wtclaude"

	id := insertQuarantineLos10bLearning(t, s, "nested worktree row", session, nested, "")

	n, err := s.QuarantineSessionInProject(root, session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if n != 1 {
		t.Fatalf("quarantined %d rows, want 1 — the .claude/worktrees layout was missed", n)
	}
	if quarantineStampLos55a(t, s, id) == "" {
		t.Error("the nested worktree row was not quarantined")
	}
}
