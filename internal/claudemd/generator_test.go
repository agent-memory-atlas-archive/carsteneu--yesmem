package claudemd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

type mockLLMClient struct {
	response  string
	err       error
	callCount int
}

func (m *mockLLMClient) Complete(system, user string, opts ...extraction.CallOption) (string, error) {
	m.callCount++
	return m.response, m.err
}
func (m *mockLLMClient) CompleteJSON(system, user string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	m.callCount++
	return m.response, m.err
}
func (m *mockLLMClient) Name() string  { return "mock" }
func (m *mockLLMClient) Model() string { return "mock-model" }

func seedLearnings(t *testing.T, s *storage.Store, project string, n int, category string) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := s.InsertLearning(&models.Learning{
			SessionID:  "test-session",
			Category:   category,
			Content:    fmt.Sprintf("learning %s #%d", category, i),
			Project:    project,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
			Source:     "llm_extracted",
		})
		if err != nil {
			t.Fatalf("seed learning: %v", err)
		}
	}
}

func mustOpenStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestGeneratorWritesFile(t *testing.T) {
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(mustOpenStore(t), &mockLLMClient{response: "## Bekannte Fallen\n- test gotcha"}, cfg)

	dir := t.TempDir()
	outPath, err := gen.writeTo(dir, ".claude", "yesmem-ops.md", "testproj", "## Bekannte Fallen\n- test gotcha", claudeHeader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "testproj") {
		t.Error("output missing project name in header")
	}
	if !strings.Contains(string(data), "test gotcha") {
		t.Error("output missing content")
	}
	if !strings.Contains(string(data), "Auto-generated") {
		t.Error("output missing auto-generated header")
	}
	// file must be in .claude/
	if filepath.Base(filepath.Dir(outPath)) != ".claude" {
		t.Errorf("expected output in .claude/, got %s", outPath)
	}
}

func TestGeneratorNeedsRefreshNoState(t *testing.T) {
	cfg := &config.ClaudeMdConfig{MaxPerCategory: map[string]int{}}
	gen := NewGenerator(mustOpenStore(t), nil, cfg)
	needs, err := gen.NeedsRefresh("unknown-project")
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("should need refresh when no state exists")
	}
}

func TestGeneratorWriteMissingDir(t *testing.T) {
	cfg := &config.ClaudeMdConfig{OutputFileName: "yesmem-ops.md"}
	gen := NewGenerator(mustOpenStore(t), nil, cfg)
	_, err := gen.writeTo("/nonexistent/path", ".claude", "yesmem-ops.md", "proj", "content", claudeHeader)
	if err == nil {
		t.Error("expected error for non-existent project dir")
	}
}

func TestGenerateFullPipeline(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 3, "gotcha")

	dir := t.TempDir()
	mock := &mockLLMClient{response: "## Fallen\n- gotcha 1\n- gotcha 2"}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	err := gen.Generate("testproj", dir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// LLM was called
	if mock.callCount != 1 {
		t.Errorf("LLM callCount: got %d, want 1", mock.callCount)
	}

	// File was written
	outPath := filepath.Join(dir, ".claude", "yesmem-ops.md")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "gotcha 1") {
		t.Error("output missing LLM content")
	}

	// State was saved
	state, err := store.GetClaudeMdState("testproj")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("state should have been saved after Generate")
	}
	if state.LearningsHash == "" {
		t.Error("state hash should not be empty")
	}
	if state.OutputPath != outPath {
		t.Errorf("state output path: got %q, want %q", state.OutputPath, outPath)
	}
}

func TestGenerateSkipsWhenHashUnchanged(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "proj", 2, "gotcha")

	dir := t.TempDir()
	mock := &mockLLMClient{response: "## Content"}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	// First call — should generate
	if err := gen.Generate("proj", dir); err != nil {
		t.Fatal(err)
	}
	if mock.callCount != 1 {
		t.Fatalf("first call: LLM callCount %d, want 1", mock.callCount)
	}

	// Second call — same learnings, should skip
	if err := gen.Generate("proj", dir); err != nil {
		t.Fatal(err)
	}
	if mock.callCount != 1 {
		t.Errorf("second call: LLM callCount %d, want still 1 (hash unchanged)", mock.callCount)
	}
}

