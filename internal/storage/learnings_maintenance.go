package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// DeleteOldNarratives deletes a project's narratives beyond the N most recent.
// Narratives don't accumulate value — only the latest 2-3 matter for briefing/profiles.
//
// The callers pass sessions.project_short, an absolute path; the former query
// compared that path against canonical_project, which holds the basename, so it
// matched nothing and every project stacked its narratives. The rows are now
// scoped to the caller's repository: canonical_project is not a safe scope for a
// delete, because two repositories named alike share it and a worktree resolves to
// its parent's value. A path therefore scopes on the repository root (worktrees
// included), a bare name on canonical_project.
//
// The keep set is ordered by created_at and id, so a tie cannot decide which
// narrative survives.
//
// spareIDs name the rows that must survive whatever their position. A narrative
// carries the session's start time, so a story written for an older session sorts
// to the bottom of its project and would be the first the retention drops while
// the caller still marks that session as narrated.
func (s *Store) DeleteOldNarratives(project string, keep int, spareIDs ...int64) (int64, error) {
	if project == "" {
		return 0, nil
	}
	if keep < 1 {
		return 0, fmt.Errorf("delete old narratives: keep must be at least 1, got %d", keep)
	}
	scope, scopeArgs := narrativeScope(project)
	narrativeRows := `SELECT id FROM learnings
			WHERE category = 'narrative' AND ` + scope

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// The spared ids come from the caller's run and can outnumber SQLite's variable
	// limit, so they go through a temp table instead of a placeholder list. It lives
	// on this connection and is emptied per call.
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS narrative_spares (id INTEGER PRIMARY KEY)`); err != nil {
		return 0, fmt.Errorf("delete old narratives: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM narrative_spares`); err != nil {
		return 0, fmt.Errorf("delete old narratives: %w", err)
	}
	for _, batch := range chunkInt64(spareIDs, narrativeIDBatch) {
		values := strings.TrimRight(strings.Repeat("(?),", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO narrative_spares (id) VALUES `+values, args...); err != nil {
			return 0, fmt.Errorf("delete old narratives: %w", err)
		}
	}

	// Everything of this project that is neither a spared row nor one of the keep
	// newest ones.
	oldRows := narrativeRows + `
			AND id NOT IN (SELECT id FROM narrative_spares)`
	args := append([]any{}, scopeArgs...)
	// Two keep sets, not one: the newest by session time is what briefing reads,
	// and the newest by insertion order is what this process just wrote. A caller
	// that prunes right after its insert, or a concurrent caller, would otherwise
	// delete a story that was written seconds ago whenever the session was old.
	oldRows += `
			AND id NOT IN (` + narrativeRows + `
				ORDER BY created_at DESC, id DESC
				LIMIT ?)
			AND id NOT IN (` + narrativeRows + `
				ORDER BY id DESC
				LIMIT ?)`
	args = append(args, scopeArgs...)
	args = append(args, keep)
	args = append(args, scopeArgs...)
	args = append(args, keep)

	// Clean up AQ-FTS entries for learnings about to be deleted
	if _, err := tx.Exec(`DELETE FROM anticipated_queries_fts WHERE learning_id IN (
		`+oldRows+`
	)`, args...); err != nil {
		return 0, fmt.Errorf("delete old narratives fts: %w", err)
	}
	// The base rows behind those entries, in the same transaction: the FTS entry is
	// gone already, so an orphan row here would be invisible and permanent.
	if _, err := tx.Exec(`DELETE FROM learning_anticipated_queries WHERE learning_id IN (`+oldRows+`)`, args...); err != nil {
		return 0, fmt.Errorf("delete old narratives anticipated queries: %w", err)
	}

	// learnings_fts carries no delete trigger (schema.go drops them on purpose),
	// and its inserts are synced by hand. A row left behind would keep the deleted
	// narrative's text in the index and skew the term statistics of every search.
	if _, err := tx.Exec(`DELETE FROM learnings_fts WHERE rowid IN (`+oldRows+`)`, args...); err != nil {
		return 0, fmt.Errorf("delete old narratives learnings fts: %w", err)
	}

	// Inbound references must be cleared before the rows disappear: a retired
	// row would otherwise keep a positive superseded_by into nothing, and a
	// surviving successor a supersedes that names no row. The retired row stays
	// retired (the sentinel is negative) and its chain ends at the boundary.
	if _, err := tx.Exec(`UPDATE learnings SET superseded_by = ? WHERE superseded_by IN (`+oldRows+`)`,
		append([]any{SupersededByDeleted}, args...)...); err != nil {
		return 0, fmt.Errorf("delete old narratives clear inbound superseded_by: %w", err)
	}
	if _, err := tx.Exec(`UPDATE learnings SET supersedes = NULL WHERE supersedes IN (`+oldRows+`)`, args...); err != nil {
		return 0, fmt.Errorf("delete old narratives clear inbound supersedes: %w", err)
	}

	result, err := tx.Exec(`DELETE FROM learnings WHERE id IN (`+oldRows+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("delete old narratives: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete old narratives: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// DanglingSupersedeEdge names one reference that points at a learning which no
// longer exists. Column is superseded_by or supersedes; the edge is always
// positive, since a non-positive value is a sentinel and not an id.
type DanglingSupersedeEdge struct {
	ID     int64
	Column string
	Target int64
}

const (
	danglingSupersededBy = `SELECT id FROM learnings l
			WHERE l.superseded_by > 0
			  AND NOT EXISTS (SELECT 1 FROM learnings t WHERE t.id = l.superseded_by)`
	danglingSupersedes = `SELECT id FROM learnings l
			WHERE l.supersedes > 0
			  AND NOT EXISTS (SELECT 1 FROM learnings t WHERE t.id = l.supersedes)`
)

// RepairDanglingSupersedeEdges reports every positive supersede edge whose
// target row is gone, and clears those references when apply is set:
// superseded_by becomes SupersededByDeleted, so the row keeps its retire and its
// chain ends there, and supersedes becomes NULL because there is no predecessor
// left to point at.
//
// The delete paths clear their inbound references themselves (YM-435), so what
// remains comes from an older binary or a manual delete. This is the sweep for
// those rows: report first, apply once the list looks right.
//
// The returned list is the report taken before the apply. The apply repeats the
// lookup as the WHERE clause of its own UPDATEs and runs even when the report is
// empty, so a row that becomes dangling between the two calls is cleared as
// well. The list is therefore not a snapshot of exactly what changed; the second
// return value is, it counts the rows the UPDATEs touched. supersede_reason is
// left alone: it still says why the row was retired, and that reason remains
// true after its successor is gone.
func (s *Store) RepairDanglingSupersedeEdges(apply bool) ([]DanglingSupersedeEdge, int64, error) {
	rows, err := s.readerDB().Query(`SELECT id, 'superseded_by' AS col, superseded_by AS target FROM learnings l
			WHERE l.superseded_by > 0
			  AND NOT EXISTS (SELECT 1 FROM learnings t WHERE t.id = l.superseded_by)
		UNION ALL
		SELECT id, 'supersedes', supersedes FROM learnings l
			WHERE l.supersedes > 0
			  AND NOT EXISTS (SELECT 1 FROM learnings t WHERE t.id = l.supersedes)
		ORDER BY id, col`)
	if err != nil {
		return nil, 0, fmt.Errorf("report dangling supersede edges: %w", err)
	}
	var edges []DanglingSupersedeEdge
	for rows.Next() {
		var e DanglingSupersedeEdge
		if err := rows.Scan(&e.ID, &e.Column, &e.Target); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan dangling supersede edge: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("report dangling supersede edges: %w", err)
	}
	rows.Close()

	if !apply {
		return edges, 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("repair dangling supersede edges: %w", err)
	}
	defer tx.Rollback()
	var cleared int64
	res, err := tx.Exec(`UPDATE learnings SET superseded_by = ? WHERE id IN (`+danglingSupersededBy+`)`,
		SupersededByDeleted)
	if err != nil {
		return nil, 0, fmt.Errorf("repair dangling superseded_by: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		cleared += n
	}
	res, err = tx.Exec(`UPDATE learnings SET supersedes = NULL WHERE id IN (` + danglingSupersedes + `)`)
	if err != nil {
		return nil, 0, fmt.Errorf("repair dangling supersedes: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		cleared += n
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("repair dangling supersede edges: %w", err)
	}
	return edges, cleared, nil
}

// narrativeScope builds the row predicate for one project. A path that names a
// repository (or lies inside a worktree checkout) scopes on the repository root;
// every other path scopes on itself, and a bare name on canonical_project.
//
// The repository check is what keeps a delete from widening: without it a plain
// parent directory such as a home directory would reach every project below it,
// and one prune would delete the narratives of all of them. A path that merely
// looks like a project is not evidence of a scope.
//
// The prefix test measures with SQLite's length(), not with len(): substr counts
// characters, len() counts bytes, and a path with non-ASCII characters would
// otherwise cut too late and drop every descendant from the scope.
func narrativeScope(project string) (string, []any) {
	clean := filepath.Clean(project)
	if !filepath.IsAbs(clean) {
		return `canonical_project = ?`, []any{models.CanonicalProject(project)}
	}
	if root := models.RepositoryRoot(clean); root != "" && isRepositoryRoot(root) {
		prefix := root + string(filepath.Separator)
		return `(project = ? OR substr(project, 1, length(?)) = ?)`, []any{root, prefix, prefix}
	}
	return `project = ?`, []any{clean}
}

// isRepositoryRoot reports whether the path carries a git directory. The check
// covers the file form as well, which is what a linked worktree uses.
func isRepositoryRoot(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

// chunkInt64 splits ids into batches of at most size entries.
func chunkInt64(ids []int64, size int) [][]int64 {
	if size < 1 || len(ids) == 0 {
		return nil
	}
	var batches [][]int64
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
}

// SupersedeNarrativesBySession marks all active narratives for a session as superseded.
// Returns the IDs of superseded learnings for vector store cleanup.
func (s *Store) SupersedeNarrativesBySession(sessionID, reason string) ([]int64, error) {
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE category = 'narrative' AND session_id = ? AND superseded_by IS NULL
		RETURNING id`, reason, sessionID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// SupersedeShortNarratives marks narratives from sessions that are both shorter
// than ShortSessionMaxMessages and finished (silent for ShortSessionQuietPeriod)
// as superseded. A session that is short only at the moment of the check may
// still grow, and its narrative then describes a session that is still being
// written. Returns the IDs of superseded learnings for vector store cleanup.
func (s *Store) SupersedeShortNarratives() ([]int64, error) {
	cutoff := fmtTime(time.Now().Add(-ShortSessionQuietPeriod))
	where := `category = 'narrative' AND superseded_by IS NULL
		AND session_id IN (
			SELECT id FROM sessions
			WHERE message_count < ?
			  AND ` + sessionFinishedWhere("") + `
		)`

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// One statement on one handle: the former SELECT on the read connection plus
	// an UPDATE on the write connection could return an id set that the update
	// never touched, which left stale vectors behind or removed live ones.
	rows, err := tx.Query(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE `+where+`
		RETURNING id`, shortNarrativeSupersedeReason, ShortSessionMaxMessages, cutoff)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// A session whose narrative a short-session cleanup removed must be narratable
	// again, so its narrative_at has to go.
	if _, err := clearSupersededNarrativeMarkers(tx); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// execer is satisfied by *sql.DB and *sql.Tx, so the marker reset can run inside
// the cleanup transaction and on its own.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// clearSupersededNarrativeMarkers drops narrative_at for sessions whose narrative
// one of the short-session cleanups removed. narrative_at is what the generator
// reads to skip a session, so a marker left behind keeps that session unnarrated
// for good once it grows.
//
// The two short reasons are the whole scope: they prove the removal was the
// cleanup's decision. A narrative superseded for any other reason may have been
// removed by hand, and handing that session back to the generator recreates the
// text the removal was meant to get rid of. Sessions that still carry a live
// narrative keep their marker.
//
// It runs on its own rather than only after a fresh supersede, because a session
// cleaned before this rule existed no longer matches the cleanup.
func clearSupersededNarrativeMarkers(db execer) (int64, error) {
	res, err := db.Exec(`UPDATE sessions SET narrative_at = NULL
		WHERE narrative_at IS NOT NULL
		  AND EXISTS (
			SELECT 1 FROM learnings gone
			WHERE gone.session_id = sessions.id AND gone.category = 'narrative'
			  AND gone.superseded_by IS NOT NULL
			  AND gone.supersede_reason IN (?, ?)
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM learnings live
			WHERE live.session_id = sessions.id
			  AND live.category = 'narrative' AND live.superseded_by IS NULL
		  )`, shortNarrativeSupersedeReason, ShortSessionSupersedeReason)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearSupersededNarrativeMarkers is the standalone entry point for the callers
// that supersede narrative rows without running the narrative cleanup.
func (s *Store) ClearSupersededNarrativeMarkers() (int64, error) {
	return clearSupersededNarrativeMarkers(s.db)
}

// ShortSessionQuietPeriod is how long a session must have been silent before the
// short-session cleanup may treat it as finished. ended_at is the session's last
// activity (for opencode: time_updated), so it proves nothing about completion on
// its own. A session that is short only at the moment of the check may still grow.
const ShortSessionQuietPeriod = 24 * time.Hour

// ShortSessionMaxMessages is the message count below which a session counts as
// short. Extraction, narrative generation and both cleanups key on it: a session
// the generator accepts must never be cleaned up as short, and a session it
// rejects must never keep its rows.
const ShortSessionMaxMessages = 6

// SessionFinished reports whether a session has stopped growing: it carries an
// end timestamp that lies at least ShortSessionQuietPeriod in the past.
func SessionFinished(endedAt, now time.Time) bool {
	return !endedAt.IsZero() && now.Sub(endedAt) >= ShortSessionQuietPeriod
}

// sessionFinishedWhere is the SQL twin of SessionFinished for a sessions table
// addressed by alias ("" for a bare column). The NULL check belongs to it:
// datetime(NULL) is NULL, so the comparison alone would silently drop every
// session that never carried an end timestamp.
func sessionFinishedWhere(alias string) string {
	col := "ended_at"
	if alias != "" {
		col = alias + ".ended_at"
	}
	return col + " IS NOT NULL AND datetime(" + col + ") <= datetime(?)"
}

// ShortSessionSupersedeReason is the supersede_reason the short-session cleanup
// writes. The audit query and the restore path key on this exact string.
const ShortSessionSupersedeReason = "session too short for extraction"

// shortNarrativeSupersedeReason is the reason the narrative cleanup writes. It
// separates that cleanup from the extraction cleanup and, together with
// ShortSessionSupersedeReason, marks the removals whose session marker
// clearSupersededNarrativeMarkers resets.
const shortNarrativeSupersedeReason = "session too short"

// userSourcedLearningSources are the sources the user stated or agreed to. Both
// cleanups spare them, and each finder and restore covers the cleanup class it
// belongs to, so a spared source can never turn into an unrecoverable one.
//
// The entries are lowercase because every comparison goes through
// LOWER(TRIM(source)): `source` is caller-written free text, and 'User_Stated' or
// ' agreed_upon ' is the same claim as the canonical spelling.
const userSourcedLearningSources = "'user_stated', 'agreed_upon', 'user_override'"

// sqlTrimSet is the character set SQLite needs spelled out: the one-argument TRIM
// only strips spaces, while Go's strings.TrimSpace also folds tabs and line
// breaks. A comparison whose two sides disagree about whitespace disagrees about
// the row, so both sides use this set.
const sqlTrimSet = `' ' || char(9) || char(10) || char(11) || char(12) || char(13)`

// userOwnedSourcePredicate is the SQL predicate for "the user owns this row". The
// column is fixed: a predicate that takes a column name by string concatenation is
// the shape an injection grows out of later. The trim set covers the ASCII
// whitespace strings.TrimSpace removes; exotic Unicode spaces are not folded, so
// a source padded with them counts as a different claim.
func userOwnedSourcePredicate() string {
	return `LOWER(TRIM(COALESCE(source, ''), ` + sqlTrimSet + `)) IN (` + userSourcedLearningSources + `)`
}

// IsUserOwnedSource reports whether a learning source carries the user's own
// statement. The cleanup paths spare that set, and cluster distillation may add
// knowledge without replacing it.
func IsUserOwnedSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "user_stated", "agreed_upon", "user_override":
		return true
	}
	return false
}

// shortSessionCleanupWhere is the shared predicate of the collect and the update
// query: only auto-extracted learnings of sessions that are both short and
// finished. Learnings the user stated, agreed to or overrode are never discarded
// because a session stayed short. SupersedeSessionLearnings uses the same source
// set, so the cleanups and the reextract path agree on what the user owns.
func shortSessionCleanupWhere() string {
	return `superseded_by IS NULL
		AND NOT (` + userOwnedSourcePredicate() + `)
		AND session_id IN (
			SELECT id FROM sessions
			WHERE message_count < ?
			  AND ` + sessionFinishedWhere("") + `
		)`
}

// SupersedeShortSessionLearnings marks auto-extracted learnings from sessions that
// are both shorter than ShortSessionMaxMessages and finished (silent for
// ShortSessionQuietPeriod) as superseded. Returns the IDs of the superseded
// learnings.
func (s *Store) SupersedeShortSessionLearnings() ([]int64, error) {
	cutoff := fmtTime(time.Now().Add(-ShortSessionQuietPeriod))
	where := shortSessionCleanupWhere()
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE `+where+`
		RETURNING id`, ShortSessionSupersedeReason, ShortSessionMaxMessages, cutoff)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// junkCleanupSupersedeReason is the reason CleanupJunkLearnings writes onto the
// rows it discards.
const junkCleanupSupersedeReason = "junk cleanup"

// findSupersededUserOwned lists the rows one cleanup discarded although the user
// owns them: a user-sourced learning superseded under the given reason. limit <= 0
// returns all rows.
func (s *Store) findSupersededUserOwned(reason string, limit int) ([]models.Learning, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.readerDB().Query(`SELECT id, COALESCE(session_id, ''), category, content,
			COALESCE(project, ''), COALESCE(source, ''), created_at
		FROM learnings
		WHERE supersede_reason = ?
		  AND `+userOwnedSourcePredicate()+`
		ORDER BY created_at DESC
		LIMIT ?`, reason, limit)
	if err != nil {
		return nil, fmt.Errorf("find wrongly superseded: %w", err)
	}
	defer rows.Close()

	var out []models.Learning
	for rows.Next() {
		var l models.Learning
		var createdAt string
		if err := rows.Scan(&l.ID, &l.SessionID, &l.Category, &l.Content,
			&l.Project, &l.Source, &createdAt); err != nil {
			return nil, fmt.Errorf("find wrongly superseded: scan: %w", err)
		}
		l.CreatedAt = parseTime(createdAt)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find wrongly superseded: %w", err)
	}
	return out, nil
}

// restoreSupersededUserOwned reactivates the given learnings of one cleanup class
// after review. The predicate repeats the finder's class, so an id outside it
// (auto-extracted content, a different supersede reason) is left untouched.
func (s *Store) restoreSupersededUserOwned(ids []int64, reason string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, reason)
	res, err := s.db.Exec(`UPDATE learnings
		SET superseded_by = NULL, supersede_reason = NULL, valid_until = NULL, supersede_status = NULL
		WHERE id IN (`+placeholders+`)
		  AND supersede_reason = ?
		  AND `+userOwnedSourcePredicate(), args...)
	if err != nil {
		return 0, fmt.Errorf("restore wrongly superseded: %w", err)
	}
	return res.RowsAffected()
}

// FindWronglySupersededShortSessionLearnings lists the learnings that the
// short-session cleanup discarded although the user stated them: a user-sourced
// learning superseded with ShortSessionSupersedeReason. Rows damaged before the
// source rule existed are repaired by the orchestrator through
// RestoreWronglySupersededShortSessionLearnings, never automatically.
// limit <= 0 returns all rows.
func (s *Store) FindWronglySupersededShortSessionLearnings(limit int) ([]models.Learning, error) {
	return s.findSupersededUserOwned(ShortSessionSupersedeReason, limit)
}

// RestoreWronglySupersededShortSessionLearnings reactivates the given learnings
// after review. Returns the number of reactivated rows.
func (s *Store) RestoreWronglySupersededShortSessionLearnings(ids []int64) (int64, error) {
	return s.restoreSupersededUserOwned(ids, ShortSessionSupersedeReason)
}

// FindWronglySupersededJunkLearnings lists the learnings that the junk cleanup
// discarded although the user stated them. The junk cleanup had no finder and no
// restore for a long time, so its victims were unreachable by any tool. limit <= 0
// returns all rows.
func (s *Store) FindWronglySupersededJunkLearnings(limit int) ([]models.Learning, error) {
	return s.findSupersededUserOwned(junkCleanupSupersedeReason, limit)
}

// RestoreWronglySupersededJunkLearnings reactivates the given junk-cleanup
// victims after review. Returns the number of reactivated rows.
func (s *Store) RestoreWronglySupersededJunkLearnings(ids []int64) (int64, error) {
	return s.restoreSupersededUserOwned(ids, junkCleanupSupersedeReason)
}

// ruleBasedSupersedeReasonPrefix is the reason prefix the rule-based dedup writes
// (near-duplicate, embedding near-duplicate, cross-chunk near-duplicate,
// substanzlos). supersedeUnlessHighTrust spares high-trust rows on the successor
// path and user-owned rows on the no-successor path, so the displacer class below
// keeps growing wherever a user-owned row loses on trust, while the dedup no
// longer grows the no-successor class (YM-467). A repair is the only way back for
// both.
const ruleBasedSupersedeReasonPrefix = "rule-based"

// RuleBasedSupersedeVictim is one learning a rule-based dedup displaced although
// the user owns it, together with the row that displaced it. A DisplacerID of 0
// means the retire had no successor at all. Content is carried because the
// no-successor class does not filter on content shape: the listing is the only
// surface that separates a statement from a tombstone marker.
type RuleBasedSupersedeVictim struct {
	ID              int64
	Source          string
	CreatedAt       time.Time
	DisplacerID     int64
	DisplacerSource string
	Reason          string
	Content         string
}

// RuleBasedSupersedeScan is what the finder reports: the candidates of both
// damaged classes, plus every other row of the same damaged class the finder
// leaves out, counted per reason. The counts are not decoration. The dry run is
// the only surface an operator decides on, and a report that names 90 of 337
// equally damaged rows reads as complete while it is not (YM-457).
type RuleBasedSupersedeScan struct {
	// Victims: a rule-based dedup displaced the row against a real displacer
	// that is still active.
	Victims []RuleBasedSupersedeVictim

	// NoDisplacer: superseded_by = 0 — retired as substance-free, with no
	// successor to point at. Not a supersede chain, so it owns neither the
	// displacer checks nor the displacer apply.
	NoDisplacer []RuleBasedSupersedeVictim

	// SkippedHighTrustDisplacer: the displacer is user-owned too, so that
	// supersede is a legitimate chain rather than a rule-based displacement.
	SkippedHighTrustDisplacer int
	// SkippedInactiveDisplacer: the displacer was itself superseded, has expired,
	// or its row no longer exists, so clearing the edge would resurrect a row whose
	// successor is gone.
	SkippedInactiveDisplacer int
	// SkippedSentinel: superseded_by < 0 — a recording path retired the row with a
	// sentinel instead of a successor (-1 cleanup, -2 resolved, -4 deleted
	// successor). Not this repair's class: the sentinel is the record of why the
	// row is gone, and a deleted successor is the case the displacer branch
	// refuses too.
	SkippedSentinel int
	// SkippedEmptyContent: the displaced row holds nothing to bring back.
	SkippedEmptyContent int
	// UnlistedByLimit: candidates beyond the requested limit, counted across both
	// classes.
	UnlistedByLimit int
}

// ruleBasedVictimWhere is the applier's re-check: a row a rule-based dedup
// displaced (a real displacer, a reason with the rule-based prefix), still
// holding content, and owned by the user.
//
// The displacer conditions sit in a correlated subquery on purpose. The source
// predicate is written without a column name, because taking one by string
// concatenation is the shape an injection grows out of later; one learnings row is
// therefore in scope, and inside the subquery the unqualified columns are the
// displacer's.
//
// FindRuleBasedSupersededUserLearnings classifies the same conditions in Go, so it
// can count the rows it leaves out instead of dropping them silently. The two
// definitions must agree: a test restores every reported victim and expects the
// update to clear exactly that set.
func ruleBasedVictimWhere() string {
	return `superseded_by > 0
		AND supersede_reason LIKE '` + ruleBasedSupersedeReasonPrefix + `%'
		AND TRIM(COALESCE(content, ''), ` + sqlTrimSet + `) <> ''
		AND ` + userOwnedSourcePredicate() + `
		AND EXISTS (
			SELECT 1 FROM learnings d
			WHERE d.id = learnings.superseded_by
			  AND (d.superseded_by IS NULL OR d.superseded_by = 0)
			  AND (d.valid_until IS NULL OR julianday(d.valid_until) > julianday('now'))
			  AND NOT (` + userOwnedSourcePredicate() + `)
		)`
}

// ruleBasedNoDisplacerVictimWhere is the applier's re-check for the second class:
// a row the substance-free retire discarded, still holding content, owned by the
// user. The displacer branch is replaced by the sentinel condition, because there
// is no displacer whose state could be checked. The condition is exactly the 0
// the substance-free retire writes: a negative sentinel belongs to a recording
// path (cleanup, resolve, deleted successor) and keeps saying why the row is
// gone.
func ruleBasedNoDisplacerVictimWhere() string {
	return `superseded_by = 0
		AND supersede_reason LIKE '` + ruleBasedSupersedeReasonPrefix + `%'
		AND TRIM(COALESCE(content, ''), ` + sqlTrimSet + `) <> ''
		AND ` + userOwnedSourcePredicate()
}

// FindRuleBasedSupersededUserLearnings lists the learnings a rule-based dedup
// displaced although the user owns them, split into the two shapes that damage
// comes in: a row that lost against a displacer, and a row that was discarded
// with no successor at all. Every other row of the damaged class the finder
// leaves out is counted. Read-only by construction (readerDB), and the default of
// the command that uses it: nothing is restored without an explicit apply.
// limit <= 0 returns all candidates; a positive limit bounds each of the two
// listings, and the rows beyond it are counted.
func (s *Store) FindRuleBasedSupersededUserLearnings(limit int) (*RuleBasedSupersedeScan, error) {
	// superseded_by IS NOT NULL: an active row that still carries a leftover
	// reason is not damaged, and the appliers would refuse it. Filtering it here
	// keeps the listing and the two UPDATEs on the same set.
	rows, err := s.readerDB().Query(`SELECT id, COALESCE(source, ''), created_at, COALESCE(superseded_by, 0),
			COALESCE((SELECT COALESCE(d.source, '') FROM learnings d WHERE d.id = learnings.superseded_by), ''),
			COALESCE(supersede_reason, ''),
			COALESCE((SELECT CASE WHEN (d.superseded_by IS NULL OR d.superseded_by = 0)
			                       AND (d.valid_until IS NULL OR julianday(d.valid_until) > julianday('now'))
			                  THEN 1 ELSE 0 END
			            FROM learnings d WHERE d.id = learnings.superseded_by), 0),
			TRIM(COALESCE(content, ''), ` + sqlTrimSet + `)
		FROM learnings
		WHERE supersede_reason LIKE '` + ruleBasedSupersedeReasonPrefix + `%'
		  AND superseded_by IS NOT NULL
		  AND ` + userOwnedSourcePredicate() + `
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("find rule-based superseded: %w", err)
	}
	defer rows.Close()

	scan := &RuleBasedSupersedeScan{}
	for rows.Next() {
		var v RuleBasedSupersedeVictim
		var createdAt, content string
		var displacerActive int
		if err := rows.Scan(&v.ID, &v.Source, &createdAt, &v.DisplacerID,
			&v.DisplacerSource, &v.Reason, &displacerActive, &content); err != nil {
			return nil, fmt.Errorf("find rule-based superseded: scan: %w", err)
		}
		v.CreatedAt = parseTime(createdAt)
		v.Content = content
		switch {
		case content == "":
			scan.SkippedEmptyContent++
		case v.DisplacerID < 0:
			scan.SkippedSentinel++
		case v.DisplacerID == 0:
			if limit > 0 && len(scan.NoDisplacer) >= limit {
				scan.UnlistedByLimit++
				break
			}
			scan.NoDisplacer = append(scan.NoDisplacer, v)
		case displacerActive == 0:
			scan.SkippedInactiveDisplacer++
		case IsUserOwnedSource(v.DisplacerSource):
			scan.SkippedHighTrustDisplacer++
		case limit > 0 && len(scan.Victims) >= limit:
			scan.UnlistedByLimit++
		default:
			scan.Victims = append(scan.Victims, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find rule-based superseded: %w", err)
	}
	return scan, nil
}

// Skipped reports how many rows of the damaged class the finder left out. The
// two listed classes are not part of it: they are candidates.
func (s *RuleBasedSupersedeScan) Skipped() int {
	return s.SkippedHighTrustDisplacer + s.SkippedInactiveDisplacer +
		s.SkippedSentinel + s.SkippedEmptyContent + s.UnlistedByLimit
}

// RestoreRuleBasedSupersededUserLearnings reactivates the given learnings after
// review. The selection is re-checked per row rather than trusted: an id list is
// operator input, and an id must not turn into a general "reactivate anything"
// switch. Returns the number of reactivated rows.
func (s *Store) RestoreRuleBasedSupersededUserLearnings(ids []int64) (int64, error) {
	return s.restoreRuleBasedSuperseded(ids, ruleBasedVictimWhere(), "restore rule-based superseded")
}

// RestoreRuleBasedSupersededUserLearningsNoDisplacer reactivates the no-successor
// class after review, on the same terms as the displacer class: its own explicit
// call, and a per-row re-check against ruleBasedNoDisplacerVictimWhere. Returns
// the number of reactivated rows.
func (s *Store) RestoreRuleBasedSupersededUserLearningsNoDisplacer(ids []int64) (int64, error) {
	return s.restoreRuleBasedSuperseded(ids, ruleBasedNoDisplacerVictimWhere(), "restore rule-based superseded without displacer")
}

// restoreRuleBasedSuperseded clears the retire for the given ids, but only where
// the class predicate still holds.
func (s *Store) restoreRuleBasedSuperseded(ids []int64, where, what string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.Exec(`UPDATE learnings
		SET superseded_by = NULL, supersede_reason = NULL, valid_until = NULL, supersede_status = NULL
		WHERE id IN (`+placeholders+`)
		  AND `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return res.RowsAffected()
}

// CleanupJunkLearnings marks garbage learnings as superseded.
// Targets: too short (<10 chars), JSON fragments, conversation snippets, overly long unfinished items.
// Learnings the user stated or agreed to are spared: the markers describe the
// shape of a row, and a shape is no reason to drop the user's own statement.
// The guard is the same source set the short-session cleanup uses, so both
// cleanups agree on what the user owns.
// Returns the IDs of superseded learnings for vector store cleanup.
func (s *Store) CleanupJunkLearnings() ([]int64, error) {
	whereClause := "superseded_by IS NULL " +
		"AND NOT (" + userOwnedSourcePredicate() + ") AND (" +
		"length(content) < 10 " +
		"OR content LIKE '{\"_%' " +
		"OR content LIKE '[%tool_use%' " +
		"OR content LIKE 'Soll ich%' " +
		"OR content LIKE 'Moment,%' " +
		"OR content LIKE 'No skills needed%' " +
		"OR content LIKE '%```json%' " +
		"OR content LIKE '%```yaml%' " +
		"OR content LIKE '%GEFIXT:%' " +
		"OR content LIKE '%RESOLVED:%' " +
		"OR content LIKE '%FIXED:%' " +
		"OR (category = 'unfinished' AND length(content) > 500))"
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE `+whereClause+`
		RETURNING id`, junkCleanupSupersedeReason)
	if err != nil {
		return nil, fmt.Errorf("cleanup junk: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// SupersedeByIDs marks specific learnings as superseded by a given learning.
func (s *Store) SupersedeByIDs(ids []int64, supersededByID int64, reason string) error {
	return s.SupersedeLearningBatch(ids, supersededByID, reason)
}

// SupersedeSessionLearnings marks all auto-extracted learnings for a session as superseded.
// Preserves the user-owned sources, using the same predicate as the cleanups:
// fresh extraction replaces a machine-written row, but it cannot rebuild an
// agreement or an instruction the user gave.
// Returns the IDs of superseded learnings for vector store cleanup.
func (s *Store) SupersedeSessionLearnings(sessionID string) ([]int64, error) {
	where := `session_id = ? AND superseded_by IS NULL AND NOT (` + userOwnedSourcePredicate() + `)`
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = ?, supersede_reason = 'reextract', valid_until = datetime('now')
		WHERE `+where+`
		RETURNING id`,
		SupersededByReextract, sessionID)
	if err != nil {
		return nil, fmt.Errorf("supersede session learnings %s: %w", sessionID, err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// MigrateHitCounts copies hit_count, last_hit_at, match_count, inject_count, use_count,
// and save_count from superseded (reextract) learnings to their new active replacements,
// matched by session_id + category.
// Returns the number of updated learnings.
func (s *Store) MigrateHitCounts() (int64, error) {
	// For each (session_id, category) pair, sum counts from superseded learnings
	// and distribute to the new active ones. Uses MAX(last_hit_at) for the timestamp.
	result, err := s.db.Exec(`
		UPDATE learnings SET
			hit_count = hit_count + COALESCE((
				SELECT SUM(COALESCE(old.hit_count, 0))
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
				  AND old.hit_count > 0
			), 0),
			last_hit_at = COALESCE(learnings.last_hit_at, (
				SELECT MAX(old.last_hit_at)
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
				  AND old.last_hit_at IS NOT NULL
			)),
			match_count = match_count + COALESCE((
				SELECT SUM(COALESCE(old.match_count, 0))
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
			), 0),
			inject_count = inject_count + COALESCE((
				SELECT SUM(COALESCE(old.inject_count, 0))
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
			), 0),
			use_count = use_count + COALESCE((
				SELECT SUM(COALESCE(old.use_count, 0))
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
			), 0),
			save_count = save_count + COALESCE((
				SELECT SUM(COALESCE(old.save_count, 0))
				FROM learnings old
				WHERE old.session_id = learnings.session_id
				  AND old.category = learnings.category
				  AND old.superseded_by = ?
			), 0)
		WHERE learnings.superseded_by IS NULL
		  AND NOT (`+userOwnedSourcePredicate()+`)
		  AND EXISTS (
			SELECT 1 FROM learnings old
			WHERE old.session_id = learnings.session_id
			  AND old.category = learnings.category
			  AND old.superseded_by = ?
		)`, SupersededByReextract, SupersededByReextract,
		SupersededByReextract, SupersededByReextract,
		SupersededByReextract, SupersededByReextract,
		SupersededByReextract)
	if err != nil {
		return 0, fmt.Errorf("migrate hit counts: %w", err)
	}
	return result.RowsAffected()
}

// BackfillContentHashes fills empty content_hash fields on existing learnings.
func (s *Store) BackfillContentHashes(hashFn func(string) string) (int64, error) {
	var count int64
	s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE content_hash = '' OR content_hash IS NULL`).Scan(&count)
	if count == 0 {
		return 0, nil
	}

	type pendingHash struct {
		id      int64
		content string
	}

	rows, err := s.readerDB().Query(`SELECT id, content FROM learnings WHERE content_hash = '' OR content_hash IS NULL`)
	if err != nil {
		return 0, err
	}

	pending := make([]pendingHash, 0, count)
	for rows.Next() {
		var item pendingHash
		if err := rows.Scan(&item.id, &item.content); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`UPDATE learnings SET content_hash = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	var updated int64
	for _, item := range pending {
		hash := hashFn(item.content)
		if _, err := stmt.Exec(hash, item.id); err == nil {
			updated++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// BackfillProjects fills empty project fields from sessions table.
func (s *Store) BackfillProjects() (int64, error) {
	result, err := s.db.Exec(`UPDATE learnings SET project = (
		SELECT s.project_short FROM sessions s WHERE s.id = learnings.session_id
	) WHERE (project IS NULL OR project = '') AND session_id != ''`)
	if err != nil {
		return 0, fmt.Errorf("backfill projects: %w", err)
	}
	return result.RowsAffected()
}

// UpdateLearningContent updates the content of an active (non-superseded) learning
// and clears embedding_text to force re-embedding on next access.
//
// modernc/sqlite has a bug where the FTS5 'delete' command inside an AFTER UPDATE
// trigger always fails with "SQL logic error (1)" on a non-external-content FTS5
// table (content_rowid= without content=). Workaround: drop the trigger, perform
// the learnings UPDATE and the FTS sync manually, then recreate the trigger — all
// inside a single transaction so the schema change is invisible to other connections.
func (s *Store) UpdateLearningContent(id int64, newContent string) error {
	// Only proceed if the learning is active.
	var count int
	if err := s.readerDB().QueryRow(
		`SELECT COUNT(*) FROM learnings WHERE id = ? AND superseded_by IS NULL`, id,
	).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil // superseded or not found — no-op
	}
	learning, err := s.GetLearning(id)
	if err != nil {
		return err
	}
	learning.Content = newContent
	learning.EmbeddingText = ""
	_, embeddingContentHash := prepareEmbeddingTracking(learning)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Drop the FTS update trigger so it does not fire during our manual UPDATE.
	if _, err = tx.Exec(`DROP TRIGGER IF EXISTS learnings_fts_update`); err != nil {
		return err
	}
	// Update the base table.
	if _, err = tx.Exec(
		`UPDATE learnings
			 SET content = ?, content_hash = ?, embedding_text = '', embedding_status = 'pending', embedding_content_hash = ?, embedded_at = NULL
			 WHERE id = ? AND superseded_by IS NULL`,
		newContent, textutil.ContentHash(newContent), embeddingContentHash, id); err != nil {
		return err
	}
	// Sync FTS manually: remove old entry, insert new one.
	if _, err = tx.Exec(`DELETE FROM learnings_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO learnings_fts(rowid, content) VALUES (?, ?)`, id, newContent); err != nil {
		return err
	}
	// No trigger is recreated here: createSchema drops the FTS triggers on
	// purpose and relies on explicit sync (see schema.go).
	return tx.Commit()
}
