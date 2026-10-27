// Package repair holds one-off repairs of already stored data. Every repair has
// a dry run that only reads and an apply that writes inside one transaction
// after copying the affected rows into a backup table, so a mistaken run can be
// undone from that table and a second run changes nothing. A repair that only
// inserts needs no backup table: it overwrites nothing, its own inserted rows are
// the undo target, and the same guard makes a second run insert nothing.
package repair

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/repo"
)

// DefaultSince is the cut-off of the session-attribution repair: the opencode v2
// service layout, which made the global active session wrong for concurrent
// sessions, started before 2026-10-01.
const DefaultSince = "2026-10-01"

// BackupTable receives a copy of every row the repair overwrites. Restoring
// from it undoes an apply.
const BackupTable = "learnings_repair_backup_20261016"

// Status describes what the repair would do with one learning.
type Status string

const (
	// StatusRepair: exactly one session contains the learning's text and the
	// stored values differ from the proposal.
	StatusRepair Status = "REPAIR"
	// StatusAlready: the stored values already match the proposal.
	StatusAlready Status = "ALREADY"
	// StatusUnresolved: no session, or more than one, contains the text. The
	// row is left untouched.
	StatusUnresolved Status = "UNRESOLVED"
)

// Row is one learning and the attribution the repair proposes for it.
type Row struct {
	ID               int64
	OldSession       string
	NewSession       string
	OldProject       string
	NewProject       string
	NewProjectSource string
	Status           Status
	Basis            string
}

// Report is the complete dry-run result.
type Report struct {
	Since      string
	Candidates int
	Rows       []Row
}

// Counts returns how many rows fall into each status.
func (r *Report) Counts() (repair, already, unresolved int) {
	for _, row := range r.Rows {
		switch row.Status {
		case StatusRepair:
			repair++
		case StatusAlready:
			already++
		default:
			unresolved++
		}
	}
	return repair, already, unresolved
}

// Table renders the dry run as the id / old / new / basis table the operator
// reads before applying.
func (r *Report) Table() string {
	var sb strings.Builder
	sb.WriteString("id\told session\tnew session\told project\tnew project\tstatus\tbasis\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&sb, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.ID, orDash(row.OldSession), orDash(row.NewSession),
			orDash(row.OldProject), orDash(row.NewProject), row.Status, row.Basis)
	}
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type candidate struct {
	id            int64
	content       string
	session       string
	project       string
	canonical     string
	projectSource string
}

// needsProject reports whether the stored project cannot stand on its own: a
// project the v0.65 migration blanked reads as an empty string, '.' never named
// a repository, and a value without a leading slash is a name the resolver
// could not turn into a path.
func needsProject(project string) bool {
	return !strings.HasPrefix(project, "/")
}

// uniqueKnownPath returns the one known absolute project path whose basename is
// canonical, or "" when none or several carry it. canonical_project holds the
// historical basename of a project, so it identifies a project only while that
// basename is unique in the database.
func uniqueKnownPath(known []string, canonical string) string {
	if canonical == "" || canonical == "." {
		return ""
	}
	match := ""
	for _, path := range known {
		if filepath.Base(path) != canonical {
			continue
		}
		if match != "" && match != path {
			return ""
		}
		match = path
	}
	return match
}