func TestGenerateRerunsWhenLearningsChange(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "proj", 2, "gotcha")

	dir := t.TempDir()
	mock := &mockLLMClient{response: "## Content"}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 10, "decision": 10},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	// First generate
	if err := gen.Generate("proj", dir); err != nil {
		t.Fatal(err)
	}

	// Add a clearly distinct learning — different category + unique content
	_, err := store.InsertLearning(&models.Learning{
		SessionID:  "new-session",
		Category:   "decision",
		Content:    "Architektur-Entscheidung: PostgreSQL statt MySQL verwenden wegen JSON-Support",
		Project:    "proj",
		Confidence: 1.0,
		CreatedAt:  time.Now(),
		Source:     "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second generate — should call LLM again
	if err := gen.Generate("proj", dir); err != nil {
		t.Fatal(err)
	}
	if mock.callCount != 2 {
		t.Errorf("LLM callCount after adding learning: got %d, want 2", mock.callCount)
	}
}

func TestGenerateNoLearnings(t *testing.T) {
	store := mustOpenStore(t)
	mock := &mockLLMClient{response: "should not be called"}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	err := gen.Generate("empty-project", t.TempDir())
	if err == nil {
		t.Error("expected error when no learnings exist")
	}
	if mock.callCount != 0 {
		t.Error("LLM should not be called when there are no learnings")
	}
}

func TestGenerateLLMError(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "proj", 2, "gotcha")

	mock := &mockLLMClient{err: fmt.Errorf("rate limited")}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	err := gen.Generate("proj", t.TempDir())
	if err == nil {
		t.Error("expected error when LLM fails")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should contain LLM error, got: %v", err)
	}

	// No state saved on failure
	state, _ := store.GetClaudeMdState("proj")
	if state != nil {
		t.Error("state should not be saved when LLM call fails")
	}
}

func TestNeedsRefreshHashChanged(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "proj", 3, "gotcha")

	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 10, "decision": 10},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, &mockLLMClient{response: "ok"}, cfg)

	// Generate to save state with hash
	if err := gen.Generate("proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	// Before adding learnings — should NOT need refresh
	needs, err := gen.NeedsRefresh("proj")
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Error("should not need refresh right after Generate")
	}

	// Add a clearly distinct learning — different category + unique content
	_, err = store.InsertLearning(&models.Learning{
		SessionID:  "new-session",
		Category:   "decision",
		Content:    "Deployment-Strategie: Blue-Green statt Rolling-Update wegen Zero-Downtime-Anforderung",
		Project:    "proj",
		Confidence: 1.0,
		CreatedAt:  time.Now(),
		Source:     "user_stated",
	})
	if err != nil {
		t.Fatal(err)
	}

	needs, err = gen.NeedsRefresh("proj")
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("should need refresh after new learning added")
	}
}

func TestHashLearningsDeterministic(t *testing.T) {
	learnings := []models.Learning{
		{ID: 1, Content: "first"},
		{ID: 2, Content: "second"},
	}
	h1 := hashLearnings(learnings)
	h2 := hashLearnings(learnings)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q != %q", h1, h2)
	}

	// Different content = different hash
	learnings2 := []models.Learning{
		{ID: 1, Content: "first"},
		{ID: 3, Content: "third"},
	}
	h3 := hashLearnings(learnings2)
	if h1 == h3 {
		t.Error("different learnings should produce different hash")
	}
}

// YM-56: both output targets share one claudemd_state row (PK project). The
// hash-only skip made the second target a silent no-op.
func TestGenerateAgentsMdAfterClaudeMdWritesBothTargets(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	mock := &mockLLMClient{response: "## Fallen\n- gotcha 1"}
	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, mock, cfg)

	if err := gen.Generate("testproj", dir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := gen.GenerateAgentsMd("testproj", dir); err != nil {
		t.Fatalf("GenerateAgentsMd: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".claude", "yesmem-ops.md")); err != nil {
		t.Errorf("yesmem-ops.md missing after Generate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing after GenerateAgentsMd: %v", err)
	}
	if mock.callCount != 2 {
		t.Errorf("LLM calls = %d, want 2 (the second target must not be skipped)", mock.callCount)
	}
}

// NeedsRefreshFor must be target-aware: a project whose stored state belongs to
// yesmem-ops.md is stale for AGENTS.md.
func TestNeedsRefreshForIsTargetAware(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	cfg := &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	}
	gen := NewGenerator(store, &mockLLMClient{response: "x"}, cfg)
	if err := gen.Generate("testproj", t.TempDir()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	needs, err := gen.NeedsRefreshFor("testproj", "yesmem-ops.md")
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Error("yesmem-ops.md should be up to date after Generate")
	}
	needs, err = gen.NeedsRefreshFor("testproj", "AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("AGENTS.md should be stale when only yesmem-ops.md was generated")
	}
}

