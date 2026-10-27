package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()

	w.Close()
	os.Stdout = old
	return <-done
}

// The proxy_*/fork_* buckets hold raw token counts, so the cost table has to run
// them through the shared price constants. Calls counts the daemon's own LLM
// calls (extract + quality), not the token buckets.
func TestCostRowsConvertTokenBuckets(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	day := "2026-10-16"
	if err := store.TrackSpend(day, "extract", 1.25); err != nil {
		t.Fatalf("track extract: %v", err)
	}
	if err := store.TrackSpend(day, "quality", 0.50); err != nil {
		t.Fatalf("track quality: %v", err)
	}
	if err := store.TrackProxyUsage(day, 1_000_000, 100_000, 0, 0); err != nil {
		t.Fatalf("track proxy: %v", err)
	}
	if err := store.TrackForkUsage(day, 500_000, 0, 0, 0); err != nil {
		t.Fatalf("track fork: %v", err)
	}

	rows, err := costRows(store, []string{day}, map[string]int{day: 2})
	if err != nil {
		t.Fatalf("costRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	r := rows[0]
	// 1M input @ $3/MTok + 100k output @ $15/MTok
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"extract", r.Extract, 1.25},
		{"quality", r.Quality, 0.50},
		{"proxy", r.Proxy, 4.50},
		{"fork", r.Fork, 1.50},
		{"total", r.Total, 7.75},
	} {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %.4f, want %.4f", c.name, c.got, c.want)
		}
	}
	if r.Calls != 2 {
		t.Errorf("calls = %d, want 2", r.Calls)
	}
}

// `yesmem cost` is the only spend overview the CLI exposes. It grouped just the
// extract and quality buckets, so the proxy_* and fork_* buckets — which carry the
// bulk of the token volume — never showed up.
func TestRunCostShowsProxyAndForkBuckets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	day := time.Now().Format("2006-01-02")
	if err := store.TrackSpend(day, "extract", 1.25); err != nil {
		t.Fatalf("track extract spend: %v", err)
	}
	if err := store.TrackProxyUsage(day, 1_000_000, 20_000, 100_000, 0); err != nil {
		t.Fatalf("track proxy usage: %v", err)
	}
	if err := store.TrackForkUsage(day, 500_000, 10_000, 0, 0); err != nil {
		t.Fatalf("track fork usage: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	out := captureStdout(t, runCost)

	for _, want := range []string{"Proxy", "Fork"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost output has no %s column:\n%s", want, out)
		}
	}
}

// YM-229: `yesmem export` opened the store writable, so a pure read ran the
// schema migrations and created runtime.db/messages.db next to yesmem.db. Like
// `yesmem cost` since YM-95 it has to open read-only.
func TestRunExportOpensDatabaseReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.InsertLearning(&models.Learning{
		Category:  "gotcha",
		Content:   "los14b read-only export payload",
		Project:   "los14b",
		Source:    "user_stated",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	// Open created the siblings; removing them makes the assertion meaningful.
	for _, sibling := range []string{"runtime.db", "messages.db"} {
		if err := os.Remove(filepath.Join(dataDir, sibling)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove sibling %s: %v", sibling, err)
		}
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	oldArgs := os.Args
	os.Args = []string{"yesmem", "export", outPath}
	t.Cleanup(func() { os.Args = oldArgs })

	out := captureStdout(t, runExport)

	for _, sibling := range []string{"runtime.db", "messages.db"} {
		if _, err := os.Stat(filepath.Join(dataDir, sibling)); err == nil {
			t.Errorf("%s was created: the export opened the store writable", sibling)
		}
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("export file missing: %v", err)
	}
	if !strings.Contains(out, "Exported 1 learnings") {
		t.Errorf("export output = %q", out)
	}
}

// YM-282 [MEM-F35]: the CLI switch has to reach the storage layer. It is the
// only way a user can import a file whose provenance claims are not trusted,
// and it must work before or after the file argument.
func TestRunImportDistrustFlagDowngradesSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	created := time.Now().Format(time.RFC3339)
	importPath := filepath.Join(t.TempDir(), "import.json")
	payload := `{"version":2,"learnings":[{"category":"preference","content":"los14c cli import","project":"los14c","source":"user_stated","created_at":"` + created + `"}]}`
	if err := os.WriteFile(importPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write import file: %v", err)
	}

	oldArgs := os.Args
	os.Args = []string{"yesmem", "import", importPath, "--distrust-file-sources"}
	t.Cleanup(func() { os.Args = oldArgs })

	out := captureStdout(t, runImport)
	if !strings.Contains(out, "file sources distrusted") {
		t.Errorf("import output does not report the distrust flag: %q", out)
	}

	ro, err := storage.OpenReadOnly(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer ro.Close()
	var source string
	if err := ro.DB().QueryRow(`SELECT source FROM learnings WHERE content = 'los14c cli import'`).Scan(&source); err != nil {
		t.Fatalf("read imported source: %v", err)
	}
	if source != "claude_suggested" {
		t.Errorf("imported source = %q, want claude_suggested", source)
	}
}

// YM-282 [MEM-F35]: an argument the parser does not understand must fail, not
// fall through. A mistyped security switch otherwise imports the file with its
// provenance claim intact (the one outcome --distrust-file-sources prevents),
// and an ignored second path imports only the first file while exiting 0.
func TestParseImportArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantPath string
		wantDist bool
		wantErr  bool
	}{
		{"path only", []string{"a.json"}, "a.json", false, false},
		{"flag after path", []string{"a.json", "--distrust-file-sources"}, "a.json", true, false},
		{"flag before path", []string{"--distrust-file-sources", "a.json"}, "a.json", true, false},
		{"mistyped flag", []string{"a.json", "--distrust-file-sourcesX"}, "", false, true},
		{"equals form", []string{"a.json", "--distrust-file-sources=true"}, "", false, true},
		{"second path", []string{"a.json", "b.json"}, "", false, true},
		{"flag only", []string{"--distrust-file-sources"}, "", false, true},
		{"no args", nil, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, distrust, err := parseImportArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseImportArgs(%q) = %q/%v, want error", tt.args, path, distrust)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseImportArgs(%q): %v", tt.args, err)
			}
			if path != tt.wantPath || distrust != tt.wantDist {
				t.Errorf("parseImportArgs(%q) = %q/%v, want %q/%v", tt.args, path, distrust, tt.wantPath, tt.wantDist)
			}
		})
	}
}
