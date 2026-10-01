package codescan

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

func TestCBMScanner_Scan(t *testing.T) {
	if FindCBMBinary() == "" {
		t.Skip("codebase-memory-mcp not installed, skipping integration test")
	}

	repo := repoRoot(t)
	// Inline reindexing was removed (decision: yesloop-cbm-load-fix D4) —
	// scanning requires an existing index; the background job indexes projects.
	if !projectIndexed(repo) {
		t.Skipf("project %s not indexed — background indexing required", cbmProjectName(repo))
	}

	scanner := NewCBMScanner()
	// Integration test must not trigger a real background reindex of the
	// live repo when the F2 staleness check deems the index stale.
	scanner.ensureIndexIfStale = nil
	result, err := scanner.Scan(repo)
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	if len(result.Packages) == 0 {
		t.Fatal("expected packages, got 0")
	}
	if result.Stats.FileCount == 0 {
		t.Fatal("expected files, got 0")
	}

	t.Logf("Packages: %d, Files: %d, LOC: %d, Tier: %s",
		len(result.Packages), result.Stats.FileCount, result.Stats.TotalLOC, result.Tier)

	// Check signatures
	sigCount := 0
	for _, pkg := range result.Packages {
		for _, f := range pkg.Files {
			sigCount += len(f.Signatures)
		}
	}
	if sigCount == 0 {
		t.Fatal("expected signatures, got 0")
	}
	t.Logf("Total signatures: %d", sigCount)

	// Verify known package
	found := false
	for _, pkg := range result.Packages {
		if pkg.Name == "internal/proxy" {
			found = true
			if pkg.FileCount == 0 {
				t.Error("internal/proxy should have files")
			}
			t.Logf("internal/proxy: %d files, %d LOC", pkg.FileCount, pkg.TotalLOC)
			break
		}
	}
	if !found {
		t.Error("expected internal/proxy package")
	}
}

