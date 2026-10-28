package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// IsFileInCodeIndex checks whether a file path appears in the codescan index
// for the given project. Accepts exact relative paths ("internal/proxy/proxy.go")
// as well as partial paths matched by suffix ("proxy.go", "codescan/cache.go").
//
// The suffix question is asked in both directions: the requested path may end with
// a stored one. A scan rooted below the checkout — written before YM-442 refused
// those documents — stores its rows relative to that root, so the row for the same
// file is the shorter of the two spellings (YM-436).
//
// Answers come from the per-path index (project_scan_files) so the multi-MB
// scan document is never parsed on the hook hot path (DT31 / YM-7).
func (s *Store) IsFileInCodeIndex(project, relPath string) bool {
	if project == "" || strings.TrimSpace(relPath) == "" {
		return false
	}

	if !s.scanFilesIndexed(project) {
		// A row written before project_scan_files existed (or by a scan whose
		// index write never landed) carries no per-path index. Materialize it
		// once; the marker row written by replaceScanFiles keeps this branch
		// from being taken again.
		s.backfillScanFiles(project)
	}

	return s.scanFilePathExists(project, relPath)
}

// IsTreeInCodeIndex answers for a path the command names, file or directory.
// A directory operand is part of the indexed tree when any indexed path lies
// below it: a recursive grep or a find names the directory, not a file, and a
// file-only lookup let exactly that call-site search through (YM-409).
//
// Both shapes are asked: anchored at the project root ("internal/hooks") and
// anywhere below it ("hooks"), because a command that runs in a subdirectory
// names its operand relative to that directory, which is what the file lookup
// already handles through its suffix rule. "." is the project root itself.
//
// A scan rooted below the checkout stays unreachable for a directory operand: the
// reversed question the file lookup asks has no counterpart here, because a
// directory carries no basename that could bound the candidate set. The guard then
// does not block, which is the harmless direction of the two (YM-436). YM-442
// stopped such documents from being written; rows from an older one still behave
// this way until the backfill drops the document.
func (s *Store) IsTreeInCodeIndex(project, relPath string) bool {
	if s.IsFileInCodeIndex(project, relPath) {
		return true
	}
	if project == "" {
		return false
	}
	dir := strings.TrimSuffix(strings.TrimSpace(relPath), "/")
	if dir == "" {
		return false
	}
	// A caller may pass "./internal"; the index holds cleaned relative paths.
	if !filepath.IsAbs(dir) {
		dir = filepath.Clean(dir)
	}

	var found int
	if dir == "." {
		err := s.db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM project_scan_files WHERE project = ? AND path <> ''
		)`, project).Scan(&found)
		return err == nil && found == 1
	}

	err := s.db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM project_scan_files
		WHERE project = ?
		  AND (substr(path, 1, length(?)) = ? OR instr(path, ?) > 0)
	)`, project, dir+"/", dir+"/", "/"+dir+"/").Scan(&found)
	return err == nil && found == 1
}

// DismissCodeNav increments the dismiss counter for a session.
func (s *Store) DismissCodeNav(sessionID string) error {
	_, err := s.db.Exec(`INSERT INTO code_nav_dismissals (session_id, dismiss_count) VALUES (?, 1)
		ON CONFLICT(session_id) DO UPDATE SET dismiss_count = dismiss_count + 1`, sessionID)
	return err
}

// CodeNavDismissed reports whether a session has lifted code-navigation blocking.
// One dismiss_code_nav call is the caller's decision to use the shell, so one call
// is enough: the tool answered ok while the block stayed in place for four more
// calls (YM-425). The counter keeps counting for the session history.
func (s *Store) CodeNavDismissed(sessionID string) bool {
	return s.IsCodeNavDismissed(sessionID, 1)
}

// IsCodeNavDismissed returns true if code-nav suggestions have been dismissed
// enough times in this session to exceed the threshold.
func (s *Store) IsCodeNavDismissed(sessionID string, threshold int) bool {
	var count int
	err := s.db.QueryRow(`SELECT dismiss_count FROM code_nav_dismissals
		WHERE session_id = ?`, sessionID).Scan(&count)
	return err == nil && count >= threshold
}

