package setup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/carsteneu/yesmem/internal/update"
)

const DefaultExtractionModel = "sonnet"

// detectUserTypeDefault picks a sensible default for the user-type prompt.
// An explicit ANTHROPIC_API_KEY env beats everything else; otherwise CLI
// (which also covers Claude Code subscription users with oauthAccount).
func detectUserTypeDefault(home, envKey string) string {
	if envKey != "" {
		return "api"
	}
	return "cli"
}

// permanentDirs lists directories that are considered permanent (not Downloads, tmp, etc.)
var permanentDirs = []string{
	"/usr/local/bin",
	"/usr/bin",
	".local/bin",
	"bin",
	"go/bin",
}

// ensurePermanentLocation checks if the binary is in a permanent location.
// If not, automatically copies it to ~/.local/bin (the standard user binary path).
func ensurePermanentLocation(home, binaryPath string) string {
	absPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return binaryPath
	}

	// Resolve symlinks
	resolved, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		absPath = resolved
	}

	if isPermanentLocation(home, absPath) {
		fmt.Printf("  ✓ Binary location: %s\n", absPath)
		ensureInPath(home, filepath.Dir(absPath))
		return absPath
	}

	// Not in a permanent location — copy to ~/.local/bin automatically
	dest := filepath.Join(home, ".local", "bin", "yesmem")
	os.MkdirAll(filepath.Dir(dest), 0755)
	var copyErr error
	withSpinner("Installing to "+dest, func() (string, error) {
		// No busy-binary workaround here any more: copyBinary installs by rename
		// and never opens the running inode for writing, so ETXTBSY cannot occur.
		// The old branch stopped both services and killed every yesmem process on
		// the host to get past it (YM-313).
		if err := copyBinary(absPath, dest); err != nil {
			copyErr = err
			return "", fmt.Errorf("%v — keeping at %s", err, absPath)
		}
		return "", nil
	})
	if copyErr != nil {
		ensureInPath(home, filepath.Dir(absPath))
		return absPath
	}
	// A running daemon and proxy keep the inode they mapped. The in-place copy this
	// replaces failed with ETXTBSY, the setup then stopped both services to get past
	// it, and step 11's `systemctl --user start` brought the new binary up. A rename
	// succeeds silently, so the switch has to be asked for here, or setup would
	// leave the old binary executing next to a new file on disk.
	restartInstalledServices()
	ensureInPath(home, filepath.Dir(dest))
	return dest
}

// restartInstalledServices lets an already-running installation pick up the
// binary copyBinary just installed. try-restart restarts a running unit and does
// nothing for an inactive one, which step 11 starts the normal way; a host
// without systemd is left alone (macOS uses launchd, started in step 11).
func restartInstalledServices() {
	if runtime.GOOS != "linux" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), systemdReloadTimeout)
	defer cancel()
	if err := systemctlRun(ctx, "--user", "try-restart", "yesmem.service", "yesmem-proxy.service"); err != nil {
		log.Printf("setup: restarting the installed services: %v (a running daemon keeps the previous binary until its next restart)", err)
	}
}

func isPermanentLocation(home, path string) bool {
	lowerPath := strings.ToLower(path)

	// Definitely not permanent
	notPermanent := []string{"download", "tmp", "temp", "desktop", "/tmp/"}
	for _, np := range notPermanent {
		if strings.Contains(lowerPath, np) {
			return false
		}
	}

	// Known permanent locations
	for _, dir := range permanentDirs {
		var fullDir string
		if filepath.IsAbs(dir) {
			fullDir = dir
		} else {
			fullDir = filepath.Join(home, dir)
		}
		if strings.HasPrefix(path, fullDir) {
			return true
		}
	}

	// If it's somewhere in home but not Downloads/tmp, it's probably fine
	if strings.HasPrefix(path, home) {
		return true
	}

	return true // Anywhere else, assume the user knows what they're doing
}

// copyBinary installs src as dst. It takes the same update lock as `yesmem
// update` and `make install`, and it replaces the file by rename instead of
// writing through the target: a running yesmem has the installed inode mapped,
// so an in-place copy fails with ETXTBSY mid-write (or leaves a truncated
// binary behind), and a parallel update would replace a path this copy is still
// writing. There is no busy-file workaround any more, because a rename never
// touches the running inode.
func copyBinary(src, dst string) error {
	unlock, err := update.LockBinaryUpdate(dst)
	if err != nil {
		return err
	}
	defer unlock()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// The temp name has to sit in the target directory: rename across filesystems
	// would copy again. os.CreateTemp reserves the name, so a leftover from an
	// aborted run cannot block the install (the .bak.tmp lesson of YM-301).
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".yesmem-copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	// cp keeps an existing target's mode; the installed binary must be 0755
	// whatever the umask and whatever the file was before.
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func ensureInPath(home, dir string) {
	path := os.Getenv("PATH")
	if strings.Contains(path, dir) {
		return
	}

	envLine := fmt.Sprintf("export PATH=\"%s:$PATH\"", dir)
	marker := dir

	for _, rcFile := range []string{".bashrc", ".zshrc", ".profile"} {
		rcPath := filepath.Join(home, rcFile)
		data, err := os.ReadFile(rcPath)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), marker) {
			continue
		}
		f, err := os.OpenFile(rcPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		fmt.Fprintf(f, "\n# YesMem binary\n%s\n", envLine)
		f.Close()
	}
	fmt.Printf("  Added %s to PATH in shell config ✓\n", dir)
}
