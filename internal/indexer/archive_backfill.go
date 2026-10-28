package indexer

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/storage"
)

// ArchiveBackfillOptions are the knobs of one catch-up run.
type ArchiveBackfillOptions struct {
	// Project limits the run to one project, as a path or a short name. Empty
	// means every project.
	Project string
	// DryRun lists the documents that would be written and writes nothing.
	DryRun bool
	// Limit stops the run after this many documents. 0 means no limit.
	Limit int
	// ExcludeProjects holds the projects the operator excluded from indexing.
	// Rows of such a project can predate the exclusion, and copying them into the
	// permanent archive would overrule the exclusion after the fact, so they are
	// left alone the same way the scanner leaves them alone.
	ExcludeProjects []string
	// Progress, when set, receives one line per document written or listed.
	Progress io.Writer
}

// ArchiveBackfill is the outcome of one catch-up run.
type ArchiveBackfill struct {
	ArchiveDir string
	// Considered counts the sessions that need an archive document of their own,
	// Present the ones that already have one.
	Considered int
	Present    int
	// Excluded counts the sessions this run skipped because their project is on
	// the operator's exclusion list.
	Excluded int
	// Incomplete lists the sessions whose stored message count disagrees with the
	// messages that could be read. They are left unwritten: a document with a
	// body shorter than the conversation would count as archived forever, because
	// nothing revisits a session that stops growing.
	Incomplete []string
	// Written lists the sessions whose document this run wrote. In a dry run it
	// lists the documents that would be written instead.
	Written []string
}

// ArchiveMissingSessions writes an archive document for every indexed session
// that has none.
//
// The scanner archives along its watermark: it only sees sessions whose
// time_updated lies past the last scan, so the sessions indexed before it
// archived at all never come back, and a session whose archive write failed is
// not revisited either once it stops growing. This is the catch-up path for both,
// deliberately a command and not daemon startup behaviour: the backlog can be
// gigabytes, and the operator decides when that runs.
//
// Only sessions without a JSONL file are candidates — a session that has one is
// archived by ArchiveFile, and an export document beside it would be a second
// copy of the same conversation. A failed ArchiveFile copy of a file-backed
// session is therefore not covered here; that path belongs to the indexer.
func ArchiveMissingSessions(store *storage.Store, arch *archive.Archiver, opts ArchiveBackfillOptions) (*ArchiveBackfill, error) {
	if store == nil || arch == nil {
		return nil, fmt.Errorf("archive backfill: store and archiver are required")
	}
	sessions, err := store.ListAllSessions(opts.Project, 0)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	exclude := normalizeExcludedProjects(opts.ExcludeProjects)

	res := &ArchiveBackfill{ArchiveDir: arch.BaseDir()}
	for _, sess := range sessions {
		if opts.Limit > 0 && len(res.Written) >= opts.Limit {
			break
		}
		if sess.JSONLPath != "" {
			continue
		}
		if projectExcluded(exclude, sess.Project, sess.ProjectShort) {
			res.Excluded++
			continue
		}
		res.Considered++

		docPath, err := arch.SessionDocPath(archive.ProjectDirName(sess.Project), archiveDocumentName(sess.ID))
		if err != nil {
			return nil, fmt.Errorf("archive path for %q: %w", sess.ID, err)
		}
		// Lstat, and only a regular file counts: a symlink or a directory squatting
		// on the name is not a document this command wrote.
		if info, err := os.Lstat(docPath); err == nil && info.Mode().IsRegular() {
			res.Present++
			continue
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %q: %w", docPath, err)
		}

		msgs, err := store.GetMessagesBySession(sess.ID)
		if err != nil {
			return nil, fmt.Errorf("messages of %q: %w", sess.ID, err)
		}
		// A document without messages is never the goal, and a body shorter than
		// the conversation would count as archived forever. The stored count is
		// written before the messages are replaced, so a read that races the
		// re-indexing scanner always comes up short and is caught here.
		if len(msgs) == 0 || len(msgs) != sess.MessageCount {
			res.Incomplete = append(res.Incomplete, sess.ID)
			continue
		}

		if opts.DryRun {
			res.Written = append(res.Written, sess.ID)
			progressLine(opts.Progress, "would archive %s\n", progressID(sess.ID))
			continue
		}
		if err := writeSessionExport(arch, &sess, msgs); err != nil {
			return nil, fmt.Errorf("archive %q: %w", sess.ID, err)
		}
		res.Written = append(res.Written, sess.ID)
		progressLine(opts.Progress, "archived %s\n", progressID(sess.ID))
	}
	return res, nil
}

func progressLine(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}

// progressID makes a session id safe for a progress line. The line format is part
// of the command's output ("would archive <id>", pinned by a test), so the id is
// not quoted; the characters that would end the line, or reorder what follows it,
// are removed instead. Ids are generated internally — this is a belt for a row an
// older version or a foreign writer left in the store (YM-332).
//
// The rendering is not the document name: archiveDocumentName keeps control
// characters, so an id carrying one prints here without it. That divergence only
// concerns values that should never have reached the store.
func progressID(id string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, id)
}
