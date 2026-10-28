package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// YM-398 (PRX-P50): the thread id comes from a client-controlled header
// (anthropicThreadID), and the status file name was built from it verbatim.
// filepath.Join cleans "..", so a header value like "x/../../../outside" wrote
// the JSON outside the cache-status directory. The id is not a path: the file
// name is an encoding of it, and writer and reader have to agree on that
// encoding.

// hostileThreadIDs are the shapes a client can send. None of them may leave the
// cache-status directory.
var hostileThreadIDs = []string{
	"x/../../../outside",
	"../../../../../../tmp/pwn",
	"opencode:ses_../../../../outside",
	"/absolute/outside",
	"..",
	".",
	"a/..",
	"dir/sub/thread",
	strings.Repeat("a", 300),
}

// The real write path is the assertion: a hostile id must not create or
// overwrite a file outside <dataDir>/cache-status.
func TestLos56bCacheStatusWriteStaysInsideItsDirectory(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	statusDir := filepath.Join(dataDir, "cache-status")
	if err := os.MkdirAll(statusDir, 0755); err != nil {
		t.Fatalf("mkdir status dir: %v", err)
	}

	w := NewCacheStatusWriter(dataDir, "ephemeral", 250000, 100000)
	t.Cleanup(w.Stop)

	for _, tid := range hostileThreadIDs {
		w.Update(time.Now(), 1000, 0, 0, tid)
	}
	w.writeStatus()

	var escaped []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if rel, relErr := filepath.Rel(statusDir, path); relErr != nil || strings.HasPrefix(rel, "..") {
			escaped = append(escaped, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(escaped) > 0 {
		t.Fatalf("status writes escaped %s:\n%s", statusDir, strings.Join(escaped, "\n"))
	}

	// Every id still gets its own status file inside the directory.
	entries, err := os.ReadDir(statusDir)
	if err != nil {
		t.Fatalf("read status dir: %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, tid := range hostileThreadIDs {
		name := filepath.Base(CacheStatusPath(dataDir, tid))
		if !names[name] {
			t.Errorf("thread id %q: writer produced no %s in the status dir", tid, name)
		}
	}
}

// The reader has to resolve the same name the writer used, otherwise the
// statusline reads a file nobody writes.
func TestLos56bCacheStatusPathStaysInsideItsDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	statusDir := filepath.Join(dataDir, "cache-status")
	for _, tid := range hostileThreadIDs {
		got := CacheStatusPath(dataDir, tid)
		if dir := filepath.Dir(got); dir != statusDir {
			t.Errorf("thread id %q: CacheStatusPath = %q, directory %q, want %q", tid, got, dir, statusDir)
		}
	}
}

// A thread id the proxy itself produces (opencode and Claude session ids)
// keeps its file name: an existing status-<id>.json must stay readable, and the
// directory layout must not change for normal traffic.
func TestLos56bCacheStatusPathKeepsValidThreadIDs(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	for _, tid := range []string{
		"opencode:ses_abc123",
		"ses_ea4ba843bffeoO7WTClMEwPAj4",
		"0f9a2c1d-7b3e-4f5a-9c8d-1e2f3a4b5c6d",
		"thread-1",
	} {
		want := filepath.Join(dataDir, "cache-status", "status-"+tid+".json")
		if got := CacheStatusPath(dataDir, tid); got != want {
			t.Errorf("valid thread id %q: CacheStatusPath = %q, want the unchanged %q", tid, got, want)
		}
	}
}

// Distinct ids keep distinct files: the encoding must not fold two thread ids
// onto one status file, because the display would then show another session's
// token counts.
func TestLos56bCacheStatusPathKeepsHostileIDsDistinct(t *testing.T) {
	dataDir := t.TempDir()
	seen := make(map[string]string, len(hostileThreadIDs))
	for _, tid := range hostileThreadIDs {
		name := filepath.Base(CacheStatusPath(dataDir, tid))
		if other, dup := seen[name]; dup {
			t.Errorf("thread ids %q and %q share the status file %q", other, tid, name)
		}
		seen[name] = tid
	}
}
