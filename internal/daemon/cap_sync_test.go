package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

type stubDDLLooker struct {
	ddl string
}

func (s *stubDDLLooker) GetCapTableDDL(capName string) (string, error) {
	return s.ddl, nil
}

func TestCapFileToParams(t *testing.T) {
	cf := capfile.CapFile{
		Name:        "test_tool",
		Description: "A test tool",
		Tags:        []string{"test", "example"},
		Tested:      true,
		AutoActive:  false,
		Scripts: []capfile.Script{
			{
				Name:    "test_tool",
				Kind:    "tool",
				Runtime: "repl",
				Lang:    "javascript",
				Body:    "async () => 42",
				Schema:  `{"type":"object"}`,
			},
		},
	}

	params := CapFileToParams(cf)

	if params["name"] != "test_tool" {
		t.Errorf("name = %v, want test_tool", params["name"])
	}
	if params["description"] != "A test tool" {
		t.Errorf("description mismatch")
	}
	scriptsJSON, ok := params["scripts"].(string)
	if !ok || scriptsJSON == "" {
		t.Fatalf("scripts param missing or wrong type: %T", params["scripts"])
	}
	var scripts []map[string]any
	if err := json.Unmarshal([]byte(scriptsJSON), &scripts); err != nil {
		t.Fatalf("scripts JSON invalid: %v", err)
	}
	if len(scripts) != 1 {
		t.Fatalf("expected 1 script in JSON, got %d", len(scripts))
	}
	if scripts[0]["body"] != "async () => 42" {
		t.Errorf("script body = %v, want async () => 42", scripts[0]["body"])
	}
	if scripts[0]["runtime"] != "repl" {
		t.Errorf("script runtime = %v, want repl", scripts[0]["runtime"])
	}
}

func TestCapFileToParams_BashRuntime(t *testing.T) {
	cf := capfile.CapFile{
		Name:        "bash_tool",
		Description: "Bash tool",
		Scripts: []capfile.Script{
			{
				Name:    "bash_tool",
				Kind:    "tool",
				Runtime: "bash",
				Lang:    "bash",
				Body:    "echo hi",
				Schema:  `{"type":"object"}`,
			},
		},
	}

	params := CapFileToParams(cf)

	scriptsJSON, _ := params["scripts"].(string)
	var scripts []map[string]any
	if err := json.Unmarshal([]byte(scriptsJSON), &scripts); err != nil {
		t.Fatalf("scripts JSON invalid: %v", err)
	}
	if scripts[0]["runtime"] != "bash" {
		t.Errorf("runtime = %v, want bash", scripts[0]["runtime"])
	}
	if scripts[0]["body"] != "echo hi" {
		t.Errorf("body = %v, want 'echo hi'", scripts[0]["body"])
	}
}

func TestWriteCapToDisk_CreatesFile(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "A test tool",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
		Tags: []string{"test"},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	want := filepath.Join(dir, "test_tool", "CAP.md")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("CAP.md not written: %v", err)
	}
	if !strings.Contains(string(data), "test_tool") {
		t.Errorf("CAP.md missing cap name")
	}
	if !strings.Contains(string(data), "echo hi") {
		t.Errorf("CAP.md missing script body")
	}
}

func TestWriteCapToDisk_HydratesDatabaseSQLFromStore(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "Has DB",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
	}

	looker := &stubDDLLooker{ddl: "CREATE TABLE cap_test_tool__items (id TEXT PRIMARY KEY);"}

	if err := WriteCapToDisk(meta, "", dir, looker, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "test_tool", "CAP.md"))
	if !strings.Contains(string(data), "CREATE TABLE cap_test_tool__items") {
		t.Errorf("Database section missing DDL — content:\n%s", string(data))
	}
}

func TestWriteCapToDisk_NilLookerSkipsHydration(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "No DB",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "test_tool", "CAP.md"))
	// Without DDL, the Database section must be omitted entirely (spec: optional).
	if strings.Contains(string(data), "## Database") {
		t.Errorf("Database section should be omitted when DDL empty:\n%s", string(data))
	}
}

