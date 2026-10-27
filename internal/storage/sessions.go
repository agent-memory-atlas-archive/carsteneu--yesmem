package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// CanonicalProjectGroup is one project after its worktree copies have been
// merged, carrying the summed session count of all copies. Canonical is the
// learnings scope (the basename on the learnings columns), ClusterKey the key the
// project's cluster rows live under (its repository root).
type CanonicalProjectGroup struct {
	Canonical  string
	ClusterKey string
	Sessions   int
}

// GroupByCanonicalProject merges the project rows that describe the same
// repository into one group. Returns groups in cluster-key order so callers
// (clustering, recurrence, distillation) iterate deterministically.
func GroupByCanonicalProject(projects []ProjectSummary) []CanonicalProjectGroup {
	index := make(map[string]int, len(projects))
	// bareIndex is kept separate on purpose: the two maps are fed from two key
	// spaces (repository roots and legacy bare names) and must not be conflated.
	bareIndex := make(map[string]int)
	groups := make([]CanonicalProjectGroup, 0, len(projects))
	// Rows without a root (legacy bare project names) are collected first: they
	// belong to a repository whose path they do not carry, so they merge below.
	var bare []ProjectSummary

	for _, p := range projects {
		key := p.Project
		if key == "" {
			key = p.ProjectShort
		}
		if key == "" {
			continue
		}
		canonical := models.CanonicalProject(key)
		if canonical == "" || canonical == "." {
			continue
		}
		if models.RepositoryRoot(key) == "" {
			bare = append(bare, ProjectSummary{Project: key, SessionCount: p.SessionCount})
			continue
		}
		clusterKey := models.ClusterProjectKey(key)
		i, ok := index[clusterKey]
		if !ok {
			i = len(groups)
			index[clusterKey] = i
			groups = append(groups, CanonicalProjectGroup{Canonical: canonical, ClusterKey: clusterKey})
		}
		groups[i].Sessions += p.SessionCount
	}

	// A legacy row naming its repository only by basename describes the same
	// repository as the path rows. It merges into the single root that carries
	// that basename; when two roots share it the name is ambiguous, so the row
	// stays its own group instead of joining the wrong cluster set.
	for _, p := range bare {
		canonical := models.CanonicalProject(p.Project)
		match := -1
		for i := range groups {
			if groups[i].Canonical != canonical {
				continue
			}
			if match >= 0 {
				match = -1
				break
			}
			match = i
		}
		if match >= 0 {
			groups[match].Sessions += p.SessionCount
			continue
		}
		i, ok := bareIndex[p.Project]
		if !ok {
			i = len(groups)
			bareIndex[p.Project] = i
			groups = append(groups, CanonicalProjectGroup{Canonical: canonical, ClusterKey: p.Project})
		}
		groups[i].Sessions += p.SessionCount
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].ClusterKey < groups[j].ClusterKey })
	return groups
}

