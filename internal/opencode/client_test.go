package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSession is one canned session row of the stub server.
type TestSession struct {
	ID       string
	ParentID string
	Agent    string
	Dir      string
	Created  int64 // Unix milliseconds; 0 omits the field
}

// newTestServer serves a canned v2 API surface in the LIVE response shape
// (location.directory, time.created) so the decoding path is exercised too.
func newTestServer(t *testing.T, sessions []TestSession, firstMsg map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"version": "2.0.26-test", "pid": 1})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		dir := r.URL.Query().Get("directory")
		data := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			if dir != "" && s.Dir != dir {
				continue
			}
			row := map[string]any{
				"id": s.ID, "agent": s.Agent, "title": "t",
				"location": map[string]any{"directory": s.Dir},
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
		if !checkAuth(w, r) {
			return
		}
		// /api/session/<id>/message
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 4 && parts[0] == "api" && parts[1] == "session" && parts[3] == "message" {
			if text, ok := firstMsg[parts[2]]; ok {
				json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]any{{"id": "msg_1", "type": "user", "text": text}},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok || u != "opencode" || p != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"_tag": "UnauthorizedError", "message": "Authentication required"})
		return false
	}
	return true
}

func testClient(srv *httptest.Server) *Client {
	return NewClient(Service{ID: "svc-1", URL: srv.URL, Password: "secret"})
}

func TestServiceDiscovery_ReadsRegistrationFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "state", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("service.json", `{"id":"a","version":"2.0.26","url":"http://127.0.0.1:49374","pid":1,"password":"p1"}`)
	write("service-yesloop.json", `{"id":"b","version":"0.0.0-x","url":"http://localhost:11979","pid":2,"password":"p2"}`)
	// Not a service registration — must be ignored.
	write("model.json", `{"model":"x"}`)

	svcs := DiscoverServices(home)
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(svcs), svcs)
	}
	for _, s := range svcs {
		if s.URL == "" || s.Password == "" {
			t.Errorf("service %q missing url/password: %+v", s.ID, s)
		}
	}
}

