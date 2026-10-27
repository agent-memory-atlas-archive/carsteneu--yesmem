package update

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// ── YM-313 (Los 56f): Install-Lock, Lock-Haerten, Version im Log ────────────

// "make install" swaps the same binary as "make deploy" and the updater. A
// plain cp replaces it in place: the write truncates the inode a running
// daemon or proxy has mapped, the copy fails with ETXTBSY (or leaves half a
// binary behind if the process dies mid-write), and nothing excludes a
// parallel update from swapping the path underneath it.
func TestLos56fMakefileInstallTakesTheUpdateLock(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := los09cRecipe(string(data), "install:")
	if len(recipe) == 0 {
		t.Fatal("no install recipe found in the Makefile")
	}
	joined := strings.Join(recipe, "\n")
	if !strings.Contains(joined, "$(UPDATE_LOCK)") {
		t.Errorf("the install recipe does not take the update lock:\n%s", joined)
	}
	var flockLine string
	for _, line := range recipe {
		if strings.Contains(line, "flock -w") {
			flockLine = line
			break
		}
	}
	if flockLine == "" {
		t.Fatalf("the install recipe has no flock invocation:\n%s", joined)
	}
	// The lock has to wrap the swap itself, and the target has to change by
	// rename: a locked "cp $(BINARY) $(INSTALL)" would still truncate the
	// running inode inside the lock.
	if !strings.Contains(flockLine, "cp ") || !strings.Contains(flockLine, "mv -f") {
		t.Errorf("the flock invocation does not cover the cp+mv swap:\n%s", flockLine)
	}
	if !strings.Contains(flockLine, "chmod 0755") {
		t.Errorf("the flock invocation does not pin the binary mode to 0755:\n%s", flockLine)
	}
	// The lock path is reached through the shell, where O_NOFOLLOW does not exist:
	// a symlink at that name would let the recipe create, chmod and flock a file
	// of someone else's choosing instead of excluding the updater.
	if !strings.Contains(joined, `-L "$(UPDATE_LOCK)"`) {
		t.Errorf("the install recipe does not refuse a symlinked lock file:\n%s", joined)
	}
	// A fixed temp name can be pre-planted as a link, and cp would write through
	// it. mktemp reserves the name (O_EXCL) in the target directory.
	if !strings.Contains(flockLine, "mktemp") {
		t.Errorf("the install recipe copies to a predictable temp name:\n%s", flockLine)
	}
}

// install and deploy swap the same binary with the same dance. Two copies of the
// recipe drift: the next lock or atomicity fix lands in one of them, and the
// other silently keeps replacing the running binary in place.
func TestLos56fInstallAndDeployShareOneSwap(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	install := los09cRecipe(string(data), "install:")
	deploy := los09cRecipe(string(data), "deploy:")
	if len(install) == 0 || len(deploy) == 0 {
		t.Fatal("install or deploy recipe missing")
	}
	los56fSwap := func(recipe []string) string {
		for _, line := range recipe {
			if strings.Contains(line, "flock -w") {
				return line
			}
		}
		return ""
	}
	is, ds := los56fSwap(install), los56fSwap(deploy)
	if is == "" || ds == "" {
		t.Fatalf("install flock line %q, deploy flock line %q", is, ds)
	}
	if is != ds {
		t.Errorf("install and deploy swap the binary differently:\ninstall: %s\ndeploy:  %s", is, ds)
	}
}

// The lock file lives next to the installed binary, a directory a local
// attacker or a stray tool can write. Opening it without O_NOFOLLOW lets a
// symlink at that name move the flock (and the future O_CREATE) onto any file
// the user owns.
func TestLos56fLockUpdateDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not a lock"), 0600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, UpdateLockName)); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	// An unusable lock has to degrade, not fail: the pre-lock behavior was
	// "update without a lock", and a planted link must not disable updates.
	unlock, err := lockUpdate(filepath.Join(dir, "yesmem"))
	if err != nil {
		t.Fatalf("lockUpdate refused instead of degrading: %v", err)
	}
	defer unlock()

	probe, err := os.Open(victim)
	if err != nil {
		t.Fatalf("open victim: %v", err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("lockUpdate took the lock through the symlink at %s (%v)", filepath.Join(dir, UpdateLockName), err)
	}
}

// The version comes from the release JSON. It lands in the log every cycle, so
// an unquoted value with a control character writes lines the reader cannot
// tell apart from real ones.
func TestLos56fVersionIsQuotedInLog(t *testing.T) {
	const raw = "v9.9.9\n[update] already up to date"
	prev := checkForUpdate
	t.Cleanup(func() { checkForUpdate = prev })
	checkForUpdate = func(string) (*UpdateInfo, error) {
		return &UpdateInfo{Available: true, Version: raw}, nil
	}

	var buf bytes.Buffer
	dest := filepath.Join(t.TempDir(), "yesmem")
	if _, err := runUpdateTo(dest, "v1.0.0", log.New(&buf, "", 0)); err == nil {
		t.Fatal("an update without a binary URL must fail")
	}
	out := buf.String()
	if strings.Contains(out, raw) {
		t.Errorf("the release version reached the log unfiltered:\n%q", out)
	}
	if !strings.Contains(out, "v9.9.9\\n") {
		t.Errorf("the version is not logged in quoted form:\n%q", out)
	}
}

// A release whose install keeps failing was downloaded again on every tick:
// extraction, download and a rolled-back migration every check_interval, with
// the same known-bad version. The attempt is expensive (a ~120 MiB archive)
// and repeats until someone looks at the release. One retry stays, because a
// single failure is usually the network and a permanently skipped release is
// worse than one repeated fetch.
func TestLos56fFailedVersionIsSkipped(t *testing.T) {
	allowLocalDownloads(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	const broken = "v9.9.9-los56f-broken"
	// The counter is process-wide and a later test must not inherit this run.
	t.Cleanup(func() { forgetInstallAttempts(broken) })
	prev := checkForUpdate
	t.Cleanup(func() { checkForUpdate = prev })
	checkForUpdate = func(string) (*UpdateInfo, error) {
		return &UpdateInfo{Available: true, Version: broken, BinaryURL: srv.URL + "/binary", ChecksumURL: srv.URL + "/checksums"}, nil
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "yesmem")
	if err := os.WriteFile(dest, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	logger := log.New(&bytes.Buffer{}, "", 0)
	for attempt := 1; attempt <= maxInstallAttemptsPerVersion; attempt++ {
		if _, err := runUpdateTo(dest, "v1.0.0", logger); err == nil {
			t.Fatalf("attempt %d against a broken release must fail", attempt)
		}
	}
	hitsBeforeSkip := atomic.LoadInt32(&hits)
	if hitsBeforeSkip == 0 {
		t.Fatal("the failing attempts did not reach the release server")
	}

	newVersion, err := runUpdateTo(dest, "v1.0.0", logger)
	if err != nil {
		t.Fatalf("the skipped attempt reported the known-bad release again: %v", err)
	}
	if newVersion != "" {
		t.Errorf("the skipped attempt installed %q", newVersion)
	}
	if got := atomic.LoadInt32(&hits); got != hitsBeforeSkip {
		t.Errorf("the known-bad version was downloaded again (%d -> %d requests)", hitsBeforeSkip, got)
	}
}
