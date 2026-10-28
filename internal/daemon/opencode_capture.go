package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// --- opencode v2 session discovery ---
//
// v1 sessions were found by querying opencode.db's `session` table for the
// newest row matching the agent's workDir. That stopped working when opencode
// v2 arrived: v2 writes to `session_v2` and leaves `session` behind, so the
// workDir query never returns anything (capture rate dropped to 0-3/11 per
// day) and agents end up with an empty opencode_session_id — whoami cannot
// resolve them, resume is impossible, and the heartbeat stops them as
// unresumable. See Learning #101594.
//
// v2 exposes a server API that is independent of the table layout, and it can
// also enumerate sessions the daemon did not start. Capture therefore goes
// through the API first, with the session tables as a fallback.

// v2PollInterval is how often the v2 API is re-queried while waiting for the
// spawned TUI to create its session.
const v2PollInterval = 500 * time.Millisecond

// v1FallbackTimeout bounds the table lookup after the v2 API came up empty, so
// a v1-only session is still captured without doubling the wait.
const v1FallbackTimeout = 3 * time.Second

// markerSinceSlack widens the lower time bound of a capture. The agent row is
// written just before the session, and the two clocks are not the same clock,
// so a small window keeps a legitimate session in range. It must stay far below
// the shortest repeat interval of a section: sections do repeat in a workdir
// (scheduled loops, restarts), and binding the previous run's session is the
// failure this bound exists to prevent.
const markerSinceSlack = 90 * time.Second

// markerWindowAfter is the upper time bound of a capture relative to the agent
// row. The TUI creates its session within seconds of spawn, so this is generous
// — but it is what keeps a *later* run of the same section out when the scan
// has no directory to narrow it.
const markerWindowAfter = 10 * time.Minute

// lateBindScanLimit is how many sessions the late bind inspects. It has no
// directory to filter by, so the scan is a store-wide window ordered by update
// time and must reach further back than the directory-scoped scan.
const lateBindScanLimit = 200

// opencodeSpawnCaptureTimeout is how long the spawn-time capture waits for the
// TUI to create its session. Measured on 2026-10-15: sessions appeared 45-114 s
// after the agent row, so the previous 30 s budget gave up before the session
// existed — every spawn of that day started unbound and therefore unresumable.
// Variable so tests do not wait it out.
var opencodeSpawnCaptureTimeout = 150 * time.Second

// opencodeLateBindTimeout bounds the second capture attempt the heartbeat makes
// for an agent whose spawn-time capture failed. It runs inside the shared
// heartbeat goroutine, so it stays short. Variable so tests do not wait it out.
var opencodeLateBindTimeout = 2 * time.Second

// opencodeV2Service is the service-discovery seam. It returns a client for a
// live v2 service, or nil when none is running (a v1-only install) — in which
// case capture must not spend its budget on the API. Variable so tests can
// point it at a stub server.
var opencodeV2Service = func() *opencode.Client {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	svc, err := opencode.FirstLiveService(home)
	if err != nil {
		return nil
	}
	return opencode.NewClient(svc)
}