func TestCBMProjectName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/tmp/foo/yesmem", "tmp-foo-yesmem"},
		{"/home/user/project", "home-user-project"},
		{"/tmp/test", "tmp-test"},
		// Worktrees must get their own DB — never collapse to parent repo.
		{
			"/tmp/foo/yesmem/.worktrees/briefing-injection",
			"tmp-foo-yesmem-.worktrees-briefing-injection",
		},
	}
	for _, tt := range tests {
		got := cbmProjectName(tt.input)
		if got != tt.want {
			t.Errorf("cbmProjectName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseImportRows(t *testing.T) {
	rows := [][]interface{}{
		{"internal/proxy/proxy.go", "internal/storage"},
		{"internal/proxy/proxy.go", "internal/daemon"},
		{"internal/daemon/daemon.go", "internal/storage"},
	}
	result := parseImportRows(rows)
	if len(result["internal/proxy/proxy.go"]) != 2 {
		t.Errorf("expected 2 imports for proxy.go, got %d", len(result["internal/proxy/proxy.go"]))
	}
	if len(result["internal/daemon/daemon.go"]) != 1 {
		t.Errorf("expected 1 import for daemon.go, got %d", len(result["internal/daemon/daemon.go"]))
	}
}

func TestParseImportRows_Empty(t *testing.T) {
	result := parseImportRows(nil)
	if len(result) != 0 {
		t.Errorf("expected empty map for nil rows, got %d entries", len(result))
	}
}

func TestParseEntryPoints(t *testing.T) {
	rows := [][]interface{}{
		{"main", "main.go"},
		{"main", "cmd/dbstats/main.go"},
	}
	result := parseEntryPoints(rows)
	if len(result) != 2 {
		t.Errorf("expected 2 entry points, got %d", len(result))
	}
	if result[0] != "main.go" {
		t.Errorf("expected main.go first, got %q", result[0])
	}
}

func TestParseEntryPoints_SkipsEmpty(t *testing.T) {
	rows := [][]interface{}{
		{"main", ""},
		{"main", "main.go"},
	}
	result := parseEntryPoints(rows)
	if len(result) != 1 {
		t.Errorf("expected 1 entry point (empty filtered), got %d", len(result))
	}
}

func TestParseTestCoverage(t *testing.T) {
	rows := [][]interface{}{
		{"store_test.go", "store.go"},
		{"store_integration_test.go", "store.go"},
		{"daemon_test.go", "daemon.go"},
	}
	result := parseTestCoverage(rows)
	if result["store.go"] != 2 {
		t.Errorf("expected 2 test files for store.go, got %d", result["store.go"])
	}
	if result["daemon.go"] != 1 {
		t.Errorf("expected 1 test file for daemon.go, got %d", result["daemon.go"])
	}
}

func TestIsBlacklistedPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	tests := []struct {
		path string
		want bool
	}{
		{"", true},
		{"/", true},
		{home, true},
	}
	for _, tt := range tests {
		got := isBlacklistedPath(tt.path)
		if got != tt.want {
			t.Errorf("isBlacklistedPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsBlacklistedPath_NoGit(t *testing.T) {
	dir := t.TempDir()
	if !isBlacklistedPath(dir) {
		t.Error("expected blacklisted for directory without .git")
	}
}

func TestIsBlacklistedPath_NormalRepo(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0755)
	if isBlacklistedPath(dir) {
		t.Error("expected NOT blacklisted for normal git repo")
	}
}

func TestIsBlacklistedPath_WorktreeWithActivity(t *testing.T) {
	root := repoRoot(t)
	if !isWorktree(root) {
		t.Skip("not a git worktree")
	}
	if !hasRecentGitActivity(root) {
		t.Skip("no recent git activity in this worktree")
	}
	if isBlacklistedPath(root) {
		t.Error("expected NOT blacklisted for active worktree")
	}
}

func TestIsBlacklistedPath_StaleWorktree(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /fake/path\n"), 0644)
	if !isWorktree(dir) {
		t.Fatal("expected isWorktree to detect .git file")
	}
	if !isBlacklistedPath(dir) {
		t.Error("expected blacklisted for stale worktree (no git history)")
	}
}

func TestParseChangeCoupling(t *testing.T) {
	rows := [][]interface{}{
		{"proxy.go", "associate.go"},
		{"associate.go", "proxy.go"}, // duplicate in reverse
		{"daemon.go", "extract.go"},
		{"daemon.go", "daemon.go"}, // self-reference
	}
	result := parseChangeCoupling(rows)
	if len(result) != 2 {
		t.Errorf("expected 2 unique pairs (deduped + self-filtered), got %d", len(result))
	}
}

func TestParseKeyFiles(t *testing.T) {
	rows := [][]interface{}{
		{"internal/storage/store.go", "mustOpen", "15"},
		{"internal/storage/store.go", "Open", "10"},
		{"internal/storage/learnings.go", "Get", "5"},
		{"internal/proxy/proxy.go", "mustHandler", "25"},
	}
	result := parseKeyFiles(rows, 1)
	if len(result["internal/storage"]) != 1 {
		t.Errorf("expected 1 key file for storage (topN=1), got %d", len(result["internal/storage"]))
	}
	if result["internal/storage"][0] != "store.go" {
		t.Errorf("expected store.go as top file, got %q", result["internal/storage"][0])
	}
	if result["internal/proxy"][0] != "proxy.go" {
		t.Errorf("expected proxy.go as top file, got %q", result["internal/proxy"][0])
	}
}

func TestParseCount(t *testing.T) {
	tests := []struct {
		input interface{}
		want  int
	}{
		{"15", 15},
		{"0", 0},
		{"", 0},
		{float64(42), 42},
		{float64(0), 0},
		{nil, 0},
	}
	for _, tt := range tests {
		got := parseCount(tt.input)
		if got != tt.want {
			t.Errorf("parseCount(%v) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestDedup(t *testing.T) {
	input := []string{"main.go", "cmd/main.go", "main.go", "routes.go", "cmd/main.go"}
	result := dedup(input)
	if len(result) != 3 {
		t.Errorf("expected 3 unique entries, got %d: %v", len(result), result)
	}
	if result[0] != "main.go" || result[1] != "cmd/main.go" || result[2] != "routes.go" {
		t.Errorf("unexpected order: %v", result)
	}
}

func TestDedup_Empty(t *testing.T) {
	result := dedup(nil)
	if len(result) != 0 {
		t.Errorf("expected empty for nil, got %v", result)
	}
}

func TestParseGitActiveZones(t *testing.T) {
	gitOutput := `internal/proxy/sawtooth.go
internal/proxy/proxy.go
internal/proxy/collapse.go
internal/daemon/handler_state.go
internal/daemon/persona.go
internal/codescan/scanner.go
internal/codescan/render.go
internal/codescan/cbm_scanner.go
internal/codescan/cbm_scanner.go
main.go
`
	zones := parseGitActiveZones(gitOutput)
	if len(zones) == 0 {
		t.Fatal("expected zones, got none")
	}
	// proxy has 3 changes, codescan has 4 (cbm_scanner.go twice)
	if zones[0].Package != "internal/codescan" || zones[0].ChangeCount != 4 {
		t.Errorf("expected internal/codescan with 4 changes first, got %s with %d", zones[0].Package, zones[0].ChangeCount)
	}
	if zones[1].Package != "internal/proxy" || zones[1].ChangeCount != 3 {
		t.Errorf("expected internal/proxy with 3 changes second, got %s with %d", zones[1].Package, zones[1].ChangeCount)
	}
}

func TestParseGitActiveZones_Empty(t *testing.T) {
	zones := parseGitActiveZones("")
	if len(zones) != 0 {
		t.Errorf("expected empty for empty input, got %v", zones)
	}
}

func TestParseGitActiveZones_RootFiles(t *testing.T) {
	gitOutput := "main.go\ncmd_scratchpad.go\n"
	zones := parseGitActiveZones(gitOutput)
	if len(zones) != 1 || zones[0].Package != "." {
		t.Errorf("expected root package '.', got %v", zones)
	}
	if zones[0].ChangeCount != 2 {
		t.Errorf("expected 2 changes, got %d", zones[0].ChangeCount)
	}
}

// writeFakeCBM installs a shell script that mimics the codebase-memory-mcp CLI.
// Every invocation is appended to a call log so tests can assert which CBM
// operations were actually attempted.
func writeFakeCBM(t *testing.T) (binPath, binDir string) {
	t.Helper()
	binDir = t.TempDir()
	binPath = filepath.Join(binDir, "codebase-memory-mcp")
	script := `#!/bin/bash
FAKE_CBM_LOG="${FAKE_CBM_LOG:?}"
FAKE_CBM_BEHAVIOR="${FAKE_CBM_BEHAVIOR:-error}"
echo "$*" >> "$FAKE_CBM_LOG"
case "$*" in
	*"daemon start"*)
		case "${FAKE_CBM_DAEMON_MODE:-permanent}" in
			permanent)
				echo "daemon: started (permanent, pid 123)" ;;
			already_permanent)
				echo "daemon: already active (permanent, pid 123)" ;;
			session)
				echo "daemon: already active (session-managed, pid 123) — it stops with its last session; run daemon stop first if you want a permanent one" ;;
			flip)
				if [ -f "${FAKE_CBM_LOG}.flip" ]; then
					echo "daemon: started (permanent, pid 123)"
				else
					echo "daemon: already active (session-managed, pid 123) — it stops with its last session"
				fi ;;
			warns)
				echo "level=warn msg=daemon.private_dir_group_writable_ancestor mode=0775"
				echo "daemon: started (permanent, pid 123)" ;;
			flip_garbage)
				if [ -f "${FAKE_CBM_LOG}.flip" ]; then
					echo "daemon: something went sideways"
				else
					echo "daemon: already active (session-managed, pid 123) — it stops with its last session"
				fi ;;
		esac
		;;
	*"daemon stop"*)
		touch "${FAKE_CBM_LOG}.flip"
		echo "daemon: stopping (pid 123)" ;;
	*"index_repository"*)
		echo "index_repository" >> "$FAKE_CBM_LOG"
		printf '%s\n' '{"content":[{"type":"text","text":"ok"}]}'
		;;
	*"index_status"*)
		printf '{"content":[{"type":"text","text":"{\\"project\\":\\"p\\",\\"root_path\\":\\"%s\\",\\"status\\":\\"ready\\"}"}]}\n' "$FAKE_CBM_ROOTPATH"
		;;
	*"query_graph"*)
		case "$FAKE_CBM_BEHAVIOR" in
			error)
				exit 1
				;;
			empty)
				printf '%s\n' '{"content":[{"type":"text","text":"rows: 0\n"}]}'
				;;
			ok)
				case "$*" in
					*"CONTAINS_FILE"*)
						printf '%s\n' '{"content":[{"type":"text","text":"rows: 1 (cols: path name loc pkg)\n  main.go main.go 10 internal\n"}]}'
						;;
					*)
						printf '%s\n' '{"content":[{"type":"text","text":"rows: 0\n"}]}'
						;;
				esac
				;;
		esac
		;;
	*)
		printf '%s\n' '{"content":[{"type":"text","text":"rows: 0\n"}]}'
		;;
esac
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, binDir
}

func tempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func readCallLog(t *testing.T, binDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(binDir, "fake-calls.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// EnsureCBMDaemon must report success ONLY when CBM confirms a permanent
// daemon. A `daemon start` that reports an already-active session-managed
// daemon exits 0 but must not count (orchestrator evidence, Learning #94513).
func TestEnsureCBMDaemonStartsPermanent(t *testing.T) {
	bin, _ := writeDaemonFake(t, "permanent")
	if err := EnsureCBMDaemon(bin); err != nil {
		t.Fatalf("EnsureCBMDaemon: %v", err)
	}
}

func TestEnsureCBMDaemonAlreadyPermanent(t *testing.T) {
	bin, _ := writeDaemonFake(t, "already_permanent")
	if err := EnsureCBMDaemon(bin); err != nil {
		t.Fatalf("EnsureCBMDaemon: %v", err)
	}
}

// Session-managed output with rc 0 must trigger the stop+start promotion.
func TestEnsureCBMDaemonPromotesSessionManaged(t *testing.T) {
	bin, logPath := writeDaemonFake(t, "flip")
	if err := EnsureCBMDaemon(bin); err != nil {
		t.Fatalf("EnsureCBMDaemon: %v", err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "daemon stop") {
		t.Fatalf("promotion did not run daemon stop, calls:\n%s", calls)
	}
}

// A session-managed daemon must NOT be stopped while a background index job
// is in flight — the daemon is serving that job's CLI calls.
func TestEnsureCBMDaemonSkipsStopWhileIndexInFlight(t *testing.T) {
	bin, logPath := writeDaemonFake(t, "session")
	root := tempGitRepo(t)
	indexFlightMu.Lock()
	indexFlight[cbmProjectName(root)] = true
	indexFlightMu.Unlock()
	t.Cleanup(func() {
		indexFlightMu.Lock()
		delete(indexFlight, cbmProjectName(root))
		indexFlightMu.Unlock()
	})

	err := EnsureCBMDaemon(bin)
	if !errors.Is(err, ErrDaemonSessionManaged) {
		t.Fatalf("expected ErrDaemonSessionManaged, got %v", err)
	}
	calls, _ := os.ReadFile(logPath)
	if strings.Contains(string(calls), "daemon stop") {
		t.Fatalf("daemon stop ran although index in flight, calls:\n%s", calls)
	}
}

// Any start output that does not confirm a permanent daemon must fail — even
// without an error exit code.
func TestEnsureCBMDaemonRejectsUnknownOutput(t *testing.T) {
	bin, _ := writeDaemonFake(t, "garbage")
	err := EnsureCBMDaemon(bin)
	if err == nil {
		t.Fatal("expected error for non-permanent start output")
	}
	if errors.Is(err, ErrDaemonSessionManaged) {
		t.Fatal("garbage output must not be classified as session-managed")
	}
}

// Real CBM emits warn lines before the status line — classification must
// scan all of the output, not just the first line.
func TestEnsureCBMDaemonWarnLinesBeforeStatus(t *testing.T) {
	bin, _ := writeDaemonFake(t, "warns")
	if err := EnsureCBMDaemon(bin); err != nil {
		t.Fatalf("EnsureCBMDaemon: %v", err)
	}
}

// Garbage output after the promotion restart must NOT be reported as
// session-managed (wrong label would cause a pointless 30s retry loop).
func TestEnsureCBMDaemonPromotionGarbageIsNotSessionManaged(t *testing.T) {
	bin, _ := writeDaemonFake(t, "flip_garbage")
	err := EnsureCBMDaemon(bin)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrDaemonSessionManaged) {
		t.Fatal("garbage after promotion misclassified as session-managed")
	}
}

func writeDaemonFake(t *testing.T, mode string) (binPath, logPath string) {
	t.Helper()
	bin, binDir := writeFakeCBM(t)
	logPath = filepath.Join(binDir, "fake-calls.log")
	t.Setenv("FAKE_CBM_LOG", logPath)
	t.Setenv("FAKE_CBM_DAEMON_MODE", mode)
	return bin, logPath
}

// Scan must not run an inline reindex when the files query fails. A query
// error can mean a timeout — indexing is only allowed when the index is
// actually missing (Trace: yesloop-cbm-load-fix decision 4).
func TestScanQueryErrorDoesNotIndexInline(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAKE_CBM_LOG", filepath.Join(tmp, "fake-calls.log"))
	t.Setenv("FAKE_CBM_BEHAVIOR", "error")
	bin, _ := writeFakeCBM(t)
	root := tempGitRepo(t)

	s := NewCBMScanner()
	s.bin = bin
	_, err := s.Scan(root)
	if err == nil {
		t.Fatal("expected error from files query")
	}

	if calls := readCallLog(t, tmp); strings.Contains(calls, "index_repository") {
		t.Fatalf("Scan ran inline reindex on query error, calls:\n%s", calls)
	}
}

// An empty query result means the index has no data. Scan must not index
// inline either — it returns an error and lets the background job index.
func TestScanEmptyIndexDoesNotIndexInline(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAKE_CBM_LOG", filepath.Join(tmp, "fake-calls.log"))
	t.Setenv("FAKE_CBM_BEHAVIOR", "empty")
	bin, _ := writeFakeCBM(t)
	root := tempGitRepo(t)

	s := NewCBMScanner()
	s.bin = bin
	s.ensureIndex = func(string) {}
	_, err := s.Scan(root)
	if err == nil {
		t.Fatal("expected error for empty index")
	}

	if calls := readCallLog(t, tmp); strings.Contains(calls, "index_repository") {
		t.Fatalf("Scan ran inline reindex on empty index, calls:\n%s", calls)
	}
}

// An empty index must trigger the background indexer exactly once.
func TestScanEmptyIndexTriggersEnsureIndex(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAKE_CBM_LOG", filepath.Join(tmp, "fake-calls.log"))
	t.Setenv("FAKE_CBM_BEHAVIOR", "empty")
	bin, _ := writeFakeCBM(t)
	root := tempGitRepo(t)

	triggered := []string{}
	s := NewCBMScanner()
	s.bin = bin
	s.ensureIndex = func(dir string) { triggered = append(triggered, dir) }
	_, _ = s.Scan(root)

	if len(triggered) != 1 {
		t.Fatalf("expected exactly 1 background index trigger, got %d", len(triggered))
	}
	if triggered[0] != root {
		t.Fatalf("trigger with wrong root: %s", triggered[0])
	}
}

// Concurrent EnsureIndex triggers must collapse into a single index run.
func TestEnsureIndexSingleFlight(t *testing.T) {
	root := tempGitRepo(t)

	var mu sync.Mutex
	var calls int
	original := indexWorker
	indexWorker = func(dir string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	t.Cleanup(func() { indexWorker = original })

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			EnsureIndex(root)
		}()
	}
	wg.Wait()
	// Wait for the background worker to finish: poll until the flight slot is free.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		indexFlightMu.Lock()
		busy := indexFlight[cbmProjectName(root)]
		indexFlightMu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected 1 index run, got %d", calls)
	}
}

// Happy path: a populated index yields a scan result without any indexing.
func TestScanHappyPathNoIndexing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAKE_CBM_LOG", filepath.Join(tmp, "fake-calls.log"))
	t.Setenv("FAKE_CBM_BEHAVIOR", "ok")
	bin, _ := writeFakeCBM(t)
	root := tempGitRepo(t)

	s := NewCBMScanner()
	s.bin = bin
	result, err := s.Scan(root)
	if err != nil {
		t.Fatalf("expected successful scan, got: %v", err)
	}
	if result == nil || len(result.Files) != 1 || result.Files[0].Path != "main.go" {
		t.Fatalf("unexpected scan result: %+v", result)
	}

	if calls := readCallLog(t, tmp); strings.Contains(calls, "index_repository") {
		t.Fatalf("Scan indexed although data existed, calls:\n%s", calls)
	}
}
