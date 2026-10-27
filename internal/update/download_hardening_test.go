package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// allowLocalDownloads lets this package's httptest servers pass the production
// host allowlist: loopback is admitted on top of the GitHub hosts, nothing else.
func allowLocalDownloads(t *testing.T) {
	t.Helper()
	prev := downloadURLAllowed
	downloadURLAllowed = func(u *url.URL) error {
		if u.Hostname() == "127.0.0.1" {
			return nil
		}
		return prev(u)
	}
	t.Cleanup(func() { downloadURLAllowed = prev })
}

func TestValidateDownloadURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"github asset", "https://github.com/carsteneu/yesmem/releases/download/v1/yesmem_1_linux_amd64.tar.gz", false},
		{"github redirect target", "https://objects.githubusercontent.com/github-production-release-asset/x", false},
		{"current asset redirect target", "https://release-assets.githubusercontent.com/github-production-release-asset/x", false},
		{"uppercase host", "https://GitHub.com/carsteneu/yesmem", false},
		{"explicit 443", "https://github.com:443/carsteneu/yesmem", false},
		{"plain http", "http://github.com/carsteneu/yesmem", true},
		{"foreign host", "https://evil.invalid/yesmem.tar.gz", true},
		{"suffix lookalike", "https://github.com.evil.invalid/yesmem.tar.gz", true},
		{"other port", "https://github.com:8443/yesmem.tar.gz", true},
		{"non-ascii lookalike", "https://gİthub.com/yesmem.tar.gz", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.raw, err)
			}
			err = validateDownloadURL(u)
			if tt.wantErr && err == nil {
				t.Errorf("%s was accepted, want a refusal", tt.raw)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("%s was refused: %v", tt.raw, err)
			}
		})
	}
}

// The release API response decides which URL the updater fetches. An asset URL
// on any host but GitHub turns a compromised release payload into a binary
// source of the attacker's choosing, so the fetch must be refused before any
// request leaves the machine.
func TestDownloadAndReplace_RejectsForeignHost(t *testing.T) {
	const asset = "yesmem_1.0.0_linux_amd64.tar.gz"
	dest := filepath.Join(t.TempDir(), "yesmem")

	err := DownloadAndReplace(
		"https://evil.invalid/"+asset,
		"https://evil.invalid/checksums.txt",
		asset,
		dest,
	)
	if err == nil {
		t.Fatal("download from a host outside the allowlist was accepted")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v, want a refusal naming the host allowlist", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest was written although the download was refused")
	}
}

// A real asset URL on github.com redirects to objects.githubusercontent.com, so
// redirects must be followed within the allowlist. A redirect that leaves it
// must not be followed at all.
func TestDownloadAndReplace_RejectsRedirectToForeignHost(t *testing.T) {
	allowLocalDownloads(t)
	const asset = "yesmem_1.0.0_linux_amd64.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.invalid/"+asset, http.StatusFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "yesmem")
	err := DownloadAndReplace(srv.URL+"/binary", srv.URL+"/checksums", asset, dest)
	if err == nil {
		t.Fatal("redirect to a host outside the allowlist was followed")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v, want a refusal naming the host allowlist", err)
	}
}

// The archive is only a transport for one file. An entry that declares a size
// beyond anything a real release ships must be refused before it is read into
// memory, otherwise a small archive expands without bound.
func TestExtractBinary_RejectsOversizedEntry(t *testing.T) {
	const declared = int64(1) << 30 // 1 GiB; a real binary is ~150 MiB

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "yesmem",
		Size:     declared,
		Mode:     0755,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	tw.Close() // the declared size is never written; the header is the payload

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(raw.Bytes())
	gw.Close()

	got, err := extractBinary(gz.Bytes())
	if err == nil {
		t.Fatalf("entry declaring %d bytes was extracted (%d bytes read)", declared, len(got))
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a size refusal", err)
	}
}

