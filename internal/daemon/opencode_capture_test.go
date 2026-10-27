package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// v2Session is one canned session row of the stub server below.
type v2Session struct {
	ID       string
	ParentID string
	Agent    string
	Dir      string
	Created  int64 // Unix milliseconds; 0 omits the field
}

// v2StubServer serves the minimal opencode v2 surface the capture path uses, in
// the live response shape. firstMsg maps session ids to their first user
// message text. A request without a directory filter lists every session.
func v2StubServer(t *testing.T, sessions []v2Session, dir string, firstMsg map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"version": "2.0.26-stub", "pid": 1})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		want := r.URL.Query().Get("directory")
		data := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			sessionDir := s.Dir
			if sessionDir == "" {
				sessionDir = dir
			}
			if want != "" && sessionDir != want {
				continue
			}
			row := map[string]any{
				"id": s.ID, "agent": s.Agent, "title": "t",
				"location": map[string]any{"directory": sessionDir},
			}
			if s.ParentID != "" {
				row["parentID"] = s.ParentID
			}
			if s.Created != 0 {
				row["time"] = map[string]any{"created": s.Created}
			}
			data = append(data, row)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /api/session/<id>/message
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 4 && parts[3] == "message" {
			if text, ok := firstMsg[parts[2]]; ok {
				json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]any{{"id": "msg_1", "type": "user", "text": text}},
				})
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// isolateHome gives the test its own HOME and returns it.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserConfigDir and os.UserCacheDir prefer XDG_* over HOME, and a CI runner
	// exports XDG_CONFIG_HOME: without these a test that writes into its temp home
	// would still read the runner's real config and find nothing there.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	return home
}

// stubV2Service points the capture seam at a stub service. HOME is left alone.
func stubV2Service(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := opencodeV2Service
	opencodeV2Service = func() *opencode.Client {
		return opencode.NewClient(opencode.Service{ID: "stub", URL: srv.URL, Password: "secret"})
	}
	t.Cleanup(func() { opencodeV2Service = orig })
}

// withV2Stub points the capture at a stub service. It also isolates HOME so a
// fallback to the session tables can never touch the developer's real
// opencode.db.
func withV2Stub(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	home := isolateHome(t)
	stubV2Service(t, srv)
	return home
}

// withoutV2Service isolates HOME and reports no v2 service — the v1-only case.
func withoutV2Service(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	orig := opencodeV2Service
	opencodeV2Service = func() *opencode.Client { return nil }
	t.Cleanup(func() { opencodeV2Service = orig })
	return home
}

func seedV1SessionDB(t *testing.T, home, id, directory string) {
	t.Helper()
	seedV1SessionDBAt(t, home, id, directory, time.Now().UnixMilli())
}

func seedV1SessionDBAt(t *testing.T, home, id, directory string, created int64) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// parent_id is part of the real v1 schema; the root-only filter needs it.
	sql := "CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER, parent_id TEXT);" +
		fmt.Sprintf("INSERT INTO session VALUES ('%s', '%s', %d, '');", id, directory, created)
	if out, err := exec.Command("sqlite3", filepath.Join(dir, "opencode.db"), sql).CombinedOutput(); err != nil {
		t.Fatalf("seed v1 db: %v (%s)", err, out)
	}
}

// seedSessionDB writes a raw opencode.db fixture under an isolated HOME and
// returns that HOME.
func seedSessionDB(t *testing.T, sql string) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sqlite3", filepath.Join(dir, "opencode.db"), sql).CombinedOutput(); err != nil {
		t.Fatalf("seed db: %v (%s)", err, out)
	}
	return home
}