func (s *Store) scanFilesIndexed(project string) bool {
	var indexed int
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM project_scan_files WHERE project = ?)`, project).Scan(&indexed)
	return err == nil && indexed == 1
}

// scanFilePathExists answers an exact or suffix match. The exact path is probed
// through the primary key first — it is the common case, because the hook
// resolves absolute paths against the working directory before the lookup.
func (s *Store) scanFilePathExists(project, relPath string) bool {
	var exact int
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM project_scan_files WHERE project = ? AND path = ?)`,
		project, relPath).Scan(&exact); err == nil && exact == 1 {
		return true
	}

	// Any match — exact path or path suffix — has the same file name, so the
	// base index bounds the candidate set to the files of that name.
	var found int
	err := s.db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM project_scan_files
		WHERE project = ? AND base = ?
		  AND substr(path, -length(?) - 1) = '/' || ?
	)`, project, filepath.Base(relPath), relPath, relPath).Scan(&found)
	if err == nil && found == 1 {
		return true
	}

	// The two roots meet from the other side: a scan rooted below the checkout
	// stores its rows relative to the directory the scanner walked, so the row for
	// the same file is a shorter spelling of the requested path
	// ("daemon/wiki_tick.go" for "internal/daemon/wiki_tick.go"). Only the narrow
	// direction was asked before, and every such row stayed unreachable for exactly
	// the commands the key was built for (YM-436). Each spelling is an equality
	// probe on the primary key, at most maxIndexTails of them.
	//
	// A row of another subtree with that tail matches too. That looseness is the
	// mirror of the suffix rule above and cannot be removed without the walk root of
	// the row, which the table does not carry; it costs a block on a file with that
	// tail, never a way past the guard.
	for _, tail := range pathTails(relPath) {
		var hit int
		if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM project_scan_files WHERE project = ? AND path = ?)`,
			project, tail).Scan(&hit); err == nil && hit == 1 {
			return true
		}
	}
	return false
}

// pathTails lists the shorter spellings of a relative path: the ones a scan rooted
// below the checkout would have written for the same file, down to the bare file
// name. The whole path itself is not listed, it was probed before. A path with many
// segments is trimmed at the long end, because the short spellings are the ones the
// common shallow scan roots produce.
func pathTails(relPath string) []string {
	var tails []string
	rest := relPath
	for {
		cut := strings.IndexByte(rest, '/')
		if cut < 0 {
			break
		}
		rest = rest[cut+1:]
		if rest == "" {
			break
		}
		tails = append(tails, rest)
	}
	if len(tails) > maxIndexTails {
		tails = tails[len(tails)-maxIndexTails:]
	}
	return tails
}

// maxIndexTails bounds the shorter spellings one lookup probes. A deeper path loses
// its longest spellings first, so the bare file name always survives.
const maxIndexTails = 8

// ErrScanRootBelowCheckout is returned for a scan document that is not the index
// of the checkout its key names: one walked from a subdirectory of that checkout,
// from a linked worktree, or from a directory outside it. Such a document lists
// other files than the checkout has, and replacing the checkout's index with it
// would drop every path outside the subtree (YM-442, YM-453).
var ErrScanRootBelowCheckout = errors.New("storage: scan document is rooted below the checkout")

