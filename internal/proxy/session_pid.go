package proxy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// peerPIDEntry maps a loopback peer endpoint to its owning process. The inode
// field is the validation anchor: if the cached inode no longer resolves, the
// entry is stale (port reuse / reconnect) and gets re-resolved.
type peerPIDEntry struct {
	pid   int
	inode uint64
	seen  time.Time
}

// peerPIDCache holds resolved peer endpoints for the proxy's loopback clients.
// OpenCode keeps persistent keep-alive connections, so entries are long-lived;
// prune() bounds the map against port recycling noise.
type peerPIDCache struct {
	mu sync.Mutex
	m  map[string]peerPIDEntry
}

func newPeerPIDCache() *peerPIDCache {
	return &peerPIDCache{m: make(map[string]peerPIDEntry)}
}

func (c *peerPIDCache) prune() {
	c.mu.Lock()
	c.pruneLocked(time.Now())
	c.mu.Unlock()
}

// pruneLocked drops every entry that has not been seen for peerPIDEntryTTL. The
// caller holds mu.
func (c *peerPIDCache) pruneLocked(now time.Time) {
	cutoff := now.Add(-peerPIDEntryTTL)
	for k, e := range c.m {
		if e.seen.Before(cutoff) {
			delete(c.m, k)
		}
	}
}

// remember stores a resolution and bounds the map in the same step. prune() grew
// no production caller for as long as entries were written straight into the map,
// so the cache only ever grew; the write is where the bound belongs (YM-271).
func (c *peerPIDCache) remember(key string, e peerPIDEntry) {
	c.mu.Lock()
	c.m[key] = e
	c.pruneLocked(e.seen)
	c.mu.Unlock()
}

// touch refreshes the last-seen stamp of an entry whose pid still holds the
// socket inode it was resolved for, and returns that pid. The /proc scan runs
// OUTSIDE the lock (it is the expensive part, one fd read per process); the write
// afterwards is guarded by the entry it was decided on, so a concurrent resolution
// that stored a newer answer is not overwritten by this stale one (cold review,
// YM-271).
//
// A keep-alive connection is the normal case for opencode, and without the refresh
// prune() would delete a live entry and the next request would pay a full /proc scan
// for nothing.
func (c *peerPIDCache) touch(key string) (int, bool) {
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if !ok || e.pid <= 0 || !dirHasSocketFD(e.pid, socketTarget(e.inode)) {
		return 0, false
	}
	c.mu.Lock()
	if cur, still := c.m[key]; still && cur.pid == e.pid && cur.inode == e.inode {
		cur.seen = time.Now()
		c.m[key] = cur
	}
	c.mu.Unlock()
	return e.pid, true
}

// peerPIDEntryTTL is how long a resolved peer endpoint stays cached without being
// seen again. Long enough that an idle keep-alive connection keeps its answer.
const peerPIDEntryTTL = 2 * time.Hour

// peerSessionPID resolves the process that owns the CLIENT side of a proxy
// connection. For an accepted connection with peer endpoint peerPort, the
// client's own socket is the mirrored row local=peerPort rem=proxyPort
// (ESTABLISHED) in /proc/net/tcp{,6}; its inode maps to the client PID via
// /proc/<pid>/fd. Returns 0 when the lookup fails — callers must treat 0 as
// "no registration".
func peerSessionPID(listenAddr, remoteAddr string) int {
	listenPort := parsePort(listenAddr)
	if listenPort <= 0 {
		return 0
	}
	peerPort := parsePort(remoteAddr)
	if peerPort <= 0 {
		return 0
	}
	cacheKey := remoteAddr

	// A cache hit is only reused while the cached pid still holds the socket inode
	// it was resolved for — the answer is self-healing, the port or pid alone is not
	// (touch checks and refreshes in one step).
	if pid, ok := defaultPeerPIDs.touch(cacheKey); ok {
		return pid
	}

	inode := peerClientSocketInode(peerPort, listenPort)
	if inode == 0 {
		return 0
	}
	pid := findPIDBySocketInode(inode)
	defaultPeerPIDs.remember(cacheKey, peerPIDEntry{pid: pid, inode: inode, seen: time.Now()})
	return pid
}