// Two updaters (daemon ticker and a manual `yesmem update`) share the dest
// directory and its temp names. Without a lock the second one removes the first
// one's files and both rename over each other.
func TestDownloadAndReplace_RejectsConcurrentUpdate(t *testing.T) {
	allowLocalDownloads(t)
	const asset = "yesmem_1.0.0_linux_amd64.tar.gz"
	binaryContent := []byte("new-binary")
	archive := createTarGz(t, "yesmem", binaryContent)
	checksumLine := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)

	entered := make(chan struct{})
	release := make(chan struct{})
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			if atomic.AddInt32(&requests, 1) == 1 {
				close(entered)
				<-release
			}
			w.Write(archive)
		case "/checksums":
			w.Write([]byte(checksumLine))
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "yesmem")
	os.WriteFile(dest, []byte("old-binary"), 0755)

	first := make(chan error, 1)
	go func() {
		first <- DownloadAndReplace(srv.URL+"/binary", srv.URL+"/checksums", asset, dest)
	}()
	<-entered // the first updater is inside the download

	second := DownloadAndReplace(srv.URL+"/binary", srv.URL+"/checksums", asset, dest)
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first updater failed: %v", err)
	}
	if second == nil {
		t.Fatal("a second updater ran in parallel with the first one")
	}
	if !strings.Contains(second.Error(), "another update") {
		t.Errorf("error = %v, want a refusal reporting the running update", second)
	}
}

// The backup link uses a fixed temp name. If that name is taken by a directory,
// os.Remove fails and the link fails with EEXIST, so .bak silently keeps the
// oldest binary forever. The backup must survive a blocked temp name.
func TestAtomicReplace_BackupSurvivesNonEmptyTempDir(t *testing.T) {
	tmpDir := t.TempDir()
	dest := filepath.Join(tmpDir, "yesmem")
	os.WriteFile(dest, []byte("old"), 0755)
	os.WriteFile(dest+".bak", []byte("ancient"), 0755)

	blocked := dest + ".bak.tmp"
	if err := os.Mkdir(blocked, 0755); err != nil {
		t.Fatalf("seed blocking dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed blocking dir content: %v", err)
	}

	if err := atomicReplace(dest, []byte("new")); err != nil {
		t.Fatalf("atomicReplace failed: %v", err)
	}

	backup, err := os.ReadFile(dest + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "old" {
		t.Errorf("backup = %q, want the previous binary \"old\"", backup)
	}
}

// The updater must keep working when the lock path is unusable. Before this
// guard, a directory (or a root-owned file) at .yesmem-update.lock disabled
// every future update with "open update lock" — the same permanent-blockage
// class the fixed .bak.tmp name had.
func TestDownloadAndReplace_ProceedsWhenLockPathUnusable(t *testing.T) {
	allowLocalDownloads(t)
	const asset = "yesmem_1.0.0_linux_amd64.tar.gz"
	binaryContent := []byte("new-binary")
	archive := createTarGz(t, "yesmem", binaryContent)
	checksumLine := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			w.Write(archive)
		case "/checksums":
			w.Write([]byte(checksumLine))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "yesmem")
	os.WriteFile(dest, []byte("old-binary"), 0755)
	if err := os.Mkdir(filepath.Join(dir, ".yesmem-update.lock"), 0755); err != nil {
		t.Fatalf("seed blocking lock dir: %v", err)
	}

	if err := DownloadAndReplace(srv.URL+"/binary", srv.URL+"/checksums", asset, dest); err != nil {
		t.Fatalf("update blocked by an unusable lock path: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, binaryContent) {
		t.Error("binary was not replaced")
	}
}

// Only the matching entry is capped per header size, so a small archive can
// still decompress without bound through the entries before it. The total
// budget is the guard for that case.
func TestExtractBinary_RejectsTotalBomb(t *testing.T) {
	prev := maxExtractBytes
	maxExtractBytes = 1024
	t.Cleanup(func() { maxExtractBytes = prev })

	archive := createTarGz(t, "filler", bytes.Repeat([]byte("A"), 4096))
	if _, err := extractBinary(archive); err == nil {
		t.Fatal("archive expanding beyond the total budget was extracted")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a size refusal", err)
	}
}

// A crash between the backup link and its rename leaves a yesmem-bak-<random>
// hardlink that holds the old binary inode. The next update must clear those.
func TestAtomicReplace_CleansStaleBackupLinks(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "yesmem")
	os.WriteFile(dest, []byte("old"), 0755)
	stale := filepath.Join(dir, "yesmem-bak-1234567")
	os.WriteFile(stale, []byte("stale-binary"), 0755)

	if err := atomicReplace(dest, []byte("new")); err != nil {
		t.Fatalf("atomicReplace failed: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale backup link survived the next update")
	}
	backup, err := os.ReadFile(dest + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "old" {
		t.Errorf("backup = %q, want the previous binary \"old\"", backup)
	}
}
