package update

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// maxInstallAttemptsPerVersion is the backoff of a release that keeps failing:
// after this many failed attempts in one process the version is left alone until
// the process restarts or a newer release appears. Two, not one, because a
// single failure is usually the network (a closed laptop, a dropped download)
// and a permanently skipped release is worse than one repeated ~120 MiB fetch.
const maxInstallAttemptsPerVersion = 2

// failedInstallAttempts counts the failed install attempts per release version
// in this process. Without it a release whose migration keeps failing is
// downloaded, extracted and rolled back again on every check_interval. The map
// is in-memory on purpose: a restart is the operator's "try again" and needs no
// state file to clean up.
var (
	failedMu       sync.Mutex
	failedAttempts = map[string]int{}
)

// installAttempts reports how often an install of version failed in this process
// and records one more attempt.
func installAttempts(version string, record bool) int {
	failedMu.Lock()
	defer failedMu.Unlock()
	n := failedAttempts[version]
	if record {
		failedAttempts[version] = n + 1
	}
	return n
}

// forgetInstallAttempts drops the counter of a version. A test drives a release
// through this state and a later test in the same package must not inherit it.
func forgetInstallAttempts(version string) {
	failedMu.Lock()
	defer failedMu.Unlock()
	delete(failedAttempts, version)
}

// BinaryPath returns the path to the currently running yesmem binary.
//
// After "make deploy" replaces the inode under the live path, /proc/self/exe
// reads "…/yesmem (deleted)". os.Executable already trims that suffix itself
// (Go 1.25, os/executable_procfs.go), so the path returned here points at the
// new binary and needs no guard of its own.
func BinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		home, _ := os.UserHomeDir()
		return home + "/.local/bin/yesmem"
	}
	return exe
}

// RunUpdate checks for an update, downloads it, and replaces the binary.
// Returns the new version string if updated, or empty string if already current.
func RunUpdate(currentVersion string, logger *log.Logger) (string, error) {
	return runUpdateTo(BinaryPath(), currentVersion, logger)
}

// runUpdateTo is RunUpdate for a given destination binary. Split from the
// exported entry point so a test can drive the whole download/replace/migrate
// path against a temp file instead of the running binary.
//
// On error the caller retries at check_interval (at least one hour). A version
// whose install failed maxInstallAttemptsPerVersion times in this process is not
// attempted again until the daemon restarts or a newer release appears: the
// attempt costs a ~120 MiB download, the migration runs again, and a failing
// migration rolls the binary back every cycle. The skip is deliberately
// in-memory — a restart is the operator's "try again" and needs no state file to
// clean up.
func runUpdateTo(dest, currentVersion string, logger *log.Logger) (string, error) {
	if logger == nil {
		logger = log.Default()
	}

	logger.Println("[update] checking for updates...")
	info, err := checkForUpdate(currentVersion)
	if err != nil {
		return "", fmt.Errorf("check failed: %w", err)
	}
	if !info.Available {
		logger.Println("[update] already up to date")
		return "", nil
	}

	if installAttempts(info.Version, false) >= maxInstallAttemptsPerVersion {
		logger.Printf("[update] skipping %q: %d install attempts failed in this process; retrying after a restart or a newer release", info.Version, maxInstallAttemptsPerVersion)
		return "", nil
	}

	logger.Printf("[update] new version available: %q (current: %q)", info.Version, currentVersion)

	if info.BinaryURL == "" {
		return "", fmt.Errorf("no binary for %s/%s in release %q", runtime.GOOS, runtime.GOARCH, info.Version)
	}
	if info.ChecksumURL == "" {
		return "", fmt.Errorf("no checksums.txt in release %q", info.Version)
	}

	v := strings.TrimPrefix(info.Version, "v")
	asset := assetName(v, runtime.GOOS, runtime.GOARCH)
	logger.Printf("[update] downloading %q → %s", asset, dest)

	// One lock for download, replace and migration. The migration runs the new
	// binary, so a second updater must not be able to swap dest underneath it.
	unlock, err := lockUpdate(dest)
	if err != nil {
		return "", err
	}
	defer unlock()

	if err := downloadAndReplace(info.BinaryURL, info.ChecksumURL, asset, dest); err != nil {
		installAttempts(info.Version, true)
		return "", fmt.Errorf("download failed: %w", err)
	}

	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".claude", "yesmem", "config.yaml")
	if err := postUpdateInstall(dest, cfgPath, logger); err != nil {
		installAttempts(info.Version, true)
		return "", err
	}

	// Only now: the migration above can still roll the install back, and a
	// "successfully" in the log before that is a lie the daemon keeps repeating.
	logger.Printf("[update] installed %q successfully", info.Version)

	return info.Version, nil
}

// postUpdateInstall runs the steps that need the new binary. It belongs inside
// the update lock: runMigrate executes dest, and only .bak can undo a build that
// fails there.
//
// The rollback restores the binary, not the database: `yesmem migrate` may have
// applied part of the new schema before it failed. The production migrations are
// additive, so the restored build still reads that schema; a real downgrade
// would need its own migration and is not attempted here.
func postUpdateInstall(dest, cfgPath string, logger *log.Logger) error {
	logger.Println("[update] running post-update migration...")
	if err := runMigrate(dest); err != nil {
		if restoreErr := restoreBackup(dest); restoreErr != nil {
			return fmt.Errorf("migration failed (%v) and the previous binary could not be restored: %w", err, restoreErr)
		}
		logger.Printf("[update] rolled the binary back; the database may already be migrated (migrations are additive): %v", err)
		return fmt.Errorf("migration failed, restored the binary backup: %w", err)
	}

	if n, err := config.MigrateConfig(cfgPath); err != nil {
		logger.Printf("[update] config migration warning: %v", err)
	} else if n > 0 {
		logger.Printf("[update] config migration: added %d new field(s) to config.yaml", n)
	}

	return nil
}

func runMigrate(binaryPath string) error {
	// Above the storage layer's SQLite busy_timeout (30s): a migration that only
	// waits for a lock must not be killed, because a killed migration now rolls
	// the binary back.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "migrate")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func restartArgs() []string {
	return []string{"restart"}
}

// RunRestart executes "yesmem restart" to restart daemon and proxy.
func RunRestart(logger *log.Logger) error {
	return runRestartTo(BinaryPath(), logger)
}

// runRestartTo is RunRestart for a given binary. Split from the exported entry
// point for the same reason as runUpdateTo.
func runRestartTo(binary string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	// The restart reaches into the same directory as the updater (binary, .bak,
	// temp names), so it must not interleave with a running update.
	unlock, err := lockUpdate(binary)
	if err != nil {
		return err
	}
	defer unlock()

	logger.Println("[update] restarting daemon + proxy...")
	return execRestart(binary)
}

// execRestart runs the restart command. A package variable so a test can
// observe whether the restart runs under the update lock without restarting
// anything; the production value stays the real exec.
var execRestart = runRestartCommand

func runRestartCommand(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, restartArgs()...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ParseCheckInterval parses a duration string like "6h", "12h".
// Returns 6h as default on empty or invalid input.
func ParseCheckInterval(s string) time.Duration {
	if s == "" {
		return 6 * time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 1*time.Hour {
		return 6 * time.Hour
	}
	return d
}
