package briefing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-338 (Los 24d): a new key in DefaultStrings reaches an existing installation
// only through a setup run. LoadStrings fills a missing key with the English
// default, so a German briefing silently mixes languages. The daemon backfills
// the missing keys at startup, translated into the language the file already
// uses.
func TestMissingStringKeysReportsOnlyAbsentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strings.yaml")
	if err := os.WriteFile(path, []byte("greeting: Hallo\ntool_hint: Nutze diese Werkzeuge.\n"), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}

	missing, err := MissingStringKeys(path)
	if err != nil {
		t.Fatalf("MissingStringKeys: %v", err)
	}
	for _, k := range missing {
		if k == "greeting" || k == "tool_hint" {
			t.Errorf("%q is present in the file but reported missing", k)
		}
	}
	if !containsKey(missing, "metamemory_header") || !containsKey(missing, "open_work") {
		t.Errorf("missing keys do not cover the file's gaps: %v", missing)
	}
}

// A missing file belongs to setup, not to the daemon: it would have to invent a
// language first. No file, no backfill.
func TestMissingStringKeysIgnoresAMissingFile(t *testing.T) {
	missing, err := MissingStringKeys(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("MissingStringKeys: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("a missing file reported %d missing keys, want none", len(missing))
	}
}

func TestMissingStringsPromptCarriesTheLanguageSampleAndTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strings.yaml")
	if err := os.WriteFile(path, []byte("greeting: Hallo\ntool_hint: Nutze diese Werkzeuge.\n"), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}

	prompt, err := BuildMissingStringsPrompt(path, []string{"metamemory_header"})
	if err != nil {
		t.Fatalf("BuildMissingStringsPrompt: %v", err)
	}
	if !strings.Contains(prompt, "Hallo") || !strings.Contains(prompt, "Nutze diese Werkzeuge") {
		t.Errorf("prompt does not carry the existing values as the language sample:\n%s", prompt)
	}
	if !strings.Contains(prompt, "metamemory_header") {
		t.Error("prompt does not name the key to translate")
	}
	if !strings.Contains(prompt, "My knowledge quality here:") {
		t.Error("prompt does not carry the English default of the missing key")
	}
}

// The merge must not rewrite what the installation already has: a wrong
// translation of an existing key would slip in with every backfill.
func TestMergeStringKeysKeepsExistingValuesAndAddsOnlyTheMissingOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strings.yaml")
	original := "greeting: Hallo\n# a comment the user wrote\ntool_hint: Nutze diese Werkzeuge.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}

	response := "metamemory_header: \"Meine Wissensqualitaet hier:\"\ngreeting: HIJACKED\nnot_a_key: ignore me\n"
	merged, err := MergeStringKeys(path, response, []string{"metamemory_header"})
	if err != nil {
		t.Fatalf("MergeStringKeys: %v", err)
	}
	if merged != 1 {
		t.Fatalf("merged %d keys, want 1", merged)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read strings: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "greeting: Hallo") {
		t.Errorf("existing value was overwritten:\n%s", text)
	}
	if strings.Contains(text, "HIJACKED") {
		t.Errorf("a key that was not reported missing was overwritten:\n%s", text)
	}
	if strings.Contains(text, "not_a_key") {
		t.Errorf("an unrequested key was written:\n%s", text)
	}
	if !strings.Contains(text, "Meine Wissensqualitaet hier:") {
		t.Errorf("the translated value is missing:\n%s", text)
	}
	if !strings.Contains(text, "# a comment the user wrote") {
		t.Errorf("the backfill dropped a user comment:\n%s", text)
	}
}

// A backfill runs over a file the installation already restricted (or that the
// user did); it must not widen the mode. A key the model left without a value
// must stay missing instead of becoming `key: null`, which would count as
// present and hide the English default forever.
func TestMergeStringKeysKeepsTheFileModeAndSkipsEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strings.yaml")
	if err := os.WriteFile(path, []byte("greeting: Hallo\n"), 0o600); err != nil {
		t.Fatalf("write strings: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod strings: %v", err)
	}

	response := "open_work: \"Was noch offen ist:\"\nmetamemory_header:\n"
	merged, err := MergeStringKeys(path, response, []string{"open_work", "metamemory_header"})
	if err != nil {
		t.Fatalf("MergeStringKeys: %v", err)
	}
	if merged != 1 {
		t.Fatalf("merged %d keys, want 1 (the valueless key is skipped)", merged)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat strings: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("strings.yaml mode = %04o after the backfill, want 0600", mode)
	}
	missing, err := MissingStringKeys(path)
	if err != nil {
		t.Fatalf("MissingStringKeys: %v", err)
	}
	if !containsKey(missing, "metamemory_header") {
		t.Error("a valueless key was written and now counts as present")
	}
}

// A model that answers with the wrong shape must not reach the file: LoadStrings
// rejects the whole document on one type error and silently falls back to English,
// and MissingStringKeys would then see every key as present.
func TestMergeStringKeysRejectsAValueOfTheWrongShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strings.yaml")
	original := "greeting: Hallo\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}

	response := "tool_hint:\n  - Werkzeug A\n"
	if _, err := MergeStringKeys(path, response, []string{"tool_hint"}); err == nil {
		t.Fatal("a list where a string belongs was accepted")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read strings: %v", err)
	}
	if string(data) != original {
		t.Errorf("the file was changed although the translation did not fit:\n%s", data)
	}
}

// SaveStrings follows a symlink (os.WriteFile); the atomic backfill has to do the
// same, or one of the two writers silently replaces the link with a regular file.
func TestMergeStringKeysKeepsASymlinkedFileASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dot", "strings.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("greeting: Hallo\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "strings.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := MergeStringKeys(link, "open_work: \"Was noch offen ist:\"\n", []string{"open_work"}); err != nil {
		t.Fatalf("MergeStringKeys: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the backfill replaced the symlink with a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), "Was noch offen ist:") {
		t.Errorf("the link target was not updated:\n%s", data)
	}
}

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