// TestCaptureOpencodeSessionID_V2BindsBySectionMarker covers the core defect:
// the session the daemon spawned is found through the v2 API, matched by the
// section marker in its first user message, and the subagent session in the
// same directory is ignored.
func TestCaptureOpencodeSessionID_V2BindsBySectionMarker(t *testing.T) {
	wd := "/wd/loop-a"
	now := time.Now()
	sessions := []v2Session{
		{ID: "ses_child", ParentID: "ses_wanted", Agent: "reviewer", Created: now.UnixMilli()},
		{ID: "ses_wanted", Agent: "build", Created: now.UnixMilli()},
	}
	firstMsg := map[string]string{
		"ses_child":  "You are working on project '/p', section 'wanted-section'.",
		"ses_wanted": "You are working on project '/p', section 'wanted-section'.",
	}
	srv := v2StubServer(t, sessions, wd, firstMsg)
	withV2Stub(t, srv)

	if got := captureOpencodeSessionID(wd, "", "wanted-section", "", now, 3*time.Second); got != "ses_wanted" {
		t.Fatalf("expected ses_wanted, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_DoesNotBindThePreviousRun: a section repeats in
// a workdir (scheduled loops, restarts). The marker alone would match the
// earlier run; the creation-time bound must reject it until this run's session
// exists.
func TestCaptureOpencodeSessionID_DoesNotBindThePreviousRun(t *testing.T) {
	wd := "/wd/scheduled"
	now := time.Now()
	sessions := []v2Session{
		// The previous run of the same section, 15 minutes ago.
		{ID: "ses_previous_run", Agent: "build", Created: now.Add(-15 * time.Minute).UnixMilli()},
	}
	firstMsg := map[string]string{
		"ses_previous_run": "You are working on project '/p', section 'scheduled-loop'.",
	}
	srv := v2StubServer(t, sessions, wd, firstMsg)
	withoutV2Fallback(t, srv)

	if got := captureOpencodeSessionID(wd, "", "scheduled-loop", "", now, 600*time.Millisecond); got != "" {
		t.Fatalf("the previous run's session must not be bound, got %q", got)
	}

	// This run's session appears: it wins.
	sessions = append(sessions, v2Session{ID: "ses_this_run", Agent: "build", Created: now.UnixMilli()})
	firstMsg["ses_this_run"] = "You are working on project '/p', section 'scheduled-loop'."
	srv2 := v2StubServer(t, sessions, wd, firstMsg)
	stubV2Service(t, srv2)
	if got := captureOpencodeSessionID(wd, "", "scheduled-loop", "", now, 3*time.Second); got != "ses_this_run" {
		t.Fatalf("expected this run's session, got %q", got)
	}
}

// withoutV2Fallback points the capture at a stub AND disables the session-table
// fallback, so a test can observe "the API found nothing" directly.
func withoutV2Fallback(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubV2Service(t, srv)
	return home
}

// TestCaptureOpencodeSessionID_V2PicksTheLoopNotTheSibling is the ambiguity
// case: two root sessions in the same worktree, disambiguated by section.
func TestCaptureOpencodeSessionID_V2PicksTheLoopNotTheSibling(t *testing.T) {
	wd := "/wd/shared"
	now := time.Now()
	sessions := []v2Session{
		{ID: "ses_sibling", Agent: "build", Created: now.UnixMilli()},
		{ID: "ses_mine", Agent: "build", Created: now.UnixMilli()},
	}
	firstMsg := map[string]string{
		"ses_sibling": "You are working on project '/p', section 'other-section'.",
		"ses_mine":    "You are working on project '/p', section 'my-section'.",
	}
	srv := v2StubServer(t, sessions, wd, firstMsg)
	withV2Stub(t, srv)

	if got := captureOpencodeSessionID(wd, "", "my-section", "", now, 3*time.Second); got != "ses_mine" {
		t.Fatalf("expected ses_mine, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_NoMarkerlessFallbackWithV2Service: when a v2
// service can answer, a marker-less store-wide table scan must not be the second
// opinion. It cannot read the session's messages, so it knows nothing about
// identity — with exactly one root session under the project in the window it
// would bind it regardless of which agent it belongs to.
func TestCaptureOpencodeSessionID_NoMarkerlessFallbackWithV2Service(t *testing.T) {
	srv := v2StubServer(t, nil, "", nil) // reachable, holds no session for the section
	home := withV2Stub(t, srv)
	seedV1SessionDB(t, home, "ses_sibling", "/repo/proj/.worktrees/x")

	if got := captureOpencodeSessionID("", "agent-x", "section-x", "/repo/proj", time.Now().Add(-time.Minute), 700*time.Millisecond); got != "" {
		t.Fatalf("a marker-less table fallback must not bind while a v2 service answers, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_FallsBackToV1WhenNoService: a v1 binary writes
// only to the old `session` table and runs no v2 service. The captured id must
// still come from there.
func TestCaptureOpencodeSessionID_FallsBackToV1WhenNoService(t *testing.T) {
	home := withoutV2Service(t)
	seedV1SessionDB(t, home, "ses_v1", "/wd/legacy")

	if got := captureOpencodeSessionID("/wd/legacy", "", "legacy-section", "", time.Now().Add(-time.Minute), 3*time.Second); got != "ses_v1" {
		t.Fatalf("expected ses_v1 from the table fallback, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_FallsBackToV1WhenV2HasNoMatch: the v2 service is
// reachable but the session was created by a v1 binary.
func TestCaptureOpencodeSessionID_FallsBackToV1WhenV2HasNoMatch(t *testing.T) {
	srv := v2StubServer(t, nil, "/wd/legacy", nil)
	home := withV2Stub(t, srv)
	seedV1SessionDB(t, home, "ses_v1", "/wd/legacy")

	if got := captureOpencodeSessionID("/wd/legacy", "", "legacy-section", "", time.Now().Add(-time.Minute), 700*time.Millisecond); got != "ses_v1" {
		t.Fatalf("expected ses_v1 from the table fallback, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_EmptyWhenNothingMatches: no false positives —
// an unknown section must not bind to some other session in the directory.
func TestCaptureOpencodeSessionID_EmptyWhenNothingMatches(t *testing.T) {
	srv := v2StubServer(t, []v2Session{{ID: "ses_other", Agent: "build", Created: time.Now().UnixMilli()}}, "/wd/a",
		map[string]string{"ses_other": "unrelated"})
	withV2Stub(t, srv)

	if got := captureOpencodeSessionID("/wd/a", "", "never-spawned", "", time.Now(), 700*time.Millisecond); got != "" {
		t.Fatalf("expected empty capture, got %q", got)
	}
}

// TestCaptureOpencodeSessionID_DeadServiceFallsThroughToV1: a registered but
// unreachable service must not block capture.
func TestCaptureOpencodeSessionID_DeadServiceFallsThroughToV1(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	orig := opencodeV2Service
	opencodeV2Service = func() *opencode.Client {
		return opencode.NewClient(opencode.Service{ID: "dead", URL: "http://127.0.0.1:1", Password: "x"})
	}
	t.Cleanup(func() { opencodeV2Service = orig })
	seedV1SessionDB(t, home, "ses_v1", "/wd/dead")

	if got := captureOpencodeSessionID("/wd/dead", "", "section", "", time.Now().Add(-time.Minute), 700*time.Millisecond); got != "ses_v1" {
		t.Fatalf("expected table fallback, got %q", got)
	}
}

// TestOpencodeSessionDir_FindsV2Session: resume bootstrap resolves a bare ses_
// id to its directory. v2 sessions exist only in session_v2, so a
// `session`-only lookup can never bootstrap a v2 session.
func TestOpencodeSessionDir_FindsV2Session(t *testing.T) {
	seedSessionDB(t, "CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, time_updated INTEGER);"+
		"INSERT INTO session_v2 VALUES ('ses_x', '/wd/v2', 1);")

	if got := opencodeSessionDir("ses_x"); got != "/wd/v2" {
		t.Fatalf("expected /wd/v2 from session_v2, got %q", got)
	}
}

// TestOpencodeRootSessionIDsInDir_ReturnsOnlyRoots: the cwd fallback must match
// agents to sessions, so subagent sessions must not appear as candidates.
func TestOpencodeRootSessionIDsInDir_ReturnsOnlyRoots(t *testing.T) {
	seedSessionDB(t,
		"CREATE TABLE session_v2 (id TEXT PRIMARY KEY, directory TEXT, parent_id TEXT, time_updated INTEGER);"+
			"INSERT INTO session_v2 VALUES ('ses_root', '/wd/a', '', 1);"+
			"INSERT INTO session_v2 VALUES ('ses_child', '/wd/a', 'ses_root', 2);"+
			"INSERT INTO session_v2 VALUES ('ses_elsewhere', '/wd/b', '', 3);")

	got := opencodeRootSessionIDsInDir(context.Background(), "/wd/a")
	if len(got) != 1 || got[0] != "ses_root" {
		t.Fatalf("expected only the root session in /wd/a, got %v", got)
	}
	if ids := opencodeRootSessionIDsInDir(context.Background(), "/wd/none"); len(ids) != 0 {
		t.Fatalf("expected no sessions for an unknown directory, got %v", ids)
	}
}

// TestOpencodeRootSessionIDsInDir_KeepsPartialRows: one unreadable session table
// must not discard the ids the healthy table already returned. The caller uses
// these ids to bind an agent to its session — a dropped row is a lost binding.
func TestOpencodeRootSessionIDsInDir_KeepsPartialRows(t *testing.T) {
	// `session` holds a valid row; `session_v2` exists but lacks `directory`, so
	// its probe fails with "no such column".
	seedSessionDB(t,
		"CREATE TABLE session (id TEXT, directory TEXT, parent_id TEXT);"+
			"INSERT INTO session VALUES ('ses_v1_root', '/wd/partial', '');"+
			"CREATE TABLE session_v2 (id TEXT, other TEXT);")

	got := opencodeRootSessionIDsInDir(context.Background(), "/wd/partial")
	if len(got) != 1 || got[0] != "ses_v1_root" {
		t.Fatalf("partial rows dropped: got %v, want [ses_v1_root]", got)
	}
}

// TestOpencodeChildSessionActive_FindsV2Child: the relay gate must see v2
// subagents, otherwise relays are injected while a task() subagent runs and
// flood the agent afterwards.
func TestOpencodeChildSessionActive_FindsV2Child(t *testing.T) {
	restoreRealOpencodeChildActive(t)
	now := time.Now().UnixMilli()
	stale := now - (opencodeActiveChildWindow + time.Minute).Milliseconds()
	sql := "CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, time_updated INTEGER);" +
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_live', 'ses_parent', %d);", now) +
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_stale', 'ses_parent', %d);", stale) +
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_other', 'ses_elsewhere', %d);", now)
	seedSessionDB(t, sql)

	if !opencodeChildActiveFn("ses_parent") {
		t.Error("expected an active v2 child of ses_parent to be detected")
	}
	if !opencodeChildActiveFn("ses_elsewhere") {
		t.Error("expected the active v2 child of ses_elsewhere to be detected")
	}
	if opencodeChildActiveFn("ses_absent") {
		t.Error("expected no active child for an unrelated parent")
	}
}

// TestOpencodeChildSessionActive_StaleV2ChildIsInactive pins the freshness
// window on the v2 table.
func TestOpencodeChildSessionActive_StaleV2ChildIsInactive(t *testing.T) {
	restoreRealOpencodeChildActive(t)
	stale := time.Now().UnixMilli() - (opencodeActiveChildWindow + time.Minute).Milliseconds()
	seedSessionDB(t, "CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, time_updated INTEGER);"+
		fmt.Sprintf("INSERT INTO session_v2 VALUES ('ses_stale', 'ses_parent', %d);", stale))

	if opencodeChildActiveFn("ses_parent") {
		t.Error("a v2 child outside the freshness window must read as inactive")
	}
}

// TestQuerySessionTables_ReadFailureIsNotNotFound: a genuine read error must
// surface as an error, not as "the row does not exist" — the DONE-guard treats
// the latter as an agent fault and would reject a compliant agent.
func TestQuerySessionTables_ReadFailureIsNotNotFound(t *testing.T) {
	ctx := context.Background()
	// A file that exists but is not a SQLite database.
	bad := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := queryFirstSessionRow(ctx, bad, func(table string) string { return "SELECT 1 FROM " + table })
	if err == nil {
		t.Fatal("expected an infrastructure error for an unreadable database")
	}
	if _, err := queryFirstSessionRow(ctx, "", func(table string) string { return "SELECT 1" }); err == nil {
		t.Fatal("expected an error for an empty database path")
	}
}

// TestQuerySessionTables_KeepsRowsFromTheHealthyTable: one broken table must not
// discard a valid answer that the other table already produced. The partial
// failure is still reported, so a caller that needs a complete read (a
// uniqueness test) can tell the difference.
func TestQuerySessionTables_KeepsRowsFromTheHealthyTable(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	// `session` exists with a row; `session_v2` exists but lacks the column the
	// query asks for, so its probe fails with "no such column".
	home := seedSessionDB(t,
		"CREATE TABLE session (id TEXT, directory TEXT);"+
			"INSERT INTO session VALUES ('ses_v1_row', '/wd/x');"+
			"CREATE TABLE session_v2 (id TEXT, other TEXT);")
	t.Setenv("HOME", home)

	dbPath := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	query := func(table string) string {
		return "SELECT id FROM " + table + " WHERE directory = '/wd/x' LIMIT 1"
	}
	rows, err := querySessionTables(context.Background(), dbPath, query)
	if err == nil {
		t.Error("a partially failed read must be reported, not hidden")
	}
	if len(rows) != 1 || rows[0] != "ses_v1_row" {
		t.Fatalf("expected the v1 row to survive, got %v", rows)
	}
	// A found row still wins for the single-row helper: the answer is real.
	row, err := queryFirstSessionRow(context.Background(), dbPath, query)
	if err != nil || row != "ses_v1_row" {
		t.Fatalf("expected ses_v1_row without an error, got %q / %v", row, err)
	}
}

// TestParseAgentCreatedAt_AcceptsProductionFormat: AgentCreate writes RFC3339,
// so a parser that only knows the legacy space format would silently leave the
// capture bound unanchored and make the heartbeat's late bind a no-op.
func TestParseAgentCreatedAt_AcceptsProductionFormat(t *testing.T) {
	rfc, ok := parseAgentCreatedAt("2026-10-15T17:20:51+02:00")
	if !ok {
		t.Fatal("the RFC3339 stamp AgentCreate writes must parse")
	}
	if rfc.Year() != 2026 || rfc.Hour() != 17 || rfc.Minute() != 20 {
		t.Fatalf("unexpected parse result %v", rfc)
	}
	legacy, ok := parseAgentCreatedAt("2026-06-18 21:31:03")
	if !ok {
		t.Fatal("legacy space-format rows must still parse")
	}
	if legacy.Year() != 2026 || legacy.Month() != time.June {
		t.Fatalf("unexpected legacy parse result %v", legacy)
	}
	if _, ok := parseAgentCreatedAt(""); ok {
		t.Error("an empty stamp must not parse")
	}
	if _, ok := parseAgentCreatedAt("garbage"); ok {
		t.Error("a garbage stamp must not parse")
	}
}

// newRestartableAgent stores an errored, permanently-restartable opencode
// agent and returns it.
func newRestartableAgent(t *testing.T, h *Handler, project, section string) *storage.Agent {
	t.Helper()
	return newRestartableAgentInDir(t, h, project, section, "")
}

// newRestartableAgentInDir is newRestartableAgent with a recorded work_dir, the
// directory a session bind is scoped by.
func newRestartableAgentInDir(t *testing.T, h *Handler, project, section, workDir string) *storage.Agent {
	t.Helper()
	id, err := h.store.AgentNextID(project)
	if err != nil {
		t.Fatal(err)
	}
	agent := storage.Agent{
		ID: id, Project: project, Section: section,
		SessionID: "11111111-2222-3333-4444-555555555555",
		Status:    "error", Backend: "opencode",
		SockPath:        "/tmp/does-not-exist.sock",
		RestartStrategy: "permanent", MaxRestarts: 3,
		WorkDir: workDir,
		// The format AgentCreate writes in production — using the legacy
		// space format here once hid a broken timestamp parser.
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := h.store.AgentCreate(agent); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.AgentGet(id)
	if err != nil || got == nil {
		t.Fatalf("agent %s not stored: %v", id, err)
	}
	return got
}

// TestAttemptRestart_BindsLateOpencodeSession: the heartbeat must try to bind an
// agent whose session id capture failed before declaring it unresumable. The
// session is found by its section marker inside the agent's own work_dir — the
// recorded directory is what makes a marker-less legacy session identifiable.
func TestAttemptRestart_BindsLateOpencodeSession(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const wd = "/wd/loop-late"
	agent := newRestartableAgentInDir(t, h, "latebind", "loop-late", wd)

	srv := v2StubServer(t,
		[]v2Session{{ID: "ses_late", Agent: "build", Created: time.Now().UnixMilli()}},
		wd,
		map[string]string{"ses_late": "You are working on project '/p', section '" + agent.Section + "'."})
	stubV2Service(t, srv)

	origTimeout := opencodeLateBindTimeout
	opencodeLateBindTimeout = 2 * time.Second
	t.Cleanup(func() { opencodeLateBindTimeout = origTimeout })

	h.attemptRestart()

	got, err := h.store.AgentGet(agent.ID)
	if err != nil || got == nil {
		t.Fatalf("agent gone: %v", err)
	}
	if got.OpencodeSessionID != "ses_late" {
		t.Fatalf("expected the session to be bound late, got %q", got.OpencodeSessionID)
	}
	if got.Status == "stopped" {
		t.Fatalf("a bindable agent must not be stopped as unresumable (progress=%q)", got.Progress)
	}
}

// TestAttemptRestart_DoesNotGuessForADirlessAgent: an agent row from before
// work_dir was recorded has no directory to scope the scan with. A session that
// carries only the section marker is then indistinguishable from a sibling
// agent's session of the same section, so the late bind declines and the agent
// keeps its previous fate — being stopped is recoverable, a foreign
// conversation is not.
func TestAttemptRestart_DoesNotGuessForADirlessAgent(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	agent := newRestartableAgent(t, h, "latebind-dirless", "loop-shared")

	srv := v2StubServer(t,
		[]v2Session{{ID: "ses_sibling", Agent: "build", Created: time.Now().UnixMilli()}},
		"",
		map[string]string{"ses_sibling": "You are working on project '/p', section '" + agent.Section + "'."})
	stubV2Service(t, srv)

	origTimeout := opencodeLateBindTimeout
	opencodeLateBindTimeout = 500 * time.Millisecond
	t.Cleanup(func() { opencodeLateBindTimeout = origTimeout })

	h.attemptRestart()

	got, err := h.store.AgentGet(agent.ID)
	if err != nil || got == nil {
		t.Fatalf("agent gone: %v", err)
	}
	if got.OpencodeSessionID != "" {
		t.Fatalf("a directory-less scan must not adopt a legacy session, got %q", got.OpencodeSessionID)
	}
	if got.Status != "stopped" {
		t.Fatalf("expected the agent to be stopped, got status %q", got.Status)
	}
}

// TestAttemptRestart_LateBindIgnoresPreviousRun: the late bind must not adopt a
// session that predates the agent — that would resume a foreign conversation.
func TestAttemptRestart_LateBindIgnoresPreviousRun(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	agent := newRestartableAgent(t, h, "latebind2", "loop-repeated")

	srv := v2StubServer(t,
		[]v2Session{{ID: "ses_previous", Agent: "build", Created: time.Now().Add(-15 * time.Minute).UnixMilli()}},
		"",
		map[string]string{"ses_previous": "You are working on project '/p', section '" + agent.Section + "'."})
	stubV2Service(t, srv)

	origTimeout := opencodeLateBindTimeout
	opencodeLateBindTimeout = 500 * time.Millisecond
	t.Cleanup(func() { opencodeLateBindTimeout = origTimeout })

	h.attemptRestart()

	got, err := h.store.AgentGet(agent.ID)
	if err != nil || got == nil {
		t.Fatalf("agent gone: %v", err)
	}
	if got.OpencodeSessionID != "" {
		t.Fatalf("a session from an earlier run must not be bound, got %q", got.OpencodeSessionID)
	}
	if got.Status != "stopped" {
		t.Fatalf("expected the agent to be stopped, got status %q", got.Status)
	}
}

// TestAttemptRestart_StopsUnbindableOpencodeAgent: without any discoverable
// session the agent keeps its previous fate — stopped, not resumed with the
// daemon UUID (opencode exits 1 on that, Learning #80228).
func TestAttemptRestart_StopsUnbindableOpencodeAgent(t *testing.T) {
	h, _ := mustHandler(t)
	agent := newRestartableAgent(t, h, "unbindable", "loop-gone")
	withoutV2Service(t)

	origTimeout := opencodeLateBindTimeout
	opencodeLateBindTimeout = 100 * time.Millisecond
	t.Cleanup(func() { opencodeLateBindTimeout = origTimeout })

	h.attemptRestart()

	got, err := h.store.AgentGet(agent.ID)
	if err != nil || got == nil {
		t.Fatalf("agent gone: %v", err)
	}
	if got.Status != "stopped" {
		t.Fatalf("expected the agent to be stopped, got status %q", got.Status)
	}
}

// TestLiveCaptureOpencodeSessionID is the daemon-side acceptance proof: the
// exact function the spawn path calls resolves the real session id of a
// previously-failing agent, using the live services (read-only GETs only).
// Opt-in: set YESMEM_LIVE_OPENCODE_E2E=1.
func TestLiveCaptureOpencodeSessionID(t *testing.T) {
	if os.Getenv("YESMEM_LIVE_OPENCODE_E2E") == "" {
		t.Skip("set YESMEM_LIVE_OPENCODE_E2E=1 to run against the live opencode services")
	}
	const (
		workDir = "/home/tester/projects/opencode/.worktrees/v2-code-fence-default"
		section = "yesloop-v2-code-fence-default"
		wantID  = "ses_ec0302557ffe0NPvG6GRwmq7Qd"
	)
	// Derive the bound from the session itself rather than hardcoding a clock
	// value: the capture window is [spawn-slack, spawn+window], so a wrong
	// constant silently tests nothing.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := opencode.FirstLiveService(home)
	if err != nil {
		t.Fatalf("no live opencode v2 service: %v", err)
	}
	truth, err := opencode.NewClient(svc).GetSession(context.Background(), wantID)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", wantID, err)
	}

	created := time.UnixMilli(truth.Created)
	if got := captureOpencodeSessionID(workDir, "", section, "/home/tester/projects/opencode", created, 10*time.Second); got != wantID {
		t.Fatalf("captured %q, want %q", got, wantID)
	}
	// A bound after the session's creation must not return THIS session: the
	// capture really is anchored to the agent's spawn time. (It may legitimately
	// find a later run of the same section in the same workdir — sections do
	// repeat, which is exactly why the bound exists.)
	if got := captureOpencodeSessionID(workDir, "", section, "/home/tester/projects/opencode", created.Add(time.Hour), 2*time.Second); got == wantID {
		t.Fatalf("a bound after the session's creation must not return it, got %q", got)
	}
}
