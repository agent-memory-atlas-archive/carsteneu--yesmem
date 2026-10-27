package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-25: the doc-sync loop walks every registered source with a path. A rules
// source (is_rules=1, path = the project's CLAUDE.md) is not a chunk source —
// re-ingesting it created doc chunks for the rules file and cleared the
// condensed block (see internal/storage/doc_chunks.go UpsertDocSource).
func TestRunDocSync_SkipsRulesSource(t *testing.T) {
	_, s := mustHandler(t)

	dir := t.TempDir()
	claude := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claude, []byte("# Rules\n\nNever commit secrets.\n"), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	rulesID, err := s.UpsertDocSource(&storage.DocSource{Name: "rules", Project: "p", Path: claude})
	if err != nil {
		t.Fatalf("upsert rules source: %v", err)
	}
	if err := s.SaveRulesContent(rulesID, "Never commit secrets.", "hash-1"); err != nil {
		t.Fatalf("save rules content: %v", err)
	}

	// A regular source must still be synced by the same run.
	docDir := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docDir, 0755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	docFile := filepath.Join(docDir, "guide.md")
	if err := os.WriteFile(docFile, []byte("# Guide\n\nRegular documentation body.\n"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	docID, err := s.UpsertDocSource(&storage.DocSource{Name: "guide", Project: "p", Path: docDir})
	if err != nil {
		t.Fatalf("upsert guide source: %v", err)
	}

	runDocSync(s, nil)

	rulesChunks, err := s.GetDocChunksBySource(rulesID)
	if err != nil {
		t.Fatalf("rules chunks: %v", err)
	}
	if len(rulesChunks) != 0 {
		t.Errorf("doc-sync ingested the rules source: %d chunks", len(rulesChunks))
	}
	if got := s.GetRulesContent("p"); got != "Never commit secrets." {
		t.Errorf("rules content = %q, want it preserved by the sync", got)
	}

	docChunks, err := s.GetDocChunksBySource(docID)
	if err != nil {
		t.Fatalf("doc chunks: %v", err)
	}
	if len(docChunks) == 0 {
		t.Errorf("regular source was not synced")
	}
}

// --- YM-27: doc-sync must never pull the source's enclosing working repo ---

// gitRun executes git in dir with an isolated identity and no user config.
func gitRun(t *testing.T, dir string, args ...string) string {
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

// setupAheadClone builds a bare origin, a clone of it and pushes one extra
// commit from a second clone, so the returned clone is exactly one commit
// behind its upstream and a pull would fast-forward it.
func setupAheadClone(t *testing.T) (origin, clone string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	gitRun(t, root, "init", "--bare", "--quiet", "-b", "main", origin)

	clone = filepath.Join(root, "clone")
	gitRun(t, root, "clone", "--quiet", origin, clone)
	if err := os.MkdirAll(filepath.Join(clone, "sub"), 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "sub", "doc.md"), []byte("# Doc\n\nBody.\n"), 0644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	gitRun(t, clone, "add", "-A")
	gitRun(t, clone, "commit", "--quiet", "-m", "doc")
	gitRun(t, clone, "push", "--quiet", "-u", "origin", "main")

	other := filepath.Join(root, "other")
	gitRun(t, root, "clone", "--quiet", origin, other)
	if err := os.WriteFile(filepath.Join(other, "upstream.md"), []byte("upstream\n"), 0644); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	gitRun(t, other, "add", "-A")
	gitRun(t, other, "commit", "--quiet", "-m", "upstream")
	gitRun(t, other, "push", "--quiet", "origin", "main")
	return origin, clone
}

// YM-27: the rules source is <repo>/CLAUDE.md. findGitRoot walked up out of the
// source into the user's working repository, so a background doc-sync ran
// 'git pull' there — on whatever branch the user was working on.
func TestTryGitPull_DoesNotPullSourceInsideWorkingRepo(t *testing.T) {
	_, work := setupAheadClone(t)

	before := gitRun(t, work, "rev-parse", "HEAD")
	if tryGitPull(filepath.Join(work, "sub", "doc.md")) {
		t.Errorf("tryGitPull reported a pull for a file inside a working repo")
	}
	if after := gitRun(t, work, "rev-parse", "HEAD"); after != before {
		t.Fatalf("working repository was pulled: %s -> %s", before, after)
	}

	// Also for a subdirectory of the working repo (a registered --path source).
	if tryGitPull(filepath.Join(work, "sub")) {
		t.Errorf("tryGitPull reported a pull for a subdirectory of a working repo")
	}
	if after := gitRun(t, work, "rev-parse", "HEAD"); after != before {
		t.Fatalf("working repository was pulled: %s -> %s", before, after)
	}
}

// The clone case must keep working: a source that IS a repository root (a docs
// clone under ~/.claude/yesmem/docs/<name>) is still updated from upstream.
func TestTryGitPull_PullsStandaloneSourceRepo(t *testing.T) {
	_, clone := setupAheadClone(t)

	before := gitRun(t, clone, "rev-parse", "HEAD")
	tryGitPull(clone)
	after := gitRun(t, clone, "rev-parse", "HEAD")
	if before == after {
		t.Fatalf("standalone clone was not updated from upstream (HEAD %s)", after)
	}
}

// A skill source keeps its body in full_content, exactly like a rules source.
// ingest.Run builds its DocSource without the skill flag, so a doc-sync pass
// both indexes SKILL.md as documentation and clears is_skill — after which
// GetSkillContent no longer finds the row (YM-25 class).
func TestRunDocSync_SkipsSkillSource(t *testing.T) {
	_, s := mustHandler(t)

	skillDir := filepath.Join(t.TempDir(), "some-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill\n\nDo the thing.\n"), 0644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	skillID, err := s.UpsertDocSource(&storage.DocSource{Name: "some-skill", Project: "p", Path: skillDir, IsSkill: true, FullContent: "Skill body."})
	if err != nil {
		t.Fatalf("upsert skill source: %v", err)
	}

	runDocSync(s, nil)

	chunks, err := s.GetDocChunksBySource(skillID)
	if err != nil {
		t.Fatalf("skill chunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("doc-sync indexed a skill source: %d chunks", len(chunks))
	}
	got, err := s.GetSkillContent("some-skill", "p")
	if err != nil {
		t.Fatalf("skill row lost its is_skill flag: %v", err)
	}
	if got != "Skill body." {
		t.Errorf("skill content = %q, want it preserved", got)
	}
}
