package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// los57dFence matches a CommonMark code fence: an optional indent, at least three
// backticks, then an optional info string.
var los57dFence = regexp.MustCompile("^(\\s*)(`{3,})(.*)$")

// YM-405 (Los 57d): to show a fence verbatim inside a sample, the outer fence has
// to be longer than the one it contains. docs/CapFeatures.md wrapped the CAP.md
// example in three backticks and showed a ```javascript fence inside it, so the
// sample broke apart at the next bare fence and the rest of the page rendered
// outside it (odd fence count). This check enforces the convention: an inner
// fence line that carries an info string must be strictly shorter than its
// container. CommonMark itself would treat such a line as content, which is
// exactly why the sample silently lost its shape instead of failing loudly.
func TestDocsFencesNestCorrectly_Los57d(t *testing.T) {
	var files []string
	err := filepath.WalkDir("docs", func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no markdown files found under docs/, so this test proves nothing")
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		openLen, openLine := 0, 0
		for i, line := range strings.Split(string(data), "\n") {
			m := los57dFence.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			run, info := len(m[2]), strings.TrimSpace(m[3])
			if openLen == 0 {
				openLen, openLine = run, i+1
				continue
			}
			if run >= openLen && info == "" {
				openLen = 0
				continue
			}
			if run >= openLen {
				t.Errorf("%s:%d: fence of length %d with info string %q inside the block opened at :%d with length %d; the outer fence must be longer",
					path, i+1, run, info, openLine, openLen)
			}
		}
		if openLen != 0 {
			t.Errorf("%s: file ends inside the fence opened at :%d", path, openLine)
		}
	}
}
