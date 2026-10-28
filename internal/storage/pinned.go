package storage

import (
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// PinnedLearning represents a pinned instruction visible in every briefing turn.
type PinnedLearning struct {
	ID        int64
	Project   string
	Content   string
	Source    string
	CreatedAt time.Time
	Scope     string // "session" or "permanent" — set on read based on source DB
}

const tablePinnedLearnings = `CREATE TABLE IF NOT EXISTS pinned_learnings (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project    TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	content    TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT 'user_stated',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`

func (s *Store) pinnedDB(scope string) *sql.DB {
	if scope == "session" {
		return s.proxyStateDB() // runtime.db
	}
	return s.db // yesmem.db
}

// PinLearning creates a new pinned learning in the appropriate DB. sessionID
// records which session created the pin, so a project-less session pin dies
// with its own session instead of with the next /clear of any project.
func (s *Store) PinLearning(scope, project, content, source, sessionID string) (int64, error) {
	if source == "" {
		source = "user_stated"
	}
	db := s.pinnedDB(scope)
	res, err := db.Exec(`INSERT INTO pinned_learnings (project, session_id, content, source) VALUES (?, ?, ?, ?)`,
		project, sessionID, content, source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UnpinLearning removes a pinned learning by ID and reports how many rows it deleted.
func (s *Store) UnpinLearning(scope string, id int64) (int64, error) {
	db := s.pinnedDB(scope)
	res, err := db.Exec(`DELETE FROM pinned_learnings WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PinnedLearningExists reports whether a pin with the given id exists in the scope.
// Used to disambiguate a bare id, since the two scopes keep separate ID sequences.
func (s *Store) PinnedLearningExists(scope string, id int64) (bool, error) {
	var count int
	err := s.pinnedDB(scope).QueryRow(`SELECT COUNT(*) FROM pinned_learnings WHERE id = ?`, id).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// pinVisibility is the resolved view a pin lookup answers for.
type pinVisibility struct {
	view string // the project value the caller asked for
	path string // its absolute path, "" when the value names no path
	key  string // the owning project identity, "" when unresolvable
}

// pinVisibilityFor resolves a caller's project value into the identity pin
// visibility compares against. A legacy short name is resolved to its path first:
// without that step the identity stayed empty, and every path-bound pin was
// invisible to a caller that asked with the name the CLI and the hooks use
// (YM-325).
func (s *Store) pinVisibilityFor(project string) pinVisibility {
	v := pinVisibility{view: project}
	if project == "" {
		return v
	}
	if filepath.IsAbs(project) {
		v.path = filepath.Clean(project)
	} else if resolved := s.legacyProjectPaths([]string{project})[project]; resolved != "" {
		// The quiet resolution: an unknown name is a normal answer for a lookup,
		// not a reason to log a warning on every briefing turn.
		v.path = resolved
	}
	v.key = s.owningProjectKey(v.path)
	return v
}

// pinReadScope returns the SQL predicate and its arguments that select the pin rows
// which can possibly be visible in the view, so the lookup does not read the whole
// table on every turn (YM-325). It over-includes on purpose (a directory below the
// view may itself be a nested project) and never under-includes: whether a row is
// visible is still decided by pinVisibleInProject after the read.
//
// Completeness rests on pinVisibleInProject: a pin is visible when its project
// resolves to the view's identity, and that identity is the nearest session-carrying
// ancestor of the view. A stored project that resolves there is the identity itself
// or a directory below it, so identity plus path prefix covers every visible row.
func pinReadScope(v pinVisibility) (string, []any) {
	if v.view == "" {
		// A project-less view asks for the global pins only.
		return `project = ''`, nil
	}
	clauses := []string{`project = ''`, `project = ?`, `project NOT LIKE '/%'`}
	args := []any{v.view}
	if v.path != "" && v.path != v.view {
		clauses = append(clauses, `project = ?`)
		args = append(args, v.path)
	}
	if v.key != "" {
		clauses = append(clauses, `project = ?`, `project LIKE ? ESCAPE '\'`)
		args = append(args, v.key, escapeLikeLiteral(v.key)+"/%")
	}
	return strings.Join(clauses, " OR "), args
}

// GetPinnedLearnings returns the pins visible in project, in the given scope: the
// pins bound to that project, the global (project-less) ones, and the pins bound
// to a directory that belongs to the same project. What a pin belongs to is
// decided by the project a session carries (OwningProject), never by path
// prefixes: a prefix rule showed a pin of a vendored clone or of a sibling
// checkout in an unrelated project. Both sides are resolved with the same function
// at the same moment, so a directory that gains a session row later moves the
// pin's home together with the view instead of losing it. The rows are read first
// and resolved afterwards: on a store whose handles share one connection
// (:memory:, read-only) a query inside the open cursor deadlocks.
func (s *Store) GetPinnedLearnings(scope, project string) ([]PinnedLearning, error) {
	db := s.pinnedDB(scope)
	view := s.pinVisibilityFor(project)
	clause, args := pinReadScope(view)
	rows, err := db.Query(`SELECT id, project, content, source, created_at FROM pinned_learnings
		WHERE `+clause+` ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	var stored []PinnedLearning
	for rows.Next() {
		var p PinnedLearning
		if err := rows.Scan(&p.ID, &p.Project, &p.Content, &p.Source, &p.CreatedAt); err != nil {
			// One unreadable row must not take every instruction down with it, but
			// it must not disappear in silence either: the pin would be missing
			// from every briefing with nothing to explain it (YM-325).
			log.Printf("[pins] cannot read a %s row of pinned_learnings, skipping it: %v", scope, err)
			continue
		}
		stored = append(stored, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	legacy := s.legacyProjectPaths(pinLegacyNames(stored))
	pins := make([]PinnedLearning, 0, len(stored))
	visible := make(map[string]bool, len(stored))
	for _, p := range stored {
		ok, known := visible[p.Project]
		if !known {
			ok = s.pinVisibleInProject(p.Project, view, legacy)
			visible[p.Project] = ok
		}
		if !ok {
			continue
		}
		p.Scope = scope
		pins = append(pins, p)
	}
	return pins, nil
}

// pinLegacyNames collects the distinct stored project values that name no path, so
// all of them can be resolved in one query.
func pinLegacyNames(pins []PinnedLearning) []string {
	names := make([]string, 0, len(pins))
	seen := make(map[string]bool, len(pins))
	for _, p := range pins {
		if p.Project == "" || filepath.IsAbs(p.Project) || seen[p.Project] {
			continue
		}
		seen[p.Project] = true
		names = append(names, p.Project)
	}
	return names
}

// pinVisibleInProject reports whether a pin bound to pinProject belongs in the
// view. legacy holds the resolved path of every short name of this lookup.
func (s *Store) pinVisibleInProject(pinProject string, v pinVisibility, legacy map[string]string) bool {
	if pinProject == "" {
		return true
	}
	if v.view == "" {
		return false
	}
	if pinProject == v.view || (v.path != "" && pinProject == v.path) {
		return true
	}
	bound := pinProject
	if !filepath.IsAbs(bound) {
		// Legacy short name from before project ids were paths. It counts only when
		// exactly one project carries it: resolving it against the caller's view
		// would make one pin appear in every project that shares the basename.
		bound = legacy[pinProject]
		if bound == "" {
			return false
		}
	}
	return v.key != "" && s.owningProjectKey(bound) == v.key
}

// owningProjectKey is OwningProject for a path and "" for anything else.
func (s *Store) owningProjectKey(value string) string {
	if !filepath.IsAbs(value) {
		return ""
	}
	return s.OwningProject(value)
}

// legacyProjectPaths resolves pre-v0.65 short project names to the single project
// path that ends in them, all names of one lookup in one query instead of one scan
// of the sessions table per name and per turn (YM-325). Unknown, ambiguous or
// unreadable names are absent from the result: fail-closed, no view-dependent
// tiebreaker and no case folding, so a pin never appears in a checkout its stored
// name does not name exactly.
func (s *Store) legacyProjectPaths(names []string) map[string]string {
	resolved := make(map[string]string, len(names))
	var conditions []string
	var args []any
	for _, name := range names {
		if name == "" {
			continue
		}
		// substr instead of LIKE for the name itself: no wildcard interpretation of
		// '%' or '_' and no ASCII case folding, which would pair "myapp" with
		// "/home/x/MyApp".
		conditions = append(conditions, `substr(project, -length(?) - 1) = '/' || ?`)
		args = append(args, name, name)
	}
	if len(conditions) == 0 {
		return resolved
	}

	rows, err := s.readerDB().Query(
		`SELECT DISTINCT project FROM sessions
		 WHERE project LIKE '/%' AND (`+strings.Join(conditions, " OR ")+`)`, args...)
	if err != nil {
		log.Printf("[pins] cannot resolve legacy short names: %v", err)
		return resolved
	}
	defer rows.Close()

	candidates := make(map[string][]string, len(names))
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			log.Printf("[pins] cannot read a session project while resolving short names: %v", err)
			return resolved
		}
		if project == "" {
			continue
		}
		// Match per requested name, not per path segment: a stored name may itself
		// contain a slash, and grouping by filepath.Base would drop it.
		for _, name := range names {
			if strings.HasSuffix(project, "/"+name) {
				candidates[name] = append(candidates[name], project)
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[pins] cannot resolve legacy short names: %v", err)
		return resolved
	}
	for name, paths := range candidates {
		if len(paths) == 1 {
			resolved[name] = paths[0]
		}
	}
	return resolved
}

// SessionPinMaxAge bounds how long an attributed session pin may live without a
// session end. The Claude Code SessionEnd hook clears session pins on /clear;
// opencode and codex have no such event, so their pins would stay in every
// briefing of the project forever. The bound is an age, not a liveness proof: a
// session that is still running after a week loses its session pins, which is the
// accepted price for bounding a leak no event can close.
const SessionPinMaxAge = 7 * 24 * time.Hour

// PurgeSessionPinsBefore deletes the session pins created before cutoff that no
// event can reach: the attributed ones (opencode and codex have no SessionEnd) and
// the unattributed project-less ones. The latter are global, so they show up in
// every briefing, and nothing can ever remove them: no session id for a
// project-less /clear to match and no project for a project-scoped one. Age is
// their only cleanup path, and a session pin is session-scoped by definition, so
// the same bound applies.
//
// Project-bound rows without a session_id are left alone: /clear reaches them by
// project. Permanent pins live in the other database and are never touched; on a
// handle without its own runtime.db (:memory:, read-only) both scopes share one
// table, so the widened clause only applies when the databases are really separate.
func (s *Store) PurgeSessionPinsBefore(cutoff time.Time) (int64, error) {
	query := `DELETE FROM pinned_learnings
		 WHERE datetime(created_at) < datetime(?) AND session_id <> ''`
	if s.proxyStateDB() != s.db {
		query = `DELETE FROM pinned_learnings
		 WHERE datetime(created_at) < datetime(?) AND (session_id <> '' OR project = '')`
	}
	res, err := s.proxyStateDB().Exec(query, cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearSessionPins deletes the session-scoped pins that belong to the session
// that just ended: every pin this session created, wherever it was bound, plus
// project-bound pins without attribution — /clear is the only end those can ever
// have, either because they predate session attribution or because their caller
// had no provable session. runtime.db is one file for all projects and sessions,
// and a project can host several live sessions at once (parallel agents in one
// repo), so the attributed part keys on the session and not on the project: a
// project-wide delete would remove the pins of every sibling session.
func (s *Store) ClearSessionPins(project, sessionID string) error {
	db := s.proxyStateDB()
	var err error
	switch {
	case sessionID != "" && project != "":
		_, err = db.Exec(`DELETE FROM pinned_learnings
			WHERE session_id = ?
			   OR (project = ? AND session_id = '')`, sessionID, project)
	case sessionID != "":
		_, err = db.Exec(`DELETE FROM pinned_learnings WHERE session_id = ?`, sessionID)
	case project != "":
		_, err = db.Exec(`DELETE FROM pinned_learnings WHERE project = ? AND session_id = ''`, project)
	}
	return err
}
