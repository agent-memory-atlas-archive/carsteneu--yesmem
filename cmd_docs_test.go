package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-231: remove-docs derived the clone directory from the caller-supplied name
// and passed it to os.RemoveAll. A name that is not a single path element
// escapes the docs directory; the URL was handed to 'git clone' without an
// option terminator, so a value starting with '-' became a git option.
func TestDocsCloneDir_RejectsNamesThatEscapeTheDocsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, name := range []string{"", ".", "..", "../victim", "a/b", "/etc/passwd", "-rf"} {
		if dir, err := docsCloneDir(name); err == nil {
			t.Errorf("docsCloneDir(%q) = %q, want an error", name, dir)
		}
	}

	dir, err := docsCloneDir("go-stdlib")
	if err != nil {
		t.Fatalf("docsCloneDir(\"go-stdlib\"): %v", err)
	}
	if want := filepath.Join(home, ".claude", "yesmem", "docs", "go-stdlib"); dir != want {
		t.Errorf("docsCloneDir = %q, want %q", dir, want)
	}
}

func TestGitCloneArgs_TerminatesOptionsBeforeURL(t *testing.T) {
	args := gitCloneArgs("--upload-pack=/bin/sh", "/tmp/x")
	seenTerminator := false
	for i, a := range args {
		if a == "--" {
			seenTerminator = true
			if i+1 >= len(args) || args[i+1] != "--upload-pack=/bin/sh" {
				t.Fatalf("'--' is not immediately followed by the URL: %v", args)
			}
		}
	}
	if !seenTerminator {
		t.Fatalf("git clone args carry no '--' terminator, so a URL starting with '-' is read as an option: %v", args)
	}
}

// YM-27: 'yesmem sync-docs' used to walk up from the source path to the nearest
// .git and run 'git pull' there — for the rules source (<repo>/CLAUDE.md) that
// is the repository the user is working in. Only a source that is itself a
// repository root (a docs clone) may be pulled.

func cliGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=yesmem-test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=yesmem-test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cliAheadClone returns a working clone that is exactly one upstream commit
// behind, plus its origin.
func cliAheadClone(t *testing.T) (origin, clone string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	cliGit(t, root, "init", "--bare", "--quiet", "-b", "main", origin)

	clone = filepath.Join(root, "clone")
	cliGit(t, root, "clone", "--quiet", origin, clone)
	if err := os.MkdirAll(filepath.Join(clone, "sub"), 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "sub", "doc.md"), []byte("# Doc\n\nBody.\n"), 0644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	cliGit(t, clone, "add", "-A")
	cliGit(t, clone, "commit", "--quiet", "-m", "doc")
	cliGit(t, clone, "push", "--quiet", "-u", "origin", "main")

	other := filepath.Join(root, "other")
	cliGit(t, root, "clone", "--quiet", origin, other)
	if err := os.WriteFile(filepath.Join(other, "upstream.md"), []byte("upstream\n"), 0644); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	cliGit(t, other, "add", "-A")
	cliGit(t, other, "commit", "--quiet", "-m", "upstream")
	cliGit(t, other, "push", "--quiet", "origin", "main")
	return origin, clone
}

func TestSyncDocsTryGitPull_DoesNotPullSourceInsideWorkingRepo(t *testing.T) {
	_, work := cliAheadClone(t)

	before := cliGit(t, work, "rev-parse", "HEAD")
	if tryGitPull(filepath.Join(work, "sub", "doc.md")) {
		t.Errorf("tryGitPull reported a pull for a file inside a working repo")
	}
	if tryGitPull(filepath.Join(work, "sub")) {
		t.Errorf("tryGitPull reported a pull for a subdirectory of a working repo")
	}
	if after := cliGit(t, work, "rev-parse", "HEAD"); after != before {
		t.Fatalf("working repository was pulled: %s -> %s", before, after)
	}
}

func TestSyncDocsTryGitPull_PullsStandaloneSourceRepo(t *testing.T) {
	_, clone := cliAheadClone(t)

	before := cliGit(t, clone, "rev-parse", "HEAD")
	if !tryGitPull(clone) {
		t.Errorf("standalone clone: tryGitPull reported no update")
	}
	if after := cliGit(t, clone, "rev-parse", "HEAD"); after == before {
		t.Fatalf("standalone clone was not updated from upstream (HEAD %s)", after)
	}
}

