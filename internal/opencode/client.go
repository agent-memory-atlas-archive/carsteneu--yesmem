package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file implements a minimal client for the opencode v2 *server* API
// (opencode serve --service). It exists because v2 sessions are no longer
// discoverable the way v1 sessions were: they are absent from the `session`
// table of opencode.db (they live in `session_v2`, and the server API is the
// only stable contract). See Learning #101778 for the API reference and
// #101594 for the schema drift.
//
// The v2 API is v1-incompatible (different docs, different operation ids —
// opencode.ai/v2/docs/api). Only the operations below are implemented;
// the running service publishes the full spec at GET <url>/openapi.json.
//
// Every call takes a context. The daemon calls this package from its spawn
// goroutine and from the shared heartbeat goroutine, so no single request and
// no scan may outlive the caller's budget.

// authUser is the fixed basic-auth user of the opencode server API. Only the
// password varies per service instance.
const authUser = "opencode"

// requestTimeout bounds one API call; the caller's context bounds a sequence.
const requestTimeout = 5 * time.Second

// probeTimeout bounds the health probe of a single registration while looking
// for a usable service. It is short because a stale registration is expected,
// and the heartbeat path pays this cost once per attempt.
const probeTimeout = 1 * time.Second

// maxErrorBody bounds how much of a service response is echoed into an error
// (and from there into the daemon log). The service controls the body, so an
// untruncated echo can flood the log and forge lines with embedded newlines.
const maxErrorBody = 200

// Service is one registration from ~/.local/state/opencode/service*.json.
// Writing this file is a password — it is read, never logged.
type Service struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Password string `json:"password"`
}

// StateDir returns the directory holding opencode's service registrations.
func StateDir(home string) string {
	return filepath.Join(home, ".local", "state", "opencode")
}

// DiscoverServices reads every service*.json registration under home. It
// returns them sorted by file name so callers see a deterministic order.
// Unreadable, malformed and non-loopback registrations are skipped: a stale or
// tampered registration must not make the daemon hand its password to a
// different host. Several services can run at once (v1 and v2 binaries side by
// side), and they share one session store — the registration only says where a
// reachable API lives.
func DiscoverServices(home string) []Service {
	if home == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(StateDir(home), "service*.json"))
	if err != nil {
		return nil
	}
	sort.Strings(paths)
	var out []Service
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s Service
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if !loopbackHTTPURL(s.URL) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// loopbackHTTPURL reports whether raw is a plain http URL on the local machine.
// A registration file is writable by whoever the daemon runs as, so this is
// defence in depth only — but it is cheap, and it keeps the stored password and
// the queried workdirs on the box. Userinfo is rejected: it would be echoed by
// callers that log URL(), and it is never needed by opencode.
func loopbackHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// FirstLiveService returns the first registered service that answers the
// health probe. Registrations go stale (the file outlives the process; PIDs
// are reused), so a registration is only a candidate — GET /api/info decides.
func FirstLiveService(home string) (Service, error) {
	svcs := DiscoverServices(home)
	if len(svcs) == 0 {
		return Service{}, fmt.Errorf("no opencode service registration under %s", StateDir(home))
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var lastErr error
	for _, s := range svcs {
		// One short probe per candidate: a registration that accepts the
		// connection and then stalls must not consume the budget of the others.
		pctx, pcancel := context.WithTimeout(ctx, probeTimeout)
		_, err := NewClient(s).Info(pctx)
		pcancel()
		if err != nil {
			lastErr = err
			continue
		}
		return s, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable opencode service")
	}
	return Service{}, lastErr
}

// Client talks to one opencode v2 service.
type Client struct {
	svc  Service
	http *http.Client
}

// NewClient returns a client for the given service registration.
func NewClient(svc Service) *Client {
	return &Client{
		svc: svc,
		http: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: rejectForeignRedirect,
		},
	}
}

// rejectForeignRedirect refuses to follow a redirect off loopback. The
// registration is already restricted to loopback, but the responder could still
// try to send the daemon somewhere else; net/http drops the Authorization
// header across hosts, so the worst case would be an unauthenticated request —
// still not something the daemon should do.
//
// A write is not followed at all. net/http re-sends the Authorization header
// when only the port changes, and replays the body for 307/308 — so following a
// redirect would hand the service password and the message to whichever port
// the responder names, for a POST that never needed a redirect to reach a
// service on this machine.
func rejectForeignRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("too many redirects")
	}
	for _, prev := range via {
		if prev.Method != http.MethodGet {
			return http.ErrUseLastResponse
		}
	}
	if !loopbackHTTPURL(req.URL.Scheme + "://" + req.URL.Host) {
		return fmt.Errorf("refusing redirect to %s", req.URL.Host)
	}
	return nil
}

