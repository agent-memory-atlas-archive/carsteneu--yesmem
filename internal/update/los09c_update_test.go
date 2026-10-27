package update

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// ── YM-301 (Los 09c): Update-Restpunkte ─────────────────────────────────────
//
// The helpers below carry a los09c prefix on purpose: parallel yesloop agents
// work in the same repository and have already produced duplicate helper names
// inside one package.

// los09cSeededDest lays out an installed binary plus the marker/asset plumbing
// the update tests need: dest, a lock file next to it, and the checksum line the
// served archive matches.
func los09cSeededDest(t *testing.T) (dir, dest, marker string) {
	t.Helper()
	dir = t.TempDir()
	dest = filepath.Join(dir, "yesmem")
	if err := os.WriteFile(dest, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	return dir, dest, filepath.Join(dir, "migrate-marker")
}

// los09cAssetServer serves the archive and its checksums.txt the way a release
// does, so the whole download/verify/extract path runs for real.
func los09cAssetServer(t *testing.T, content []byte) (binaryURL, checksumURL string) {
	t.Helper()
	allowLocalDownloads(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			w.Write(content)
		case "/checksums":
			w.Write([]byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(content), los09cAssetName())))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/binary", srv.URL + "/checksums"
}

func los09cAssetName() string {
	return assetName("9.9.9", runtime.GOOS, runtime.GOARCH)
}

// los09cMigrateScript is the content of the "new binary" the archive ships. It
// records whether the update lock was held while it ran: the migration step is
// exactly the window the audit found unprotected. The probe first proves that
// flock works at all and fails the run loudly otherwise, so neither a missing
// nor a broken flock can turn the caller's assertion into a vacuous pass.
func los09cMigrateScript(dir, marker string, exitCode int) []byte {
	lock := filepath.Join(dir, ".yesmem-update.lock")
	probe := filepath.Join(dir, ".flock-probe")
	return []byte(fmt.Sprintf(`#!/bin/sh
command -v flock >/dev/null 2>&1 || { echo lock-probe-unavailable > %q; exit 2; }
flock -n %q true 2>/dev/null || { echo lock-probe-broken > %q; exit 2; }
if flock -n %q true 2>/dev/null; then
  echo lock-free > %q
else
  echo locked > %q
fi
exit %d
`, marker, probe, marker, lock, marker, marker, exitCode))
}

// los09cStubRelease points the updater at the local asset server.
func los09cStubRelease(t *testing.T, binaryURL, checksumURL string) {
	t.Helper()
	prev := checkForUpdate
	checkForUpdate = func(string) (*UpdateInfo, error) {
		return &UpdateInfo{
			Available:   true,
			Version:     "v9.9.9",
			BinaryURL:   binaryURL,
			ChecksumURL: checksumURL,
		}, nil
	}
	t.Cleanup(func() { checkForUpdate = prev })
}

// The lock exists so that no two updaters touch the binary at the same time, but
// it was released before the migration step: migrate runs the new binary, which
// a concurrent update may already have replaced again.
func TestLos09cRunUpdateHoldsLockAcrossMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, dest, marker := los09cSeededDest(t)
	binaryURL, checksumURL := los09cAssetServer(t, createTarGz(t, "yesmem", los09cMigrateScript(dir, marker, 0)))
	los09cStubRelease(t, binaryURL, checksumURL)

	got, err := runUpdateTo(dest, "v1.0.0", nil)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got != "v9.9.9" {
		t.Fatalf("installed version = %q, want v9.9.9", got)
	}
	state, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the new binary never ran its migration: %v", err)
	}
	if strings.TrimSpace(string(state)) != "locked" {
		t.Errorf("migration ran with the update lock released (marker %q); a concurrent update can replace the binary mid-migration",
			strings.TrimSpace(string(state)))
	}
}

// A new binary that cannot run its own migration leaves the installation on a
// build nobody has proven. atomicReplace keeps the previous binary at .bak, so
// the update must put it back instead of reporting success.
func TestLos09cRunUpdateRollsBackOnMigrateFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, dest, marker := los09cSeededDest(t)
	binaryURL, checksumURL := los09cAssetServer(t, createTarGz(t, "yesmem", los09cMigrateScript(dir, marker, 1)))
	los09cStubRelease(t, binaryURL, checksumURL)

	got, err := runUpdateTo(dest, "v1.0.0", nil)
	if err == nil {
		t.Fatalf("a new binary whose migration failed was reported as installed (%q)", got)
	}
	content, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read dest: %v", readErr)
	}
	if string(content) != "old-binary" {
		t.Errorf("dest = %q, want the previous binary restored from .bak", string(content))
	}
}

