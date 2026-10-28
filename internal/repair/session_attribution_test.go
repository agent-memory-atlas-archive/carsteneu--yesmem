package repair

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"

	_ "modernc.org/sqlite"
)

// The repair reads two databases: yesmem.db holds the learnings, opencode.db
// holds the tool calls that wrote them. The fixtures recreate only the columns
// the repair touches, so a change in the queries shows up as a test failure
// rather than as a silent miss on the live data.

func fixtureYesmemDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yesmem.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open yesmem fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE learnings (
		id INTEGER PRIMARY KEY,
		content TEXT NOT NULL,
		session_id TEXT,
		project TEXT,
		project_source TEXT DEFAULT '',
		canonical_project TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		origin_tool TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create learnings: %v", err)
	}
	// The repair reads known project paths from the sessions table only; it is
	// the indexer's record, unlike a learning's caller-supplied project.
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, project TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	return db, path
}

func fixtureOpencodeDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open opencode fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, data TEXT NOT NULL, time_created INTEGER NOT NULL)`,
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create opencode fixture: %v", err)
		}
	}
	return db, path
}

func insertLearning(t *testing.T, db *sql.DB, id int64, content, session, project, projectSource, created string) {
	t.Helper()
	// canonical_project is what the insert path derives from project
	// (handler_learnings.go); the fixture reproduces it so the apply path is
	// tested against the column set production has.
	if _, err := db.Exec(
		`INSERT INTO learnings (id, content, session_id, project, project_source, canonical_project, created_at, origin_tool) VALUES (?, ?, ?, ?, ?, ?, ?, 'user')`,
		id, content, session, project, projectSource, models.CanonicalProject(project), created,
	); err != nil {
		t.Fatalf("insert learning %d: %v", id, err)
	}
}

// insertToolCall stores one assistant tool call the way opencode does: the part
// carries a name and a decoded input object.
func insertToolCall(t *testing.T, db *sql.DB, session, name string, input map[string]any) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	data := fmt.Sprintf(`{"content":[{"type":"tool","name":%q,"state":{"input":%s}}]}`, name, raw)
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, data, time_created) VALUES (?, ?, 'assistant', ?, 1)`,
		fmt.Sprintf("%s-%d", session, len(data)), session, data,
	); err != nil {
		t.Fatalf("insert tool call: %v", err)
	}
}

func insertSession(t *testing.T, db *sql.DB, sessionID, directory string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO session_v2 (id, directory) VALUES (?, ?)`, sessionID, directory); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// insertLearningRow inserts a learning with full control over the project
// columns, so a NULL project (what the v0.65 migration wrote) can be seeded.
func insertLearningRow(t *testing.T, db *sql.DB, id int64, content, session string, project any, canonical, projectSource, created string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO learnings (id, content, session_id, project, project_source, canonical_project, created_at, origin_tool) VALUES (?, ?, ?, ?, ?, ?, ?, 'user')`,
		id, content, session, project, projectSource, canonical, created,
	); err != nil {
		t.Fatalf("insert learning %d: %v", id, err)
	}
}

// gitRootWithWorktree returns a repository root and a worktree directory inside
// it, the layout a loop agent runs in.
func gitRootWithWorktree(t *testing.T, name string) (root, worktree string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "worktrees", name), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(root, ".worktrees", name)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(root, ".git", "worktrees", name)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, worktree
}

