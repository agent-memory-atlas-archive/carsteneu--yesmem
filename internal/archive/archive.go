package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf16"
)

// archiveFingerprintBytes is the size of each probe window that is compared
// against the source before the missing tail is appended. Same idea as the index
// watermark's prefix fingerprint: cheap reads instead of proof, at a cost that
// does not grow with the file.
//
// It is a sample, not a hash of the copy: three windows (start, middle, end of the
// copied range) catch a rewrite anywhere a rewrite is plausible, but a change
// confined to the bytes between two windows still passes unnoticed, and then the
// copy keeps the pre-rewrite bytes there. Hashing the whole copy on every append
// would mean reading it — the cost that incremental archiving exists to avoid.
const archiveFingerprintBytes = 256

// Archiver copies JSONL session files to a permanent archive directory.
// Protects against Claude Code's 30-day cleanup (cleanupPeriodDays).
type Archiver struct {
	baseDir string // e.g. ~/.claude/yesmem/archive/

	// Counters per archive mode. Kept unexported and atomic: the daemon archives
	// from several indexing workers at once, and the tests use them to tell an
	// incremental append from a full copy.
	fullCopies     atomic.Int64
	appendedCopies atomic.Int64
	docWrites      atomic.Int64
}

// New creates an archiver targeting the given base directory.
func New(baseDir string) *Archiver {
	return &Archiver{baseDir: baseDir}
}

// EnsureBaseDir creates the archive base directory owner-only and reports a base
// that is already more open. The archive holds a second copy of every transcript,
// so the tree is part of the protection: the copies are 0600 (restrictToOwner) and
// the folder names below the base carry project paths and session ids.
//
// An existing directory is reported, not narrowed. paths.archive accepts any
// absolute path, so the base may be a directory yesmem did not create, and a
// silent chmod of a user's shared folder is worse than a warning. Setup hardens
// its own dataDir/archive separately.
func EnsureBaseDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errWithPath("mkdir archive base", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return errWithPath("stat archive base", dir, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		// %q, not %s: a directory name can carry a newline and would otherwise fake
		// a second log line (errWithPath quotes the path for the same reason).
		log.Printf("[archive] %q is %04o, accessible beyond the owner; the transcripts inside stay 0600", dir, mode)
	}
	return nil
}

// ArchiveFile copies a JSONL file into the archive under the given project
// subdirectory, identified by its original filename (the session UUID).
//
// A session file grows while it is being written and is archived again after
// every write, so copying the whole file each time costs O(file size) per append
// — for a GB-class rollout, GBs of I/O per turn. When the archive already holds
// a copy that is a byte-prefix of the source, only the missing tail is appended;
// everything else (first archive, unchanged size, a shorter or rewritten file, an
// unreadable copy) takes a full copy.
func (a *Archiver) ArchiveFile(srcPath, project string) error {
	// The project name becomes a directory below the archive base, so it gets the
	// same gate SessionDocPath applies to its components: a value carrying a
	// separator or a traversal would put a copy beside the archive instead of in
	// it (defense in depth, every current caller passes an encoded name).
	if !plainPathComponent(project) {
		return fmt.Errorf("archive project name %q is not a plain directory name", project)
	}

	// Verify source exists
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return errWithPath("stat source", srcPath, err)
	}

	// Create project subdirectory
	destDir := filepath.Join(a.baseDir, project)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return errWithPath("mkdir", destDir, err)
	}

	destPath := filepath.Join(destDir, filepath.Base(srcPath))

	destInfo, err := os.Stat(destPath)
	if err != nil {
		return a.fullCopy(srcPath, destPath)
	}
	if destInfo.Size() == srcInfo.Size() {
		return nil // already archived, same size
	}
	if destInfo.Size() < srcInfo.Size() && prefixMatches(destPath, srcPath, destInfo.Size()) {
		if err := a.appendTail(srcPath, destPath, destInfo.Size()); err != nil {
			return err
		}
		// Appending is not idempotent by itself: two callers that both measured the
		// copy before either appended write the same tail twice, and in a real
		// corpus three destinations are fed by two source paths each. The length
		// says whether the copy ended up matching the source; a mismatch is
		// repaired here rather than left for the next pass, which for the last pass
		// of a session would never come.
		if info, err := os.Stat(destPath); err != nil || info.Size() != srcInfo.Size() {
			return a.fullCopy(srcPath, destPath)
		}
		return nil
	}
	return a.fullCopy(srcPath, destPath)
}