// io.LimitReader truncates silently, so a body beyond the cap is not rejected
// here: it surfaces later as a checksum mismatch, which names the wrong problem
// and hides a proxy or server that cut the download short.
func TestLos09cDownloadRejectsOversizedBody(t *testing.T) {
	allowLocalDownloads(t)
	prev := maxDownloadSize
	maxDownloadSize = 1024
	t.Cleanup(func() { maxDownloadSize = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("A"), 2048))
	}))
	defer srv.Close()

	if _, err := download(&http.Client{}, srv.URL+"/binary"); err == nil {
		t.Fatal("a body beyond the download limit was accepted silently")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a size refusal naming the limit", err)
	}
}

// Asset URLs on a release can carry a signature in the query string. The error
// is logged by the daemon ticker (daemon.go: `log.Printf("[update] check failed:
// %v", err)`), so the URL must not survive into the message.
func TestLos09cDownloadErrorRedactsAssetURL(t *testing.T) {
	allowLocalDownloads(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rawURL := srv.URL + "/yesmem.tar.gz?X-Amz-Signature=LOS09CSECRET"
	srv.Close() // every request now fails at the transport

	_, err := download(&http.Client{}, rawURL)
	if err == nil {
		t.Fatal("a download against a closed server reported success")
	}
	if strings.Contains(err.Error(), "LOS09CSECRET") {
		t.Errorf("error leaks the signed asset URL: %v", err)
	}
}

// A restart replaces the process that serves the install directory. It shares
// the binary and its .bak with the updater, so it must not interleave with a
// running update.
func TestLos09cRunRestartTakesTheUpdateLock(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "yesmem")

	prev := execRestart
	t.Cleanup(func() { execRestart = prev })
	var heldDuringExec bool
	execRestart = func(string) error {
		f, err := os.OpenFile(filepath.Join(dir, ".yesmem-update.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		heldDuringExec = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
		return nil
	}

	if err := runRestartTo(binary, nil); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	if !heldDuringExec {
		t.Error("the restart ran with the update lock released")
	}
}

// The other direction: a restart while an update holds the lock must report
// instead of pulling the binary out from under the downloader.
func TestLos09cRunRestartRefusesWhileUpdateRuns(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "yesmem")
	unlock, err := lockUpdate(binary)
	if err != nil {
		t.Fatalf("take update lock: %v", err)
	}
	defer unlock()

	prev := execRestart
	t.Cleanup(func() { execRestart = prev })
	executed := false
	execRestart = func(string) error {
		executed = true
		return nil
	}

	err = runRestartTo(binary, nil)
	if err == nil {
		t.Fatal("a restart ran while an update was in progress")
	}
	if !strings.Contains(err.Error(), "another update") {
		t.Errorf("error = %v, want a refusal reporting the running update", err)
	}
	if executed {
		t.Error("the restart command was executed although the update lock is held")
	}
}

// "make deploy" installs the binary with the same cp+mv dance the updater uses,
// on the same path. Without the updater's lock the two interleave: an update
// that is downloading while a deploy swaps the inode replaces a file nobody
// holds any more, and the deploy's own binary can be the loser.
func TestLos09cMakefileDeployTakesTheUpdateLock(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := los09cRecipe(string(data), "deploy:")
	if len(recipe) == 0 {
		t.Fatal("no deploy recipe found in the Makefile")
	}
	joined := strings.Join(recipe, "\n")
	if !strings.Contains(joined, "$(UPDATE_LOCK)") {
		t.Errorf("the deploy recipe does not take the update lock:\n%s", joined)
	}
	// The lock has to wrap the swap itself. A recipe that splits into an
	// unlocked "cp" and a locked "mv" would leave exactly the inode exchange
	// unprotected that the lock exists for.
	var flockLine string
	for _, line := range recipe {
		if strings.Contains(line, "flock -w") {
			flockLine = line
			break
		}
	}
	if flockLine == "" {
		t.Fatalf("the deploy recipe has no flock invocation:\n%s", joined)
	}
	if !strings.Contains(flockLine, "cp ") || !strings.Contains(flockLine, "mv -f") {
		t.Errorf("the flock invocation does not cover the cp+mv swap:\n%s", flockLine)
	}
	// cp honours the process umask and keeps an existing target's mode, so the
	// installed binary's mode would depend on the caller's umask and on leftovers
	// of an aborted run. The updater always installs 0755 (atomicReplace).
	if !strings.Contains(flockLine, "chmod 0755") {
		t.Errorf("the flock invocation does not pin the binary mode to 0755:\n%s", flockLine)
	}
	// The wait has to outlast a legitimate updater, otherwise deploy gives up in
	// the middle of an update that is doing exactly what the lock allows. Upper
	// bound on the Go side: two downloads with the client's 5-minute timeout plus
	// the migration budget (runMigrate).
	const goMaxHoldSeconds = 2*5*60 + 90
	wait := los09cLockWait(t, flockLine)
	if wait <= goMaxHoldSeconds {
		t.Errorf("deploy waits %ds for the update lock, but an update can hold it for up to %ds", wait, goMaxHoldSeconds)
	}
	// The lock file has to be the one lockUpdate creates, otherwise the two
	// processes lock different files and neither is excluded.
	var lockDef string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "UPDATE_LOCK") {
			lockDef = strings.TrimSpace(line)
			break
		}
	}
	if lockDef == "" {
		t.Fatal("the Makefile does not define UPDATE_LOCK")
	}
	if !strings.Contains(lockDef, "$(dir $(INSTALL))") {
		t.Errorf("UPDATE_LOCK = %q does not sit next to the installed binary", lockDef)
	}
	if !strings.Contains(lockDef, UpdateLockName) {
		t.Errorf("UPDATE_LOCK = %q does not name the updater's lock file %q", lockDef, UpdateLockName)
	}
}