// replaceScanFiles rewrites the per-path index of a project inside tx. It also
// writes a marker row (empty path), so a scan document without any path still
// counts as materialized — otherwise every following hook call would parse the
// document again, which is the cost this index exists to remove (DT31 / YM-7).
// The marker cannot match a lookup: filepath.Base of a non-empty path is never
// empty.
func replaceScanFiles(tx *sql.Tx, project, scanJSON string) error {
	if err := checkScanRoot(project, scanJSON); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_scan_files WHERE project = ?`, project); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO project_scan_files (project, path, base) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range scanPaths(scanJSON) {
		if _, err := stmt.Exec(project, p, filepath.Base(p)); err != nil {
			return err
		}
	}
	_, err = stmt.Exec(project, "", "")
	return err
}

// checkScanRoot refuses a document the index must not be replaced with. The rule
// itself lives in codescan.ScannedRepositoryRoot: the key names a checkout, and
// the document must be that checkout's scan. This function asks that one rule
// instead of carrying a second reading of it — the daemon path and the CLI path
// answered the worktree case in opposite ways while both were green (YM-453).
//
// A document without a root, or a row without a key, keeps the old semantics: an
// older build writes the first, and the second is what the per-path backfill sees
// for rows from the retired basename scheme.
func checkScanRoot(project, scanJSON string) error {
	root := scanDocumentRoot(scanJSON)
	if root == "" {
		// A document without a root predates the field; refusing it would freeze the
		// index of an installation that can never produce one again.
		return nil
	}
	if strings.TrimSpace(project) == "" {
		return nil
	}
	if codescan.ScannedRepositoryRoot(project, root) {
		return nil
	}
	return fmt.Errorf("%w: root %q, project %q", ErrScanRootBelowCheckout, filepath.Clean(root), filepath.Clean(project))
}

// scanDocumentRoot returns the directory a stored scan document was walked from.
// An older document carries none.
func scanDocumentRoot(scanJSON string) string {
	var doc struct {
		RootDir string
	}
	if json.Unmarshal([]byte(scanJSON), &doc) != nil {
		return ""
	}
	return doc.RootDir
}

// commitScanFilesTx is a seam for the tests: a COMMIT cannot be made to fail
// through the public API without manipulating the connection, and the rollback
// path still has to be proven (YM-366). A failed COMMIT leaves no half-written
// index either way: the driver rolls the connection back itself when COMMIT
// fails, and the deferred Rollback covers the injected failure.
var commitScanFilesTx = (*sql.Tx).Commit

// backfillScanFiles builds the per-path index for a scan row that predates it,
// or whose index write never landed.
func (s *Store) backfillScanFiles(project string) {
	row, err := s.GetProjectScan(project)
	if err != nil || row == nil || row.ScanJSON == "" {
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	if err := replaceScanFiles(tx, project, row.ScanJSON); err != nil {
		if errors.Is(err, ErrScanRootBelowCheckout) {
			// The stored document is no checkout's index of this key's files — walked
			// from a subdirectory, from a linked worktree, or from a key of the retired
			// basename scheme. Drop it — a cache row whose next reader would be served
			// those paths — and mark the index materialized: this runs on the hook hot
			// path, and without the marker every call would parse the document again
			// looking for an index that never lands. The delete names the refused
			// document, so one written meanwhile stays untouched.
			tx.Rollback()
			if _, dErr := s.db.Exec(`DELETE FROM project_scan WHERE project = ? AND scan_json = ?`, project, row.ScanJSON); dErr != nil {
				log.Printf("[storage] drop subordinate scan document for %q: %v", project, dErr)
			}
			if _, mErr := s.db.Exec(`INSERT OR REPLACE INTO project_scan_files (project, path, base) VALUES (?, '', '')`, project); mErr != nil {
				log.Printf("[storage] mark path index for %q materialized: %v", project, mErr)
			}
			return
		}
		log.Printf("[storage] backfill path index for %q failed: %v", project, err)
		return
	}
	if err := commitScanFilesTx(tx); err != nil {
		log.Printf("[storage] backfill path index for %q: commit failed: %v", project, err)
	}
}

// scanPaths extracts the indexed file paths from a stored scan document.
func scanPaths(scanJSON string) []string {
	var scan struct {
		Packages []struct {
			Files []struct {
				Path string `json:"Path"`
			} `json:"Files"`
		} `json:"Packages"`
	}
	if json.Unmarshal([]byte(scanJSON), &scan) != nil {
		return nil
	}

	var paths []string
	for _, pkg := range scan.Packages {
		for _, f := range pkg.Files {
			if f.Path != "" {
				paths = append(paths, f.Path)
			}
		}
	}
	return paths
}