var defaultPeerPIDs = newPeerPIDCache()

// peerSessionPIDFn is peerSessionPID; a var so a test driving a handler through
// httptest can supply the peer it would otherwise have no socket for.
var peerSessionPIDFn = peerSessionPID

func socketTarget(inode uint64) string {
	return "socket:[" + strconv.FormatUint(inode, 10) + "]"
}

func parsePort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// peerClientSocketInode scans /proc/net/tcp{,6} for the client-side row of the
// connection: local=peerPort rem=proxyPort, ESTABLISHED. The inode belongs to
// the opencode process (its outgoing socket fd), not to our accepted socket.
func peerClientSocketInode(peerPort, listenPort int) uint64 {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if i == 0 || !strings.Contains(line, ":") {
				continue // header / malformed
			}
			f := strings.Fields(line)
			// sl local_address rem_address st tx:rx ... inode
			if len(f) < 10 || f[3] != "01" { // 01 = TCP_ESTABLISHED
				continue
			}
			localPort, err := strconv.ParseUint(afterColon(f[1]), 16, 16)
			if err != nil || int(localPort) != peerPort {
				continue
			}
			remPort, err := strconv.ParseUint(afterColon(f[2]), 16, 16)
			if err != nil || int(remPort) != listenPort {
				continue
			}
			inode, err := strconv.ParseUint(f[9], 10, 64)
			if err != nil || inode == 0 {
				continue
			}
			return inode
		}
	}
	return 0
}

// findPIDBySocketInode scans /proc for the process holding the socket fd.
func findPIDBySocketInode(inode uint64) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if dirHasSocketFD(pid, socketTarget(inode)) {
			return pid
		}
	}
	return 0
}

func dirHasSocketFD(pid int, target string) bool {
	fds, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		return false
	}
	for _, fd := range fds {
		link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + fd.Name())
		if err == nil && link == target {
			return true
		}
	}
	return false
}

func afterColon(s string) string {
	_, rest, ok := strings.Cut(s, ":")
	if !ok {
		return s
	}
	return rest
}

// codexChannelAddress is the mailbox of a codex request: codex sends no session
// header and addresses its mail to "codex:<session uuid>", which the parity path
// could only approximate with a derived thread hash — nothing it was sent was
// ever found. The uuid comes from the client process, because codex keeps its
// rollout jsonl open for the session's lifetime.
//
// The kernel names the process behind the connection; that is an attribution,
// not a trust decision. Any same-uid process can open a file with a
// rollout-shaped name, so the link has to live in the directory codex itself
// writes (codexSessionsRoot), and an ambiguous answer yields "".
func codexChannelAddress(peerPID int) string {
	if peerPID <= 0 {
		return ""
	}
	if id := codexSessionUUIDForPID(peerPID); id != "" {
		return "codex:" + id
	}
	return ""
}

// codexIdentityWarned bounds the unresolved-identity warning to one line per
// client thread. A host where the lookup cannot work (no /proc, codex writing
// to CODEX_HOME) would otherwise warn on every single codex request.
var codexIdentityWarned sync.Map

// warnUnresolvedCodexIdentity reports a codex request whose session could not be
// attributed. Without it the failure is indistinguishable from the defect this
// resolver was added for: mail that simply never arrives.
func (s *Server) warnUnresolvedCodexIdentity(threadID string, pid int) {
	if s.logger == nil {
		return
	}
	if _, seen := codexIdentityWarned.LoadOrStore(threadID, struct{}{}); seen {
		return
	}
	s.logger.Printf("codex channel identity unresolved (peer pid %d, thread %s) — its mail stays unread until the identity resolves", pid, journalToken(threadID))
}