// A project the v0.65 migration blanked is rebuilt from the session that wrote
// the learning.
func TestPlanProposesProjectForBlankedRow(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	root, worktree := gitRootWithWorktree(t, "v2-release-notes")
	insertSession(t, odb, "ses_BLANK", worktree)
	insertLearningRow(t, ydb, 40, "note whose project was blanked", "opencode:ses_BLANK", nil, "v2-release-notes", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_BLANK", "yesmem_remember", map[string]any{"text": "note whose project was blanked"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	row := rep.Rows[0]
	if row.NewProject != root || row.NewProjectSource != "repo" {
		t.Fatalf("project %q → %q (source %q), want the session's repository root %q", row.OldProject, row.NewProject, row.NewProjectSource, root)
	}
	if row.Status != StatusRepair {
		t.Fatalf("status = %q, want %q (basis: %s)", row.Status, StatusRepair, row.Basis)
	}
	if _, err := Apply(ypath, rep); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var project, canonical string
	if err := ydb.QueryRow(`SELECT project, canonical_project FROM learnings WHERE id = 40`).Scan(&project, &canonical); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if project != root || canonical != models.CanonicalProject(root) {
		t.Fatalf("after apply: project %q canonical %q, want %q / %q", project, canonical, root, models.CanonicalProject(root))
	}
}

// insertKnownProject records a project the indexer saw, the only evidence the
// canonical fallback accepts.
func insertKnownProject(t *testing.T, db *sql.DB, sessionID, project string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sessions (id, project) VALUES (?, ?)`, sessionID, project); err != nil {
		t.Fatalf("insert known project: %v", err)
	}
}

// Without a session, the historical basename in canonical_project identifies
// the project when exactly one indexed path carries it.
func TestPlanFallsBackToCanonicalProject(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	_, opath := fixtureOpencodeDB(t)
	insertKnownProject(t, ydb, "ses_indexed", "/repo/wal")
	insertLearningRow(t, ydb, 41, "row without any tool call", "opencode:ses_GONE", nil, "wal", "explicit", "2026-10-15T10:00:00+02:00")
	insertLearningRow(t, ydb, 42, "already absolute row", "opencode:ses_OTHER", "/repo/other", "other", "explicit", "2026-10-15T10:00:00+02:00")

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byID := map[int64]Row{}
	for _, row := range rep.Rows {
		byID[row.ID] = row
	}
	got := byID[41]
	if got.NewProject != "/repo/wal" || got.Status != StatusRepair || got.NewProjectSource != "canonical" {
		t.Fatalf("row 41 = %+v, want the canonical_project fallback to /repo/wal with source canonical", got)
	}
	if other := byID[42]; other.NewProject != "" {
		t.Fatalf("row 42 = %+v, want no project proposal for an already absolute project", other)
	}
}

// Two indexed paths with the same basename make canonical_project worthless as
// evidence — the row stays untouched rather than being pointed at a guess.
func TestPlanLeavesAmbiguousCanonicalProjectAlone(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	_, opath := fixtureOpencodeDB(t)
	insertKnownProject(t, ydb, "ses_one", "/repo/one/wal")
	insertKnownProject(t, ydb, "ses_two", "/repo/two/wal")
	insertLearningRow(t, ydb, 43, "row with an ambiguous canonical project", "opencode:ses_GONE", nil, "wal", "explicit", "2026-10-15T10:00:00+02:00")

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	row := rep.Rows[0]
	if row.NewProject != "" || row.Status != StatusUnresolved {
		t.Fatalf("row = %+v, want no proposal for an ambiguous canonical project", row)
	}
}

// A blanked project with no evidence at all must not be reported as settled:
// the row still needs work and the report has to say so.
func TestPlanReportsBlankedProjectWithoutEvidence(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearningRow(t, ydb, 47, "blanked row whose session is known", "opencode:ses_KNOWN", nil, "wal", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_KNOWN", "yesmem_remember", map[string]any{"text": "blanked row whose session is known"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	row := rep.Rows[0]
	if row.Status != StatusUnresolved {
		t.Fatalf("row = %+v, want UNRESOLVED — a blank project without evidence is not settled", row)
	}
	if !strings.Contains(row.Basis, "project still unresolved") {
		t.Fatalf("basis = %q, want it to name the unresolved project", row.Basis)
	}
}

// A '.' project is as unusable as an empty one.
func TestPlanTreatsDotProjectAsMissing(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	root, worktree := gitRootWithWorktree(t, "dot-project")
	insertSession(t, odb, "ses_DOT", worktree)
	insertLearningRow(t, ydb, 46, "note with a dot project", "opencode:ses_DOT", ".", ".", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_DOT", "yesmem_remember", map[string]any{"text": "note with a dot project"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	row := rep.Rows[0]
	if row.NewProject != root {
		t.Fatalf("project %q → %q, want the session's repository root %q", row.OldProject, row.NewProject, root)
	}
}

// createLegacyPartTable adds the pre-v2 tool-call table. Tests that never call
// it also cover the graceful path for a database without that table.
func createLegacyPartTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, data TEXT NOT NULL, time_created INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create part table: %v", err)
	}
}

// insertLegacyToolCall stores a tool call the pre-v2 way: the row is the part
// itself and the tool is named in `tool`, not in `name`.
func insertLegacyToolCall(t *testing.T, db *sql.DB, session, name string, input map[string]any) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	data := fmt.Sprintf(`{"type":"tool","tool":%q,"state":{"status":"completed","input":%s}}`, name, raw)
	if _, err := db.Exec(
		`INSERT INTO part (id, message_id, session_id, data, time_created) VALUES (?, ?, ?, ?, 1)`,
		fmt.Sprintf("part-%s-%d", session, len(data)), "msg-"+session, session, data,
	); err != nil {
		t.Fatalf("insert legacy tool call: %v", err)
	}
}

// Sessions recorded before the v2 schema keep their tool calls in the part
// table; without reading it their learnings look unattributable.
func TestPlanReadsToolCallsFromTheLegacyPartTable(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	createLegacyPartTable(t, odb)
	insertLearning(t, ydb, 20, "note stored by an older session", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertLegacyToolCall(t, odb, "ses_LEGACY", "yesmem_remember", map[string]any{"text": "note stored by an older session"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].NewSession != "opencode:ses_LEGACY" {
		t.Fatalf("rows = %+v, want one proposal for opencode:ses_LEGACY", rep.Rows)
	}
	if rep.Rows[0].Status != StatusRepair {
		t.Fatalf("status = %q, want %q", rep.Rows[0].Status, StatusRepair)
	}
}

// Both generations are unioned: agreement keeps the match, disagreement makes
// it ambiguous.
func TestPlanUnionsBothStorageGenerations(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	createLegacyPartTable(t, odb)
	insertLearning(t, ydb, 21, "text seen by one session twice", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertLearning(t, ydb, 22, "text seen by two sessions", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_SAME", "yesmem_remember", map[string]any{"text": "text seen by one session twice"})
	insertLegacyToolCall(t, odb, "ses_SAME", "yesmem_remember", map[string]any{"text": "text seen by one session twice"})
	insertToolCall(t, odb, "ses_MSG", "yesmem_remember", map[string]any{"text": "text seen by two sessions"})
	insertLegacyToolCall(t, odb, "ses_PART", "yesmem_remember", map[string]any{"text": "text seen by two sessions"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byID := map[int64]Row{}
	for _, row := range rep.Rows {
		byID[row.ID] = row
	}
	if got := byID[21]; got.Status != StatusRepair || got.NewSession != "opencode:ses_SAME" {
		t.Errorf("row 21 = %+v, want a unique match on opencode:ses_SAME", got)
	}
	if got := byID[22]; got.Status != StatusUnresolved {
		t.Errorf("row 22 = %+v, want UNRESOLVED because the two sources disagree", got)
	}
}

func TestPlanFindsTheUniqueSessionFromTheToolCall(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 1, "patched-release.ts excludes the upstream wave", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_CORRECT", "yesmem_remember", map[string]any{
		"text": "patched-release.ts excludes the upstream wave", "category": "gotcha",
	})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rep.Rows), rep.Rows)
	}
	row := rep.Rows[0]
	if row.Status != StatusRepair {
		t.Fatalf("status = %q, want %q (basis: %s)", row.Status, StatusRepair, row.Basis)
	}
	if row.OldSession != "opencode:ses_WRONG" || row.NewSession != "opencode:ses_CORRECT" {
		t.Fatalf("session %q → %q, want opencode:ses_WRONG → opencode:ses_CORRECT", row.OldSession, row.NewSession)
	}
}

// A Code Mode call carries the same remember argument inside its JS source: the
// live misattributions (#101896/#101897) were written this way, so a matcher
// that only looks at yesmem_remember calls finds nothing.
func TestPlanMatchesRememberTextInsideCodeModeCall(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 7, "Fork release bodies differ upstream", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_CODE", "execute", map[string]any{
		"code": `await tools.yesmem["remember"]({"text":"Fork release bodies differ upstream","category":"gotcha"})`,
	})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].NewSession != "opencode:ses_CODE" {
		t.Fatalf("rows = %+v, want one proposal for opencode:ses_CODE", rep.Rows)
	}
}

// The same text in two sessions cannot be attributed: the row stays untouched
// rather than being pointed at a guess.
func TestPlanLeavesAmbiguousMatchesAlone(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 2, "shared sentence about the proxy", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_A", "yesmem_remember", map[string]any{"text": "shared sentence about the proxy"})
	insertToolCall(t, odb, "ses_B", "yesmem_remember", map[string]any{"text": "shared sentence about the proxy"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Status != StatusUnresolved || rep.Rows[0].NewSession != "" {
		t.Fatalf("rows = %+v, want one UNRESOLVED row", rep.Rows)
	}
}

func TestPlanLeavesUnmatchedRowsAlone(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	_, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 3, "text nobody ever remembered", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Status != StatusUnresolved {
		t.Fatalf("rows = %+v, want one UNRESOLVED row", rep.Rows)
	}
}

// A learning older than the cut-off, one written by extraction rather than by a
// tool call, and one already attributed to the same session need no repair.
func TestPlanSelectsOnlyToolWrittenOpencodeRowsAfterTheCutOff(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 4, "written before the cut-off", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-09-30T10:00:00+02:00")
	insertLearning(t, ydb, 5, "extracted from a session transcript", "opencode:ses_WRONG", "/repo/main", "", "2026-10-15T10:00:00+02:00")
	insertLearning(t, ydb, 6, "already attributed", "opencode:ses_RIGHT", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	if _, err := ydb.Exec(`UPDATE learnings SET origin_tool = 'llm_extracted_session' WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	insertToolCall(t, odb, "ses_RIGHT", "yesmem_remember", map[string]any{"text": "already attributed"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d, want only the row that needs no change: %+v", len(rep.Rows), rep.Rows)
	}
	if rep.Rows[0].ID != 6 || rep.Rows[0].Status != StatusAlready {
		t.Fatalf("row = %+v, want id 6 with status %q", rep.Rows[0], StatusAlready)
	}
}

// A project that is not an absolute path cannot be stored as it is; the matched
// session's directory identifies the repository.
func TestPlanDerivesProjectFromTheMatchedSessionDirectory(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	insertSession(t, odb, "ses_PROJ", filepath.Join(root, ".worktrees", "v2-release-notes"))
	insertLearning(t, ydb, 8, "note from the release worktree", "opencode:ses_PROJ", "v2-release-notes", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_PROJ", "yesmem_remember", map[string]any{"text": "note from the release worktree"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	row := rep.Rows[0]
	if row.NewProject != root || row.NewProjectSource != "repo" {
		t.Fatalf("project %q → %q (source %q), want %q with source repo", row.OldProject, row.NewProject, row.NewProjectSource, root)
	}
	if row.Status != StatusRepair {
		t.Fatalf("status = %q, want %q", row.Status, StatusRepair)
	}

	// The apply also has to keep canonical_project in step with project:
	// canonical_project is derived on insert and every canonical_projekt filter
	// reads it.
	if _, err := Apply(ypath, rep); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var session, project, canonical, source string
	if err := ydb.QueryRow(`SELECT session_id, project, canonical_project, project_source FROM learnings WHERE id = 8`).
		Scan(&session, &project, &canonical, &source); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if project != root || canonical != models.CanonicalProject(root) {
		t.Fatalf("after apply: project %q canonical_project %q, want %q / %q", project, canonical, root, models.CanonicalProject(root))
	}
	if session != "opencode:ses_PROJ" || source != "repo" {
		t.Fatalf("after apply: session %q source %q", session, source)
	}
}

// A tool call that merely quotes the text is not a remember call: without the
// tool filter such a row could become the unique match and attribute the
// learning to the quoting session.
func TestPlanIgnoresQuotingToolCalls(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 30, "quoted sentence about the proxy", "opencode:ses_NOISE", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_QUOTER", "shell", map[string]any{
		"code": `grep -rn "quoted sentence about the proxy" . # remember`,
	})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Status != StatusUnresolved {
		t.Fatalf("rows = %+v, want one UNRESOLVED row (a quoting shell call is no evidence)", rep.Rows)
	}
}

func TestApplyWritesOnlyUnambiguousRowsAndIsIdempotent(t *testing.T) {
	ydb, ypath := fixtureYesmemDB(t)
	odb, opath := fixtureOpencodeDB(t)
	insertLearning(t, ydb, 10, "row that will be repaired", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertLearning(t, ydb, 11, "row with two candidate sessions", "opencode:ses_WRONG", "/repo/main", "explicit", "2026-10-15T10:00:00+02:00")
	insertToolCall(t, odb, "ses_CORRECT", "yesmem_remember", map[string]any{"text": "row that will be repaired"})
	insertToolCall(t, odb, "ses_A", "yesmem_remember", map[string]any{"text": "row with two candidate sessions"})
	insertToolCall(t, odb, "ses_B", "yesmem_remember", map[string]any{"text": "row with two candidate sessions"})

	rep, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Plan is a dry run: the database must be untouched while it runs.
	if got := learningSession(t, ydb, 10); got != "opencode:ses_WRONG" {
		t.Fatalf("Plan wrote to the database: session = %q", got)
	}

	changed, err := Apply(ypath, rep)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1 (only the unambiguous row)", changed)
	}
	if got := learningSession(t, ydb, 10); got != "opencode:ses_CORRECT" {
		t.Fatalf("session after apply = %q, want opencode:ses_CORRECT", got)
	}
	if got := learningSession(t, ydb, 11); got != "opencode:ses_WRONG" {
		t.Fatalf("ambiguous row was changed: session = %q", got)
	}
	if got := backupCount(t, ydb, 10); got != 1 {
		t.Fatalf("backup rows for id 10 = %d, want 1", got)
	}

	// Second run: the proposal is already in place, so nothing changes.
	rep2, err := Plan(ypath, opath, "2026-10-01")
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	changed, err = Apply(ypath, rep2)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if changed != 0 {
		t.Fatalf("second apply changed %d rows, want 0", changed)
	}
	if got := backupCount(t, ydb, 10); got != 1 {
		t.Fatalf("second apply duplicated the backup row: %d", got)
	}
	if got := learningSession(t, ydb, 10); got != "opencode:ses_CORRECT" {
		t.Fatalf("second apply altered the repaired row: %q", got)
	}
}

func learningSession(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var session sql.NullString
	if err := db.QueryRow(`SELECT session_id FROM learnings WHERE id = ?`, id).Scan(&session); err != nil {
		t.Fatalf("read learning %d: %v", id, err)
	}
	return session.String
}

func backupCount(t *testing.T, db *sql.DB, id int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM `+BackupTable+` WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("read backup for %d: %v", id, err)
	}
	return n
}
