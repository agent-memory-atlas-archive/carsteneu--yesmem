package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Release archives ship ~120 MiB compressed (goreleaser embeds the full
// binary), so the download limit must exceed that. 100 MiB truncated real
// releases (checksum mismatch), hence 200 MiB safety headroom. Variable so a
// test can exercise the bound without a 200 MiB fixture, like maxExtractBytes.
var maxDownloadSize int64 = 200 << 20 // 200 MB safety limit

// maxBinarySize bounds one extracted archive entry. A real release binary is
// ~150 MiB; the cap only has to stop an archive bomb, so it sits well above the
// shipped size.
const maxBinarySize = 512 << 20 // 512 MB

// maxExtractBytes bounds the decompression of the whole archive. The per-entry
// check only sees the matching entry, so the entries before it are the way a
// small archive unboundedly expands. Variable so a test can exercise the budget
// without building a gigabyte fixture.
var maxExtractBytes int64 = 1 << 30 // 1 GiB

// downloadHosts are the only hosts a release asset may come from. The release
// API hands out browser_download_url on github.com, which redirects to
// release-assets.githubusercontent.com (verified 2026-10-17:
// curl -sI .../releases/download/v2.3.19/yesmem_2.3.19_linux_amd64.tar.gz →
// 302 location: https://release-assets.githubusercontent.com/...);
// objects.githubusercontent.com stays allowed for older assets and regions. The
// asset URL comes from remote JSON and must not become a fetch primitive.
var downloadHosts = map[string]bool{
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// downloadURLAllowed guards every hop of an asset download. It is a package
// variable so tests can admit their own httptest server; the production value
// stays strict.
var downloadURLAllowed = validateDownloadURL

func validateDownloadURL(u *url.URL) error {
	rawHost := u.Hostname()
	// Lowercasing is Unicode aware while the allowlist is ASCII: "gİthub.com"
	// folds to "github.com" here, but net/http dials its Punycode form.
	if !isASCII(rawHost) {
		return fmt.Errorf("refusing download from non-ASCII host %q", rawHost)
	}
	host := strings.ToLower(rawHost)
	if u.Scheme != "https" {
		return fmt.Errorf("refusing download from %q over %q (https required)", host, u.Scheme)
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("refusing download from %q on port %s", host, p)
	}
	if !downloadHosts[host] {
		return fmt.Errorf("refusing download from host %q", host)
	}
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// DownloadAndReplace downloads a binary archive, verifies its checksum,
// extracts the binary, and atomically replaces the destination file.
//
// It takes the update lock for its own duration, but nothing in production calls
// it any more: RunUpdate takes that same lock across download, replace and
// migration, and therefore calls downloadAndReplace directly. flock hangs on the
// open file description, so a second OpenFile in the same process would be
// refused as a concurrent updater. Kept as the standalone entry point (and the
// lock's regression surface).
func DownloadAndReplace(binaryURL, checksumURL, assetFilename, dest string) error {
	unlock, err := lockUpdate(dest)
	if err != nil {
		return err
	}
	defer unlock()

	return downloadAndReplace(binaryURL, checksumURL, assetFilename, dest)
}

func downloadAndReplace(binaryURL, checksumURL, assetFilename, dest string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("refusing download: more than 10 redirects")
			}
			return downloadURLAllowed(req.URL)
		},
	}

	// Download archive
	archiveData, err := download(client, binaryURL)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}

	// Download and verify checksum
	checksumData, err := download(client, checksumURL)
	if err != nil {
		return fmt.Errorf("download checksum: %w", err)
	}
	if err := verifyChecksum(archiveData, checksumData, assetFilename); err != nil {
		return err
	}

	// Extract binary from tar.gz
	binary, err := extractBinary(archiveData)
	if err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}

	return atomicReplace(dest, binary)
}

// UpdateLockName is the lock file lockUpdate creates next to the installed
// binary. "make install", "make deploy" and a setup copy all swap that same
// binary, so every one of them has to name the same file;
// TestLos09cMakefileDeployTakesTheUpdateLock binds the recipe to it.
const UpdateLockName = ".yesmem-update.lock"

// LockBinaryUpdate takes the update lock for dest and returns the function that
// releases it. It is the entry point for the other installers of the same
// binary: the setup copy in internal/setup takes it before it replaces the file,
// so an install cannot interleave with a download or a deploy.
func LockBinaryUpdate(dest string) (release func(), err error) {
	return lockUpdate(dest)
}

// lockUpdate takes an exclusive lock next to dest for the download and the
// replace. The daemon ticker and a manual `yesmem update` share dest and its
// temp names, so two updaters would remove each other's files mid-flight. The
// lock file stays behind on purpose: removing it would let a later process lock
// a different inode. Non-blocking, so a second updater reports instead of
// queueing behind a download that may take minutes.
//
// An unusable lock (path is a directory, foreign owner, filesystem without
// flock support) must not disable updates forever: the pre-lock behavior was
// "update without a lock", so that is what is left here. Only a lock that is
// demonstrably held blocks the run.
//
// O_NOFOLLOW because the lock file sits in the install directory: a symlink at
// that name would move the flock (and the O_CREATE) onto a file of someone
// else's choosing. A link is an unusable lock, not a reason to refuse.
func lockUpdate(dest string) (func(), error) {
	path := filepath.Join(filepath.Dir(dest), UpdateLockName)
	noop := func() {}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return noop, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another update is already running (%s)", path)
		}
		return noop, nil
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func download(client *http.Client, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", redactURLError(err))
	}
	if err := downloadURLAllowed(parsed); err != nil {
		return nil, err
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, redactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// One byte past the limit tells a truncated read apart from a complete one.
	// Reading up to the limit alone silently cut oversized bodies short, and the
	// truncation only surfaced later as a checksum mismatch.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxDownloadSize {
		return nil, fmt.Errorf("download too large: more than %d bytes", maxDownloadSize)
	}
	return data, nil
}

