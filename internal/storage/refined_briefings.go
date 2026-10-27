package storage

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// refinedBriefingTTL is how long a refined briefing may be served. It mirrors the
// daemon's refresh cadence (24h ticker and daemon-start scan in daemon.go), so an
// expired row is regenerated at the next scan instead of being served stale. A
// shorter TTL does not make the text fresher, it only forces an LLM call before
// the scan can run, and the 2h cycle was changed to 24h on purpose to save cost
// (commit 155d7723, "briefing refine interval: 2h -> 24h").
//
// The TTL applies to the text and to the change hash alike: a hash that outlived
// the row would make the refresh loop compare a fresh fingerprint against it and
// skip regeneration.
const refinedBriefingTTL = 24 * time.Hour

// freshRefinedBriefingWhere is the TTL predicate, written with julianday because
// generated_at arrives in two layouts: RFC3339 from SaveRefinedBriefing and
// SQLite's CURRENT_TIMESTAMP default. A lexical compare would order them wrong.
//
// The column is unqualified ("generated_at") for a plain read and qualified
// ("rb.generated_at") where the predicate belongs in a join condition, which is the
// only correct place for it: in the WHERE clause of a LEFT JOIN it would turn the
// join into an inner one and hide the projects without any row. It takes exactly one
// placeholder, refinedBriefingCutoff().
func freshRefinedBriefingWhere(column string) string {
	return `julianday(` + column + `) > julianday(?)`
}

func refinedBriefingCutoff() string {
	return fmtTime(time.Now().Add(-refinedBriefingTTL))
}

// GetRefinedBriefing returns the cached refined briefing for a project, or empty string if not found.
func (s *Store) GetRefinedBriefing(project string) (string, error) {
	var text string
	err := s.readerDB().QueryRow("SELECT refined_text FROM refined_briefings WHERE project = ? AND "+freshRefinedBriefingWhere("generated_at"),
		project, refinedBriefingCutoff()).Scan(&text)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return text, nil
}

