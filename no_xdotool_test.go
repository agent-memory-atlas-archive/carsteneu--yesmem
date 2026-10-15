package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// User rule: the product never drives the desktop through xdotool (no focus
// stealing, no typing into windows). Guards every shipped source file in the
// repo plus the skill and cap instructions that tell agents what to run;
// tests, history (CHANGELOG) and design notes may mention it.
func TestProductDoesNotUseXdotool(t *testing.T) {
	skipDirs := map[string]bool{".git": true, "node_modules": true, ".yesmem": true, ".worktrees": true, "vendor": true, "yesdocs": true, "docs": true}
	exts := map[string]bool{".go": true, ".ts": true, ".tsx": true, ".js": true, ".sh": true, ".py": true}
	// Markdown counts only where it is executable guidance for agents.
	instructionRoots := []string{"skills" + string(filepath.Separator), "caps" + string(filepath.Separator)}
	var hits []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.Contains(name, "_test.") {
			return nil
		}
		ext := filepath.Ext(name)
		guarded := exts[ext]
		if ext == ".md" {
			for _, root := range instructionRoots {
				if strings.HasPrefix(path, root) {
					guarded = true
				}
			}
		}
		if !guarded {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(data)), "xdotool") {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("%s mentions xdotool; the product must not drive windows via xdotool", h)
	}
}