// UpsertSession inserts or updates a session.
// If a previously-extracted short session (below ShortSessionMaxMessages) grows
// past the threshold, extracted_at is reset so the extraction pipeline
// re-processes it.
func (s *Store) UpsertSession(sess *models.Session) error {
	sourceAgent := models.NormalizeSourceAgent(sess.SourceAgent)
	// Normalize project_short from the full project path so storage always
	// carries the canonical absolute identifier, regardless of caller.
	if sess.Project != "" {
		sess.ProjectShort = models.ProjectShortFromPath(sess.Project)
	}

	// On conflict the identity fields (project, started_at, first_message,
	// git_branch) are healed, never replaced: they are written when they are
	// still empty, because a later pass may be the first one to know them — the
	// incremental append is the only writer that sees a session whose first
	// index ran before its first user prompt existed. A value that is already
	// set stays, so no partial pass can blank out what an earlier, better
	// informed one stored.
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, project, project_short, git_branch, first_message,
			message_count, started_at, ended_at, jsonl_path, jsonl_size, indexed_at,
			parent_session_id, agent_type, source_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			message_count = excluded.message_count,
			ended_at = excluded.ended_at,
			jsonl_size = excluded.jsonl_size,
			indexed_at = excluded.indexed_at,
			parent_session_id = excluded.parent_session_id,
			agent_type = excluded.agent_type,
			source_agent = excluded.source_agent,
			first_message = CASE WHEN sessions.first_message IS NULL OR sessions.first_message = ''
				THEN excluded.first_message ELSE sessions.first_message END,
			started_at = CASE WHEN sessions.started_at IS NULL OR sessions.started_at = ''
				THEN excluded.started_at ELSE sessions.started_at END,
			project = CASE WHEN sessions.project IS NULL OR sessions.project = ''
				THEN excluded.project ELSE sessions.project END,
			-- The short name describes the project that won above, so it is only
			-- adopted while it is empty and while it belongs to the same path.
			project_short = CASE
				WHEN (sessions.project_short IS NULL OR sessions.project_short = '')
					AND (sessions.project IS NULL OR sessions.project = '' OR sessions.project = excluded.project)
				THEN excluded.project_short ELSE sessions.project_short END,
			git_branch = CASE WHEN sessions.git_branch IS NULL OR sessions.git_branch = ''
				THEN excluded.git_branch ELSE sessions.git_branch END,
			extracted_at = CASE
				WHEN sessions.extracted_at IS NOT NULL AND sessions.message_count <= ? AND excluded.message_count > ?
				THEN NULL ELSE sessions.extracted_at END,
			-- A session that grew gets a fresh retry budget. The earlier failures
			-- were about the content it had before, not about the tail it gained;
			-- this must hold for a partially extracted session too, whose
			-- extracted_at is still empty (YM-362).
			extraction_attempts = CASE
				WHEN excluded.message_count > sessions.message_count
				THEN 0 ELSE sessions.extraction_attempts END`,
		sess.ID, sess.Project, sess.ProjectShort, sess.GitBranch, sess.FirstMessage,
		sess.MessageCount, fmtTime(sess.StartedAt), fmtTime(sess.EndedAt),
		sess.JSONLPath, sess.JSONLSize, fmtTime(sess.IndexedAt),
		nullString(sess.ParentSessionID), nullString(sess.AgentType), sourceAgent,
		ShortSessionMaxMessages-1, ShortSessionMaxMessages-1)
	if err != nil {
		return fmt.Errorf("upsert session %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession retrieves a session by ID.
func (s *Store) GetSession(id string) (*models.Session, error) {
	row := s.readerDB().QueryRow(`SELECT id, project, project_short, git_branch, first_message,
		message_count, started_at, ended_at, jsonl_path, jsonl_size, indexed_at,
		COALESCE(parent_session_id, ''), COALESCE(agent_type, ''), COALESCE(source_agent, 'claude'),
		COALESCE(extracted_at, ''), COALESCE(narrative_at, '')
		FROM sessions WHERE id = ?`, id)

	sess := &models.Session{}
	var startedAt, endedAt, indexedAt, extractedAt, narrativeAt string
	err := row.Scan(&sess.ID, &sess.Project, &sess.ProjectShort, &sess.GitBranch,
		&sess.FirstMessage, &sess.MessageCount, &startedAt, &endedAt,
		&sess.JSONLPath, &sess.JSONLSize, &indexedAt,
		&sess.ParentSessionID, &sess.AgentType, &sess.SourceAgent, &extractedAt, &narrativeAt)
	if err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}
	sess.StartedAt = parseTime(startedAt)
	sess.EndedAt = parseTime(endedAt)
	sess.IndexedAt = parseTime(indexedAt)
	sess.ExtractedAt = parseTime(extractedAt)
	sess.NarrativeAt = parseTime(narrativeAt)
	return sess, nil
}

// ListSessions returns sessions ordered by started_at DESC.
// Subagent sessions are excluded — use ListAllSessions to include them.
func (s *Store) ListSessions(project string, limit int) ([]models.Session, error) {
	return s.listSessions(project, limit, false)
}

// CountProjectSessions counts the top-level sessions of a project, keyed the
// same way ListSessions and ListProjects resolve a project.
func (s *Store) CountProjectSessions(project string) (int, error) {
	var count int
	err := s.readerDB().QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE parent_session_id IS NULL AND (project_short = ? OR project = ?)`,
		project, project).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count project sessions: %w", err)
	}
	return count, nil
}

// projectHasSessions reports whether a session carries the project path, top-level
// or subagent. projectHasTopLevelSessions is the narrower question.
func (s *Store) projectHasSessions(project string) bool {
	return s.projectHasPath(project, `project_short = ? OR project = ?`)
}

// projectHasTopLevelSessions reports whether a top-level session carries the path.
// It uses the same predicate as listSessions, so "the project's own sessions" means
// the same thing in both places.
func (s *Store) projectHasTopLevelSessions(project string) bool {
	return s.projectHasPath(project, `parent_session_id IS NULL AND (project_short = ? OR project = ?)`)
}

