package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedDumpFiles writes n files matching the debug-dump name scheme into dir.
func seedDumpFiles(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("req_20200101-000000.%03d_ses-old_1_body.json", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

func dumpFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "req_*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	return matches
}

// Regression YM-3: reqIdx is the process-local request counter and starts at 0
// again after a proxy restart. Dumps named <kind>/req_<reqIdx>_body.json were
// silently overwritten by the next process lifetime.
func TestDebugDumps_DoNotOverwriteAcrossRestart(t *testing.T) {
	t.Setenv("YESMEM_PROXY_DEBUG", "1")
	dir := t.TempDir()

	// Same reqIdx twice = two process lifetimes writing request #7.
	writeDebugDump(dir, "opencode:ses_ebc14c08ffeX3thcK0w81q4OZ", dumpKindBody, 7, []byte(`{"lifetime":1}`))
	writeDebugDump(dir, "opencode:ses_ebc14c08ffeX3thcK0w81q4OZ", dumpKindBody, 7, []byte(`{"lifetime":2}`))

	files := dumpFilesIn(t, filepath.Join(dir, "debug"))
	if len(files) != 2 {
		t.Fatalf("expected 2 request dumps after two lifetimes, got %d: %v", len(files), files)
	}
}

// Regression YM-3: the forwarded-body dump wrote <dataDir>/logs/req_<idx>_body.json
// while the pre/post dumps went to <dataDir>/debug/. All dumps belong in one
// directory.
func TestDebugDumps_LiveInDebugDir(t *testing.T) {
	t.Setenv("YESMEM_PROXY_DEBUG", "1")
	dir := t.TempDir()

	writeDebugDump(dir, "sess-1759abc", dumpKindBody, 3, []byte(`{"hi":1}`))

	if files := dumpFilesIn(t, filepath.Join(dir, "debug")); len(files) != 1 {
		t.Fatalf("expected the dump in %s/debug, got %d files", dir, len(files))
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "req_3_body.json")); !os.IsNotExist(err) {
		t.Errorf("dump still written to logs/ (err=%v)", err)
	}
}

// Regression YM-3: dumps are full transcripts and were written world-readable.
func TestDebugDumps_ModeIs0600(t *testing.T) {
	t.Setenv("YESMEM_PROXY_DEBUG", "1")
	dir := t.TempDir()

	writeDebugDump(dir, "", dumpKindBody, 5, []byte(`{"hi":1}`))

	files := dumpFilesIn(t, filepath.Join(dir, "debug"))
	if len(files) != 1 {
		t.Fatalf("expected exactly one dump, got %v", files)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("dump mode = %04o, want 0600", perm)
	}
}

// Regression YM-3: neither logs/ nor debug/ had any size or age bound (53 GB
// during the incident). The new scheme prunes itself and touches nothing else.
func TestDebugDumps_PruneKeepsBoundAndSparesForeignFiles(t *testing.T) {
	t.Setenv("YESMEM_PROXY_DEBUG", "1")
	t.Setenv("YESMEM_PROXY_DEBUG_MAX_FILES", "10")
	lastDumpPruneNano.Store(0) // otherwise the throttle skips this prune
	dir := t.TempDir()
	dumpDir := filepath.Join(dir, "debug")

	seedDumpFiles(t, dumpDir, 40)
	foreign := map[string]string{
		filepath.Join(dumpDir, "req_7_body.json"):                              "legacy debug dump",
		filepath.Join(dumpDir, "readme.txt"):                                   "note",
		filepath.Join(dir, "logs", "req_7_body.json"):                          "legacy logs dump",
		filepath.Join(dir, "logs", "dumps-pre-deploy-20261016-1353", "a.json"): "pre-deploy archive",
	}
	for path, content := range foreign {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	writeDebugDump(dir, "sess-new", dumpKindBody, 1, []byte(`{"hi":1}`))

	files := dumpFilesIn(t, dumpDir)
	// 10 kept + the legacy req_7_body.json (different scheme, never pruned).
	if len(files) > 11 {
		t.Errorf("dump dir not pruned to the configured bound: %d files", len(files))
	}
	for path := range foreign {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("prune deleted a foreign file %s: %v", path, err)
		}
	}
}

// YM-3: the name must carry a millisecond timestamp, a filename-safe thread
// token and the request index, so dumps from two process lifetimes never collide.
func TestDebugDumpFileName_Shape(t *testing.T) {
	now := time.Date(2026, 10, 16, 11, 35, 12, 347000000, time.UTC)
	got := dumpFileName(now, "opencode:ses_ebc14c08ffeX3thcK0w81q4OZ", dumpKindPre, 3465)
	want := "req_20261016-113512.347_ses-ebc14c08_3465_pre.json"
	if got != want {
		t.Errorf("dumpFileName = %q, want %q", got, want)
	}
}

func TestThreadToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "nothread"},
		{"sess-1759abc", "sess-1759abc"},
		{"opencode:ses_ebc14c08ffeX3thcK0w81q4OZ", "ses-ebc14c08"},
		{"weird id/with:slashes", "slashes"},
	}
	for _, tc := range cases {
		if got := threadToken(tc.in); got != tc.want {
			t.Errorf("threadToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// YM-3: the cleanup must only ever consider files matching the current scheme.
func TestPruneDebugDumps_IgnoresForeignNames(t *testing.T) {
	dir := t.TempDir()
	seedDumpFiles(t, dir, 12)
	foreign := []string{"req_7_body.json", "req_20261016-113512_3465_pre.json", "notes.md"}
	for _, name := range foreign {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	pruneDebugDumps(dir, time.Now(), 3, 0, 0)

	for _, name := range foreign {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("prune removed %s: %v", name, err)
		}
	}
	newScheme := 0
	for _, path := range dumpFilesIn(t, dir) {
		if dumpNameRe.MatchString(filepath.Base(path)) {
			newScheme++
		}
	}
	if newScheme != 3 {
		t.Errorf("expected 3 dumps kept, got %d", newScheme)
	}
}

// YM-3: an age bound on top of the file-count bound.
func TestPruneDebugDumps_RemovesOldByAge(t *testing.T) {
	dir := t.TempDir()
	seedDumpFiles(t, dir, 3)
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range dumpFilesIn(t, dir) {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	pruneDebugDumps(dir, time.Now(), 0, 24*time.Hour, 0)

	if kept := dumpFilesIn(t, dir); len(kept) != 0 {
		t.Errorf("expected all aged dumps removed, kept %v", kept)
	}
}

// Cold-review finding: a file count is no disk bound — one dump reached 49.9 MB,
// so the directory also has to be trimmed by total size.
func TestPruneDebugDumps_RemovesByTotalSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("req_20200101-00000%d.000_ses-old_1_body.json", i)
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 1000), 0600); err != nil {
			t.Fatal(err)
		}
	}

	pruneDebugDumps(dir, time.Now(), 0, 0, 2500)

	if files := dumpFilesIn(t, dir); len(files) != 2 {
		t.Errorf("expected 2 dumps within 2500 bytes, got %d: %v", len(files), files)
	}
}
