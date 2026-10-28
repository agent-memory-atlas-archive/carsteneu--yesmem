package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/skills"
)

// Same bug class as Los 18h (YM-363): the announcement and the deletion have to
// name the same targets. Uninstall removes every bundled command and every
// bundled skill directory it installed, but the preview named neither, so the
// user could not see that ~/.claude/commands and ~/.claude/skills get swept.
// The expectation is read from the embedded bundle, independently of the
// implementation, so it cannot follow a wrong list silently.
func TestUninstallPreviewNamesBundledCommandsAndSkillsItRemoves(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	commandsDir := filepath.Join(home, ".claude", "commands")
	skillsDir := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatal(err)
	}

	commandEntries, err := skills.BundledCommands.ReadDir(".")
	if err != nil {
		t.Fatalf("read bundled commands: %v", err)
	}
	var commandNames []string
	for _, e := range commandEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		commandNames = append(commandNames, e.Name())
		if err := os.WriteFile(filepath.Join(commandsDir, e.Name()), []byte("// command"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	skillEntries, err := skills.BundledSkills.ReadDir("bundled-skills")
	if err != nil {
		t.Fatalf("read bundled skills: %v", err)
	}
	var skillNames []string
	for _, e := range skillEntries {
		if !e.IsDir() {
			continue
		}
		skillNames = append(skillNames, e.Name())
		dir := filepath.Join(skillsDir, e.Name())
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		// The fixture has to hold what InstallBundledSkills writes, byte for byte:
		// the sweep removes a same-named directory only when it is the bundled
		// copy, so placeholder content would model a user's own skill instead
		// (YM-350).
		files, err := skills.BundledSkills.ReadDir(filepath.Join("bundled-skills", e.Name()))
		if err != nil {
			t.Fatalf("read bundle for %q: %v", e.Name(), err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			data, err := skills.BundledSkills.ReadFile(filepath.Join("bundled-skills", e.Name(), f.Name()))
			if err != nil {
				t.Fatalf("read bundled file %q: %v", f.Name(), err)
			}
			if err := os.WriteFile(filepath.Join(dir, f.Name()), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(commandNames) == 0 || len(skillNames) == 0 {
		t.Fatal("the embedded bundle is empty, the test cannot pin anything")
	}

	removeBundledCommands(home)
	removeBundledSkills(home)

	preview := strings.Join(uninstallPreviewLines(), "\n")
	for _, name := range commandNames {
		if _, err := os.Lstat(filepath.Join(commandsDir, name)); !os.IsNotExist(err) {
			t.Errorf("command %s survived the uninstall: lstat err=%v", name, err)
		}
		if !strings.Contains(preview, name) {
			t.Errorf("the uninstall announcement does not name the removed command %s:\n%s", name, preview)
		}
	}
	for _, name := range skillNames {
		if _, err := os.Lstat(filepath.Join(skillsDir, name)); !os.IsNotExist(err) {
			t.Errorf("skill %s survived the uninstall: lstat err=%v", name, err)
		}
		if !strings.Contains(preview, name) {
			t.Errorf("the uninstall announcement does not name the removed skill %s:\n%s", name, preview)
		}
	}
	// A bare file name is weak evidence: the user has to be able to tell which
	// directory the sweep reaches.
	for _, dir := range []string{".claude/commands", ".claude/skills"} {
		if !strings.Contains(preview, dir) {
			t.Errorf("the uninstall announcement does not name the swept directory %s:\n%s", dir, preview)
		}
	}
}

// The sweep hands bundle entry names to os.Remove and os.RemoveAll, so a name
// that is not a plain base name has to be refused before it reaches the target
// path (Los 18h lesson: the list is the authority for paths).
func TestLos18iBundledSweepIgnoresEntryNamesWithAPath(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b.md", "../commands-neighbour.md", "..\\b.md"} {
		if isPlainEntryName(name) {
			t.Errorf("entry name %q was accepted as a sweep target", name)
		}
	}
	for _, name := range []string{"swarm.md", "risk-based-code-review"} {
		if !isPlainEntryName(name) {
			t.Errorf("entry name %q was refused", name)
		}
	}
}