// BaseDir returns the archive base directory.
func (a *Archiver) BaseDir() string {
	return a.baseDir
}

// MoveProjectDir moves the archive folder of one project name onto another and
// reports how many documents it moved, kept or found nothing to do for.
//
// A project rename in the database (MigrateProject) leaves the folder under the
// old encoded name behind, and the next archiving pass then writes every session
// document a second time under the new name — the archive grows a duplicate of
// the whole project (YM-320). The folder names are the ones ProjectDirName
// produces; anything else is refused, because the caller's names come from a
// command line.
//
// A destination folder that already holds documents is merged, not replaced. A
// same-name collision is settled by content: if one file is a byte prefix of the
// other, the shorter one is provably contained and goes, otherwise both stay. The
// source folder is removed when it is empty afterwards; a folder that still holds
// something this function does not own stays and is reported through kept.
func (a *Archiver) MoveProjectDir(from, to string) (moved, kept int, err error) {
	if !plainPathComponent(from) || !plainPathComponent(to) {
		return 0, 0, fmt.Errorf("archive project name %q -> %q is not a plain directory name", from, to)
	}
	if from == to {
		return 0, 0, nil
	}
	fromDir := filepath.Join(a.baseDir, from)
	toDir := filepath.Join(a.baseDir, to)

	entries, err := os.ReadDir(fromDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, errWithPath("read archive dir", fromDir, err)
	}
	// Anything that is not a plain file stays where it is: this function moves
	// session documents, and a directory under the project folder is not one.
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, e.Name())
			continue
		}
		kept++
	}
	if len(files) > 0 {
		if err := os.MkdirAll(toDir, 0755); err != nil {
			return 0, kept, errWithPath("mkdir", toDir, err)
		}
	}
	for _, name := range files {
		srcPath := filepath.Join(fromDir, name)
		destPath := filepath.Join(toDir, name)
		srcInfo, err := os.Lstat(srcPath)
		if err != nil {
			return moved, kept, errWithPath("stat", srcPath, err)
		}
		if destInfo, err := os.Lstat(destPath); err == nil {
			// A collision means the same document name exists on both sides, which is
			// exactly the duplicate a rename produces. Which copy goes is decided by
			// content, not by size, and by a full byte comparison: this branch can
			// delete a file, so the sampled comparison prefixMatches — cheap enough
			// for the append path — is not evidence enough here. Two different
			// documents with the same name stay on both sides.
			contained, cmpErr := false, error(nil)
			if destInfo.Mode().IsRegular() {
				if destInfo.Size() >= srcInfo.Size() {
					// The source has to be a prefix of the destination.
					contained, cmpErr = isBytePrefix(destPath, srcPath, srcInfo.Size())
				} else {
					// The destination has to be a prefix of the source.
					contained, cmpErr = isBytePrefix(srcPath, destPath, destInfo.Size())
				}
			}
			switch {
			case cmpErr != nil:
				// Unreadable on one side: keep both, the operator decides.
				kept++
				continue
			case !contained:
				kept++
				continue
			case destInfo.Size() >= srcInfo.Size():
				// The destination holds at least everything the source has.
				if err := os.Remove(srcPath); err != nil {
					return moved, kept, errWithPath("remove duplicate", srcPath, err)
				}
				kept++
				continue
			default:
				// The source is the grown version of the destination.
				if err := os.Rename(srcPath, destPath); err != nil {
					return moved, kept, errWithPath("replace truncated copy at", destPath, err)
				}
				moved++
				continue
			}
		}
		if err := os.Rename(srcPath, destPath); err != nil {
			return moved, kept, errWithPath("move to", destPath, err)
		}
		moved++
	}
	// Non-empty means the folder carries something this function did not move.
	os.Remove(fromDir)
	return moved, kept, nil
}

