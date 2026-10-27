package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// YM-205 / OPS-40: the auto-download followed releases/latest, so a new machine
// (or a deleted binary) silently got whatever upstream had published — 2.x, whose
// flag semantics differ — instead of the version this code was tested against.
// The download must ask for a fixed tag and refuse anything that does not match
// the pinned checksum.
func TestResolveAiJailAssetURL_AsksForThePinnedTag(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + aiJailPinnedVersion,
			"assets": []map[string]string{{
				"name":                 "ai-jail-linux-x86_64.tar.gz",
				"browser_download_url": "https://example.invalid/ai-jail-linux-x86_64.tar.gz",
			}},
		})
	}))
	defer server.Close()

	restore := swapAiJailAPIBase(t, server.URL)
	defer restore()

	url, err := resolveAiJailAssetURL("ai-jail-linux-x86_64.tar.gz")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotPath != "/repos/"+aiJailRepo+"/releases/tags/v"+aiJailPinnedVersion {
		t.Errorf("download must ask for the pinned tag, requested %q", gotPath)
	}
	if url != "https://example.invalid/ai-jail-linux-x86_64.tar.gz" {
		t.Errorf("url = %q", url)
	}
}

func TestDownloadAiJail_InstallsThePinnedArchive(t *testing.T) {
	requireAiJailPlatform(t)
	archive := buildTarGz(t, []tarEntry{{name: "ai-jail", mode: 0o755, body: []byte("#!/bin/sh\necho pinned\n")}}).Bytes()
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreAPI, restoreAssets := withFakeAiJailRelease(t, archive, digestOf(archive))
	defer restoreAPI()
	defer restoreAssets()

	path, err := downloadAiJail()
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if want := filepath.Join(home, ".local", "bin", "ai-jail"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("binary not installed: %v", err)
	}
}

// The supply-chain guard: a tarball whose bytes differ from the pinned release is
// discarded, and nothing is written to the install path (fail closed).
func TestDownloadAiJail_RejectsArchiveThatFailsItsChecksum(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "ai-jail", mode: 0o755, body: []byte("#!/bin/sh\necho tampered\n")}}).Bytes()
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreAPI, restoreAssets := withFakeAiJailRelease(t, archive, "0000000000000000000000000000000000000000000000000000000000000000")
	defer restoreAPI()
	defer restoreAssets()

	_, err := downloadAiJail()
	if err == nil {
		t.Fatal("expected the download to be refused on a checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error should name the checksum, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", "ai-jail")); statErr == nil {
		t.Error("a tarball with a bad checksum must not be installed")
	}
}

// Only the platforms the pinned release actually publishes have a checksum. A
// missing entry is a refusal, not a fall-through to "latest".
func TestDownloadAiJail_RefusesPlatformWithoutPinnedChecksum(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restore := swapAiJailDigests(t, map[string]string{})
	defer restore()

	_, err := downloadAiJail()
	if err == nil {
		t.Skip("current platform has a pinned checksum, the refusal path is unreachable here")
	}
	if !strings.Contains(err.Error(), aiJailPinnedVersion) {
		t.Errorf("error should name the pinned release, got %v", err)
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A stale pinned digest would make a fresh machine unable to install ai-jail at
// all, so verify the map against the real release. Opt-in network test:
//
//	YESMEM_TEST_AIJAIL_NETWORK=1 go test ./internal/daemon/ -run PinnedReleaseDigests
func TestPinnedReleaseDigestsMatchUpstream(t *testing.T) {
	if os.Getenv("YESMEM_TEST_AIJAIL_NETWORK") == "" {
		t.Skip("set YESMEM_TEST_AIJAIL_NETWORK=1 to check the pinned digests against GitHub")
	}
	assetName, err := aiJailAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no published asset for this platform: %v", err)
	}
	url, err := resolveAiJailAssetURL(assetName)
	if err != nil {
		t.Fatalf("resolve %s: %v", assetName, err)
	}

	archive := filepath.Join(t.TempDir(), assetName)
	if err := downloadToFile(url, archive); err != nil {
		t.Fatalf("download %s: %v", url, err)
	}
	got, err := fileSHA256(archive)
	if err != nil {
		t.Fatal(err)
	}
	if want := aiJailAssetDigests[assetName]; got != want {
		t.Errorf("pinned digest for %s is stale: upstream %s, pinned %s", assetName, got, want)
	}
}

func swapAiJailAPIBase(t *testing.T, base string) func() {
	t.Helper()
	original := aiJailAPIBase
	aiJailAPIBase = base
	return func() { aiJailAPIBase = original }
}

func swapAiJailDigests(t *testing.T, digests map[string]string) func() {
	t.Helper()
	original := aiJailAssetDigests
	aiJailAssetDigests = digests
	return func() { aiJailAssetDigests = original }
}

// withFakeAiJailRelease serves a GitHub-shaped release response plus the archive
// itself, and pins the digest the download has to verify against. Call the
// returned func to undo both.
func withFakeAiJailRelease(t *testing.T, archive []byte, digest string) (restoreAPI func(), restoreAssets func()) {
	t.Helper()
	// The platform of this run, not a fixed one: the fake release has to name the
	// asset the downloader asks for, or the fixture tests nothing on macOS.
	assetName, err := aiJailAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("ai-jail has no asset for this platform: %v", err)
	}

	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + aiJailPinnedVersion,
			"assets": []map[string]string{{
				"name":                 assetName,
				"browser_download_url": assets.URL + "/" + assetName,
			}},
		})
	}))

	restoreDigests := swapAiJailDigests(t, map[string]string{assetName: digest})
	return swapAiJailAPIBase(t, api.URL),
		func() {
			restoreDigests()
			assets.Close()
			api.Close()
		}
}
