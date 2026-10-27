package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/skills"
)

// YM-350 [PLG-F30] Teil 1 (Los 56i): removeBundledSkills loeschte jedes
// Verzeichnis unter ~/.claude/skills, das so heisst wie ein gebuendeltes Skill.
// Das Verzeichnis gehoert dem Nutzer mit; ein eigenes Skill gleichen Namens war
// beim Uninstall weg. Entfernt wird nur noch, was inhaltlich das Bundle ist.
func TestLos56i_RemoveBundledSkillsKeepsAForeignSkillOfTheSameName(t *testing.T) {
	home := t.TempDir()
	names := bundledSkillNames()
	if len(names) == 0 {
		t.Fatal("the bundle names no skill")
	}
	name := names[0]
	dir := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(foreign, []byte("# my own skill, unrelated to yesmem\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := removeBundledSkills(home); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the sweep deleted a user's own skill named %q: %v", name, err)
	}
}

// Das Gegenstueck: ein Verzeichnis, das genau dem Bundle entspricht, ist die
// Installation und wird entfernt.
func TestLos56i_RemoveBundledSkillsRemovesTheInstalledCopy(t *testing.T) {
	home := t.TempDir()
	names := bundledSkillNames()
	if len(names) == 0 {
		t.Fatal("the bundle names no skill")
	}
	name := names[0]
	dir := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	entries, err := skills.BundledSkills.ReadDir(filepath.Join("bundled-skills", name))
	if err != nil || len(entries) == 0 {
		t.Fatalf("read bundle for %q: %v", name, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := skills.BundledSkills.ReadFile(filepath.Join("bundled-skills", name, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := removeBundledSkills(home); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the installed copy of %q survived the sweep (err=%v)", name, err)
	}
}

// YM-350 Teil 3: cleanMCPJSON loeschte .mcp.json komplett, sobald der letzte
// MCP-Server der yesmem-Eintrag war. Die Datei ist geteilt und kann weitere
// Top-Level-Schluessel tragen; die duerfen nicht mitverschwinden.
func TestLos56i_CleanMCPJSONKeepsForeignTopLevelKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".mcp.json")
	body := `{
  "mcpServers": {
    "yesmem": {"command": "yesmem", "args": ["mcp"]}
  },
  "projects": {
    "/home/user/work": {"enabledMcpjsonServers": ["yesmem"]}
  }
}`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	cleanMCPJSON(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf(".mcp.json is gone although it still carries a foreign key: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the cleaned file: %v", err)
	}
	if servers, ok := doc["mcpServers"].(map[string]any); ok {
		if _, has := servers["yesmem"]; has {
			t.Errorf("the yesmem server entry survived: %v", servers)
		}
	}
	if _, ok := doc["projects"]; !ok {
		t.Errorf("the foreign top-level key was dropped:\n%s", data)
	}
}

// Eine Datei, in der yesmem der einzige Inhalt ist, bleibt nicht als leeres
// Gerippe stehen.
func TestLos56i_CleanMCPJSONRemovesAnEmptyFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"yesmem": {"command": "yesmem"}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cleanMCPJSON(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an empty .mcp.json survived (err=%v)", err)
	}
}

func TestLos56i_NoPathDiscardsTheHomeError(t *testing.T) {
	for _, path := range []string{"setup.go", "status.go", "uninstall.go", "../../cmd_update.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(source), ", _ := os.UserHomeDir()") {
			t.Errorf("%s still discards the os.UserHomeDir error (`, _ := os.UserHomeDir()`): a missing HOME must stop the run instead of writing into the working directory", path)
		}
	}
}

// Cold-Review-Befund (Los 56i): der Sweep verschluckte jeden Fehler, also war
// "Verzeichnis fehlt" (Normalfall) nicht von "Verzeichnis nicht lesbar"
// unterscheidbar, und der Uninstall meldete Erfolg. removeBundledOpencodeCommands
// sammelt Fehler seit 18j; der Skill-Sweep zieht nach.
func TestLos56i_RemoveBundledSkillsReportsAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	home := t.TempDir()
	names := bundledSkillNames()
	if len(names) == 0 {
		t.Fatal("the bundle names no skill")
	}
	dir := filepath.Join(home, ".claude", "skills", names[0])
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	if _, err := removeBundledSkills(home); err == nil {
		t.Error("an unreadable skill directory was reported as a clean sweep")
	}
}
