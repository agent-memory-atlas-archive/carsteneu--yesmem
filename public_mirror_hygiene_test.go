package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// blockedMirrorPatterns are the literals the pre-sync security scan refuses to
// publish. That scan only runs when somebody syncs, which on 16.10.2026 let 55
// test files written during the YM campaign carry a local path for twelve days
// before the next sync noticed. This test applies the same rule at test time, so
// the failure lands in the loop that introduced it.
var blockedMirrorPatterns = []string{
	"/home/chief",
	"/home/carsten",
	"/Users/chief",
	"/Users/carsten",
	"sk-ant-api",
	"192.168.122.",
	"cinnamon-dev@",
	"enterprise@papoo",
	"carsten@ccm19.de",
	"carsten@example.com",
	"chief@chief",
}

// mirrorScanExtensions mirrors the file types the sync script scans. .ts, .tsx
// and .sql are not scanned there either.
var mirrorScanExtensions = map[string]bool{
	".go": true, ".md": true, ".yaml": true, ".yml": true,
	".json": true, ".jsonl": true, ".sh": true,
}

// mirrorSkipped reports whether the path never reaches the public mirror or is
// outside the scan. Duplicated from scripts/sync-public.sh on purpose: the guard
// must not depend on running the sync. A path missing here only makes the guard
// noisier, never blinder, because the scan itself still runs before a sync.
func mirrorSkipped(rel string) bool {
	skipDirs := []string{
		".git/", ".worktrees/", ".yesmem/", ".claude/", ".codex/",
		"yesdocs/", "research/", "docs/", "archiv/", "logs/",
		"gopath/", "go-path/", "go-cache/", ".cache/", "node_modules/",
		"plugins/opencode-sidebar/", "cmd/embedtest/",
	}
	for _, dir := range skipDirs {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	skipFiles := []string{
		"RULES.md",
		"CLAUDE.md",
		"scripts/sync-public.sh",
		"scripts/generate-changelog.sh",
		"scripts/extract_skills.ts",
	}
	for _, file := range skipFiles {
		if rel == file {
			return true
		}
	}
	return false
}

func readScanAllowlist(t *testing.T, root string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "scripts", "scan-allowlist.txt"))
	if err != nil {
		t.Fatalf("read allowlist: %v", err)
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			allowed[line] = true
		}
	}
	return allowed
}

func mirrorRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no go.mod found above the test working directory")
		}
		dir = parent
	}
}

func TestNoMirrorBlockedLiteralOutsideTheScanAllowlist(t *testing.T) {
	root := mirrorRepoRoot(t)
	allowed := readScanAllowlist(t, root)

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}

	var violations []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || allowed[rel] || mirrorSkipped(rel) {
			continue
		}
		if !mirrorScanExtensions[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		for _, pattern := range blockedMirrorPatterns {
			i := strings.Index(text, pattern)
			if i < 0 {
				continue
			}
			violations = append(violations, fmt.Sprintf("%s:%d contains %q", rel, 1+strings.Count(text[:i], "\n"), pattern))
			break
		}
	}

	if len(violations) > 0 {
		t.Fatalf("%d tracked file(s) carry a literal the public mirror scan blocks:\n  %s\n\nUse a generic path such as /home/tester, or build a key-shaped string from parts. A fixture that genuinely needs the literal belongs in scripts/scan-allowlist.txt.",
			len(violations), strings.Join(violations, "\n  "))
	}
}
