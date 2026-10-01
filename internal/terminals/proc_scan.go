package terminals

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sessionFromCmdline extracts an opencode session id ("ses_...") from the
// NUL-split /proc/<pid>/cmdline arguments of an opencode main process.
// TUI helper processes (__opencode_tui_server__) carry no session id.
func sessionFromCmdline(cmdline []string) string {
	for i, a := range cmdline {
		if a == "__opencode_tui_server__" {
			return ""
		}
		if a == "-s" {
			if i+1 < len(cmdline) && strings.HasPrefix(cmdline[i+1], "ses_") {
				return cmdline[i+1]
			}
			return ""
		}
		if v, ok := strings.CutPrefix(a, "--session="); ok {
			if strings.HasPrefix(v, "ses_") {
				return v
			}
			return ""
		}
	}
	return ""
}

// pidCmdline reads and NUL-splits /proc/<pid>/cmdline.
func pidCmdline(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

// uuidFromRolloutPath extracts the codex session UUID from a rollout file
// name like rollout-2026-09-02T22-59-57-<uuid>.jsonl. The UUID is the last
// 36 characters (8-4-4-4-12, hex) of the base name.
func uuidFromRolloutPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	if len(base) < 36 {
		return ""
	}
	u := base[len(base)-36:]
	parts := strings.Split(u, "-")
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(parts) != 5 || len(parts[i]) != want || !isHex(parts[i]) {
			return ""
		}
	}
	return u
}

// isHex reports whether s consists solely of hex digits.
func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return s != ""
}

// codexSessionIDFromFdLinks picks a rollout link out of raw /proc/<pid>/fd
// symlink targets and returns its session UUID, "" if none qualifies.
func codexSessionIDFromFdLinks(links []string) string {
	for _, l := range links {
		if strings.Contains(l, "/.codex/sessions/") && strings.HasSuffix(l, ".jsonl") {
			if id := uuidFromRolloutPath(l); id != "" {
				return id
			}
		}
	}
	return ""
}

// codexSessionIDFromFds discovers the session UUID a codex TUI process holds
// open. codex keeps its rollout jsonl open for the session's lifetime
// (verified live 2026-09-02).
func codexSessionIDFromFds(pid int) string {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	names, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var links []string
	for _, n := range names {
		l, err := os.Readlink(filepath.Join(dir, n.Name()))
		if err != nil {
			continue
		}
		links = append(links, l)
	}
	return codexSessionIDFromFdLinks(links)
}

// enrichFromProc annotates the snapshot with live opencode/codex sessions
// directly from /proc — independent of daemon pidMap registrations.
//   - opencode main processes carry "-s ses_..." in their cmdline
//   - codex processes hold their rollout jsonl open (session UUID in the path)
// Window matching uses owner-PID ancestry, identical to enrichFromDaemon.
// Entries for sessions that vanished since the last scan are pruned, so the
// 15-min ticker cannot accumulate tombstones behind PruneMissing's
// XID==""-keep rule.
func enrichFromProc(snap *Snapshot, ws []Window) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	owners := ownerMap(ws)
	live := map[string]bool{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		kind := procKind(pid)
		if kind != "opencode" && kind != "codex" {
			continue
		}
		var sessionID string
		if kind == "opencode" {
			sessionID = sessionFromCmdline(pidCmdline(pid))
		} else {
			sessionID = codexSessionIDFromFds(pid)
		}
		if sessionID == "" {
			continue
		}
		live[sessionID] = true
		w := Window{Kind: kind, WorkDir: procCwd(pid), SessionID: sessionID}
		if l := matchUniqueWindow(ws, owners, pidSet(ancestors(pid))); l != nil {
			w.XID = l.XID
			w.Emulator = l.Emulator
			w.Workspace = l.Workspace
			w.X, w.Y, w.W, w.H = l.X, l.Y, l.W, l.H
			w.Title = l.Title
			w.Maximized = l.Maximized
		}
		UpsertWindow(snap, w)
	}
	pruneDeadSessions(snap, live)
}

// pruneDeadSessions removes XID-less opencode/codex entries whose session is
// no longer alive in /proc. claude entries are untouched — there the Touch()
// hooks are the liveness authority. XID-backed entries stay: pruning them is
// PruneMissing's job on the next window scan.
func pruneDeadSessions(snap *Snapshot, liveSessions map[string]bool) {
	out := snap.Windows[:0]
	for _, w := range snap.Windows {
		if w.XID == "" && (w.Kind == "opencode" || w.Kind == "codex") && w.SessionID != "" && !liveSessions[w.SessionID] {
			continue
		}
		out = append(out, w)
	}
	snap.Windows = out
}

// dedupeSessionEntries keeps one snapshot entry per session id. Da repeated
// session can arrive twice — enrichFromDaemon under the window key
// ("x:..."), the /proc scan under the session key ("s:...") — and Restore
// would start it twice. The XID-backed entry wins (richer geometry), a
// window-less survivor is kept when no richer one exists.
func dedupeSessionEntries(s *Snapshot) {
	pos := map[string]int{}
	keep := s.Windows[:0]
	for _, w := range s.Windows {
		if w.SessionID == "" {
			keep = append(keep, w)
			continue
		}
		if j, ok := pos[w.SessionID]; ok {
			if w.XID != "" && keep[j].XID == "" {
				keep[j] = w
			}
			continue
		}
		pos[w.SessionID] = len(keep)
		keep = append(keep, w)
	}
	s.Windows = keep
}
