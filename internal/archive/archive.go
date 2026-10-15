package archive

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
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
}

// New creates an archiver targeting the given base directory.
func New(baseDir string) *Archiver {
	return &Archiver{baseDir: baseDir}
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
	// Verify source exists
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", srcPath, err)
	}

	// Create project subdirectory
	destDir := filepath.Join(a.baseDir, project)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
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
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// appendTail appends everything after offset from src to dst.
func (a *Archiver) appendTail(src, dst string, offset int64) error {
	a.appendedCopies.Add(1)

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := in.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