// captureOpencodeSessionID resolves the opencode session id of an agent, given
// the directory it may have run in (empty = unknown, searched store-wide under
// projectDir), its agent id, its section, and the moment its agent row was
// created.
//
// The v2 API is asked first: it is queried by directory and the candidate is
// matched by the spawn markers in its first user message — `agent '<agent-id>'`
// for identity, `section '<section>'` as the legacy evidence — which is the only
// per-spawn identifier the daemon controls without passing --session at spawn
// (user decision: the daemon must also find sessions it did not start). Neither
// marker is enough on its own — a section repeats in a workdir and a sibling's
// session can fall inside this spawn's window — so a session must also have been
// created at or after `since`. Pass an empty workDir to search every directory
// (used when the spawn-time directory was not recorded).
//
// Without a live v2 service the session tables are polled for the whole
// timeout, so v1 installs keep their previous behaviour. With one, the tables
// only get a short grace period.
func captureOpencodeSessionID(workDir, agentID, section, projectDir string, since time.Time, timeout time.Duration) string {
	q := opencode.MarkerQuery{
		Directory: workDir,
		AgentID:   agentID,
		Section:   section,
		// Limit 0 lets the client use its directory-scoped default.
	}
	if workDir == "" {
		// No directory to narrow the scan: it becomes a store-wide window
		// ordered by update time, so it has to reach further back.
		q.Limit = lateBindScanLimit
	}
	if !since.IsZero() {
		q.Since = since.Add(-markerSinceSlack)
		q.Until = since.Add(markerWindowAfter)
	}

	client := opencodeV2Service()
	if client == nil {
		if workDir == "" {
			return pollOpencodeAnyDirSessionID(projectDir, q.Since, q.Until, timeout)
		}
		return pollOpencodeSessionID(workDir, q.Since, q.Until, timeout)
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		// The context carries the caller's deadline into the scan, so a slow
		// service cannot make one iteration outlive the whole budget.
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		sid, err := client.FindSession(ctx, q)
		cancel()
		if err == nil && sid != "" {
			log.Printf("[agent_capture] opencode v2 session captured for section %q via %s: %s", section, client.URL(), sid)
			return sid
		}
		if err != nil {
			lastErr = err
		}
		if !sleepWithin(deadline, v2PollInterval) {
			break
		}
	}
	if lastErr != nil {
		log.Printf("[agent_capture] opencode v2 lookup for section %q failed: %v", section, lastErr)
	}

	// The API was reachable but saw no matching session: a v1 binary may have
	// created it in the legacy table instead. Only that table is consulted — the
	// v2 rows this scan already refused must not come back in through a
	// message-blind uniqueness query (YM-419).
	if sid := pollOpencodeSessionIDIn(opencodeV1OnlyTables, workDir, q.Since, q.Until, v1FallbackTimeout); sid != "" {
		log.Printf("[agent_capture] opencode session captured for section %q via session table fallback: %s", section, sid)
		return sid
	}
	// Deliberately no marker-less store-wide fallback here: a table query cannot
	// read the session's messages, so with a v2 service reachable it could only
	// guess from the creation window — the sibling-session mis-bind this path
	// exists to prevent. The v2 lookup above had its chance at identity, and an
	// agent without a recorded work_dir stays unbound rather than being bound to
	// a stranger's conversation. (Without a v2 service the branch above has no
	// identity mechanism at all; there the bounded unique-under-project scan is
	// the only option left, see pollOpencodeAnyDirSessionID.)
	return ""
}

// sleepWithin sleeps for d but never past deadline, and reports whether time is
// left afterwards.
func sleepWithin(deadline time.Time, d time.Duration) bool {
	if remaining := time.Until(deadline); remaining < d {
		if remaining <= 0 {
			return false
		}
		d = remaining
	}
	time.Sleep(d)
	return time.Until(deadline) > 0
}

// agentCreatedAt parses the agents table's local-time created_at stamp, which
// is the lower bound for the agent's session.
func (h *Handler) agentCreatedAt(id string) (time.Time, bool) {
	a, err := h.store.AgentGet(id)
	if err != nil || a == nil {
		return time.Time{}, false
	}
	return parseAgentCreatedAt(a.CreatedAt)
}