func (s *Store) projectHasPath(project, predicate string) bool {
	var count int
	err := s.readerDB().QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE `+predicate, project, project).Scan(&count)
	return err == nil && count > 0
}

// OwningProject returns the project a directory belongs to: the directory itself
// when a session carries it, else the nearest ancestor (at most three levels) that
// does, else the directory as given. This is the project identity the briefing
// keys its project-scoped lookups on; pin lookups use it too, because a pin
// written from a subdirectory belongs to the project that owns that subdirectory.
//
// A directory whose only sessions were spawned as subagents is a home too, but only
// in a second pass: a worktree or a vendored copy that ran nothing but subagents
// still belongs to the checkout above it, and losing that would take the checkout's
// pins and briefing scope away from it (YM-325).
func (s *Store) OwningProject(dir string) string {
	if dir == "" {
		return ""
	}
	project := models.ProjectShortFromPath(dir)
	// The repository root is the FLOOR of the ascent below, not its destination. A
	// nested checkout is its own project, so the search must not climb out of it into the
	// enclosing repository (YM-393); a directory inside a checkout keeps climbing, because
	// a project registered below a checkout ("<repo>/docs/plans") is a project of its own
	// as long as its sessions name it (cold review Los 56h, measured on the live rows).
	// RepositoryRoot answers with the input path when no checkout encloses it, so the floor
	// only counts where a checkout really exists.
	floor := models.RepositoryRoot(project)
	if floor != "" && !isCheckoutRoot(floor) {
		floor = ""
	}
	if home, ok := nearestProjectHomeWithin(project, floor, s.projectHasTopLevelSessions); ok {
		return home
	}
	if home, ok := nearestProjectHomeWithin(project, floor, s.projectHasSessions); ok {
		return home
	}
	if floor != "" {
		// No session anywhere on the way up to the checkout: the checkout is the project,
		// not the deepest path inside it. That is the same identity a nested clone gets,
		// and it keeps the answer from depending on how deep the caller's path happens to
		// be.
		return floor
	}
	return project
}

// isCheckoutRoot reports whether dir carries its own repository marker. A worktree
// checkout keeps a ".git" file rather than a directory, and both are the marker.
func isCheckoutRoot(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// nearestProjectHome returns the directory itself or the nearest ancestor within
// three levels whose sessions satisfy match.
func nearestProjectHome(project string, match func(string) bool) (string, bool) {
	return nearestProjectHomeWithin(project, "", match)
}

// nearestProjectHomeWithin is nearestProjectHome with a floor: the ascent stops once it
// reaches floor, and floor itself still counts. An empty floor leaves the ascent
// unbounded.
func nearestProjectHomeWithin(project, floor string, match func(string) bool) (string, bool) {
	if match(project) {
		return project, true
	}
	for i, ancestor := 0, project; i < 3; i++ {
		if floor != "" && ancestor == floor {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor || parent == "/" {
			break
		}
		if match(parent) {
			return parent, true
		}
		ancestor = parent
	}
	return "", false
}

// ResolveProjectPath returns the filesystem path of a project identifier.
//
// A project id IS its absolute path since v0.65, so a path is returned cleaned and
// without a lookup. A legacy short name is resolved by basename; an ambiguous or
// unknown name yields "". Callers that must tell those two apart use
// ResolveProjectPathStrict.
func (s *Store) ResolveProjectPath(project string) string {
	path, _ := s.ResolveProjectPathStrict(project)
	return path
}

// ResolveProjectPathStrict is ResolveProjectPath with the reason a name stayed
// unresolved: an ambiguous short name is an error, because every caller has to
// decide whether to refuse or to fall back; an unknown name only yields "".
func (s *Store) ResolveProjectPathStrict(project string) (string, error) {
	if project == "" {
		return "", nil
	}
	if filepath.IsAbs(project) {
		return filepath.Clean(project), nil
	}
	resolved, err := s.ResolveProjectShortStrict(project, "")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", nil
	}
	return resolved, nil
}

// AmbiguousProjectError is returned when a short project name resolves to
// multiple candidates and no cwd tiebreaker is available. The Candidates
// field lists all canonical paths so callers can disambiguate explicitly.
type AmbiguousProjectError struct {
	Name       string
	Candidates []string
}

func (e *AmbiguousProjectError) Error() string {
	return fmt.Sprintf("ambiguous project name %q matches %d candidates: %s", e.Name, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// ResolveProjectShort finds the project identifier for a directory path.
// Full absolute paths (starting with '/') are returned cleaned — they are already
// unique since ProjectShortFromPath moved to abs-path semantics. Legacy short-name
// inputs fall back to DB-best-effort matching for backward compatibility with tests
// and any callers that have not migrated to full paths yet.
func (s *Store) ResolveProjectShort(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	if projectDir[0] == '/' {
		return filepath.Clean(projectDir)
	}
	// A short name carries no separator. A relative path with one is reduced to its
	// last element, so the basename match below cannot widen to a suffix of a
	// stored path (security review, YM-300).
	if strings.Contains(projectDir, "/") {
		projectDir = filepath.Base(filepath.Clean(projectDir))
		if projectDir == "." || projectDir == "/" {
			return ""
		}
	}

	// Legacy fallback: short name input — best-effort DB match.
	var count int
	err := s.readerDB().QueryRow("SELECT COUNT(*) FROM sessions WHERE project_short = ?", projectDir).Scan(&count)
	if err == nil && count > 0 {
		return projectDir
	}

	// The basename match is exact, like ResolveProjectShortStrict: the substring
	// match routed "yesmem" into every checkout whose path contained the name
	// (YM-300). The literal is escaped: "_" and "%" are LIKE wildcards, so
	// "pro_ect" matched a project called "proXect" (YM-295).
	//
	// The candidates are collected and the cursor closed BEFORE the per-candidate
	// query: on a single-connection store the inner query waits for the connection
	// the open cursor holds, and the call never returns (YM-300).
	names := []string{filepath.Base(projectDir)}
	rows, err := s.readerDB().Query(
		`SELECT DISTINCT project_short FROM sessions WHERE project LIKE '/%' AND project LIKE '%/' || ? ESCAPE '\'`,
		escapeLikeLiteral(projectDir),
	)
	if err == nil {
		for rows.Next() {
			var ps string
			if rows.Scan(&ps) == nil && ps != "" {
				names = append(names, ps)
			}
		}
		// A cursor that broke mid-iteration leaves an incomplete candidate list, and
		// the "latest session" pick below would then choose from what it did read.
		if err := rows.Err(); err != nil {
			log.Printf("WARNING: ResolveProjectShort: reading candidate projects for %q: %v", projectDir, err)
		}
		rows.Close()
	}

	best, bestLatest := "", ""
	for _, name := range names {
		var latest string
		if err := s.readerDB().QueryRow("SELECT MAX(started_at) FROM sessions WHERE project_short = ?", name).Scan(&latest); err != nil {
			continue
		}
		if latest != "" && latest > bestLatest {
			best, bestLatest = name, latest
		}
	}

	if best != "" {
		return best
	}
	return filepath.Base(projectDir)
}