// captureOutput runs fn with os.Stdout redirected and returns what it printed.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// YM-29: registered doc sources whose path no longer exists (the /tmp ingests
// from the audit) went unnoticed — list-docs showed them like any other source
// and nothing removed them.
func TestDocsCLI_MissingPathsAreFlaggedAndPruned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "yesmem"), 0755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	healthyDir := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(healthyDir, 0755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(healthyDir, "guide.md"), []byte("# Guide\n\nBody text.\n"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	stalePath := filepath.Join(t.TempDir(), "gone", "go-stdlib")

	store, err := storage.Open(filepath.Join(home, ".claude", "yesmem", "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.UpsertDocSource(&storage.DocSource{Name: "healthy-docs", Project: "p", Path: healthyDir}); err != nil {
		t.Fatalf("upsert healthy: %v", err)
	}
	if _, err := store.UpsertDocSource(&storage.DocSource{Name: "go-stdlib", Project: "p", Path: stalePath}); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	store.Close()

	// list-docs must point out the dead path ...
	out := captureOutput(t, func() { runListDocs([]string{"--project", "p"}) })
	staleLine, ok := lineContaining(out, "go-stdlib")
	if !ok {
		t.Fatalf("stale source not listed:\n%s", out)
	}
	if !strings.Contains(staleLine, "missing") {
		t.Errorf("dead path not flagged in list-docs: %q", staleLine)
	}
	if healthyLine, ok := lineContaining(out, "healthy-docs"); ok && strings.Contains(healthyLine, "missing") {
		t.Errorf("live path wrongly flagged as missing: %q", healthyLine)
	}

	// ... and sync-docs --prune must remove it, leaving the healthy source alone.
	captureOutput(t, func() { runSyncDocs([]string{"--all", "--prune"}) })

	store2, err := storage.Open(filepath.Join(home, ".claude", "yesmem", "yesmem.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	if _, err := store2.GetDocSource("go-stdlib", "p"); err == nil {
		t.Errorf("stale source survived --prune")
	}
	if _, err := store2.GetDocSource("healthy-docs", "p"); err != nil {
		t.Errorf("healthy source was pruned: %v", err)
	}
}

func lineContaining(out, needle string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line, true
		}
	}
	return "", false
}

func seedDocsDB(t *testing.T, home string, sources ...storage.DocSource) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "yesmem"), 0755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(home, ".claude", "yesmem", "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	for i := range sources {
		id, err := store.UpsertDocSource(&sources[i])
		if err != nil {
			t.Fatalf("upsert %s: %v", sources[i].Name, err)
		}
		if sources[i].IsRules {
			if err := store.SaveRulesContent(id, "Never commit secrets.", "hash-prune"); err != nil {
				t.Fatalf("save rules: %v", err)
			}
		}
	}
}

func openDocsDB(t *testing.T, home string) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(home, ".claude", "yesmem", "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// YM-29: a missing path says nothing about a rules or skill row — their body
// lives in full_content, and nothing regenerates it. --prune must only remove
// sources whose content is exclusively on disk.
func TestDocsCLI_PruneKeepsRulesAndSkillSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	gone := filepath.Join(t.TempDir(), "gone")
	seedDocsDB(t, home,
		storage.DocSource{Name: "plain-docs", Project: "p", Path: filepath.Join(gone, "docs")},
		storage.DocSource{Name: "rules", Project: "p", Path: filepath.Join(gone, "CLAUDE.md"), IsRules: true},
		storage.DocSource{Name: "some-skill", Project: "p", Path: filepath.Join(gone, "skill"), IsSkill: true, FullContent: "Skill body."},
	)

	captureOutput(t, func() { runSyncDocs([]string{"--all", "--prune"}) })

	store := openDocsDB(t, home)
	if _, err := store.GetDocSource("plain-docs", "p"); err == nil {
		t.Errorf("plain source with a dead path survived --prune")
	}
	if got := store.GetRulesContent("p"); got != "Never commit secrets." {
		t.Errorf("--prune removed the rules block (content = %q)", got)
	}
	if _, err := store.GetSkillContent("some-skill", "p"); err != nil {
		t.Errorf("--prune removed a skill body: %v", err)
	}
}

// A relative path was recorded against some unknown working directory, so it can
// neither be verified nor treated as dead — pruning on it would delete live
// sources when the command runs from another directory.
func TestDocsCLI_PruneIgnoresRelativePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Run from a directory where "docs" does not exist — otherwise the check
	// passes by accident and pins nothing.
	t.Chdir(t.TempDir())

	seedDocsDB(t, home, storage.DocSource{Name: "relative-docs", Project: "p", Path: "docs"})

	out := captureOutput(t, func() { runSyncDocs([]string{"--all", "--prune"}) })

	store := openDocsDB(t, home)
	if _, err := store.GetDocSource("relative-docs", "p"); err != nil {
		t.Errorf("source with a relative path was pruned: %v", err)
	}
	if strings.Contains(out, `Removed "relative-docs"`) {
		t.Errorf("relative path reported as removed:\n%s", out)
	}
}

// YM-231: a relative source path was re-read against the cwd of whoever ran the
// sync, so a directory that happens to share the name silently fed foreign
// content into the source. The path cannot be resolved reliably, so the sync
// must refuse it and say why.
func TestDocsCLI_SyncRefusesRelativeSourcePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A decoy tree that carries the same name as the registered relative path.
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "docs"), 0755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "docs", "decoy.md"), []byte("# Decoy\n\nForeign content that must not be indexed."), 0644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	t.Chdir(decoy)

	seedDocsDB(t, home, storage.DocSource{Name: "relative-docs", Project: "p", Path: "docs"})

	var logBuf bytes.Buffer
	prevLog := log.Default().Writer()
	log.SetOutput(&logBuf)
	captureOutput(t, func() { runSyncDocs([]string{"--all"}) })
	log.SetOutput(prevLog)

	store := openDocsDB(t, home)
	src, err := store.GetDocSource("relative-docs", "p")
	if err != nil {
		t.Fatalf("source vanished: %v", err)
	}
	chunks, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("%d chunk(s) were created from the cwd-relative path %q — foreign content indexed under the source name", len(chunks), src.Path)
	}
	if !strings.Contains(logBuf.String(), "relative") {
		t.Errorf("sync did not explain why the source was skipped:\n%s", logBuf.String())
	}
}
