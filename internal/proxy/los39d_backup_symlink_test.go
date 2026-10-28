package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PRX-P45 (3): writeFileAtomic resolves a symlink at its destination, which is
// right for the user's config but wrong for the backup yesmem writes beside it.
// A link at the backup path sent the config copy over an unrelated file. The
// backup is now written without resolving: the rename replaces the link, the
// link target keeps its content, and the patch still runs.
func TestLos39dMaybePatchReplacesSymlinkedBackupPath(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "options": {}
    }
  }
}`, los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	victim := filepath.Join(home, "unrelated.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	backupPath := path + autoconfBackupSuffix
	if err := os.Symlink(victim, backupPath); err != nil {
		t.Fatalf("symlink backup path: %v", err)
	}

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	if patched, patchErr := maybePatchOpenCodeBaseURL(path, active); patchErr != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v, want the patch to run and replace the link", patched, patchErr)
	}

	got, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("read victim: %v", readErr)
	}
	if string(got) != "keep me" {
		t.Errorf("the config was written through the backup symlink: %q", got)
	}
	info, lstatErr := os.Lstat(backupPath)
	if lstatErr != nil {
		t.Fatalf("lstat backup: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the backup path is still a symlink, the link was not replaced")
	}
	backup, readErr := os.ReadFile(backupPath)
	if readErr != nil {
		t.Fatalf("read backup: %v", readErr)
	}
	if string(backup) != string(before) {
		t.Errorf("backup is not the pre-patch content:\n%s", backup)
	}
	cfg, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if !strings.Contains(string(cfg), `"baseURL": "http://localhost:9099/v1"`) {
		t.Errorf("config was not patched:\n%s", cfg)
	}
}

// A directory at the backup path cannot be replaced by a rename, so the patch
// has to abort with a clear error instead of writing the config without a
// restore point.
func TestLos39dMaybePatchRejectsDirectoryAtBackupPath(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "options": {}
    }
  }
}`, los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(path+autoconfBackupSuffix, 0o700); err != nil {
		t.Fatalf("mkdir backup path: %v", err)
	}

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	patched, err := maybePatchOpenCodeBaseURL(path, active)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("patched=%d err=%v, want the directory refusal", patched, err)
	}
	cfg, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if strings.Contains(string(cfg), `"baseURL"`) {
		t.Errorf("config was patched without a backup:\n%s", cfg)
	}
}