// ResolveProjectShortStrict resolves a project identifier to a single project path.
// Absolute paths are returned cleaned. Short names are matched by basename against
// sessions.project (which stores full paths since v0.65). A unique hit returns the
// full path. Multiple candidates are disambiguated via cwd: the candidate that
// equals cwd or is a path-ancestor of cwd wins. If cwd doesn't match any candidate,
// an *AmbiguousProjectError is returned with all candidates — callers must
// disambiguate or surface the error. Zero candidates resolve to the caller's cwd
// when that directory carries the name, to "" when a cwd was given and does not,
// and to the name itself otherwise (a cwd-less caller has nothing to compare
// against; persisting that value is the caller's call).
func (s *Store) ResolveProjectShortStrict(name string, cwd string) (resolved string, err error) {
	if name == "" {
		return "", nil
	}
	if name[0] == '/' {
		return filepath.Clean(name), nil
	}

	// Basename match against sessions.project (full paths since v0.65).
	// Using LIKE '%/' || ? ensures we match only the last path segment; the
	// literal is escaped first, because "_" and "%" are wildcards — "pro_ect"
	// used to match a project called "proXect".
	rows, err := s.readerDB().Query(
		`SELECT DISTINCT project FROM sessions WHERE project LIKE '/%' AND project LIKE '%/' || ? ESCAPE '\' ORDER BY project`,
		escapeLikeLiteral(name),
	)
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", name, err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			candidates = append(candidates, p)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("resolve project %q: %w", name, err)
	}

	switch len(candidates) {
	case 0:
		// Nothing carries this name. A cwd that is named like it is the project:
		// that is the fresh checkout whose first run has no session row yet. It has
		// to be an absolute path — a relative one would mint a project scope out of
		// caller bytes, and the value would slip past every marker check below
		// (security review, YM-300).
		if cwd != "" && filepath.IsAbs(cwd) {
			if dir := filepath.Clean(cwd); filepath.Base(dir) == name {
				return dir, nil
			}
		}
		if cwd != "" {
			// The caller stands somewhere else, so the name resolves to nothing. A
			// basename is not a project path, and returning it made callers persist
			// it (YM-300); the dispatch reports the name back to the caller.
			log.Printf("WARNING: ResolveProjectShortStrict: no project found for short name %q (cwd %q)", name, cwd)
			return "", nil
		}
		// No directory to compare against: the name is handed back unchanged so
		// callers that address a project by its short name keep working. Callers
		// that persist the result must not treat it as a path (see
		// resolveProjectParam's _project_unresolved).
		log.Printf("WARNING: ResolveProjectShortStrict: no project found for short name %q", name)
		return name, nil
	case 1:
		return candidates[0], nil
	default:
		// Try cwd tiebreaker first — exact match or longest path-ancestor wins.
		if cwd != "" {
			if match := resolveByCWD(candidates, cwd); match != "" {
				return match, nil
			}
		}
		// Ambiguity unresolved by cwd: return typed error with all candidates
		// so callers can present a choice or require explicit disambiguation.
		// Never fall back to an arbitrary first candidate — that silently
		// routes operations to the wrong project.
		log.Printf("WARNING: ResolveProjectShortStrict: ambiguous short name %q (%d candidates %v)",
			name, len(candidates), candidates)
		return "", &AmbiguousProjectError{Name: name, Candidates: candidates}
	}
}