// los09cLockWait reads the seconds out of a "flock -w <seconds>" invocation.
func los09cLockWait(t *testing.T, line string) int {
	t.Helper()
	m := regexp.MustCompile(`flock -w (\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no flock -w <seconds> in %q", line)
	}
	wait, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse flock wait %q: %v", m[1], err)
	}
	return wait
}

// los09cRecipe returns the tab-indented recipe lines of one Makefile target.
func los09cRecipe(makefile, target string) []string {
	var recipe []string
	inTarget := false
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, target) {
			inTarget = true
			continue
		}
		if !inTarget {
			continue
		}
		if strings.HasPrefix(line, "\t") {
			recipe = append(recipe, line)
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		break
	}
	return recipe
}

// A rollback must prove that the backup it restores is really a binary. Three
// shapes make .bak unusable, and each has to fail loudly instead of moving a
// foreign inode to dest, which the next restart would execute.
func TestLos09cRestoreBackupRejectsUnusableBackup(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, backup string)
		want  string
	}{
		{
			name:  "missing",
			setup: func(*testing.T, string) {},
			want:  "no backup",
		},
		{
			name: "symlink",
			setup: func(t *testing.T, backup string) {
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.WriteFile(target, []byte("not-a-binary"), 0755); err != nil {
					t.Fatalf("seed symlink target: %v", err)
				}
				if err := os.Symlink(target, backup); err != nil {
					t.Fatalf("symlink backup: %v", err)
				}
			},
			want: "not a regular file",
		},
		{
			name: "directory",
			setup: func(t *testing.T, backup string) {
				if err := os.MkdirAll(filepath.Join(backup, "keep"), 0755); err != nil {
					t.Fatalf("seed backup dir: %v", err)
				}
			},
			want: "not a regular file",
		},
		{
			name: "not executable",
			setup: func(t *testing.T, backup string) {
				if err := os.WriteFile(backup, []byte("old-binary"), 0644); err != nil {
					t.Fatalf("seed backup: %v", err)
				}
			},
			want: "not executable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "yesmem")
			if err := os.WriteFile(dest, []byte("new-binary"), 0755); err != nil {
				t.Fatalf("seed dest: %v", err)
			}
			tt.setup(t, dest+".bak")

			err := restoreBackup(dest)
			if err == nil {
				t.Fatal("an unusable backup was restored")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			content, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatalf("read dest: %v", readErr)
			}
			if string(content) != "new-binary" {
				t.Errorf("dest = %q, want it untouched", string(content))
			}
		})
	}
}

// Without a backup there is nothing to roll back to: the new binary stays, and
// the caller must get an error that names the missing path instead of a version.
func TestLos09cPostUpdateInstallWithoutBackupFails(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "yesmem")
	failing := []byte("#!/bin/sh\nexit 1\n")
	if err := os.WriteFile(dest, failing, 0755); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	err := postUpdateInstall(dest, filepath.Join(dir, "config.yaml"), log.New(&bytes.Buffer{}, "", 0))
	if err == nil {
		t.Fatal("a failed migration without a backup was reported as success")
	}
	if !strings.Contains(err.Error(), "could not be restored") {
		t.Errorf("error = %v, want the missing rollback path to be named", err)
	}
	content, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read dest: %v", readErr)
	}
	if string(content) != string(failing) {
		t.Error("dest was replaced although there was nothing to restore")
	}
}
