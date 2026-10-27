package daemon

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const aiJailRepo = "akitaonrails/ai-jail"

// aiJailPinnedVersion is the release this code was tested against (see
// sandbox.go for the flag semantics it relies on). The download asks for this
// tag, never for releases/latest: a newer release changes sandbox behaviour
// without a code change on our side, and an unreviewed download of whatever
// upstream published last is a supply-chain risk (YM-205 / OPS-40).
const aiJailPinnedVersion = "2.10.0"

// aiJailAssetDigests pins the SHA256 of every archive the pinned release ships
// (from the release's asset metadata). Only these platforms are installable; a
// platform without an entry is refused instead of falling back to "latest".
// Upgrading means bumping the version and every digest together.
var aiJailAssetDigests = map[string]string{
	"ai-jail-linux-x86_64.tar.gz":  "cd4b4b5dad69d6fb11a0cd9a17e6c2e1848cac4ba3d8da0a507a6eb535dc0714",
	"ai-jail-macos-aarch64.tar.gz": "de065b48d7df14fc3035b8d2ce4a284081b0e70a458b6a4ee0956a7cd93a892c",
}

// aiJailAPIBase is the GitHub API root; a test seam.
var aiJailAPIBase = "https://api.github.com"

func downloadAiJail() (string, error) {
	if os.Getenv("YESMEM_DISABLE_AIJAIL_DOWNLOAD") != "" {
		return "", errors.New("ai-jail download disabled by YESMEM_DISABLE_AIJAIL_DOWNLOAD env")
	}
	installDir := filepath.Join(os.Getenv("HOME"), ".local", "bin")
	destPath := filepath.Join(installDir, "ai-jail")

	if info, err := os.Stat(destPath); err == nil && info.Mode().IsRegular() {
		return destPath, nil
	}

	assetName, err := aiJailAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	expectedDigest, ok := aiJailAssetDigests[assetName]
	if !ok {
		return "", fmt.Errorf("ai-jail: no pinned checksum for %s in release v%s — refusing to install an unverified build", assetName, aiJailPinnedVersion)
	}
	url, err := resolveAiJailAssetURL(assetName)
	if err != nil {
		return "", fmt.Errorf("resolve release: %w", err)
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", err
	}

	log.Printf("[sandbox] downloading ai-jail v%s from %s", aiJailPinnedVersion, url)
	archivePath := destPath + ".tar.gz.tmp"
	if err := downloadToFile(url, archivePath); err != nil {
		os.Remove(archivePath)
		return "", err
	}
	defer os.Remove(archivePath)

	// Fail closed: the archive is only unpacked once its bytes match the pinned
	// release, so a moved tag or a tampered download never reaches the install
	// path.
	actualDigest, err := fileSHA256(archivePath)
	if err != nil {
		return "", err
	}
	if actualDigest != expectedDigest {
		return "", fmt.Errorf("ai-jail: checksum mismatch for %s (want %s, got %s) — download discarded", assetName, expectedDigest, actualDigest)
	}

	archive, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer archive.Close()

	tmp := destPath + ".tmp"
	if err := extractAiJailFromTarGz(archive, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("extract ai-jail: %w", err)
	}

	if err := os.Rename(tmp, destPath); err != nil {
		os.Remove(tmp)
		return "", err
	}

	log.Printf("[sandbox] ai-jail v%s installed to %s", aiJailPinnedVersion, destPath)
	return destPath, nil
}

// maxAiJailArchiveBytes bounds a release download. The pinned archive is ~1.6 MB;
// the ceiling keeps a compromised or misbehaving source from filling the disk.
const maxAiJailArchiveBytes = 32 << 20

func downloadToFile(url, dest string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxAiJailArchiveBytes+1))
	if err != nil {
		f.Close()
		return err
	}
	if written > maxAiJailArchiveBytes {
		f.Close()
		return fmt.Errorf("download exceeds %d bytes, refusing", maxAiJailArchiveBytes)
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func resolveAiJailAssetURL(assetName string) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/v%s", aiJailAPIBase, aiJailRepo, aiJailPinnedVersion)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("release lookup failed: %s", resp.Status)
	}

	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	for _, a := range release.Assets {
		if a.Name == assetName || a.Name == assetName+".tar.gz" || a.Name == assetName+".zip" {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("asset %q not found in release v%s", assetName, aiJailPinnedVersion)
}

// aiJailAssetName maps a Go runtime.GOOS/GOARCH pair to the actual asset
// filename published by https://github.com/akitaonrails/ai-jail. Upstream uses
// macos (not darwin) and x86_64/aarch64 (not amd64/arm64). Combinations not
// shipped upstream return an explicit error so the daemon falls back to the
// unsandboxed path with a clear log line instead of a silent 404.
func aiJailAssetName(goos, goarch string) (string, error) {
	var osName string
	switch goos {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "macos"
	default:
		return "", fmt.Errorf("ai-jail: unsupported OS %q (only linux and darwin are released)", goos)
	}
	var archName string
	switch goarch {
	case "amd64":
		archName = "x86_64"
	case "arm64":
		archName = "aarch64"
	default:
		return "", fmt.Errorf("ai-jail: unsupported arch %q (only amd64 and arm64 are released)", goarch)
	}
	return fmt.Sprintf("ai-jail-%s-%s.tar.gz", osName, archName), nil
}

// extractAiJailFromTarGz reads a gzip-compressed tar archive from r, finds the
// first regular file whose basename is "ai-jail" (the upstream tarball ships it
// flat at the root, but a nested layout is tolerated), and writes it to
// destPath with mode 0o755. Caller is responsible for the atomic rename.
func extractAiJailFromTarGz(r io.Reader, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != "ai-jail" {
			continue
		}
		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	return errors.New("ai-jail binary not found in tarball")
}