// ProjectDirName encodes a project path the way Claude Code names its project
// directories (~/.claude/projects/<encoded>): every character outside
// [A-Za-z0-9] becomes a dash, so "/home/x/.claude" and "/home/x/-claude" share
// one folder. That is Claude Code's own rule, collisions included. ArchiveFile
// derives the same name from the source file's parent directory, so both kinds
// of session record for one project end up in the same folder.
//
// An encoded name longer than claudeEncodedNameLimit is cut there and gets "-"
// plus a base36 hash of the full path appended, as the shipped CLI does (the
// XM/Te/Kle trio in the 2.1.301 build): without the hash two long paths sharing
// a prefix would land in one folder.
//
// The count is in UTF-16 code units, as in the shipped CLI: its
// replace(/[^a-zA-Z0-9]/g, "-") carries no "u" flag, so a non-BMP character
// becomes two dashes, and String.length as well as slice(0, 200) count code
// units rather than runes. One deviation remains: the hash covers the cleaned
// path, which is the form a session record holds; Claude Code hashes the
// argument it is handed.
func ProjectDirName(projectPath string) string {
	clean := filepath.Clean(strings.TrimSpace(projectPath))
	if clean == "" || clean == "." || clean == string(os.PathSeparator) {
		return "unknown"
	}
	// UTF-16 code units, not runes: the shipped encoder runs
	// replace(/[^a-zA-Z0-9]/g, "-") without a "u" flag, so both halves of a
	// surrogate pair become their own dash.
	units := utf16.Encode([]rune(clean))
	encoded := make([]byte, 0, len(units))
	for _, unit := range units {
		switch {
		case unit >= '0' && unit <= '9', unit >= 'a' && unit <= 'z', unit >= 'A' && unit <= 'Z':
			encoded = append(encoded, byte(unit))
		default:
			encoded = append(encoded, '-')
		}
	}
	// Every unit outside the ASCII alphanumerics became a single dash, so the
	// result is ASCII and a byte offset is also a code-unit offset.
	if len(encoded) <= claudeEncodedNameLimit {
		return string(encoded)
	}
	return string(encoded[:claudeEncodedNameLimit]) + "-" + claudePathHashBase36(clean)
}

// claudeEncodedNameLimit is where Claude Code stops writing the encoded project
// path and switches to a truncated name plus a hash.
const claudeEncodedNameLimit = 200

// claudePathHashBase36 is Claude Code's project path hash: a 31-style hash over
// the path's UTF-16 code units with int32 wrap, printed as the absolute value in
// base 36. It is appended to a truncated name to tell long paths apart.
func claudePathHashBase36(path string) string {
	var h int32
	for _, unit := range utf16.Encode([]rune(path)) {
		h = h*31 + int32(unit)
	}
	n := int64(h)
	if n < 0 {
		n = -n
	}
	return strconv.FormatInt(n, 36)
}

// plainPathComponent reports whether v can be used as a single path element.
func plainPathComponent(v string) bool {
	return v != "" && v != "." && v != ".." && v == filepath.Base(v)
}

// errWithPath renders a filesystem error without repeating the path raw: the path
// is quoted, and only the reason from the OS error is kept. Source and
// destination names come from the client (a session file name), and a name
// carrying a newline would otherwise split the log line that prints the error
// (YM-311). Both error shapes are unwrapped, *os.PathError from a single path and
// *os.LinkError from a rename, so no raw path survives in the reason.
func errWithPath(action, path string, err error) error {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		err = pathErr.Err
	case errors.As(err, &linkErr):
		err = linkErr.Err
	}
	return fmt.Errorf("%s %q: %w", action, path, err)
}

// SessionDocPath is the path WriteSessionDoc writes a document to. Callers that
// have to ask whether a document exists use it as well, so the check cannot drift
// from the layout the writer uses.
func (a *Archiver) SessionDocPath(project, name string) (string, error) {
	// Both path components are formed in other packages, so they are checked here
	// where the path is built: a separator, a traversal or an empty value would
	// leave the project folder.
	if !plainPathComponent(project) {
		return "", fmt.Errorf("archive project name %q is not a plain directory name", project)
	}
	if !plainPathComponent(name) {
		return "", fmt.Errorf("archive document name %q is not a plain file name", name)
	}
	return filepath.Join(a.baseDir, project, name), nil
}

// WriteSessionDoc replaces <baseDir>/<project>/<name> with what write produces. It
// serves sessions that have no JSONL file of their own — the sessions the
// opencode scanner indexes from its database, which the archive is meant to
// outlive.
//
// Unlike ArchiveFile this is always a full replace: a session document has no
// appendable tail. The content is streamed to a temporary file in the target
// directory and renamed into place, so a reader or a crash never sees a
// half-written copy and the previous version stays readable until the new one is
// complete. write is called once; a partial document never reaches the archive.
func (a *Archiver) WriteSessionDoc(project, name string, write func(io.Writer) error) error {
	destPath, err := a.SessionDocPath(project, name)
	if err != nil {
		return err
	}
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return errWithPath("mkdir", destDir, err)
	}

	tmp, err := os.CreateTemp(destDir, ".session-*.tmp")
	if err != nil {
		return errWithPath("create temp in", destDir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename succeeded

	if err := write(tmp); err != nil {
		tmp.Close()
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			// The temporary file sits in the directory named after the project,
			// which comes from a client path, so the OS error carries it (YM-311).
			return errWithPath("write", tmpPath, err)
		}
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errWithPath("sync", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return errWithPath("close", tmpPath, err)
	}
	// A session document holds the conversation itself, and the database it comes
	// from is owner-only: the copy must not widen that.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return errWithPath("chmod", tmpPath, err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return errWithPath("rename to", destPath, err)
	}
	a.docWrites.Add(1)
	return nil
}