// parseAgentCreatedAt parses the stamp the agents table holds. AgentCreate
// writes RFC3339 (storage/agents.go), while rows predating 2026-06-18 carry the
// older space-separated local-time format; both are accepted so the bound
// actually anchors on the agent row instead of silently falling back to "now".
func parseAgentCreatedAt(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// bindOpencodeSession makes a late attempt to bind the opencode session id of
// an agent whose spawn-time capture failed — the session can appear after the
// capture window, or the daemon may have restarted in between. On success the
// id is persisted, which is what makes the agent resumable and lets whoami
// resolve it.
//
// The call is scoped by the work_dir persisted at spawn, so the scan stays
// inside the agent's own tree (a loop agent's session lives in its worktree
// while agents.project holds the repository root). Rows from before work_dir
// was recorded still fall back to the store-wide window. The agent marker plus
// the agent's creation time carry the identity either way. When the agent row
// cannot be dated the bind is skipped rather than guessed.
func (h *Handler) bindOpencodeSession(agent storage.Agent) string {
	created, ok := h.agentCreatedAt(agent.ID)
	if !ok {
		log.Printf("[heartbeat] agent %s: undated agent row, skipping late opencode session bind", agent.ID)
		return ""
	}
	sid := captureOpencodeSessionID(agent.WorkDir, agent.ID, agent.Section, agent.Project, created, opencodeLateBindTimeout)
	if sid == "" || !h.bindAgentSession(&agent, sid, "late bind") {
		return ""
	}
	return sid
}

// opencodeSessionTables lists the tables a session row can live in, in lookup
// order. v1 wrote `session`; v2 writes `session_v2` (Learning #101594). Both
// live in the same database file, and every service instance shares it — a
// session started on any server is visible here.
var opencodeSessionTables = []string{"session", "session_v2"}

// opencodeV2OnlyTables is the v2 store alone. v1's `session` table has not been
// written since 2026-10-12, so a caller that describes the *current* state (the
// capture-timeout hint) must not read it.
var opencodeV2OnlyTables = []string{"session_v2"}

// sessionTableQueryTimeout bounds each sqlite3 subprocess so a locked database
// cannot stall the heartbeat or the DONE-guard.
const sessionTableQueryTimeout = 5 * time.Second

// diagnosticQueryTimeout bounds the "recent session dirs" hint logged on a
// capture timeout. It is separate from the caller's budget so the hint does not
// extend the overrun it explains.
const diagnosticQueryTimeout = 1 * time.Second

// sqlite3Binary resolves the sqlite3 CLI the daemon executes.
//
// The bare name "sqlite3" left the choice to PATH, and the daemon then ran
// whatever answered to that name. This refuses a candidate whose file is group- or
// world-writable — a launcher any local writer may replace — and requires a
// regular executable file; symlinks are followed because package managers link the
// binary. Same heuristic as setup's isTrustedExecutable.
//
// What it does not do: it checks the file, not its directory, so a writable
// directory earlier in PATH can still supply a clean-looking replacement; the owner
// is not checked (that has to build on every platform); and a window between this
// check and the exec remains. It keeps a sloppy PATH from deciding what the daemon
// runs; it does not stop a local attacker who already controls HOME.
func sqlite3Binary() (string, error) {
	path, err := exec.LookPath("sqlite3")
	if err != nil {
		return "", fmt.Errorf("sqlite3 not found: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve sqlite3 %q: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat sqlite3 %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("sqlite3 %q is not a regular file", resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("sqlite3 %q is not executable", resolved)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("sqlite3 %q is writable by group or others — refusing to execute a binary any local writer can replace", resolved)
	}
	return resolved, nil
}

// sessionQueryParam is one value bound into a session-table query. The values
// travel as their own sqlite3 argv entries (the CLI binds through .parameter
// dot-commands), so caller-supplied data never becomes SQL text (YM-216).
type sessionQueryParam struct {
	name  string // binding name including its sigil, e.g. "@parent"
	value string
}

// parameterArgs renders the .parameter dot-commands that bind the given values.
// The sqlite3 CLI parses each of these as its own statement, so the value is
// quoted with the dot-command's own rule and stays a value: the parser reads it
// as one token, and the query text that follows only ever carries the name.
//
// The parser honours backslash escapes inside double quotes but not SQL quote
// doubling, and it cannot carry a line break in one argv entry. A value that
// cannot be expressed is refused rather than bound to something else — the CLI
// would otherwise print its usage text, which the row reader would take as data.
func parameterArgs(params []sessionQueryParam) ([]string, error) {
	args := make([]string, 0, len(params))
	for _, p := range params {
		quoted, err := dotCommandValue(p.value)
		if err != nil {
			return nil, fmt.Errorf("bind %s: %w", p.name, err)
		}
		args = append(args, fmt.Sprintf(`.parameter set %s "%s"`, p.name, quoted))
	}
	return args, nil
}

// dotCommandValue escapes a value for the sqlite3 CLI's .parameter dot-command.
//
// A value that starts with a bind sigil would be read as a reference to another
// parameter and bound as NULL, so it is refused: no session id starts with one,
// and a silent NULL would turn a lookup into "no child" instead of an error.
func dotCommandValue(s string) (string, error) {
	if s != "" {
		switch s[0] {
		case '$', ':', '@', '?':
			return "", fmt.Errorf("value starts with the bind sigil %q, which the dot-command would read as a parameter reference", s[0])
		}
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		case '\n', '\r', 0:
			return "", fmt.Errorf("value contains %q, which a dot-command argument cannot carry", r)
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// querySessionTables runs one query per existing session table and returns all
// non-empty rows. query receives the table name and returns the full SQL
// statement. ctx bounds the whole call — each probe gets at most
// sessionTableQueryTimeout of whatever is left, so a caller with a tight budget
// (the heartbeat) is not made to wait for the worst case.
//
// A table that does not exist is skipped — a v1 database has no session_v2 and
// vice versa. Any other failure is reported as an error, because the difference
// matters to callers: an error means infrastructure (skip the check, do not
// accuse the agent), whereas an empty result means the row is genuinely absent.
// Rows already read from a healthy table are returned alongside that error, so
// one broken table does not discard a valid answer from the other; callers that
// need a complete read (a uniqueness test) must check the error.
func querySessionTables(ctx context.Context, dbPath string, query func(table string) string) ([]string, error) {
	return querySessionTablesIn(ctx, dbPath, opencodeSessionTables, query)
}

// querySessionTablesIn is querySessionTables against an explicit table list, for
// callers that must not read one of the two stores (see opencodeV2OnlyTables).
func querySessionTablesIn(ctx context.Context, dbPath string, tables []string, query func(table string) string) ([]string, error) {
	return querySessionTablesParams(ctx, dbPath, tables, query, nil)
}

// querySessionTablesParams is querySessionTablesIn with bound values: the SQL text
// is built per table and only ever carries the binding names.
func querySessionTablesParams(ctx context.Context, dbPath string, tables []string, query func(table string) string, params []sessionQueryParam) ([]string, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("empty opencode database path")
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return nil, statErr
	}
	var rows []string
	readable := 0
	var lastErr error
	bindArgs, err := parameterArgs(params)
	if err != nil {
		return nil, err
	}
	// Resolved before the loop: the daemon must not let PATH pick the program it
	// runs (see sqlite3Binary).
	bin, err := sqlite3Binary()
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		pctx, cancel := context.WithTimeout(ctx, sessionTableQueryTimeout)
		args := append([]string{dbPath}, bindArgs...)
		args = append(args, query(table))
		out, err := exec.CommandContext(pctx, bin, args...).Output()
		cancel()
		if err != nil {
			if strings.Contains(errText(err), "no such table") {
				continue
			}
			lastErr = err
			continue
		}
		readable++
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				rows = append(rows, line)
			}
		}
	}
	if lastErr != nil {
		return rows, lastErr
	}
	if readable == 0 {
		return nil, fmt.Errorf("no session table found in %s", dbPath)
	}
	return rows, nil
}