func TestCapMetaToCapFile(t *testing.T) {
	meta := CapMeta{
		Name:        "myrepl",
		Description: "REPL tool",
		Scripts: []ScriptMeta{
			{Name: "myrepl", Kind: "tool", Runtime: "repl", Body: "async () => 42", Schema: `{"type":"object"}`},
		},
		Tags: []string{"a"},
	}
	cf := CapMetaToCapFile(meta, "myproject", ProfileStandard)

	if cf.Name != "myrepl" {
		t.Errorf("Name = %q, want myrepl", cf.Name)
	}
	if cf.Scope != "project" {
		t.Errorf("Scope = %q, want project", cf.Scope)
	}
	if len(cf.Scripts) != 1 {
		t.Fatalf("Scripts: want 1, got %d", len(cf.Scripts))
	}
	sc := cf.Scripts[0]
	if sc.Runtime != "repl" {
		t.Errorf("Script.Runtime = %q, want repl", sc.Runtime)
	}
	if sc.Body != "async () => 42" {
		t.Errorf("Script.Body = %q, want async () => 42", sc.Body)
	}
	if sc.Lang != "javascript" {
		t.Errorf("Script.Lang = %q, want javascript", sc.Lang)
	}
}

func TestCapMetaToCapFile_NoProject(t *testing.T) {
	meta := CapMeta{
		Name:        "global_tool",
		Description: "Global",
		Scripts: []ScriptMeta{
			{Name: "global_tool", Kind: "tool", Runtime: "bash", Body: "echo global", Schema: `{"type":"object"}`},
		},
	}
	cf := CapMetaToCapFile(meta, "", ProfileStandard)

	if cf.Scope != "user" {
		t.Errorf("Scope = %q, want user", cf.Scope)
	}
	if len(cf.Scripts) != 1 {
		t.Fatalf("Scripts: want 1, got %d", len(cf.Scripts))
	}
	if cf.Scripts[0].Runtime != "bash" {
		t.Errorf("Script.Runtime = %q, want bash", cf.Scripts[0].Runtime)
	}
}

const v1Cap = "---\nname: my_cap\ndescription: %s\n---\n\n## Purpose\n\nTest.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho %s\n```\n"

func TestCapsDirWatcher_DetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	if changed := w.ScanChanged(dir); len(changed) != 0 {
		t.Fatalf("empty dir: want 0 changed, got %d", len(changed))
	}

	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: my_cap\ndescription: test\n---\n\n## Purpose\n\nTest.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho hi\n```\n")
	if err := os.WriteFile(filepath.Join(capDir, "CAP.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	changed := w.ScanChanged(dir)
	if len(changed) != 1 {
		t.Fatalf("new file: want 1 changed, got %d", len(changed))
	}
	if changed[0].Name != "my_cap" {
		t.Errorf("name: want my_cap, got %q", changed[0].Name)
	}

	if changed = w.ScanChanged(dir); len(changed) != 0 {
		t.Errorf("no change: want 0, got %d", len(changed))
	}
}

func TestCapsDirWatcher_DetectsModifiedFile(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(capDir, "CAP.md")
	body1 := []byte("---\nname: my_cap\ndescription: v1\n---\n\n## Purpose\n\nV1.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho v1\n```\n")
	if err := os.WriteFile(capPath, body1, 0o644); err != nil {
		t.Fatal(err)
	}
	w.ScanChanged(dir)

	body2 := []byte("---\nname: my_cap\ndescription: v2\n---\n\n## Purpose\n\nV2.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho v2\n```\n")
	if err := os.WriteFile(capPath, body2, 0o644); err != nil {
		t.Fatal(err)
	}

	changed := w.ScanChanged(dir)
	if len(changed) != 1 {
		t.Fatalf("modified: want 1 changed, got %d", len(changed))
	}
	if changed[0].Description != "v2" {
		t.Errorf("description: want v2, got %q", changed[0].Description)
	}
}

func TestCapsDirWatcher_IgnoresParseErrors(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	capDir := filepath.Join(dir, "bad_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "CAP.md"), []byte("not valid yaml frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	if changed := w.ScanChanged(dir); len(changed) != 0 {
		t.Errorf("parse error: want 0 changed, got %d", len(changed))
	}
}

func writeDiskCap(t *testing.T, dir, name string, version int, body string) string {
	t.Helper()
	capDir := filepath.Join(dir, name)
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: disk content\nversion: %d\n---\n\n## Scripts\n\n### %s\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\n%s\n```\n", name, version, name, body)
	capPath := filepath.Join(capDir, "CAP.md")
	if err := os.WriteFile(capPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return capPath
}

func readDiskCap(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read disk cap: %v", err)
	}
	return string(data)
}

func TestWriteCapToDisk_SkipsWhenDiskVersionEqual(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 5, "echo disk_v5")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo disk_v5") {
		t.Errorf("disk content was overwritten despite equal version:\n%s", content)
	}
	if strings.Contains(content, "echo db_v5") {
		t.Errorf("disk content was replaced with DB content:\n%s", content)
	}
}