// escapeLikeLiteral neutralizes the LIKE metacharacters of a literal, so a short
// project name matches only itself.
func escapeLikeLiteral(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// resolveByCWD returns the candidate that best matches cwd: exact match wins,
// otherwise the longest path-ancestor (to prefer nested projects like worktrees).
// Path-ancestor match requires a trailing "/" after the candidate to avoid false
// positives like /home/user/code/yes matching /home/user/code/yesmem.
// Returns "" if no candidate matches.
func resolveByCWD(candidates []string, cwd string) string {
	var best string
	for _, c := range candidates {
		if c == cwd {
			return c // exact match always wins
		}
		if strings.HasPrefix(cwd, c+"/") {
			if len(c) > len(best) {
				best = c
			}
		}
	}
	return best
}

// ListAllSessions returns all sessions including subagent sessions.
// Used by extraction pipeline to extract learnings from subagent work.
func (s *Store) ListAllSessions(project string, limit int) ([]models.Session, error) {
	return s.listSessions(project, limit, true)
}

func (s *Store) listSessions(project string, limit int, includeSubagents bool) ([]models.Session, error) {
	query := `SELECT id, project, project_short, git_branch, first_message,
		message_count, started_at, ended_at, jsonl_path, jsonl_size, indexed_at,
		COALESCE(parent_session_id, ''), COALESCE(agent_type, ''), COALESCE(source_agent, 'claude'),
		COALESCE(extracted_at, ''), COALESCE(narrative_at, '')
		FROM sessions`
	args := []any{}

	if !includeSubagents {
		query += ` WHERE parent_session_id IS NULL`
	} else {
		query += ` WHERE 1=1`
	}

	if project != "" {
		query += ` AND (project_short = ? OR project = ?)`
		args = append(args, project, project)
	}
	query += ` ORDER BY started_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []models.Session
	for rows.Next() {
		var sess models.Session
		var startedAt, endedAt, indexedAt, extractedAt, narrativeAt string
		if err := rows.Scan(&sess.ID, &sess.Project, &sess.ProjectShort, &sess.GitBranch,
			&sess.FirstMessage, &sess.MessageCount, &startedAt, &endedAt,
			&sess.JSONLPath, &sess.JSONLSize, &indexedAt,
			&sess.ParentSessionID, &sess.AgentType, &sess.SourceAgent, &extractedAt, &narrativeAt); err != nil {
			return nil, err
		}
		sess.StartedAt = parseTime(startedAt)
		sess.EndedAt = parseTime(endedAt)
		sess.IndexedAt = parseTime(indexedAt)
		sess.ExtractedAt = parseTime(extractedAt)
		sess.NarrativeAt = parseTime(narrativeAt)
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// ListProjects returns distinct projects with session counts.
func (s *Store) ListProjects() ([]ProjectSummary, error) {
	rows, err := s.readerDB().Query(`
		SELECT project_short, project, COUNT(*) as session_count,
			MAX(started_at) as last_active
		FROM sessions
		WHERE parent_session_id IS NULL
		GROUP BY project_short, project
		ORDER BY last_active DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ProjectShort, &p.Project, &p.SessionCount, &p.LastActive); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ListProjectsWithRepositoryRoots is ListProjects plus one row per repository that
// appears only as the parent of worktree sessions. A repository whose work all
// happened inside worktree checkouts has no session of its own, so ListProjects
// drops it, and the briefing refresh loop — which keys its worklist on that list —
// never generates a briefing for it.
//
// A synthetic root row carries its own session count (0 when all work sits in
// worktrees) and the most recent activity anywhere in the repository, so the row
// does not read as never-active. Rows stay per project: summing SessionCount
// across them would count a worktree's sessions twice.
func (s *Store) ListProjectsWithRepositoryRoots() ([]ProjectSummary, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}

	known := make(map[string]struct{}, len(projects))
	lastActive := make(map[string]string, len(projects))
	for _, p := range projects {
		known[p.ProjectShort] = struct{}{}
		key := p.Project
		if key == "" {
			key = p.ProjectShort
		}
		// Only a path inside a worktree checkout says "this repository works in
		// copies": for every other path the repository root is the path itself, and
		// once RepositoryRoot resolves an enclosing checkout (YM-321) a session in a
		// plain subdirectory would otherwise plant a root row for a project that
		// never had one.
		if models.CanonicalProject(key) == filepath.Base(key) {
			continue
		}
		// The root of a worktree copy comes from the marker cut, not from
		// models.RepositoryRoot: that one ascends to the enclosing checkout (YM-321)
		// and would name a repository the session path does not belong to.
		root := projectReadRoot(key)
		if root == "" {
			continue
		}
		if p.LastActive > lastActive[root] {
			lastActive[root] = p.LastActive
		}
	}

	for root, last := range lastActive {
		if _, ok := known[root]; ok {
			continue
		}
		// Only a root the read path resolves to is worth a row. OwningProject climbs to
		// the nearest ancestor that carries sessions, so a briefing written under a root
		// that a session there would not resolve to is stored under a key nobody reads.
		// Checked once per distinct root, not once per worktree row.
		if s.OwningProject(root) != root {
			continue
		}
		projects = append(projects, ProjectSummary{ProjectShort: root, Project: root, LastActive: last})
	}

	sort.SliceStable(projects, func(i, j int) bool {
		if projects[i].LastActive != projects[j].LastActive {
			return projects[i].LastActive > projects[j].LastActive
		}
		return projects[i].ProjectShort < projects[j].ProjectShort
	})
	return projects, nil
}