// URL is the base URL the client talks to. Callers log this to record which
// service served a session.
func (c *Client) URL() string { return c.svc.URL }

// Info is the /api/info health probe response.
type Info struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Info probes the service. A non-nil error means the service is not usable.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	if err := c.get(ctx, "/api/info", nil, &out); err != nil {
		return Info{}, err
	}
	return out, nil
}

// Session is one row of GET /api/session. The live service nests the
// directory and creation time; the flat fields are filled by UnmarshalJSON so
// callers never have to walk the nesting.
type Session struct {
	ID        string `json:"id"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	Agent     string `json:"agent"`
	Directory string `json:"directory"`
	// Created is the session creation time in Unix milliseconds.
	Created int64 `json:"created"`
}

// UnmarshalJSON accepts both the live shape (location.directory,
// time.created) and a flat one.
func (s *Session) UnmarshalJSON(data []byte) error {
	type flat Session
	var raw struct {
		flat
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
		Time struct {
			Created int64 `json:"created"`
		} `json:"time"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = Session(raw.flat)
	if s.Directory == "" {
		s.Directory = raw.Location.Directory
	}
	if s.Created == 0 {
		s.Created = raw.Time.Created
	}
	return nil
}

// ListSessions returns the sessions of one directory, newest first (the
// server orders by update time). An empty directory lists everything.
// Root sessions carry no ParentID; task() subagents do.
func (c *Client) ListSessions(ctx context.Context, directory string, limit int) ([]Session, error) {
	q := url.Values{}
	if directory != "" {
		q.Set("directory", directory)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out struct {
		Data []Session `json:"data"`
	}
	if err := c.get(ctx, "/api/session", q, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// GetSession returns one session by id — the lookup for sessions the daemon
// did not start (foreign sessions, sessions on another server).
func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var out struct {
		Data Session `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(sessionID), nil, &out); err != nil {
		return Session{}, err
	}
	return out.Data, nil
}

// FirstUserMessageText returns the text of a session's first user message, or
// "" when the session has none. The daemon's startup prompt is recorded as
// that first message, which is what makes the section marker matchable.
func (c *Client) FirstUserMessageText(ctx context.Context, sessionID string) (string, error) {
	q := url.Values{}
	q.Set("type", "user")
	q.Set("order", "asc")
	q.Set("limit", "1")
	var out struct {
		Data []struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(sessionID)+"/message", q, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", nil
	}
	return out.Data[0].Text, nil
}

// MessagePart is one content part of an assistant message: a text part, a
// reasoning part, or a tool part. Only the fields the daemon reads are
// modelled — State.Status is the tool's own lifecycle
// (streaming/running/completed/error), which is what tells an executed call
// from one that ended in failure.
type MessagePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	State struct {
		Status string `json:"status"`
		// Metadata is the tool's result metadata. The shell tool moves a
		// background command into a job and reports it here: status "running"
		// plus a shell id, while the tool part itself is completed. Shape taken
		// from the plugin source (packages/core/src/tool/plugin/shell.ts,
		// backgroundResult) and the projector that stores it
		// (packages/core/src/session/message-updater.ts, ToolStateCompleted).
		// Any tool that reports a running job this way counts, not only the
		// shell tool.
		Metadata struct {
			Status  string `json:"status"`
			ShellID string `json:"shellID"`
		} `json:"metadata"`
	} `json:"state"`
}

// AssistantMessage is one assistant message of session.message.list, reduced to
// the fields the daemon acts on. Finish is the provider's stop reason ("stop",
// "tool-calls", …) and is empty while the turn is still streaming;
// Time.Completed is stamped when the turn ended.
type AssistantMessage struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Finish  string        `json:"finish"`
	Content []MessagePart `json:"content"`
	Time    struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// LastAssistantMessage returns a session's newest assistant message. The second
// result is false when the session has none — a session that has not answered
// yet, or one whose id the service does not have (the id is also read from
// rows that outlive their service).
//
// The list is asked for newest-first with a single row, so the answer is the
// latest turn rather than the first page of the oldest: a session with more
// messages than one page would otherwise report its opening turn as the last.
func (c *Client) LastAssistantMessage(ctx context.Context, sessionID string) (AssistantMessage, bool, error) {
	q := url.Values{}
	q.Set("type", "assistant")
	q.Set("order", "desc")
	q.Set("limit", "1")
	var out struct {
		Data []AssistantMessage `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(sessionID)+"/message", q, &out); err != nil {
		return AssistantMessage{}, false, err
	}
	if len(out.Data) == 0 {
		return AssistantMessage{}, false, nil
	}
	return out.Data[0], true, nil
}

// assistantMessagePage bounds the session read of AssistantActivitySince. The
// question it answers is "did the agent produce more than the answer to a
// relay", so the newest page decides it; a session with more turns than this is
// working.
const assistantMessagePage = 50

// SessionActivity is what a session's recent assistant turns say about whether
// its agent is producing right now.
type SessionActivity struct {
	// Since counts assistant turns completed at or after the cutoff.
	Since int
	// Working reports work in progress the turns themselves prove: the newest
	// turn is still open (the model is writing), it carries a tool that is still
	// running, or it moved a command into a background job that has not
	// finished. That is what a long tool call, a build or a subagent looks like
	// — the agent is working although no new turn lands.
	Working bool
	// Background marks Working as the background job's work rather than a turn's.
	// The caller bounds the two differently: an open turn that never ends is
	// aborted after a few minutes, while a job legitimately runs much longer.
	Background bool
	// WorkingSince is when that work started. It is what bounds the signal: a
	// turn that has been open for a long time is an aborted one. The caller owns
	// that bound, so the answer stays independent of any cutoff the caller moves
	// with its own relays (YM-306). Unknown or implausible stamps are handed over
	// as the zero time.
	WorkingSince time.Time
}

// AssistantActivitySince reads a session's newest assistant turns and reports
// how many are new since the given moment and whether the newest one is still
// working.
//
// It is the idle guard's second progress source next to the turn counter, which
// is missing whenever telemetry could not be bound to the agent's row.
//
// "Working" deliberately ignores the cutoff. The caller re-arms that cutoff with
// every relay it sends, so a start-time comparison would erase a tool call that
// was already running: the guard relayed into a five-minute test run, the
// baseline moved past the turn's start, and the very same read then reported the
// agent as idle (live 2026-10-17, YM-306).
func (c *Client) AssistantActivitySince(ctx context.Context, sessionID string, since time.Time) (SessionActivity, error) {
	q := url.Values{}
	q.Set("type", "assistant")
	q.Set("order", "desc")
	q.Set("limit", strconv.Itoa(assistantMessagePage))
	var out struct {
		Data []AssistantMessage `json:"data"`
	}
	if err := c.get(ctx, "/api/session/"+url.PathEscape(sessionID)+"/message", q, &out); err != nil {
		return SessionActivity{}, err
	}
	now := time.Now()
	cutoff := since.UnixMilli()
	var act SessionActivity
	seenNewest := false
	for _, m := range out.Data {
		if m.Type != "assistant" {
			continue
		}
		newest := !seenNewest
		seenNewest = true
		if m.Time.Completed == 0 {
			// An open turn is the model writing right now. Age, not the cutoff,
			// says whether it is still that: an aborted turn stays open forever
			// and the caller drops it once it is older than its bound.
			if newest {
				act.Working = true
				act.WorkingSince = stampOrZero(m.Time.Created, now)
			}
			continue
		}
		if m.Time.Completed >= cutoff && !StampInFuture(m.Time.Completed, now) {
			act.Since++
		}
		// A settled turn still works while one of its tools runs — the shape the
		// daemon's broken-toolcall guard reads the same way.
		if newest && toolRunning(m) {
			act.Working = true
			act.WorkingSince = stampOrZero(m.Time.Created, now)
		}
	}
	// A background shell job outlives the turn that started it: the tool part is
	// completed and the turn settled while the job runs, so neither the open-turn
	// nor the running-tool check sees it. The job is work, and the turn's own
	// start is what bounds it — the metadata keeps saying "running" after the job
	// finished. Only the newest turn counts: once the agent has answered the
	// notification, the job is no longer why it is quiet.
	if started, ok := backgroundJobRunning(out.Data, now); ok && !act.Working {
		act.Working = true
		act.WorkingSince = started
		act.Background = true
	}
	return act, nil
}

// activityFutureTolerance is how far a stamp of the service may run ahead of the
// daemon's clock and still count as agreement. A completion further in the future
// is not a turn that finished: it would sit "after the cutoff" on every read and
// report work for as long as it stays in the newest page.
//
// The tradeoff: a daemon clock that steps backwards makes recent completions look
// future, and they are then left out of Since until they age past the step. That
// costs one rung of the idle protocol in a rare case, while trusting the stamp
// hands the agent a permanent "I am working" (YM-309).
const activityFutureTolerance = 2 * time.Minute

// StampInFuture reports whether a millisecond stamp of the service lies beyond
// the tolerated clock difference. Exported because the same plausibility rule
// decides more than the activity window: a daemon guard that reads a completion
// stamp as "this turn ended" has to reject the same stamps (YM-335).
func StampInFuture(ms int64, now time.Time) bool {
	if ms == 0 {
		return false
	}
	return time.UnixMilli(ms).After(now.Add(activityFutureTolerance))
}

// stampOrZero turns a millisecond stamp into a time, but not into a future one:
// an implausible start is handed over as unknown so the caller's own window
// decides instead of a stamp no clock can reach (5.3 finding 4).
func stampOrZero(ms int64, now time.Time) time.Time {
	if StampInFuture(ms, now) {
		return time.Time{}
	}
	return millisTime(ms)
}

// backgroundJobRunning returns the start of the newest assistant turn if it moved
// a command into a background job that is still marked running. The messages
// arrive newest-first, so only the first assistant turn is considered: a job the
// agent has already moved past is not the reason it is quiet.
func backgroundJobRunning(messages []AssistantMessage, now time.Time) (time.Time, bool) {
	for _, m := range messages {
		if m.Type != "assistant" {
			continue
		}
		for _, p := range m.Content {
			if p.Type != "tool" || p.State.Metadata.ShellID == "" {
				continue
			}
			if !strings.EqualFold(p.State.Metadata.Status, "running") {
				continue
			}
			return stampOrZero(m.Time.Created, now), true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// millisTime converts a millisecond stamp of the service into a time, keeping
// the zero value for an absent stamp.
func millisTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// toolRunning reports whether a turn carries a tool that has not finished.
func toolRunning(m AssistantMessage) bool {
	for _, p := range m.Content {
		if p.Type == "tool" && strings.EqualFold(p.State.Status, "running") {
			return true
		}
	}
	return false
}

// Delivery modes of session.prompt.
const (
	// DeliveryQueue files the turn behind the work a busy session is already
	// doing, instead of interrupting it.
	DeliveryQueue = "queue"
	// DeliverySteer interrupts a running session with the turn.
	DeliverySteer = "steer"
)

// PromptOptions are the optional fields of session.prompt. The service rejects
// the request when any other field is present, so this stays the whole body.
type PromptOptions struct {
	// Delivery is DeliveryQueue or DeliverySteer; empty leaves the service
	// default (steer).
	Delivery string
	// Resume wakes an idle session so it processes the turn without waiting
	// for somebody to type in it.
	Resume bool
	// Metadata travels with the inbox entry. Clients that do not render it
	// still have the text.
	Metadata map[string]any
}

// promptBody is the session.prompt request body.
type promptBody struct {
	Text     string         `json:"text"`
	Delivery string         `json:"delivery,omitempty"`
	Resume   bool           `json:"resume"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// PromptUserTurn files a user turn into an existing session and returns the id
// of the inbox entry it created (operation session.prompt). It is the only
// write operation the daemon uses: everything else here reads.
//
// A session is reachable only through the service that hosts it, so a turn for
// a session of another running service is answered with 404 — the caller keeps
// its fallback rather than this call inventing a session.
func (c *Client) PromptUserTurn(ctx context.Context, sessionID, text string, opts PromptOptions) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id is empty")
	}
	if text == "" {
		return "", fmt.Errorf("prompt text is empty")
	}
	body := promptBody{
		Text:     text,
		Delivery: opts.Delivery,
		Resume:   opts.Resume,
		Metadata: opts.Metadata,
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/session/"+url.PathEscape(sessionID)+"/prompt", body, &out); err != nil {
		return "", err
	}
	return out.Data.ID, nil
}

// rootSessionScanLimit bounds how many sessions of a directory are inspected
// while looking for one marker. The wanted session is the newest, so the cap
// only matters for directories with a long session history — and since the
// identity scan must see every candidate inside the window (the legacy rule
// picks the *earliest* one, not the first listed), it has to be deep enough
// that a busy workdir cannot push a legitimate session out of the list.
const rootSessionScanLimit = 100

// MarkerQuery describes the search for the session the daemon spawned for one
// section.
type MarkerQuery struct {
	// Directory restricts the scan to one workdir. Empty scans every
	// directory, which needs a much larger Limit to reach far enough back.
	Directory string
	// Section is the daemon's section marker, matched against the session's
	// first user message.
	Section string
	// AgentID is the daemon's per-agent marker (`agent '<id>'` in the startup
	// prompt). Sections repeat — a restart or a rescheduled loop reuses one —
	// so the agent marker, not the section, decides which session belongs to
	// this spawn. Empty falls back to the section-only match.
	AgentID string
	// Since is a lower bound on the session's creation time (zero = no bound).
	// Sections repeat in a workdir, so this is what keeps a previous run out.
	Since time.Time
	// Until is an upper bound on the session's creation time (zero = no bound).
	// It keeps a *later* run out when the scan has no directory to narrow it.
	Until time.Time
	// Limit caps how many sessions the scan inspects.
	Limit int
}

// FindSession returns the id of the root session matching q, or "" when none
// does.
//
// The startup prompt embeds `agent '<agent-id>'` and `section '<section>'` (see
// handleSpawnAgent). opencode generates session titles itself and the yesloop
// agent renames its session later, so the prompt is the only per-spawn
// identifier the daemon controls without passing a session id at spawn.
//
// Identity, in order of strength:
//   - a session whose first user message carries this agent's marker is the
//     spawn's own session, whatever the server's list order says;
//   - a session carrying a *different* agent marker belongs to a sibling agent
//     and is rejected even when its section matches (a section repeats, and a
//     sibling's session can fall inside this spawn's window);
//   - a session with no agent marker at all predates the marker (legacy) or
//     was recorded by a client that never wrote one. Neither the section marker
//     nor an empty first message says which tree such a session lives in, so a
//     legacy candidate is used only when the scan is scoped to a known
//     directory. There:
//     · with the section marker, section plus window are the evidence and the
//     *earliest* created candidate wins — the server lists by update time, and
//     for a legacy spawn the oldest session in the window is the one it
//     created;
//     · with no message at all nothing is left to match on. Directory and
//     window are shared by every sibling of a wave (all loop agents of one
//     spawn record the same work_dir), and opencode writes the session row
//     seconds before the injected prompt lands in it, so "the only blank
//     session in the workdir" is a state of this instant, not an identity.
//     Such a session is never bound (live case 21.10.2026, YM-419: five
//     sibling captures adopted the one blank session of the wave and the first
//     writer kept it). The caller keeps polling and the on-demand and late
//     binds retry, so an agent binds as soon as its own marker is there; an
//     agent whose prompt never lands stays unbound rather than being handed a
//     stranger's conversation.
//
// Guards against binding somebody else's session:
//   - subagent sessions (ParentID set) are never candidates;
//   - a session must belong to the requested directory, and a session whose
//     directory the server does not report is rejected rather than assumed to
//     match;
//   - a session outside [Since, Until] is rejected, and so is one the server
//     cannot date — an undated session cannot be shown to belong to this spawn;
//   - a directory-less scan with an AgentID binds only a session that carries
//     this agent's marker. Leaving the agent unbound is the correct outcome
//     when the only candidates are indistinguishable from a sibling's.
//
// No match is not an error: the caller is polling and will retry.
func (c *Client) FindSession(ctx context.Context, q MarkerQuery) (string, error) {
	if q.Section == "" {
		return "", nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = rootSessionScanLimit
	}
	sessions, err := c.ListSessions(ctx, q.Directory, limit)
	if err != nil {
		return "", err
	}
	sectionMarker := "section '" + q.Section + "'"
	ownMarker := ""
	if q.AgentID != "" {
		ownMarker = "agent '" + q.AgentID + "'"
	}
	var evidenced []Session
	for _, s := range sessions {
		if s.ParentID != "" {
			continue
		}
		if q.Directory != "" && s.Directory != q.Directory {
			continue
		}
		if !inWindow(s.Created, q.Since, q.Until) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		text, err := c.FirstUserMessageText(ctx, s.ID)
		if err != nil {
			// A single unreadable session must not abort the scan — but a
			// cancelled scan must.
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		if ownMarker == "" {
			if strings.Contains(text, sectionMarker) {
				return s.ID, nil
			}
			continue
		}
		if strings.Contains(text, ownMarker) {
			// Only one session can carry this agent's marker, so the scan can
			// stop here even though a stronger legacy candidate might follow.
			return s.ID, nil
		}
		if hasAgentMarker(text) {
			// A sibling agent's session: the section matches, the identity does
			// not. Adopting it would hand this agent a foreign conversation.
			continue
		}
		// Everything below is legacy evidence: a section marker without an agent
		// marker. That does not say which tree the session lives in, so it can
		// only be used when the scan is scoped to a known directory. Without one
		// a section marker of a *sibling* agent of the same section is
		// indistinguishable from this agent's own session, so the agent stays
		// unbound rather than being given a foreign conversation.
		if q.Directory == "" {
			continue
		}
		if strings.Contains(text, sectionMarker) {
			evidenced = append(evidenced, s)
			continue
		}
		// Neither a marker nor a section hit: an empty first message belongs
		// here too, and it is no evidence at all (see the identity rule above).
	}
	// The section marker is evidence that the session belongs to this section,
	// so the earliest candidate in the window wins (see the rule above).
	if len(evidenced) > 0 {
		oldest := evidenced[0]
		for _, s := range evidenced[1:] {
			if s.Created != 0 && (oldest.Created == 0 || s.Created < oldest.Created) {
				oldest = s
			}
		}
		return oldest.ID, nil
	}
	return "", nil
}

// agentMarkerPattern matches a spawn marker (`agent '<agent-id>'`) the daemon
// embeds in the startup prompt. It is a pattern rather than a substring because
// the question is only "does this session belong to *some* other agent?", and
// the answer must not depend on knowing which one.
var agentMarkerPattern = regexp.MustCompile(`agent '[^']*'`)

// hasAgentMarker reports whether a session's first user message carries any
// agent marker, i.e. whether it was spawned by a marker-aware daemon.
func hasAgentMarker(text string) bool {
	return agentMarkerPattern.MatchString(text)
}

// inWindow reports whether a session creation time (Unix milliseconds; 0 when
// the server did not report one) satisfies the optional bounds.
func inWindow(createdMs int64, since, until time.Time) bool {
	if createdMs == 0 {
		// Undated: usable only when no bound at all was requested.
		return since.IsZero() && until.IsZero()
	}
	if !since.IsZero() && createdMs < since.UnixMilli() {
		return false
	}
	if !until.IsZero() && createdMs > until.UnixMilli() {
		return false
	}
	return true
}

// get performs an authenticated GET and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// post performs an authenticated POST with a JSON body and decodes the JSON
// response into out.
func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// ResponseError is a response from the service that refused the request. It is
// told apart from every other failure on purpose: only a refusal proves the
// service did not act on the request, and a caller that must decide whether to
// retry a write can rely on nothing else.
type ResponseError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// isRefusalStatus reports whether a status proves the service decided not to
// act, or never carried the request out: a client-error status is decided before
// the request is performed, and a redirect we refuse to follow is a response to
// a request the service did not perform either.
//
// 408 and 429 are excluded because they say nothing about whether the service
// acted, and so is every 5xx — one raised after the turn was taken would
// otherwise be mistaken for a refusal. The service documents 400, 401, 404 and
// 409 for session.prompt; a client-error status it does not document is treated
// the same way, because every one of them is decided before the work.
func isRefusalStatus(code int) bool {
	if code >= 300 && code < 400 {
		return true
	}
	if code < 400 || code > 499 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// Rejected reports whether err is a service refusal rather than a transport,
// deadline or server failure. A write that timed out — or that the service
// failed on after taking it — may well have taken effect, so treating every
// error alike would either duplicate a delivery or drop one.
func Rejected(err error) bool {
	var refused *ResponseError
	return errors.As(err, &refused)
}

// do performs one authenticated request. Both verbs share it so the auth, the
// body cap and the error shape cannot drift apart between read and write.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	if c.svc.URL == "" {
		return fmt.Errorf("opencode service url is empty")
	}
	u := strings.TrimSuffix(c.svc.URL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(authUser, c.svc.Password)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is opencode's own error JSON. The password travels in the
		// Authorization header and is never echoed here; the body is truncated
		// so a hostile or broken service cannot flood the daemon log.
		if isRefusalStatus(resp.StatusCode) {
			return &ResponseError{Method: method, Path: path, Status: resp.StatusCode, Body: sanitizeErrorBody(respBody)}
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, sanitizeErrorBody(respBody))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// sanitizeErrorBody flattens and truncates a service response for error text.
func sanitizeErrorBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	return s
}