// queryFirstSessionRow returns the first row of a session-table query, or ""
// when no table holds a matching row. A real answer beats a partial failure: the
// error is only reported when no row was found at all.
func queryFirstSessionRow(ctx context.Context, dbPath string, query func(table string) string) (string, error) {
	return queryFirstSessionRowParams(ctx, dbPath, query, nil)
}

// queryFirstSessionRowParams is queryFirstSessionRow with bound values.
func queryFirstSessionRowParams(ctx context.Context, dbPath string, query func(table string) string, params []sessionQueryParam) (string, error) {
	rows, err := querySessionTablesParams(ctx, dbPath, opencodeSessionTables, query, params)
	if len(rows) > 0 {
		return rows[0], nil
	}
	if err != nil {
		return "", err
	}
	return "", nil
}

// recentRootSessionDirs names the directories of the newest root sessions, for
// the capture-timeout hint. It answers "where is opencode creating sessions
// right now?" — the live store is the v2 API and the session_v2 table behind it.
// The v1 `session` table is deliberately not consulted: it has been frozen since
// 2026-10-12, and a hint taken from it names directories that no current spawn
// could have used, which is exactly the misleading hint this replaces.
func recentRootSessionDirs(ctx context.Context, dbPath string) []string {
	if client := opencodeV2Service(); client != nil {
		sessions, err := client.ListSessions(ctx, "", 20)
		if err == nil {
			var dirs []string
			seen := map[string]bool{}
			for _, s := range sessions {
				if s.ParentID != "" || s.Directory == "" || seen[s.Directory] {
					continue
				}
				seen[s.Directory] = true
				dirs = append(dirs, s.Directory)
				if len(dirs) == 3 {
					return dirs
				}
			}
			if len(dirs) > 0 {
				return dirs
			}
		}
	}
	rows, err := querySessionTablesIn(ctx, dbPath, opencodeV2OnlyTables, func(table string) string {
		return fmt.Sprintf("SELECT directory FROM %s WHERE 1=1%s GROUP BY directory ORDER BY MAX(time_created) DESC LIMIT 3",
			table, rootOnlyClause())
	})
	if err != nil {
		return nil
	}
	return rows
}