// codexRegisteredChannel remembers, per daemon, derived thread id and client
// process, which mailbox was last handed to that daemon. Codex sends register_pid
// on every request — one interaction sends dozens — and each call rewrote two
// proxy_state rows and the PID file (audit 16.10.2026, YM-328). A repeat of the
// same channel by the same process adds nothing: a daemon that restarts on the
// same data dir rebuilds its PID map from that file. The data dir is part of the
// key because the cache must never speak for a different daemon, and the pid
// because the daemon's mapping is pid-based: a codex process that continues the
// same rollout file under a new pid has to register, or the daemon keeps the old
// pid and the new one resolves to nothing. A different channel under the same
// thread still registers, because a codex thread id is derived from message
// content and repeats across sessions.
//
// The key grows with client-controlled input, so the map is capped: clearing it
// costs one registration per live thread, nothing more.
var codexRegisteredChannel sync.Map

const codexRegistrationCap = 512

var codexRegistrationStores atomic.Int64

// codexRegistrationKey names one (daemon, client thread, client process) triple.
func codexRegistrationKey(dataDir, threadID string, pid int) string {
	return dataDir + "\x00" + threadID + "\x00" + strconv.Itoa(pid)
}

// registerCodexIdentity resolves the mailbox of a codex request and records the
// agent kind with it. Codex sends no session header, so register_pid is the only
// place the daemon learns the session and that the caller is not Claude: without
// the write, daemon-side readers (agent rows, briefing profile) keep the Claude
// default.
func (s *Server) registerCodexIdentity(ctx *openAIRequestContext, remoteAddr string) {
	pid := peerSessionPIDFn(s.cfg.ListenAddr, remoteAddr)
	ctx.ChannelID = codexChannelAddress(pid)
	if ctx.ChannelID == "" {
		s.warnUnresolvedCodexIdentity(ctx.ThreadID, pid)
		return
	}
	key := codexRegistrationKey(s.cfg.DataDir, ctx.ThreadID, pid)
	if prev, seen := codexRegisteredChannel.Load(key); seen && prev == ctx.ChannelID {
		return
	}
	// Cache only a registration that reached the daemon: a failed one has to be
	// retried by the next request.
	if _, err := s.queryDaemon("register_pid", map[string]any{
		"session_id":   ctx.ChannelID,
		"pid":          float64(pid),
		"source_agent": models.SourceAgentCodex,
	}); err != nil {
		return
	}
	codexRegisteredChannel.Store(key, ctx.ChannelID)
	if codexRegistrationStores.Add(1) > codexRegistrationCap {
		codexRegisteredChannel.Range(func(k, _ any) bool {
			codexRegisteredChannel.Delete(k)
			return true
		})
		codexRegistrationStores.Store(0)
	}
}

// codexSessionsRoot is the directory codex writes rollouts to. A var so a test
// can point the lookup at its own directory instead of the real ~/.codex.
var codexSessionsRoot = defaultCodexSessionsRoot

func defaultCodexSessionsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// codexSessionUUIDForPID reads the rollout file a codex process holds open and
// returns the session uuid in its name.
func codexSessionUUIDForPID(pid int) string {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	links := make([]string, 0, len(fds))
	for _, fd := range fds {
		if link, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil {
			links = append(links, link)
		}
	}
	return codexSessionUUIDFromLinks(links)
}

// codexSessionUUIDFromLinks picks the rollout link out of /proc/<pid>/fd
// targets. A rollout name ends in the session uuid (8-4-4-4-12), so a name that
// merely contains 36 trailing characters is not enough. Two different rollouts
// open at once name two sessions — there is no tie-break that would be right,
// so the answer is "" rather than a guess.
func codexSessionUUIDFromLinks(links []string) string {
	root := codexSessionsRoot()
	if root == "" {
		return ""
	}
	prefix := filepath.Join(root, "") + string(os.PathSeparator)
	seen := ""
	for _, l := range links {
		if !strings.HasPrefix(l, prefix) || !strings.HasSuffix(l, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(l), ".jsonl")
		if len(base) < 36 {
			continue
		}
		uuid := base[len(base)-36:]
		if !isSessionUUID(uuid) {
			continue
		}
		if seen != "" && seen != uuid {
			return ""
		}
		seen = uuid
	}
	return seen
}