// TestServiceDiscovery_RejectsNonLoopback: the registration file decides where
// the daemon sends the service password. A remote or non-http URL must never
// be used.
func TestServiceDiscovery_RejectsNonLoopback(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "state", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, url := range []string{
		"http://evil.example.com:49374",
		"http://10.0.0.5:49374",
		"https://127.0.0.1:49374",
		"ftp://127.0.0.1:49374",
		"not-a-url",
		"",
		// Userinfo would be echoed by any caller that logs URL().
		"http://user:pw@127.0.0.1:49374",
		"http://127.0.0.1.evil.example.com:49374",
	} {
		body := `{"id":"bad","url":"` + url + `","password":"p"}`
		if err := os.WriteFile(filepath.Join(dir, "service-bad"+string(rune('0'+i))+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := DiscoverServices(home); len(got) != 0 {
		t.Fatalf("expected no usable services, got %+v", got)
	}
	// A loopback IPv4 and IPv6 registration are accepted.
	for name, url := range map[string]string{"service-ok4.json": "http://127.0.0.1:1", "service-ok6.json": "http://[::1]:1"} {
		body := `{"id":"ok","url":"` + url + `","password":"p"}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := DiscoverServices(home); len(got) != 2 {
		t.Fatalf("expected the loopback registrations to be accepted, got %+v", got)
	}
}

func TestInfo_ProbesHealth(t *testing.T) {
	srv := newTestServer(t, nil, nil)
	info, err := testClient(srv).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Version != "2.0.26-test" {
		t.Errorf("expected version from /api/info, got %q", info.Version)
	}
}

func TestInfo_RejectsBadPassword(t *testing.T) {
	srv := newTestServer(t, nil, nil)
	c := NewClient(Service{ID: "svc-1", URL: srv.URL, Password: "wrong"})
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("expected an error for a wrong password, got nil")
	}
}

// TestInfo_ErrorNeverContainsPassword: the error text is logged by the daemon,
// so the credential must not be able to reach it.
func TestInfo_ErrorNeverContainsPassword(t *testing.T) {
	srv := newTestServer(t, nil, nil)
	const secret = "sup3r-secret-password"
	c := NewClient(Service{ID: "svc-1", URL: srv.URL, Password: secret})
	_, err := c.Info(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text leaks the password: %q", err.Error())
	}
}

// TestInfo_ErrorBodyIsTruncated: a service controls its own response body; an
// unbounded echo would flood the log and could forge log lines.
func TestInfo_ErrorBodyIsTruncated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("[heartbeat] forged line\n" + strings.Repeat("A", 4096) + "\nmore"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := NewClient(Service{ID: "s", URL: srv.URL, Password: "x"}).Info(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if len(msg) > 400 {
		t.Errorf("error text must be truncated, got %d bytes", len(msg))
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("error text must not carry newlines, got %q", msg)
	}
}

func TestListSessions_FiltersByDirectory(t *testing.T) {
	sessions := []TestSession{
		{ID: "ses_root", Dir: "/wd/a", Agent: "build", Created: time.Now().UnixMilli()},
		{ID: "ses_other", Dir: "/wd/b", Agent: "build"},
	}
	srv := newTestServer(t, sessions, nil)
	got, err := testClient(srv).ListSessions(context.Background(), "/wd/a", 20)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ses_root" {
		t.Fatalf("expected only ses_root for /wd/a, got %+v", got)
	}
	if got[0].Directory != "/wd/a" {
		t.Errorf("nested location.directory must be decoded, got %q", got[0].Directory)
	}
	if got[0].Created == 0 {
		t.Error("nested time.created must be decoded")
	}
}

// findRoot runs the marker match the way the capture does and fails on error.
func findRoot(t *testing.T, c *Client, dir, section string, since, until time.Time) string {
	t.Helper()
	got, err := c.FindSession(context.Background(), MarkerQuery{
		Directory: dir, Section: section, Since: since, Until: until,
	})
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	return got
}

func TestRootSessionByMarker_PicksRootBySectionMarker(t *testing.T) {
	wd := "/wd/a"
	now := time.Now().UnixMilli()
	sessions := []TestSession{
		// Subagent of some parent — must be ignored even if the marker matches.
		{ID: "ses_child", Dir: wd, ParentID: "ses_parent", Agent: "general", Created: now},
		// Wrong section — must be ignored.
		{ID: "ses_other_section", Dir: wd, Agent: "build", Created: now},
		// The session we spawned.
		{ID: "ses_wanted", Dir: wd, Agent: "build", Created: now},
	}
	first := map[string]string{
		"ses_child":         "some prompt with section 'wanted-section'",
		"ses_other_section": "You are working on project '/p', section 'other-section'.",
		"ses_wanted":        "You are working on project '/p', section 'wanted-section'.",
	}
	srv := newTestServer(t, sessions, first)
	if got := findRoot(t, testClient(srv), wd, "wanted-section", time.Time{}, time.Time{}); got != "ses_wanted" {
		t.Fatalf("expected ses_wanted, got %q", got)
	}
}

// TestRootSessionByMarker_RejectsSessionFromAnEarlierRun is the guard against
// binding the previous run's session: sections repeat in a workdir (scheduled
// loops restart on a cadence), and the marker alone cannot tell them apart.
func TestRootSessionByMarker_RejectsSessionFromAnEarlierRun(t *testing.T) {
	wd := "/wd/loop"
	now := time.Now()
	spawn := now.UnixMilli()
	sessions := []TestSession{
		// The previous run of the same section, 15 minutes ago.
		{ID: "ses_previous_run", Dir: wd, Agent: "build", Created: now.Add(-15 * time.Minute).UnixMilli()},
	}
	first := map[string]string{
		"ses_previous_run": "You are working on project '/p', section 'scheduled-loop'.",
	}
	srv := newTestServer(t, sessions, first)

	if got := findRoot(t, testClient(srv), wd, "scheduled-loop", time.UnixMilli(spawn), time.Time{}); got != "" {
		t.Fatalf("a session older than the spawn must not be bound, got %q", got)
	}

	// Once this run's session exists, it wins.
	sessions = append(sessions, TestSession{ID: "ses_this_run", Dir: wd, Agent: "build", Created: spawn + 2000})
	first["ses_this_run"] = "You are working on project '/p', section 'scheduled-loop'."
	srv2 := newTestServer(t, sessions, first)
	if got := findRoot(t, testClient(srv2), wd, "scheduled-loop", time.UnixMilli(spawn), time.Time{}); got != "ses_this_run" {
		t.Fatalf("expected this run's session, got %q", got)
	}
}

// TestRootSessionByMarker_RejectsSessionFromALaterRun: the store-wide late-bind
// scan has no directory to narrow it, so a *later* run of the same section must
// be excluded by the upper bound.
func TestRootSessionByMarker_RejectsSessionFromALaterRun(t *testing.T) {
	now := time.Now()
	agentCreated := now.Add(-30 * time.Minute)
	sessions := []TestSession{
		// A later run of the same section, well after this agent's window.
		{ID: "ses_later_run", Dir: "/wd/other", Agent: "build", Created: now.UnixMilli()},
	}
	first := map[string]string{
		"ses_later_run": "You are working on project '/p', section 'shared-section'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findRoot(t, testClient(srv), "", "shared-section",
		agentCreated.Add(-90*time.Second), agentCreated.Add(10*time.Minute))
	if got != "" {
		t.Fatalf("a session from a later run must not be bound, got %q", got)
	}
}

// TestRootSessionByMarker_RejectsUndatedSession: without a creation time a
// session cannot be shown to belong to this spawn, so it must not be bound.
func TestRootSessionByMarker_RejectsUndatedSession(t *testing.T) {
	sessions := []TestSession{{ID: "ses_undated", Dir: "/wd/a", Agent: "build"}}
	first := map[string]string{"ses_undated": "section 'sec'"}
	srv := newTestServer(t, sessions, first)

	if got := findRoot(t, testClient(srv), "/wd/a", "sec", time.Now(), time.Time{}); got != "" {
		t.Fatalf("an undated session must not be bound when a bound is given, got %q", got)
	}
	// Without any bound an undated session is still usable (no time filter).
	if got := findRoot(t, testClient(srv), "/wd/a", "sec", time.Time{}, time.Time{}); got != "ses_undated" {
		t.Fatalf("expected the undated session to match without a bound, got %q", got)
	}
}

// TestRootSessionByMarker_RejectsForeignDirectory: a session the server reports
// in another directory is not ours, even if it carries the marker.
func TestRootSessionByMarker_RejectsForeignDirectory(t *testing.T) {
	now := time.Now().UnixMilli()
	// The stub only returns rows whose Dir matches the query, so this row is
	// served under the requested directory but reports a different one — the
	// shape a hostile or buggy service would produce.
	sessions := []TestSession{{ID: "ses_foreign", Dir: "/elsewhere", Agent: "build", Created: now}}
	first := map[string]string{"ses_foreign": "section 'sec'"}
	srv := newTestServer(t, sessions, first)

	if got := findRoot(t, testClient(srv), "/wd/a", "sec", time.Time{}, time.Time{}); got != "" {
		t.Fatalf("a session reported in another directory must not be bound, got %q", got)
	}
}

func TestRootSessionByMarker_NoMatchIsNotAnError(t *testing.T) {
	srv := newTestServer(t, []TestSession{{ID: "ses_x", Dir: "/wd/a", Agent: "build"}},
		map[string]string{"ses_x": "unrelated prompt"})
	if got := findRoot(t, testClient(srv), "/wd/a", "missing-section", time.Time{}, time.Time{}); got != "" {
		t.Fatalf("expected empty id, got %q", got)
	}
}

// findSession runs one query and fails on error, for the tests that need the
// full MarkerQuery (agent marker).
func findSession(t *testing.T, c *Client, q MarkerQuery) string {
	t.Helper()
	got, err := c.FindSession(context.Background(), q)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	return got
}

// TestFindSession_AgentMarkerBeatsSection is the identity fix: two agents share
// a section inside one window, so the section marker alone cannot tell them
// apart — only the agent marker names the owner. The sibling is listed first,
// so a scan that stops at the first section hit binds the wrong session.
func TestFindSession_AgentMarkerBeatsSection(t *testing.T) {
	wd := "/wd/shared"
	now := time.Now()
	sessions := []TestSession{
		{ID: "ses_other_agent", Dir: wd, Agent: "build", Created: now.UnixMilli()},
		{ID: "ses_mine", Dir: wd, Agent: "build", Created: now.Add(time.Second).UnixMilli()},
	}
	first := map[string]string{
		"ses_other_agent": "You are working on project '/p', agent 'agent-20261015-09', section 'shared'.",
		"ses_mine":        "You are working on project '/p', agent 'agent-20261015-08', section 'shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Directory: wd, Section: "shared", AgentID: "agent-20261015-08",
	})
	if got != "ses_mine" {
		t.Fatalf("expected the session carrying this agent's marker, got %q", got)
	}
}

// TestFindSession_RejectsOtherAgentsMarker: this agent's own session does not
// exist yet (the live case: its session had no user message while the sibling's
// lay inside its window). Adopting the sibling would hand the agent a foreign
// conversation on resume, so a foreign agent marker must reject the session.
func TestFindSession_RejectsOtherAgentsMarker(t *testing.T) {
	wd := "/wd/shared"
	now := time.Now()
	sessions := []TestSession{{ID: "ses_other_agent", Dir: wd, Agent: "build", Created: now.UnixMilli()}}
	first := map[string]string{
		"ses_other_agent": "You are working on project '/p', agent 'agent-20261015-09', section 'shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Directory: wd, Section: "shared", AgentID: "agent-20261015-08",
	})
	if got != "" {
		t.Fatalf("a session carrying another agent's marker must not be bound, got %q", got)
	}
}

// TestFindSession_LegacyPicksEarliestInWindow: sessions spawned before the
// agent marker existed carry only the section marker. The server lists by
// update time, so "first listed" is the newest — for a legacy session the
// oldest inside the window is the one this spawn created.
func TestFindSession_LegacyPicksEarliestInWindow(t *testing.T) {
	wd := "/wd/loop"
	spawn := time.Now()
	sessions := []TestSession{
		// Newest update first, as the server orders them.
		{ID: "ses_later", Dir: wd, Agent: "build", Created: spawn.Add(3 * time.Minute).UnixMilli()},
		{ID: "ses_earliest", Dir: wd, Agent: "build", Created: spawn.Add(10 * time.Second).UnixMilli()},
	}
	first := map[string]string{
		"ses_later":    "You are working on project '/p', section 'shared'.",
		"ses_earliest": "You are working on project '/p', section 'shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Directory: wd, Section: "shared", AgentID: "agent-20261015-08",
		Since: spawn, Until: spawn.Add(10 * time.Minute),
	})
	if got != "ses_earliest" {
		t.Fatalf("a legacy scan must pick the earliest session in the window, got %q", got)
	}
}

// TestFindSession_AgentMarkerWinsOverALegacyCandidate: an agent that did bind
// before (its marker is in the session) must not be re-bound to a bare
// section-only session that happens to be older.
func TestFindSession_AgentMarkerWinsOverALegacyCandidate(t *testing.T) {
	wd := "/wd/loop"
	spawn := time.Now()
	sessions := []TestSession{
		{ID: "ses_legacy", Dir: wd, Agent: "build", Created: spawn.Add(5 * time.Second).UnixMilli()},
		{ID: "ses_mine", Dir: wd, Agent: "build", Created: spawn.Add(20 * time.Second).UnixMilli()},
	}
	first := map[string]string{
		"ses_legacy": "You are working on project '/p', section 'shared'.",
		"ses_mine":   "You are working on project '/p', agent 'agent-20261015-08', section 'shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Directory: wd, Section: "shared", AgentID: "agent-20261015-08",
		Since: spawn, Until: spawn.Add(10 * time.Minute),
	})
	if got != "ses_mine" {
		t.Fatalf("the agent marker must win over an older legacy candidate, got %q", got)
	}
}

// TestFindSession_IgnoresLegacyCandidatesWithoutDirectory is the live
// regression case: agent-20261015-08's own session carries no user message at
// all, while the sibling's session of the *same section* (agent-09, started
// three minutes later) carries the section marker and lies inside agent-08's
// window. The row predates work_dir, so the scan has no directory — and a
// section marker cannot tell a sibling of the same section from this agent's
// own session. Binding the sibling is the defect this change exists to remove,
// so the scan declines instead.
func TestFindSession_IgnoresLegacyCandidatesWithoutDirectory(t *testing.T) {
	now := time.Now()
	sessions := []TestSession{
		// The sibling's session: same section, no agent marker (legacy prompt).
		{ID: "ses_sibling", Dir: "/wd/shared-wt", Agent: "build", Created: now.Add(-3 * time.Minute).UnixMilli()},
	}
	first := map[string]string{
		"ses_sibling": "You are working on project '/p', section 'yesloop-shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Section: "yesloop-shared", AgentID: "agent-20261015-08",
		Since: now.Add(-4 * time.Minute), Until: now.Add(10 * time.Minute), Limit: 200,
	})
	if got != "" {
		t.Fatalf("a directory-less scan must not adopt a legacy session, got %q", got)
	}

	// In a known directory the same session is admissible: the directory is the
	// evidence that was missing above.
	if got := findSession(t, testClient(srv), MarkerQuery{
		Directory: "/wd/shared-wt", Section: "yesloop-shared", AgentID: "agent-20261015-08",
		Since: now.Add(-4 * time.Minute), Until: now.Add(10 * time.Minute),
	}); got != "ses_sibling" {
		t.Fatalf("a directory-scoped scan must still bind the legacy session, got %q", got)
	}
}

// TestFindSession_IgnoresBlankSessionWithoutDirectory: a session whose first
// user message is empty carries no evidence at all — not this agent's marker, not
// even the section marker — so it is never bound. Before YM-419 it was a
// candidate inside a known directory; the live 21.10.2026 case showed that
// uniqueness among siblings is not identity, and the rule is gone. This test
// pins the store-wide half of it.
func TestFindSession_IgnoresBlankSessionWithoutDirectory(t *testing.T) {
	spawn := time.Now()
	sessions := []TestSession{
		{ID: "ses_foreign_blank", Dir: "/somewhere/else", Agent: "build", Created: spawn.Add(5 * time.Second).UnixMilli()},
	}
	srv := newTestServer(t, sessions, nil)

	got := findSession(t, testClient(srv), MarkerQuery{
		Section: "shared", AgentID: "agent-20261015-08",
		Since: spawn, Until: spawn.Add(10 * time.Minute), Limit: 200,
	})
	if got != "" {
		t.Fatalf("a marker-less session without a directory scope must not be bound, got %q", got)
	}
}

// TestFindSession_DoesNotBindABlankSessionForAnAgentQuery: a session without a
// user message carries no identity evidence at all, and "the only blank one in
// the workdir" is a state of this instant, not a match. Live case 21.10.2026
// (YM-419): eight agents shared one work_dir, the session whose prompt had not
// been injected yet was the single blank candidate — five sibling captures
// returned it, the first writer kept it, and that agent then carried another
// agent's conversation for good (whoami resolved the wrong row; the column
// cannot be repaired from inside).
//
// The marker is the only per-spawn evidence, so a blank candidate leaves the
// agent unbound. Nothing is lost: the spawn capture keeps polling and the
// on-demand and late binds retry, so the id is bound as soon as the prompt that
// names the agent is in the session. This scan never binds such a session, and a
// capture with a v2 service does not fall back to a message-blind table scan over
// v2 rows either (see opencodeV1OnlyTables); without a service the table scan has
// no messages to read and keeps its own uniqueness rule. This test replaces the
// two blank-rule cases (one blank in the workdir, two ambiguous blanks): neither
// rule survives, because a session without a message is no evidence in either
// situation.
func TestFindSession_DoesNotBindABlankSessionForAnAgentQuery(t *testing.T) {
	wd := "/wd/shared-wt"
	spawn := time.Now()
	sessions := []TestSession{
		{ID: "ses_sibling", Dir: wd, Agent: "build", Created: spawn.Add(4 * time.Minute).UnixMilli()},
		{ID: "ses_blank_sibling", Dir: wd, Agent: "build", Created: spawn.UnixMilli()},
	}
	first := map[string]string{
		"ses_sibling": "You are working on project '/p', agent 'agent-20261015-09', section 'shared'.",
	}
	srv := newTestServer(t, sessions, first)

	got := findSession(t, testClient(srv), MarkerQuery{
		Directory: wd, Section: "shared", AgentID: "agent-20261015-08",
		Since: spawn, Until: spawn.Add(10 * time.Minute),
	})
	if got != "" {
		t.Fatalf("a session without a user message proves no identity and must not be bound, got %q", got)
	}
}

// TestRootSessionByMarker_HonoursContextDeadline: the scan runs inside the
// daemon's heartbeat goroutine, so a slow service must not hold it longer than
// the caller allows.
func TestRootSessionByMarker_HonoursContextDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := testClient(srv).FindSession(ctx, MarkerQuery{Directory: "/wd/a", Section: "sec"}); err == nil {
		t.Fatal("expected a deadline error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("scan exceeded its context deadline: %s", elapsed)
	}
}

func TestFirstUserMessageText_EmptyWhenSessionUnknown(t *testing.T) {
	srv := newTestServer(t, nil, nil)
	got, err := testClient(srv).FirstUserMessageText(context.Background(), "ses_nope")
	if err != nil {
		t.Fatalf("FirstUserMessageText: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty text, got %q", got)
	}
}

func TestGetSession_ReturnsRow(t *testing.T) {
	dir := t.TempDir()
	// A second server whose session row is only reachable via GetSession.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"version": "2.0.26-test"})
	})
	mux.HandleFunc("/api/session/ses_foreign", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": "ses_foreign", "agent": "build", "title": "foreign",
			"location": map[string]any{"directory": dir},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s, err := testClient(srv).GetSession(context.Background(), "ses_foreign")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.ID != "ses_foreign" || s.Directory != dir {
		t.Fatalf("unexpected session %+v", s)
	}
}

// TestClient_RefusesRedirectOffLoopback: the registration is restricted to
// loopback, but the responder could still redirect elsewhere. Refuse before the
// request is made — the client carries a password. The assertion pins the
// guard's own error: a client without CheckRedirect would fail on DNS for the
// unresolvable target and would silently satisfy a bare "expected an error".
func TestClient_RefusesRedirectOffLoopback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/api/info", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := testClient(srv).Info(context.Background())
	if err == nil {
		t.Fatal("expected the off-loopback redirect to be refused")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("expected the redirect guard to refuse, got: %v", err)
	}
}

func TestFirstLiveService_SkipsDeadRegistrations(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "state", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Dead port (nothing listening) + a live test server.
	dead := `{"id":"dead","version":"0.0.0","url":"http://127.0.0.1:1","pid":999999,"password":"p"}`
	if err := os.WriteFile(filepath.Join(dir, "service-dead.json"), []byte(dead), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, nil, nil)
	live := `{"id":"live","version":"2.0.26","url":"` + srv.URL + `","pid":1,"password":"secret"}`
	if err := os.WriteFile(filepath.Join(dir, "service-live.json"), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}

	svc, err := FirstLiveService(home)
	if err != nil {
		t.Fatalf("FirstLiveService: %v", err)
	}
	if svc.ID != "live" {
		t.Fatalf("expected the live service, got %+v", svc)
	}
}

func TestFirstLiveService_NoneRunning(t *testing.T) {
	home := t.TempDir()
	if _, err := FirstLiveService(home); err == nil {
		t.Fatal("expected an error when no service registration exists")
	}
}

// AssistantActivitySince is the idle guard's progress source: it reads only
// assistant turns, newest first, counts the ones completed after the cutoff, and
// reports an unfinished newest turn as work in progress.
func TestAssistantActivitySince_CountsOnlyTurnsAfterTheCutoff(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		gotQuery = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "msg_3", "type": "assistant", "finish": "tool-calls",
				"time": map[string]any{"created": 4500, "completed": 5000}},
			{"id": "msg_2", "type": "user", "time": map[string]any{"created": 4000, "completed": 4000}},
			{"id": "msg_1", "type": "assistant", "finish": "stop",
				"time": map[string]any{"created": 1500, "completed": 2000}},
		}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if got.Since != 1 {
		t.Errorf("Since = %d, want 1 (only the assistant turn completed after the cutoff)", got.Since)
	}
	if got.Working {
		t.Error("Working = true, want false: the newest turn is finished and its tool is not running")
	}
	if q := gotQuery.Get("type"); q != "assistant" {
		t.Errorf("type = %q, want assistant", q)
	}
	if q := gotQuery.Get("order"); q != "desc" {
		t.Errorf("order = %q, want desc (the newest page decides the answer)", q)
	}
	if q := gotQuery.Get("limit"); q == "" {
		t.Error("limit must bound the read")
	}
}

// An open newest turn is the model still writing: the agent is producing right
// now, even though no turn completed.
func TestAssistantActivitySince_OpenNewestTurnIsWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "msg_3", "type": "assistant", "time": map[string]any{"created": 4000, "completed": 0}},
			{"id": "msg_1", "type": "assistant", "time": map[string]any{"created": 1500, "completed": 2000}},
		}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Error("Working = false, want true: the newest turn has no completion time")
	}
}

// A settled turn whose tool still runs is a long command or a subagent: work,
// even though the turn itself is finished.
func TestAssistantActivitySince_RunningToolIsWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "msg_2", "type": "assistant", "finish": "tool-calls",
				"content": []map[string]any{{"type": "tool", "name": "bash",
					"state": map[string]any{"status": "running"}}},
				"time": map[string]any{"created": 4000, "completed": 4500}},
		}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Error("Working = false, want true: a tool of the newest turn is still running")
	}
}

// An old unfinished turn is reported with its own start: the caller bounds the
// signal with that stamp, because the cutoff it passes moves with its own relays
// and cannot say how long the turn has been working (YM-306).
func TestAssistantActivitySince_OldTurnCarriesItsStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "msg_2", "type": "assistant", "time": map[string]any{"created": 1000, "completed": 0}},
			{"id": "msg_1", "type": "assistant", "finish": "tool-calls",
				"content": []map[string]any{{"type": "tool", "name": "bash",
					"state": map[string]any{"status": "running"}}},
				"time": map[string]any{"created": 1000, "completed": 1500}},
		}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Error("Working = false, want true: the newest turn is open")
	}
	if want := time.UnixMilli(1000); !got.WorkingSince.Equal(want) {
		t.Errorf("WorkingSince = %v, want %v (the turn's own start bounds the signal)", got.WorkingSince, want)
	}
	if got.Since != 0 {
		t.Errorf("Since = %d, want 0", got.Since)
	}
}

// A non-assistant row must not steal the "newest" position: the open turn right
// behind it is still the newest assistant turn.
func TestAssistantActivitySince_IgnoresNonAssistantRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "msg_3", "type": "user", "time": map[string]any{"created": 5000, "completed": 5000}},
			{"id": "msg_2", "type": "assistant", "time": map[string]any{"created": 4000, "completed": 0}},
		}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if !got.Working {
		t.Error("Working = false, want true: the newest assistant turn is open")
	}
}

func TestAssistantActivitySince_NoMessagesIsIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	t.Cleanup(srv.Close)

	got, err := testClient(srv).AssistantActivitySince(context.Background(), "ses_1", time.Now())
	if err != nil {
		t.Fatalf("AssistantActivitySince: %v", err)
	}
	if got != (SessionActivity{}) {
		t.Errorf("activity = %+v, want the zero value", got)
	}
}