// opencodeRootSessionIDsInDir returns the ids of root sessions (no parent) that
// ran in dir. It answers "which opencode session lives in this directory?"
// without depending on agents.project, which holds the repository root for a
// loop agent while its session lives in the worktree.
//
// A row found in one session table is a real answer, so a failure of the other
// table does not discard it (same rule as queryFirstSessionRow). The caller
// binds an agent to these ids; dropping a partial result loses the binding.
//
// The trade is deliberate and bounded: in the partial case the error is dropped
// too, so a second session living in the failed table is not seen and the caller
// resolves a workdir that a complete read would have refused as ambiguous (see
// agentByCallerCWD). The ids that are returned are still exact matches against a
// running agent's opencode_session_id — a partial read can narrow the answer,
// never invent one.
//
// Returning nothing instead is not the safer branch: agentByCallerCWD reads an
// empty result as "no answer" and resolveSessionID then falls through to its
// last-resort guess (whichever session wrote last), so an incomplete read would
// trade a wrong-but-plausible agent for an unrelated session. The residual cost
// of a misbinding is real either way — the resolved session decides the implicit
// ACK in handleSendTo — and it needs schema drift (a session table that exists
// but lacks `directory`) plus two agents sharing one workdir to happen at all.
//
// It runs on the daemon's request path, so it stays quiet: callers fall back to
// their previous behaviour when it returns nothing, and logging here would emit
// one line per request on an installation without a session database.
func opencodeRootSessionIDsInDir(ctx context.Context, dir string) []string {
	return opencodeRootSessionsInDirFromDB(ctx, dir)
}

// opencodeRootSessionsInDirFromDB is the session-table read behind
// opencodeRootSessionIDsInDir. The var above is a test seam: a broadcast label is
// decided by this lookup, so a test has to be able to drive it without a real
// opencode.db on the host (YM-271).
func opencodeRootSessionsInDirFromDB(ctx context.Context, dir string) []string {
	if dir == "" {
		return nil
	}
	escaped := strings.ReplaceAll(dir, "'", "''")
	rows, err := querySessionTables(ctx, opencodeDBPath(), func(table string) string {
		return fmt.Sprintf("SELECT id FROM %s WHERE directory = '%s'%s", table, escaped, rootOnlyClause())
	})
	if len(rows) == 0 && err != nil {
		return nil
	}
	return rows
}

// errText returns stderr of a failed sqlite3 invocation, falling back to the
// error string. The sqlite3 CLI reports schema problems on stderr.
func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}