// knownProjectPaths lists the absolute project paths the indexer recorded. Only
// the sessions table is consulted: its project column is written from the
// transcript, while a learning's project is caller-supplied
// (remember(project=…)) — using it as evidence would let a caller plant the path
// this repair then applies to another row.
func knownProjectPaths(db *sql.DB) ([]string, error) {
	sessions, err := tableExists(db, "sessions")
	if err != nil {
		return nil, err
	}
	if !sessions {
		return nil, nil
	}
	rows, err := db.Query(`SELECT DISTINCT project FROM sessions WHERE project LIKE '/%'`)
	if err != nil {
		return nil, fmt.Errorf("list project paths: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// Plan reads both databases and proposes an attribution per learning: the
// session whose assistant tool call contains the learning's text. It opens the
// databases read-only, so a dry run cannot write.
func Plan(yesmemDBPath, opencodeDBPath, since string) (*Report, error) {
	if since == "" {
		since = DefaultSince
	}
	ydb, err := openReadOnly(yesmemDBPath)
	if err != nil {
		return nil, fmt.Errorf("open yesmem db: %w", err)
	}
	defer ydb.Close()
	odb, err := openReadOnly(opencodeDBPath)
	if err != nil {
		return nil, fmt.Errorf("open opencode db: %w", err)
	}
	defer odb.Close()

	candidates, err := loadCandidates(ydb, since)
	if err != nil {
		return nil, err
	}
	report := &Report{Since: since, Candidates: len(candidates)}
	if len(candidates) == 0 {
		return report, nil
	}

	matches, err := matchToolCalls(odb, candidates)
	if err != nil {
		return nil, err
	}
	known, err := knownProjectPaths(ydb)
	if err != nil {
		return nil, err
	}

	for _, c := range candidates {
		row := Row{ID: c.id, OldSession: c.session, OldProject: c.project, Status: StatusUnresolved}
		sessions := matches[c.id]
		switch {
		case len(sessions) == 0:
			row.Basis = "no session contains the remember text"
		case len(sessions) > 1:
			row.Basis = fmt.Sprintf("%d sessions contain the remember text", len(sessions))
		default:
			row.NewSession = "opencode:" + onlyKey(sessions)
			row.Basis = "unique session with the remember text"
		}

		// The project is repaired on its own evidence: the session that wrote the
		// learning ran in the repository, and when no session can be identified
		// the historical basename in canonical_project still names the project —
		// as long as exactly one known path carries it.
		if needsProject(c.project) {
			if len(sessions) == 1 {
				if root, err := sessionRepositoryRoot(odb, onlyKey(sessions)); err != nil {
					return nil, err
				} else if root != "" {
					row.NewProject, row.NewProjectSource = root, "repo"
					row.Basis += "; project from the session directory"
				}
			}
			if row.NewProject == "" {
				if path := uniqueKnownPath(known, c.canonical); path != "" {
					row.NewProject, row.NewProjectSource = path, "canonical"
					row.Basis += "; project from canonical_project"
				}
			}
		}

		proposed := row.NewSession != "" || row.NewProject != ""
		switch {
		case needsProject(c.project) && row.NewProject == "":
			// The project still needs rebuilding and nothing identifies it, so the
			// row is not settled whatever the session axis says.
			row.Status = StatusUnresolved
			row.Basis += "; project still unresolved"
		case !proposed:
			row.Status = StatusUnresolved
		case effectiveSession(row) == c.session && effectiveProject(row) == c.project:
			row.Status = StatusAlready
		default:
			row.Status = StatusRepair
		}
		report.Rows = append(report.Rows, row)
	}
	sort.Slice(report.Rows, func(i, j int) bool { return report.Rows[i].ID < report.Rows[j].ID })
	return report, nil
}

// Apply writes the proposed values for the unambiguous rows inside one
// transaction, after copying the affected rows into BackupTable. Rows whose
// values already match are skipped, so a second run is a no-op.
//
// A project repair writes canonical_project alongside project but leaves
// embedding_text and embedding_vector as they are — those are rebuilt by the
// embedding pipeline, not here; no live row needs a project repair today.
func Apply(yesmemDBPath string, report *Report) (int, error) {
	if report == nil {
		return 0, fmt.Errorf("no report to apply")
	}
	db, err := openForApply(yesmemDBPath)
	if err != nil {
		return 0, fmt.Errorf("open yesmem db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS ` + BackupTable + ` (
		id INTEGER PRIMARY KEY,
		session_id TEXT,
		project TEXT,
		project_source TEXT,
		canonical_project TEXT,
		repaired_at TEXT
	)`); err != nil {
		return 0, fmt.Errorf("create backup table: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	changed := 0
	for _, row := range report.Rows {
		if row.Status != StatusRepair {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO `+BackupTable+` (id, session_id, project, project_source, canonical_project, repaired_at)
			SELECT id, session_id, project, project_source, canonical_project, ? FROM learnings
			WHERE id = ? AND id NOT IN (SELECT id FROM `+BackupTable+`)`, now, row.ID); err != nil {
			return 0, fmt.Errorf("backup learning %d: %w", row.ID, err)
		}
		session := row.NewSession
		if session == "" {
			session = row.OldSession
		}
		var res sql.Result
		var err error
		if row.NewProject != "" {
			// canonical_project is derived from project on insert; a project
			// write that skipped it would leave the row unreadable for every
			// canonical_project filter.
			res, err = tx.Exec(`UPDATE learnings SET session_id = ?, project = ?, canonical_project = ?, project_source = ? WHERE id = ?`,
				session, row.NewProject, models.CanonicalProject(row.NewProject), row.NewProjectSource, row.ID)
		} else {
			res, err = tx.Exec(`UPDATE learnings SET session_id = ? WHERE id = ?`, session, row.ID)
		}
		if err != nil {
			return 0, fmt.Errorf("update learning %d: %w", row.ID, err)
		}
		// Count what the statement actually changed, not what was proposed: a row
		// deleted between plan and apply must not be reported as repaired.
		if n, err := res.RowsAffected(); err == nil {
			changed += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// openReadOnly opens a database the dry run must not write, with two layers: the
// DSN asks for mode=ro and carries query_only, and the connection verifies it.
// query_only belongs in the DSN and not only in a PRAGMA: it dies with its
// connection, so a pool that replaces connections would otherwise hand out a
// writable handle under a read-only name (same reasoning as
// storage.readerPragmas).
func openReadOnly(path string) (*sql.DB, error) {
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	db, err := sql.Open("sqlite", fileDSN(path, query))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// The driver connects lazily, so this statement is where a missing file or a
	// missing permission surfaces: the message has to name the open, not the
	// pragma that happened to run first.
	if _, err := db.Exec(`PRAGMA query_only = 1`); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// fileDSN renders a database path as a file: URI with the path escaped: a ? or #
// in a directory name would otherwise end the URI early — and for the read-only
// connection that would silently drop mode=ro, opening the wrong file for
// writing instead.
func fileDSN(path string, query url.Values) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: query.Encode()}
	return uri.String()
}

// openForApply opens the database for the repair's writes. The daemon keeps
// writing to it concurrently, so the connection waits for a lock instead of
// failing with SQLITE_BUSY (same 30s bound as internal/storage), and it caps the
// WAL: this repair is the largest single writer the file sees, and a writer
// without the limit leaves the log at its peak size for the next reader. mode=rw
// rather than the driver default READWRITE|CREATE: an apply must never create a
// database where one disappeared.
func openForApply(path string) (*sql.DB, error) {
	query := url.Values{}
	query.Set("mode", "rw")
	query.Add("_pragma", "busy_timeout(30000)")
	query.Add("_pragma", "journal_size_limit(10485760)")
	db, err := sql.Open("sqlite", fileDSN(path, query))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 30000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func loadCandidates(db *sql.DB, since string) ([]candidate, error) {
	rows, err := db.Query(`SELECT id, content, COALESCE(session_id, ''), COALESCE(project, ''), COALESCE(canonical_project, ''), COALESCE(project_source, '')
		FROM learnings
		WHERE created_at >= ? AND origin_tool = 'user' AND session_id LIKE 'opencode:%'
		ORDER BY id`, since)
	if err != nil {
		return nil, fmt.Errorf("select learnings: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.content, &c.session, &c.project, &c.canonical, &c.projectSource); err != nil {
			return nil, err
		}
		if c.content == "" {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// storedPart is one tool call as opencode persists it. The two storage
// generations differ in where the tool name lives (v2 message parts: `name`,
// older part rows: `tool`) and in whether the call sits inside a content list;
// the arguments reach both generations under state.input.
type storedPart struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Tool  string `json:"tool"`
	State struct {
		Input struct {
			Text string `json:"text"`
			Code string `json:"code"`
		} `json:"input"`
	} `json:"state"`
}

// toolName is the name of the called tool, whichever field this generation uses.
func (p storedPart) toolName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Tool
}

// carriesRemember reports whether this call can have written the learning. A
// direct call names the remember tool; a Code Mode call is an `execute` whose
// script embeds the direct call. Without the filter any tool that merely
// quotes the text — a search, a relay — would count as evidence and could turn
// a wrong session into the unique match.
func (p storedPart) carriesRemember() bool {
	name := strings.ToLower(p.toolName())
	return strings.Contains(name, "remember") || name == "execute"
}

// storedParts returns the tool calls of one stored row: either the parts of a
// message (session_message) or the row itself (part).
func storedParts(data string) []storedPart {
	var wrapper struct {
		Content []storedPart `json:"content"`
	}
	if err := json.Unmarshal([]byte(data), &wrapper); err == nil && len(wrapper.Content) > 0 {
		return wrapper.Content
	}
	var single storedPart
	if err := json.Unmarshal([]byte(data), &single); err == nil {
		return []storedPart{single}
	}
	return nil
}

// matchToolCalls returns, per learning id, the sessions whose assistant tool
// call carries that learning's text.
//
// A remember reaches the database in two shapes: a direct yesmem_remember call
// whose text argument is the learning content, and an opencode Code Mode
// execute call whose JavaScript source embeds the same call — the misattributed
// #101896/#101897 were written the second way, so matching only the direct
// shape would find nothing.
//
// Both storage generations are read. opencode v2 writes assistant messages to
// session_message; sessions recorded before the v2 schema keep their tool calls
// in the part table. Reading only session_message left 464 of 1486 candidates
// without a session on 2026-10-16, 251 of which the part table resolves.
// Sessions from both sources are unioned before the uniqueness decision, so a
// second source can only make a match safer, never narrower.
func matchToolCalls(db *sql.DB, candidates []candidate) (map[int64]map[string]struct{}, error) {
	byContent := make(map[string][]candidate, len(candidates))
	for _, c := range candidates {
		byContent[c.content] = append(byContent[c.content], c)
	}

	matches := make(map[int64]map[string]struct{}, len(candidates))
	record := func(id int64, session string) {
		if matches[id] == nil {
			matches[id] = map[string]struct{}{}
		}
		matches[id][session] = struct{}{}
	}
	scan := func(query string) error {
		rows, err := db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var session, data string
			if err := rows.Scan(&session, &data); err != nil {
				return err
			}
			session = strings.TrimPrefix(session, "opencode:")
			for _, part := range storedParts(data) {
				if part.Type != "tool" || !part.carriesRemember() {
					continue
				}
				if text := part.State.Input.Text; text != "" {
					for _, c := range byContent[text] {
						record(c.id, session)
					}
				}
				if code := part.State.Input.Code; code != "" {
					for _, c := range candidates {
						if strings.Contains(code, c.content) {
							record(c.id, session)
						}
					}
				}
			}
		}
		return rows.Err()
	}

	if err := scan(`SELECT session_id, data FROM session_message
		WHERE type = 'assistant' AND instr(data, 'remember') > 0`); err != nil {
		return nil, fmt.Errorf("select session messages: %w", err)
	}
	legacy, err := tableExists(db, "part")
	if err != nil {
		return nil, err
	}
	if legacy {
		if err := scan(`SELECT session_id, data FROM part WHERE instr(data, 'remember') > 0`); err != nil {
			return nil, fmt.Errorf("select stored parts: %w", err)
		}
	}
	return matches, nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up table %s: %w", name, err)
	}
	return true, nil
}

// sessionRepositoryRoot returns the repository the session ran in. An empty
// result means there is nothing to derive from: either the session is unknown or
// the database predates the v2 session table. The caller then leaves the project
// untouched.
func sessionRepositoryRoot(db *sql.DB, sessionID string) (string, error) {
	known, err := tableExists(db, "session_v2")
	if err != nil {
		return "", err
	}
	if !known {
		return "", nil
	}
	var directory string
	err = db.QueryRow(`SELECT directory FROM session_v2 WHERE id = ?`, sessionID).Scan(&directory)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read session %s: %w", sessionID, err)
	}
	return repo.Root(directory), nil
}

func onlyKey(m map[string]struct{}) string {
	for k := range m {
		return k
	}
	return ""
}

// effectiveProject is the project the row will have after apply: an empty
// proposal leaves the stored value in place.
func effectiveProject(row Row) string {
	if row.NewProject != "" {
		return row.NewProject
	}
	return row.OldProject
}

// effectiveSession is the session the row will carry after apply, on the same
// rule.
func effectiveSession(row Row) string {
	if row.NewSession != "" {
		return row.NewSession
	}
	return row.OldSession
}