// redactURLError drops the request URL from a transport error. Asset URLs come
// from remote JSON and can carry a signature in the query; the daemon ticker
// logs this error with %v, so the URL must not survive into the message.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

func verifyChecksum(data, checksumFile []byte, filename string) error {
	actual := sha256.Sum256(data)
	actualHex := fmt.Sprintf("%x", actual)
	for _, line := range strings.Split(string(checksumFile), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == filename {
			if parts[0] != actualHex {
				return fmt.Errorf("checksum mismatch: got %s, want %s", actualHex, parts[0])
			}
			return nil
		}
	}
	return fmt.Errorf("checksum for %s not found", filename)
}

func extractBinary(archiveData []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, err
	}
	defer gr.Close()

	// The total budget stops an archive whose skipped entries put the matching
	// entry behind gigabytes of decompression.
	limited := &io.LimitedReader{R: gr, N: maxExtractBytes}
	tr := tar.NewReader(limited)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if limited.N <= 0 {
				return nil, fmt.Errorf("archive too large: more than %d bytes extracted", maxExtractBytes)
			}
			return nil, err
		}
		if filepath.Base(hdr.Name) == "yesmem" && hdr.Typeflag == tar.TypeReg {
			if hdr.Size > maxBinarySize {
				return nil, fmt.Errorf("binary too large: %d bytes (limit %d)", hdr.Size, maxBinarySize)
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxBinarySize+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > maxBinarySize {
				return nil, fmt.Errorf("binary too large: more than %d bytes", maxBinarySize)
			}
			return data, nil
		}
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("archive too large: more than %d bytes extracted", maxExtractBytes)
	}
	return nil, fmt.Errorf("binary \"yesmem\" not found in archive")
}

// restoreBackup puts the previous binary back at dest after the new one failed
// its post-update step. atomicReplace left it as a hardlink at dest+".bak", so
// one rename restores a runnable inode; without this the installation stayed on
// a build whose own migration never ran through.
//
// The backup is best effort: if the link failed, .bak is an older build (or
// absent). Restoring an older yesmem still beats keeping a binary nobody has
// seen run, and "no backup" is reported as such. The rename consumes .bak, so a
// second attempt at the same update has no source left.
//
// Lstat, a regular-file check and an exec bit, not Stat: a symlink at .bak
// survives a Stat and the rename would move the link itself over dest, so the
// next restart would execute whatever it points at.
func restoreBackup(dest string) error {
	backup := dest + ".bak"
	info, err := os.Lstat(backup)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no backup at %s", backup)
		}
		return fmt.Errorf("stat backup %s: %w", backup, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup at %s is not a regular file (%s)", backup, info.Mode())
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("backup at %s is not executable (%s)", backup, info.Mode())
	}
	if err := os.Rename(backup, dest); err != nil {
		return fmt.Errorf("rename backup: %w", err)
	}
	// The forward swap syncs the directory so its rename survives a power loss.
	// The rollback is the same kind of rename and needs the same.
	if dir, err := os.Open(filepath.Dir(dest)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// reserveBackupLink returns an unused name in dir for the backup hardlink. A
// fixed name is not usable: whatever occupies it may be impossible to remove
// (a non-empty directory), and then the link fails with EEXIST forever.
func reserveBackupLink(dir string) (string, error) {
	f, err := os.CreateTemp(dir, "yesmem-bak-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

// clearStaleBackupLinks removes backup links a crashed run left behind. Each of
// them pins the previous binary's inode for the life of the installation.
func clearStaleBackupLinks(dir string) {
	stale, err := filepath.Glob(filepath.Join(dir, "yesmem-bak-*"))
	if err != nil {
		return
	}
	for _, name := range stale {
		os.Remove(name)
	}
}

// atomicReplace writes new binary content to dest, keeping the previous binary
// at dest+".bak".
//
// The new content is written to a temp file in the same directory and closed
// BEFORE dest is touched, and the swap is a single rename. A failed write or a
// failed rename therefore leaves dest exactly as it was: the running binary
// stays reachable under its own name, which is the only state the updater can
// roll back to. Moving dest aside first would turn any later error into a
// missing binary.
func atomicReplace(dest string, content []byte) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, "yesmem-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(content); err != nil {
		discard()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		discard()
		return fmt.Errorf("chmod temp: %w", err)
	}
	// The rename is only atomic for the metadata; without the sync the new file
	// can still be empty after a power loss.
	if err := tmp.Sync(); err != nil {
		discard()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}

	// Backup as a hardlink, not a rename: dest keeps its name and only the
	// final rename below ever removes it. The link is created under a fresh
	// temp name and then renamed over .bak, so a filesystem without hardlink
	// support keeps the previous backup instead of losing it. A fixed temp name
	// is cleared first for interrupted older runs, but it cannot be trusted: a
	// non-empty directory there survives os.Remove, the link then fails with
	// EEXIST and .bak silently stops being updated. Best effort, as before — a
	// missing backup must not fail the update.
	if _, err := os.Stat(dest); err == nil {
		os.Remove(dest + ".bak.tmp") // legacy fixed name
		clearStaleBackupLinks(dir)
		if link, err := reserveBackupLink(dir); err == nil {
			if os.Link(dest, link) == nil {
				if err := os.Rename(link, dest+".bak"); err != nil {
					os.Remove(link)
				}
			}
		}
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	// Persist the rename itself.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
