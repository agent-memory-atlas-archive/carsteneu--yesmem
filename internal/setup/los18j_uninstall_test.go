package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/skills"
)

// InstallOpencodeCommands (setup.go) writes the bundled opencode commands into
// ~/.config/opencode/commands; uninstall had no counterpart, so the files stayed
// behind and the announcement did not name them (YM-384). The expectation is
// read from the embedded bundle, independently of the implementation.
func TestLos18jUninstallRemovesBundledOpencodeCommands(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cmdsDir := filepath.Join(home, ".config", "opencode", "commands")
	if err := os.MkdirAll(cmdsDir, 0755); err != nil {
		t.Fatal(err)
	}

	entries, err := skills.BundledOpencodeCommands.ReadDir("bundled-commands/opencode")
	if err != nil {
		t.Fatalf("read bundled opencode commands: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		if err := os.WriteFile(filepath.Join(cmdsDir, e.Name()), []byte("// command"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if len(names) == 0 {
		t.Fatal("the embedded opencode command bundle is empty, the test cannot pin anything")
	}
	// The directory is shared with the user: a command yesmem never wrote stays.
	userCmd := filepath.Join(cmdsDir, "my-own-command.md")
	if err := os.WriteFile(userCmd, []byte("// mine"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := removeBundledOpencodeCommands(home); err != nil {
		t.Fatalf("removeBundledOpencodeCommands: %v", err)
	}

	preview := strings.Join(uninstallPreviewLines(), "\n")
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(cmdsDir, name)); !os.IsNotExist(err) {
			t.Errorf("opencode command %s survived the uninstall: lstat err=%v", name, err)
		}
		if !strings.Contains(preview, name) {
			t.Errorf("the uninstall announcement does not name the removed opencode command %s:\n%s", name, preview)
		}
	}
	if _, err := os.Stat(userCmd); err != nil {
		t.Errorf("a user command in the shared directory was removed: %v", err)
	}
	// A bare file name is weak evidence: the user has to be able to tell which
	// directory the sweep reaches.
	if !strings.Contains(preview, ".config/opencode/commands") {
		t.Errorf("the uninstall announcement does not name the swept directory .config/opencode/commands:\n%s", preview)
	}
}

// A commands directory that is itself a symlink would carry the unlink into the
// directory it points at, so the sweep refuses it (cold and security review,
// Los 18j). The neighbouring plugin installer refuses the same shape.
func TestLos18jOpencodeCommandSweepRefusesASymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	foreign := filepath.Join(root, "elsewhere")
	cmdsDir := filepath.Join(home, opencodeCommandsRelDir)
	if err := os.MkdirAll(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, cmdsDir); err != nil {
		t.Fatal(err)
	}

	entries, err := skills.BundledOpencodeCommands.ReadDir("bundled-commands/opencode")
	if err != nil {
		t.Fatalf("read bundled opencode commands: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		data := []byte("P RECIOUS")
		if err := os.WriteFile(filepath.Join(foreign, e.Name()), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if len(names) == 0 {
		t.Fatal("the embedded opencode command bundle is empty, the test cannot pin anything")
	}

	if err := removeBundledOpencodeCommands(home); err == nil {
		t.Error("the sweep accepted a symlinked commands directory")
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(foreign, name)); err != nil {
			t.Errorf("the sweep deleted %s behind the symlink: %v", name, err)
		}
	}
}