// YM-56 review finding: AGENTS.md sits in the project root, where a hand-written
// file is the norm. --all --opencode must not truncate it.
func TestGenerateAgentsMdRefusesForeignFile(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	foreign := "# handgeschriebene Regeln\n- nicht ueberschreiben\n"
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}

	gen := NewGenerator(store, &mockLLMClient{response: "## Fallen\n- x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.GenerateAgentsMd("testproj", dir); err == nil {
		t.Error("expected refusal for a hand-written AGENTS.md")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != foreign {
		t.Errorf("hand-written AGENTS.md was overwritten: %q", string(data))
	}
}

// A file yesmem generated itself is refreshed as before.
func TestGenerateAgentsMdRefreshesOwnFile(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	gen := NewGenerator(store, &mockLLMClient{response: "## Fallen\n- x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.GenerateAgentsMd("testproj", dir); err != nil {
		t.Fatalf("first run: %v", err)
	}
	seedLearnings(t, store, "testproj", 1, "gotcha")
	if err := gen.GenerateAgentsMd("testproj", dir); err != nil {
		t.Fatalf("refresh of own file refused: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Auto-generated by yesmem") {
		t.Errorf("own AGENTS.md lost its header: %q", string(data))
	}
}

// A symlink must not route the write outside the project directory.
func TestGenerateAgentsMdRefusesSymlink(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.Symlink(outside, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gen := NewGenerator(store, &mockLLMClient{response: "x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.GenerateAgentsMd("testproj", dir); err == nil {
		t.Error("expected refusal for a symlinked AGENTS.md")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("write followed the symlink: %v", err)
	}
}

// The same ownership rule guards the .claude/ target: a hand-written
// yesmem-ops.md is not ours to replace, even though the path looks like ours
// (YM-266 / DEV-DT43).
func TestGenerateRefusesForeignClaudeMd(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	foreign := "# handgeschriebene Notizen\n- nicht ueberschreiben\n"
	target := filepath.Join(dir, ".claude", "yesmem-ops.md")
	if err := os.WriteFile(target, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLLMClient{response: "## Fallen\n- x"}
	gen := NewGenerator(store, mock, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.Generate("testproj", dir); err == nil {
		t.Error("expected refusal for a hand-written .claude/yesmem-ops.md")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("refusal came from somewhere else: %v", err)
	}
	if mock.callCount != 0 {
		t.Error("the refusal must come before the LLM call")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != foreign {
		t.Errorf("hand-written .claude/yesmem-ops.md was overwritten: %q", string(data))
	}
}

// A symlink at the .claude/ target must not route the write to another file.
func TestGenerateRefusesSymlinkClaudeMd(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.Symlink(outside, filepath.Join(claudeDir, "yesmem-ops.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gen := NewGenerator(store, &mockLLMClient{response: "x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.Generate("testproj", dir); err == nil {
		t.Error("expected refusal for a symlinked .claude/yesmem-ops.md")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("refusal came from somewhere else: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("write followed the symlink: %v", err)
	}
}

// The rule must hold where the write happens, not only at the entry points: a
// caller that reaches generate() directly must not overwrite a foreign file
// either (YM-266 review).
func TestGenerateRefusesForeignTargetAtTheWrite(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	foreign := "# handgeschrieben\n- x\n"
	target := filepath.Join(dir, ".claude", "yesmem-ops.md")
	if err := os.WriteFile(target, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}

	gen := NewGenerator(store, &mockLLMClient{response: "## Fallen\n- x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.generate("testproj", dir, ".claude", "yesmem-ops.md", claudeHeader); err == nil {
		t.Error("expected refusal at the write")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("refusal came from somewhere else: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != foreign {
		t.Errorf("foreign file was overwritten: %q", string(data))
	}
}

// A .claude/yesmem-ops.md written by yesmem before afccf1df carries the German
// header. It is ours: refusing it would freeze every such file forever, because
// the state is never rewritten (YM-266 review finding).
func TestGenerateRefreshesLegacyGermanHeader(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := "# Operative Hinweise — testproj\n> Auto-generiert von yesmem (2026-03-22 01:54)\n> Nicht manuell editieren — wird bei Änderungen überschrieben\n\n## Alte Fallen\n- alt\n"
	target := filepath.Join(claudeDir, "yesmem-ops.md")
	if err := os.WriteFile(target, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	gen := NewGenerator(store, &mockLLMClient{response: "## Fallen\n- neu"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.Generate("testproj", dir); err != nil {
		t.Fatalf("legacy yesmem file refused: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- neu") {
		t.Errorf("legacy file was not refreshed: %q", string(data))
	}
}

// The marker must sit in the header; a hand-written file may mention it in prose.
func TestGenerateAgentsMdRefusesForeignFileMentioningMarker(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	foreign := "# Regeln\n\nDiese Datei wird nicht von Auto-generated by yesmem verwaltet.\n"
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}
	gen := NewGenerator(store, &mockLLMClient{response: "x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.GenerateAgentsMd("testproj", dir); err == nil {
		t.Error("expected refusal: marker in the body is not ownership")
	}
}

// A symlinked .claude directory must not route the write out of the project: the
// last path component is a plain file name, so only the directory carries the
// redirect (YM-341).
func TestGenerateRefusesSymlinkedClaudeDir(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".claude")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	mock := &mockLLMClient{response: "x"}
	gen := NewGenerator(store, mock, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	if err := gen.Generate("testproj", dir); err == nil {
		t.Error("expected refusal for a symlinked .claude directory")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("refusal came from somewhere else: %v", err)
	}
	if mock.callCount != 0 {
		t.Error("the refusal must come before the LLM call")
	}
	if _, err := os.Stat(filepath.Join(outside, "yesmem-ops.md")); !os.IsNotExist(err) {
		t.Errorf("write followed the .claude symlink: %v", err)
	}
}

// A named pipe at the target must be refused, not read: opening it waits for a
// writer that never comes, which hangs the whole run (YM-341).
func TestGenerateRefusesFIFOAtTarget(t *testing.T) {
	store := mustOpenStore(t)
	seedLearnings(t, store, "testproj", 2, "gotcha")

	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(claudeDir, "yesmem-ops.md"), 0644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	gen := NewGenerator(store, &mockLLMClient{response: "x"}, &config.ClaudeMdConfig{
		MaxPerCategory: map[string]int{"gotcha": 5},
		OutputFileName: "yesmem-ops.md",
	})
	done := make(chan error, 1)
	go func() { done <- gen.Generate("testproj", dir) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected refusal for a FIFO at the target")
		} else if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("refusal came from somewhere else: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the generator blocked on the FIFO instead of refusing it")
	}
}

// output_file comes from config.yaml. A value with a separator or ".." must not
// move the write out of the project (YM-341).
func TestGenerateRefusesOutputFileNameOutsideTheProject(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		outside  string // path inside the temp root that must stay absent
	}{
		{"climbs two levels", filepath.Join("..", "..", "escaped.md"), "escaped.md"},
		{"climbs one level", filepath.Join("..", "escaped.md"), "project/escaped.md"},
		{"contains a directory", filepath.Join("sub", "nested.md"), "project/.claude/sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := mustOpenStore(t)
			seedLearnings(t, store, "testproj", 2, "gotcha")

			root := t.TempDir()
			projectDir := filepath.Join(root, "project")
			if err := os.MkdirAll(projectDir, 0755); err != nil {
				t.Fatal(err)
			}
			mock := &mockLLMClient{response: "x"}
			gen := NewGenerator(store, mock, &config.ClaudeMdConfig{
				MaxPerCategory: map[string]int{"gotcha": 5},
				OutputFileName: tc.fileName,
			})
			err := gen.Generate("testproj", projectDir)
			if err == nil {
				t.Fatalf("output_file %q was accepted", tc.fileName)
			}
			if !strings.Contains(err.Error(), "plain file name") {
				t.Errorf("refusal came from somewhere else: %v", err)
			}
			if mock.callCount != 0 {
				t.Error("the refusal must come before the LLM call")
			}
			if _, statErr := os.Stat(filepath.Join(root, tc.outside)); !os.IsNotExist(statErr) {
				t.Errorf("output_file %q wrote outside the target name: %v", tc.fileName, statErr)
			}
		})
	}
}