func TestWriteCapToDisk_SkipsWhenDiskVersionHigher(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 7, "echo disk_v7")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo disk_v7") {
		t.Errorf("disk content was overwritten despite higher version:\n%s", content)
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskVersionLower(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 3, "echo disk_v3")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo db_v5") {
		t.Errorf("disk content was NOT overwritten despite lower version:\n%s", content)
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskMissing(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	capPath := filepath.Join(dir, "my_cap", "CAP.md")
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatalf("CAP.md not written: %v", err)
	}
	if !strings.Contains(string(data), "echo db_v5") {
		t.Errorf("CAP.md missing expected content:\n%s", string(data))
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskCorrupt(t *testing.T) {
	dir := t.TempDir()
	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(capDir, "CAP.md")
	if err := os.WriteFile(capPath, []byte("not valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo db_v5") {
		t.Errorf("corrupt disk content was NOT overwritten:\n%s", content)
	}
}

// storedCapMeta returns the active stored metadata for a cap name, or nil.
func storedCapMeta(t *testing.T, s *storage.Store, name string) *CapMeta {
	t.Helper()
	learnings, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	for _, l := range learnings {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		if meta.Name == name {
			return &meta
		}
	}
	return nil
}

// seedCap writes a cap learning straight into the store. The state it builds —
// a project-bound cap whose script runs sandbox=none — is no longer reachable
// through save_cap after YM-210; it is exactly the legacy state D12's repair
// path has to keep reachable.
func seedCapMeta(t *testing.T, s *storage.Store, meta CapMeta, project string) {
	t.Helper()
	ctxJSON, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("meta json: %v", err)
	}
	content := meta.Name + " — " + meta.Description
	l := &models.Learning{
		Content:          content,
		Category:         "cap",
		Source:           "user_stated",
		Project:          project,
		CanonicalProject: canonicalProjectFor(project),
		Confidence:       1.0,
		Context:          ctxJSON,
		Domain:           "code",
		TriggerRule:      "cap:" + meta.Name,
		CreatedAt:        timeNow(),
		ContentHash:      textutil.ContentHash(content),
		EmbeddingText:    content,
	}
	if _, err := s.InsertLearning(l); err != nil {
		t.Fatalf("seed cap %s: %v", meta.Name, err)
	}
}

// YM-175/OPS-21: CapFileToParams dropped Script.Sandbox, so a sandbox edit in
// CAP.md never reached the stored script — the disk edit was lost on sync.
func TestCapFileToParams_CarriesScriptSandbox(t *testing.T) {
	cf := capfile.CapFile{
		Name:        "sandboxed",
		Description: "Carries a profile",
		Version:     1,
		Scope:       "user",
		Scripts: []capfile.Script{{
			Name:    "run",
			Kind:    "tool",
			Runtime: "repl",
			Lang:    "javascript",
			Body:    "async () => 1",
			Sandbox: "strict",
		}},
	}

	params := CapFileToParams(cf)

	scriptsJSON, _ := params["scripts"].(string)
	var scripts []ScriptMeta
	if err := json.Unmarshal([]byte(scriptsJSON), &scripts); err != nil {
		t.Fatalf("scripts JSON invalid: %v", err)
	}
	if len(scripts) != 1 {
		t.Fatalf("want 1 script, got %d", len(scripts))
	}
	if scripts[0].Sandbox != "strict" {
		t.Errorf("Sandbox = %q, want strict — the disk profile must survive the flattening", scripts[0].Sandbox)
	}
}

// YM-223/OPS-51: allowed_hosts only binds through a sandbox profile, so a cap
// installed from CAP.md ran without a profile and its host list was inert.
func TestSyncCapsFromDisk_CarriesProfileAndAllowedHosts(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	cf := capfile.CapFile{
		Name: "hostbound", Description: "Egress via allowlist", Version: 3,
		Scope: "user", Purpose: "P",
		Scripts: []capfile.Script{{
			Name: "fetch", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "curl -s https://api.example.com", Schema: `{"type":"object"}`,
			Sandbox: "standard", AllowedHosts: []string{"api.example.com"},
		}},
	}
	if err := capfile.WriteFile(&cf, filepath.Join(dir, "hostbound")); err != nil {
		t.Fatal(err)
	}

	SyncCapsFromDisk(h, dir, "")

	meta := storedCapMeta(t, s, "hostbound")
	if meta == nil {
		t.Fatal("cap was not synced")
	}
	if len(meta.Scripts) != 1 {
		t.Fatalf("want 1 script, got %d", len(meta.Scripts))
	}
	if meta.Scripts[0].Sandbox != "standard" {
		t.Errorf("Sandbox = %q, want standard — without it allowed_hosts stays inert", meta.Scripts[0].Sandbox)
	}
	if len(meta.Scripts[0].AllowedHosts) != 1 || meta.Scripts[0].AllowedHosts[0] != "api.example.com" {
		t.Errorf("AllowedHosts = %v, want [api.example.com]", meta.Scripts[0].AllowedHosts)
	}
}

// YM-176/OPS-22: a CAP.md without auto_active means false, but the import fell
// through to save_cap's opt-out default of true.
func TestSyncCapsFromDisk_HonoursAutoActiveFalse(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	cf := capfile.CapFile{
		Name: "quiet", Description: "Not auto-active", Version: 1,
		Scope: "user", Purpose: "P", AutoActive: false,
		Scripts: []capfile.Script{{
			Name: "quiet", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo ok",
		}},
	}
	if err := capfile.WriteFile(&cf, filepath.Join(dir, "quiet")); err != nil {
		t.Fatal(err)
	}

	SyncCapsFromDisk(h, dir, "")

	meta := storedCapMeta(t, s, "quiet")
	if meta == nil {
		t.Fatal("cap was not synced")
	}
	if meta.AutoActive {
		t.Error("AutoActive = true, want false — an absent auto_active in CAP.md is opt-out")
	}
}

// YM-174/OPS-20: the sync skipped an import only when the disk version
// EQUALled the stored one, so any older on-disk version superseded the cap on
// every restart and the version only ever climbed.
func TestHandleSaveCap_StaleVersionIsNotAnUpdate(t *testing.T) {
	h, _ := mustHandler(t)
	base := map[string]any{
		"name":        "churn",
		"description": "churn",
		"scripts":     `[{"name":"run","kind":"tool","runtime":"bash","body":"echo hi","schema":"{}"}]`,
	}
	for i := 0; i < 3; i++ {
		if resp := h.handleSaveCap(base); resp.Error != "" {
			t.Fatalf("save %d: %s", i, resp.Error)
		}
	}

	resp := h.handleSaveCap(map[string]any{
		"name":        "churn",
		"description": "churn",
		"version":     float64(1),
		"scripts":     `[{"name":"run","kind":"tool","runtime":"bash","body":"echo hi","schema":"{}"}]`,
	})
	if resp.Error != "" {
		t.Fatalf("stale save: %s", resp.Error)
	}

	meta := storedCapMeta(t, h.store, "churn")
	if meta == nil {
		t.Fatal("cap disappeared")
	}
	if meta.Version != 3 {
		t.Errorf("Version = %d, want 3 — an older on-disk version is not a new version", meta.Version)
	}
}

// YM-179/LIVE-4: ~/.claude/caps/telegram (Kopie) is the stray copy that drives
// the OPS-20 version churn on every daemon start. The directory is user data,
// so the scan has to skip it instead of importing it as the original cap.
func TestSyncCapsFromDisk_StrayCopyDoesNotSupersedeOriginal(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	seedCapMeta(t, s, CapMeta{
		Name:        "telegram",
		Description: "Telegram",
		Version:     7,
		Scripts: []ScriptMeta{{
			Name: "telegram", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo ok",
		}},
	}, "")

	live := capfile.CapFile{
		Name: "telegram", Description: "Telegram", Version: 7,
		Scope: "user", Purpose: "P",
		Scripts: []capfile.Script{{
			Name: "telegram", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo ok",
		}},
	}
	if err := capfile.WriteFile(&live, filepath.Join(dir, "telegram")); err != nil {
		t.Fatal(err)
	}

	stale := live
	stale.Version = 2
	if err := capfile.WriteFile(&stale, filepath.Join(dir, "telegram (Kopie)")); err != nil {
		t.Fatal(err)
	}

	SyncCapsFromDisk(h, dir, "")

	meta := storedCapMeta(t, s, "telegram")
	if meta == nil {
		t.Fatal("cap disappeared")
	}
	if meta.Version != 7 {
		t.Errorf("Version = %d, want 7 — the stray copy must not supersede the original", meta.Version)
	}

	learnings, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, l := range learnings {
		if m, err := ParseCapMeta(l.Context); err == nil && m.Name == "telegram" {
			active++
		}
	}
	if active != 1 {
		t.Errorf("active telegram caps = %d, want 1", active)
	}
}

// syncLogBuffer collects log output; handleSaveCap spawns a goroutine, so the
// writer is shared across goroutines.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A CAP.md whose content the store already has is not an update, and the sync must
// not report one: an operator restoring from disk reads "synced" as "applied", and
// a log claiming a profile adoption for a save that was refused is worse than no
// log. What counts as "the same" is the content (capUnchanged), not the version.
func TestSyncCapsFromDisk_ReportsAnUnchangedFileAsUnchanged(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	cf := capfile.CapFile{
		Name: "same", Description: "Same", Version: 4, Scope: "user", Purpose: "P",
		Scripts: []capfile.Script{{
			Name: "same", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo ok",
			Sandbox: "standard", AllowedHosts: []string{"api.example.com"},
		}},
	}
	if err := capfile.WriteFile(&cf, filepath.Join(dir, "same")); err != nil {
		t.Fatal(err)
	}
	seedCapMeta(t, s, CapMeta{
		Name: "same", Description: "Same", Version: 4, Scope: "user",
		Scripts: []ScriptMeta{{
			Name: "same", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo ok",
			Sandbox: "standard", AllowedHosts: []string{"api.example.com"},
		}},
	}, "")

	captured := &syncLogBuffer{}
	prev := log.Writer()
	log.SetOutput(captured)
	defer log.SetOutput(prev)

	SyncCapsFromDisk(h, dir, "")

	out := captured.String()
	if strings.Contains(out, "synced same") {
		t.Errorf("an unchanged file must not be reported as synced:\n%s", out)
	}
	if strings.Contains(out, "takes sandbox profile") {
		t.Errorf("an unchanged file adopts no profile — the log must not claim one:\n%s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("the sync must say the file was left alone:\n%s", out)
	}
}

// YM-223 asks for the profile of an existing cap not to change unnoticed, and
// both directions count: a CAP.md that drops the sandbox line strips the stored
// profile only because the merge replaces the whole script.
func TestSyncCapsFromDisk_LogsProfileRemoval(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	cf := capfile.CapFile{
		Name: "lossy", Description: "Lossy", Version: 3, Scope: "user", Purpose: "P",
		Scripts: []capfile.Script{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "echo ok", Schema: `{"type":"object"}`,
		}},
	}
	if err := capfile.WriteFile(&cf, filepath.Join(dir, "lossy")); err != nil {
		t.Fatal(err)
	}
	seedCapMeta(t, s, CapMeta{
		Name: "lossy", Description: "Lossy", Version: 2,
		Scripts: []ScriptMeta{{
			Name: "run", Kind: "tool", Runtime: "bash", Body: "echo ok",
			Schema: `{"type":"object"}`, Sandbox: "standard", AllowedHosts: []string{"api.example.com"},
		}},
	}, "")

	captured := &syncLogBuffer{}
	prev := log.Writer()
	log.SetOutput(captured)
	defer log.SetOutput(prev)

	SyncCapsFromDisk(h, dir, "")

	if !strings.Contains(captured.String(), "loses sandbox profile") {
		t.Errorf("a CAP.md that drops the sandbox line strips the stored profile — the sync must say so:\n%s", captured.String())
	}
}