// ProjectSummary holds project-level aggregation data.
type ProjectSummary struct {
	ProjectShort string `json:"project_short"`
	Project      string `json:"project"`
	SessionCount int    `json:"session_count"`
	LastActive   string `json:"last_active"`
}

// ListProjectPaths returns distinct absolute project paths from sessions and
// learnings — the universe of known projects for content-based attribution.
func (s *Store) ListProjectPaths() ([]string, error) {
	rows, err := s.readerDB().Query(`
		SELECT project FROM sessions WHERE project LIKE '/%'
		UNION
		SELECT project FROM learnings WHERE project LIKE '/%'`)
	if err != nil {
		return nil, fmt.Errorf("list project paths: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// ParseTimestamp decodes the two timestamp layouts yesmem stores: RFC3339 (written
// by fmtTime, carrying the writer's UTC offset) and the SQLite CURRENT_TIMESTAMP
// default "2006-01-02 15:04:05" (UTC). Callers that compare against time.Now()
// need this: a string comparison is wrong as soon as the offsets differ.
func ParseTimestamp(s string) time.Time {
	return parseTime(s)
}

// parseTime parses the two timestamp layouts yesmem stores: RFC3339 (written by
// fmtTime) and the SQLite CURRENT_TIMESTAMP default "2006-01-02 15:04:05" (UTC)
// used by knowledge_gaps. Aggregates such as MAX(reviewed_at) lose the column's
// declared type, so the driver hands those values back as raw SQLite text.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t
}

// validateFilterBounds rejects a since/before value that cannot be parsed. The
// storage layer owns the SQL comparison, so it must not silently turn an
// unreadable bound into "no filter" (fail-open) on its own.
func validateFilterBounds(since, before string) error {
	if since != "" && ParseFilterTime(since).IsZero() {
		return fmt.Errorf("invalid since bound %q", since)
	}
	if before != "" && ParseFilterTime(before).IsZero() {
		return fmt.Errorf("invalid before bound %q", before)
	}
	return nil
}

// ParseFilterTime parses a since/before bound in the forms the search tools
// document: RFC3339 with or without offset, the SQLite "2006-01-02 15:04:05"
// form, or a bare date. An unparsable value yields the zero time.
func ParseFilterTime(s string) time.Time {
	if t := parseTime(s); !t.IsZero() {
		return t
	}
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// SQLiteFilterTime renders a bound in the layout SQLite's date functions always
// parse, so a bound never depends on how the caller spelled it.
func SQLiteFilterTime(s string) string {
	return ParseFilterTime(s).UTC().Format("2006-01-02 15:04:05")
}

// nullString returns nil for empty strings so SQLite stores NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SubagentSummary holds aggregated subagent info for a parent session.
type SubagentSummary struct {
	ParentSessionID string `json:"parent_session_id"`
	AgentType       string `json:"agent_type"`
	Count           int    `json:"count"`
	TotalMessages   int    `json:"total_messages"`
}

// GetSubagentSummary returns aggregated subagent stats for a parent session.
func (s *Store) GetSubagentSummary(parentID string) ([]SubagentSummary, error) {
	rows, err := s.readerDB().Query(`
		SELECT parent_session_id, COALESCE(agent_type, 'unknown'), COUNT(*), COALESCE(SUM(message_count), 0)
		FROM sessions
		WHERE parent_session_id = ?
		GROUP BY agent_type`, parentID)
	if err != nil {
		return nil, fmt.Errorf("get subagent summary for %s: %w", parentID, err)
	}
	defer rows.Close()

	var summaries []SubagentSummary
	for rows.Next() {
		var sum SubagentSummary
		if err := rows.Scan(&sum.ParentSessionID, &sum.AgentType, &sum.Count, &sum.TotalMessages); err != nil {
			return nil, err
		}
		summaries = append(summaries, sum)
	}
	return summaries, rows.Err()
}

// GetSubagentCounts returns a map of parent_session_id → subagent count.
// Efficient bulk query for briefing rendering.
func (s *Store) GetSubagentCounts(parentIDs []string) (map[string]int, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	// Build placeholders
	placeholders := make([]string, len(parentIDs))
	args := make([]any, len(parentIDs))
	for i, id := range parentIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT parent_session_id, COUNT(*)
		FROM sessions
		WHERE parent_session_id IN (` + strings.Join(placeholders, ",") + `)
		GROUP BY parent_session_id`

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get subagent counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var pid string
		var count int
		if err := rows.Scan(&pid, &count); err != nil {
			return nil, err
		}
		counts[pid] = count
	}
	return counts, rows.Err()
}

// SessionMeta holds lightweight session metadata for search result annotation.
type SessionMeta struct {
	ParentSessionID string
	AgentType       string
	Project         string
	SourceAgent     string
}

// GetSessionMetaBulk returns agent_type, parent_session_id, project and source_agent for a list of session IDs.
func (s *Store) GetSessionMetaBulk(ids []string) (map[string]SessionMeta, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT id, COALESCE(parent_session_id, ''), COALESCE(agent_type, ''),
		COALESCE(project, ''), COALESCE(source_agent, 'claude')
		FROM sessions
		WHERE id IN (` + strings.Join(placeholders, ",") + `)`

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get session meta bulk: %w", err)
	}
	defer rows.Close()

	meta := make(map[string]SessionMeta)
	for rows.Next() {
		var id, parent, agentType, project, sourceAgent string
		if err := rows.Scan(&id, &parent, &agentType, &project, &sourceAgent); err != nil {
			return nil, err
		}
		meta[id] = SessionMeta{
			ParentSessionID: parent,
			AgentType:       agentType,
			Project:         project,
			SourceAgent:     sourceAgent,
		}
	}
	return meta, rows.Err()
}

// MarkSessionExtracted sets extracted_at to now, marking the session as already processed.
// It clears the failed-attempt counter as well: the budget counts consecutive
// failures, and a session that just succeeded has proven it can be extracted.
func (s *Store) MarkSessionExtracted(sessionID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET extracted_at = ?, extraction_attempts = 0 WHERE id = ?`,
		time.Now().Format(time.RFC3339), sessionID)
	return err
}

// ResetSessionExtracted clears extracted_at so the session will be re-extracted on next run.
// It clears the failed-attempt counter as well: an explicit re-extraction is the
// operator's way back for a session the batch path gave up on (see
// extractionAttemptLimit in the daemon). skip_extraction is left alone, because
// that column carries the skip_indexing instruction.
func (s *Store) ResetSessionExtracted(sessionID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET extracted_at = NULL, extraction_attempts = 0 WHERE id = ?`, sessionID)
	return err
}

// ExtractionAttemptLimit is the number of failed extraction attempts after which
// the batch path stops handing a session to the extractor. It lives here because
// the counter that enforces it lives here; the daemon reads it for the skip check
// and the pending count.
const ExtractionAttemptLimit = 5

// ExtractionAttemptsExhausted reports whether the session used up its failed
// extraction attempts. A read error counts as "not exhausted" (fail-open, like
// IsExtractionSkipped): the cost is one more attempt, not a lost session.
func (s *Store) ExtractionAttemptsExhausted(sessionID string) bool {
	var attempts int
	err := s.readerDB().QueryRow(`SELECT COALESCE(extraction_attempts, 0) FROM sessions WHERE id = ?`, sessionID).Scan(&attempts)
	return err == nil && attempts >= ExtractionAttemptLimit
}

// RecordExtractionAttempt increments the failed-extraction counter of a session
// and returns the new value, in one statement so concurrent extraction phases
// cannot lose an increment. The counter is what bounds retries: without it a
// session whose extraction never succeeds stays pending and is attempted again on
// every cycle (YM-317).
func (s *Store) RecordExtractionAttempt(sessionID string) (int, error) {
	var attempts int
	err := s.db.QueryRow(`UPDATE sessions SET extraction_attempts = COALESCE(extraction_attempts, 0) + 1
		WHERE id = ? RETURNING extraction_attempts`, sessionID).Scan(&attempts)
	if err != nil {
		return 0, err
	}
	return attempts, nil
}

// MarkShortSessionsExtracted marks sessions with ≤maxMessages as extracted,
// so they don't show as pending. Returns the number of sessions marked.
func (s *Store) MarkShortSessionsExtracted(maxMessages int) (int64, error) {
	res, err := s.db.Exec(`UPDATE sessions SET extracted_at = ? WHERE extracted_at IS NULL AND message_count <= ?`,
		time.Now().Format(time.RFC3339), maxMessages)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountPendingExtractions returns the number of sessions that need extraction
// (not yet extracted, more than minMessages messages, still within the retry
// budget). Sessions that exhausted their attempts stay extracted_at NULL; without
// the budget filter they would keep a cycle running for a queue it skips.
//
// Passing ShortSessionMaxMessages asks the question the short-session cleanup
// asks; the parameter exists because the extraction pipeline and the cleanup use
// different thresholds and a caller-chosen one would drift from both (YM-298).
func (s *Store) CountPendingExtractions(minMessages int) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions
		WHERE extracted_at IS NULL AND message_count > ?
		  AND COALESCE(extraction_attempts, 0) < ?`, minMessages, ExtractionAttemptLimit).Scan(&count)
	return count, err
}

// CountUncoveredTails counts sessions that a fork marked as extracted although
// its coverage rows do not reach the last message. Those sessions carry no
// extracted_at gap, so the batch pass needs this count to notice them at all.
//
// growingSlack mirrors the batch selector: a session that is still growing only
// counts once it carries that many uncovered messages, a session that has been
// silent for ShortSessionQuietPeriod counts from the first one.
func (s *Store) CountUncoveredTails(growingSlack int) (int, error) {
	if growingSlack < 1 {
		growingSlack = 1
	}
	cutoff := fmtTime(time.Now().Add(-ShortSessionQuietPeriod))
	query := `SELECT COUNT(*)
		FROM sessions s
		WHERE s.extracted_at IS NOT NULL
		  AND s.message_count > ?
		  AND EXISTS (SELECT 1 FROM fork_coverage f WHERE f.session_id = s.id)
		  AND NOT EXISTS (SELECT 1 FROM fork_coverage c
			WHERE c.session_id = s.id AND c.from_msg_idx <= 0
			  AND c.to_msg_idx >= s.message_count - CASE
				WHEN ` + sessionFinishedWhere("s") + ` THEN 1
				ELSE ? END)`
	var count int
	err := s.db.QueryRow(query, ShortSessionMaxMessages-1, cutoff, growingSlack).Scan(&count)
	return count, err
}

// MarkSessionNarrative sets narrative_at to now, marking the session as having a narrative.
func (s *Store) MarkSessionNarrative(sessionID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET narrative_at = ? WHERE id = ?`,
		time.Now().Format(time.RFC3339), sessionID)
	return err
}