// isBytePrefix reports whether the first size bytes of aPath equal the same
// range of bPath, compared byte for byte. Callers that may delete a file use
// this; prefixMatches below answers the same question from three sampled windows
// and exists for the append path, where reading the whole copy is the cost that
// incremental archiving avoids.
func isBytePrefix(aPath, bPath string, size int64) (bool, error) {
	if size <= 0 {
		return true, nil
	}
	a, err := os.Open(aPath)
	if err != nil {
		return false, errWithPath("open", aPath, err)
	}
	defer a.Close()
	b, err := os.Open(bPath)
	if err != nil {
		return false, errWithPath("open", bPath, err)
	}
	defer b.Close()

	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for remaining := size; remaining > 0; {
		n := int64(len(bufA))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(a, bufA[:n]); err != nil {
			return false, errWithPath("read", aPath, err)
		}
		if _, err := io.ReadFull(b, bufB[:n]); err != nil {
			return false, errWithPath("read", bPath, err)
		}
		if !bytes.Equal(bufA[:n], bufB[:n]) {
			return false, nil
		}
		remaining -= n
	}
	return true, nil
}

// prefixMatches reports whether the first size bytes of both files agree. It
// compares windows at the start, in the middle and at the end of that range, so
// an append that grew the source is recognised in constant time instead of by
// hashing the whole copy.
func prefixMatches(destPath, srcPath string, size int64) bool {
	if size <= 0 {
		return true // an empty copy is a prefix of anything
	}
	window := int64(archiveFingerprintBytes)
	if size < window {
		window = size
	}

	last := size - window
	checked := make(map[int64]bool, 3)
	for _, offset := range []int64{0, last / 2, last} {
		if checked[offset] {
			continue // small file: the windows coincide
		}
		checked[offset] = true

		destBytes, err := readAt(destPath, offset, window)
		if err != nil {
			return false
		}
		srcBytes, err := readAt(srcPath, offset, window)
		if err != nil {
			return false
		}
		if !bytes.Equal(destBytes, srcBytes) {
			return false
		}
	}
	return true
}

// readAt reads exactly n bytes at offset from a file.
func readAt(path string, offset, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// fullCopy replaces the archived copy with the whole source file.
func (a *Archiver) fullCopy(src, dst string) error {
	a.fullCopies.Add(1)

	in, err := os.Open(src)
	if err != nil {
		return errWithPath("open source", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return errWithPath("create", dst, err)
	}
	defer out.Close()
	restrictToOwner(out, dst)

	if _, err := io.Copy(out, in); err != nil {
		return errWithPath("copy to", dst, err)
	}
	if err := out.Sync(); err != nil {
		return errWithPath("sync", dst, err)
	}
	return nil
}

// appendTail appends everything after offset from src to dst.
func (a *Archiver) appendTail(src, dst string, offset int64) error {
	a.appendedCopies.Add(1)

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errWithPath("open", dst, err)
	}
	defer out.Close()
	restrictToOwner(out, dst)

	in, err := os.Open(src)
	if err != nil {
		return errWithPath("open source", src, err)
	}
	defer in.Close()

	if _, err := in.Seek(offset, io.SeekStart); err != nil {
		return errWithPath("seek in source", src, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return errWithPath("copy to", dst, err)
	}
	if err := out.Sync(); err != nil {
		return errWithPath("sync", dst, err)
	}
	return nil
}

// restrictToOwner keeps an archived transcript copy owner-only. The mode on
// create is subject to the umask, and an existing copy may still carry the mode
// of an older version, so it is set explicitly. A filesystem that cannot chmod
// is reported, not fatal: the copy sits in a 0700 directory either way.
func restrictToOwner(f *os.File, path string) {
	if err := f.Chmod(0o600); err != nil {
		log.Printf("[archive] cannot restrict %s to owner-only: %v", path, err)
	}
}