// SaveRefinedBriefing upserts the refined briefing cache for a project.
func (s *Store) SaveRefinedBriefing(project, rawHash, refinedText, modelUsed string) error {
	_, err := s.db.Exec(`INSERT INTO refined_briefings (project, raw_hash, refined_text, model_used, generated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(project) DO UPDATE SET raw_hash=excluded.raw_hash, refined_text=excluded.refined_text, model_used=excluded.model_used, generated_at=excluded.generated_at`,
		project, rawHash, refinedText, modelUsed, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetRefinedBriefingHash returns the raw_hash of the cached briefing, or empty if not found.
// An expired row reads as empty so callers that compare the hash before
// regenerating (the daemon's refresh loop) see a miss instead of a stale hit.
func (s *Store) GetRefinedBriefingHash(project string) (string, error) {
	var hash string
	err := s.readerDB().QueryRow("SELECT raw_hash FROM refined_briefings WHERE project = ? AND "+freshRefinedBriefingWhere("generated_at"),
		project, refinedBriefingCutoff()).Scan(&hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return hash, nil
}

// CountFreshRefinedBriefings counts the cached briefings the read path would
// actually serve. An expired row is not a briefing, so a plain row count overstates
// the cache in the setup status.
func (s *Store) CountFreshRefinedBriefings() (int, error) {
	var count int
	err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM refined_briefings WHERE `+freshRefinedBriefingWhere("generated_at"),
		refinedBriefingCutoff()).Scan(&count)
	return count, err
}

// ProjectChangeFingerprint computes a lightweight fingerprint from learnings + sessions counts
// and timestamps. This is stable between checks unless actual data changes (new sessions,
// new/superseded learnings). Avoids generating the full raw briefing just to check for changes.
//
// A failed read is returned as an error, never folded into the value: the refresh
// loop compares the fingerprint against the stored hash, so a zero-value fingerprint
// would read as "nothing changed" and hold a stale briefing in place without a trace.
func (s *Store) ProjectChangeFingerprint(projectShort string) (string, error) {
	// learnings.project holds the full path for rows written after the path
	// rekeying, a legacy short name for older rows, and a worktree path for rows
	// written inside a checkout. A fingerprint scoped to project alone stayed
	// constant for the latter two, so their briefing was never refreshed.
	scoped, scopeArgs := learningFingerprintScope(projectShort)
	sessionScoped, sessionArgs := sessionFingerprintScope(projectShort)

	var sessionCount int
	var maxSessionAt sql.NullString
	if err := s.readerDB().QueryRow(`SELECT COUNT(*), MAX(indexed_at) FROM sessions WHERE `+sessionScoped, sessionArgs...).Scan(&sessionCount, &maxSessionAt); err != nil {
		return "", fmt.Errorf("fingerprint sessions: %w", err)
	}

	var learningCount int
	var maxLearningAt sql.NullString
	if err := s.readerDB().QueryRow(`SELECT COUNT(*), MAX(created_at) FROM learnings WHERE `+scoped+` AND superseded_by IS NULL`,
		scopeArgs...).Scan(&learningCount, &maxLearningAt); err != nil {
		return "", fmt.Errorf("fingerprint learnings: %w", err)
	}

	// The same split as in the cluster fingerprint: a row whose successor was
	// hard-deleted is not a replacement, and the transition between the two must
	// move the hash or the briefing is never rebuilt (YM-443).
	var supersededCount, detachedCount int
	if err := s.readerDB().QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN superseded_by = ? THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(CASE WHEN superseded_by = ? THEN 1 ELSE 0 END), 0)
		FROM learnings WHERE `+scoped+` AND superseded_by IS NOT NULL`,
		append([]any{SupersededByDeleted, SupersededByDeleted}, scopeArgs...)...).Scan(&supersededCount, &detachedCount); err != nil {
		return "", fmt.Errorf("fingerprint superseded learnings: %w", err)
	}

	var maxDirectiveAt sql.NullString
	if err := s.readerDB().QueryRow(`SELECT MAX(generated_at) FROM persona_directives`).Scan(&maxDirectiveAt); err != nil {
		return "", fmt.Errorf("fingerprint persona directives: %w", err)
	}

	raw := fmt.Sprintf("s:%d:%s|l:%d:%s|x:%d|d:%d|p:%s", sessionCount, maxSessionAt.String, learningCount, maxLearningAt.String, supersededCount, detachedCount, maxDirectiveAt.String)
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h[:16]), nil
}

// sessionFingerprintScope builds the same row predicate as learningFingerprintScope,
// on the sessions table. project_short carries the full path since ProjectShortFromPath,
// so a session written inside a worktree checkout keeps its own value and the parent
// project's fingerprint stayed constant while work happened there.
//
// Unlike learnings, sessions have no canonical_project column. A row that still
// carries the legacy short name is deliberately out of scope: matching it by basename
// would pull in every repository that shares the folder name, the collision the
// project scope exists to prevent.
func sessionFingerprintScope(project string) (string, []any) {
	if project == "" {
		return `project_short = ''`, nil
	}
	if filepath.IsAbs(project) {
		clean := filepath.Clean(project)
		root := models.RepositoryRoot(clean)
		if root != "" && isRepositoryRoot(root) {
			prefix := root + string(filepath.Separator)
			return `(project_short = ? OR substr(project_short, 1, length(?)) = ?)`,
				[]any{root, prefix, prefix}
		}
		return `project_short = ?`, []any{clean}
	}
	return `project_short = ?`, []any{project}
}

// learningFingerprintScope builds the row predicate for one project's learnings.
//
// A path scopes on its repository root, worktree checkouts included, plus the rows
// that predate the path rekeying and still carry the short name. Matching
// canonical_project alone would merge repositories that share a folder name, the
// collision models.ProjectMatches exists to prevent and the reason the narrative
// prune scopes the same way. The prefix test measures with SQLite's length(), not
// len(): substr counts characters and a non-ASCII path would otherwise cut wrong.
func learningFingerprintScope(project string) (string, []any) {
	if project == "" {
		return `project = ''`, nil
	}
	if filepath.IsAbs(project) {
		clean := filepath.Clean(project)
		root := models.RepositoryRoot(clean)
		if root != "" && isRepositoryRoot(root) {
			prefix := root + string(filepath.Separator)
			return `(project = ? OR substr(project, 1, length(?)) = ? OR (project NOT LIKE '/%' AND canonical_project = ?))`,
				[]any{root, prefix, prefix, models.CanonicalProject(clean)}
		}
		return `(project = ? OR (project NOT LIKE '/%' AND canonical_project = ?))`,
			[]any{clean, models.CanonicalProject(clean)}
	}
	return `(project = ? OR canonical_project = ?)`, []any{project, models.CanonicalProject(project)}
}