var ExtractionSessionSignatures = []string{
	"You are a knowledge extractor",     // Main extraction (prompt.go:42)
	"You are a knowledge organizer",     // Cluster labeling (cluster.go:213)
	"You read an excerpt from a Claude", // Summarization pass 1 (prompt.go:161)
	"You compare new learnings with",    // Evolution (prompt.go:209)
	"You analyze ALL learnings of a",    // Bulk evolution (prompt.go:218)
	"Du analysierst Learnings aus",      // Cross-project evolution (prompt.go:239)
	"Du destillierst einen Cluster",     // Cluster distillation (prompt.go:281)
	"You review Knowledge Gaps from",    // Knowledge gap review (gap_review.go)
	"Du bist der Briefing-Autor",        // Narrative/briefing generation (narrative.go)
}

// MarkExtractionSessionsExtracted marks daemon-internal LLM sessions (cluster labeling,
// evolution, distillation etc.) as already extracted. These sessions stay below
// ShortSessionMaxMessages and are self-referential waste from the extraction
// pipeline's own LLM calls.
func (s *Store) MarkExtractionSessionsExtracted() (int64, error) {
	var total int64
	now := time.Now().Format(time.RFC3339)
	for _, sig := range ExtractionSessionSignatures {
		res, err := s.db.Exec(`
			UPDATE sessions SET extracted_at = ?
			WHERE extracted_at IS NULL
			AND message_count <= ?
			AND first_message LIKE ?`, now, ShortSessionMaxMessages-1, "%"+sig+"%")
		if err != nil {
			return 0, fmt.Errorf("mark extraction sessions: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
