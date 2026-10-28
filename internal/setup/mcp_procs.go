package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// userHZ is the tick rate /proc reports starttime in. It is a fixed kernel ABI
// constant, 100 on every mainstream configuration; only an unusual embedded
// build reports another value, and a mismatch then skews the displayed age.
const userHZ = 100

// maxCmdlineBytes bounds the cmdline read. A hostile process can present a
// multi-megabyte argv and only the first arguments decide whether it is a
// helper.
const maxCmdlineBytes = 8 << 10

// procRootPath is the process table `yesmem status` scans. Systems without
// /proc (macOS) fail the read and get a notice line instead.
var procRootPath = "/proc"

// reaperParents are the system process managers that take over orphans. A
// helper parented by one of them lost its client even though the parent pid
// still resolves to a live process.
var reaperParents = map[string]bool{"systemd": true, "init": true, "launchd": true}

// MCPHelper is one live `yesmem mcp` stdio helper process. The helpers are
// expected — one per connected client session — but they accumulate when a
// client leaves one behind, and a stale binary keeps serving an outdated tool
// schema until the process dies.
type MCPHelper struct {
	PID    int
	PPID   int
	Age    time.Duration // since start, -1 when the boot time is unknown
	Stale  bool          // binary replaced on disk after the process started
	Orphan bool          // no client owns it any more, nobody will reap it
}

// isMCPHelperArgs reports whether a cmdline belongs to a `yesmem mcp` helper:
// the executable basename carries "yesmem" and "mcp" is the FIRST argument.
// Anchoring on argv[0] keeps a grep for "mcp" inside a yesmem path out of the
// list, and it means a helper started through a wrapper shell (argv[0] = "sh")
// is not listed either. The position matters because main.go dispatches
// exclusively on os.Args[1]: "yesmem search mcp" is a search, not a helper.
func isMCPHelperArgs(args []string) bool {
	if len(args) < 2 || !strings.Contains(filepath.Base(args[0]), "yesmem") {
		return false
	}
	return args[1] == "mcp"
}

// procCmdline reads the NUL-separated argument list of a process.
func procCmdline(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCmdlineBytes))
	if err != nil {
		return nil
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	// The cap can cut the last argument in half; a half argument must not count
	// as an "mcp" match.
	if len(data) == maxCmdlineBytes && !strings.HasSuffix(string(data), "\x00") && len(args) > 0 {
		args = args[:len(args)-1]
	}
	return args
}

// procStat reads ppid and starttime (field 22) from a stat file. The comm field
// may contain spaces and parentheses, so both are read after the LAST ')'.
func procStat(path string) (ppid, startTicks int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+1:]) // field 3 onwards
	if len(fields) < 20 {
		return 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1]) // field 4
	if err != nil {
		return 0, 0, false
	}
	startTicks, err = strconv.Atoi(fields[19]) // field 22
	if err != nil {
		return 0, 0, false
	}
	return ppid, startTicks, true
}

// bootTime reads btime — the boot time in seconds — from a proc stat file.
func bootTime(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		value, found := strings.CutPrefix(line, "btime ")
		if !found {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return secs, true
	}
	return 0, false
}

// isReaperParent reports whether the parent is gone: it is init itself, its pid
// vanished, or the pid now belongs to the process manager that adopts orphans.
// A pid that was recycled by an unrelated process still reads as a live parent —
// a stateless scan cannot tell that apart from a healthy one.
func isReaperParent(procRoot string, ppid int) bool {
	if ppid <= 1 {
		return true
	}
	dir := filepath.Join(procRoot, strconv.Itoa(ppid))
	if !isDir(dir) {
		return true
	}
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return false
	}
	return reaperParents[strings.TrimSpace(string(comm))]
}

// scanMCPHelpers lists the live `yesmem mcp` helpers below procRoot, sorted by
// pid. procRoot is a parameter so tests can point it at a fixture tree.
func scanMCPHelpers(procRoot string, now time.Time) ([]MCPHelper, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	btime, haveBtime := bootTime(filepath.Join(procRoot, "stat"))

	var helpers []MCPHelper
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		if !isMCPHelperArgs(procCmdline(filepath.Join(dir, "cmdline"))) {
			continue
		}
		ppid, startTicks, ok := procStat(filepath.Join(dir, "stat"))
		if !ok {
			continue
		}
		h := MCPHelper{PID: pid, PPID: ppid, Age: -1}
		if haveBtime {
			h.Age = now.Sub(time.Unix(btime+int64(startTicks/userHZ), 0)).Truncate(time.Second)
			if h.Age < 0 {
				h.Age = 0 // clock skew: keep 0 distinct from the -1 unknown marker
			}
		}
		if target, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			h.Stale = strings.HasSuffix(target, " (deleted)")
		}
		h.Orphan = isReaperParent(procRoot, ppid)
		helpers = append(helpers, h)
	}

	sort.Slice(helpers, func(i, j int) bool { return helpers[i].PID < helpers[j].PID })
	return helpers, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// formatMCPHelperSection renders the helpers as status lines. A failed scan
// (no /proc) becomes a notice, not an error. The summary line carries what the
// per-process list hides: more than one helper per client means the client is
// leaking them.
func formatMCPHelperSection(helpers []MCPHelper, err error) []string {
	if err != nil {
		return []string{fmt.Sprintf("  unavailable (%v)", err)}
	}
	if len(helpers) == 0 {
		return []string{"  none"}
	}
	lines := make([]string, 0, len(helpers)+1)
	lines = append(lines, formatMCPHelperSummary(helpers))
	for _, h := range helpers {
		lines = append(lines, formatMCPHelper(h))
	}
	return lines
}

func formatMCPHelperSummary(helpers []MCPHelper) string {
	maxAge := time.Duration(-1)
	counts := map[int]int{}
	orphanGroups := map[int]bool{}
	busiestPid, busiestCount := 0, 0
	for _, h := range helpers {
		if h.Age > maxAge {
			maxAge = h.Age
		}
		counts[h.PPID]++
		if h.Orphan {
			orphanGroups[h.PPID] = true
		}
		if counts[h.PPID] > busiestCount {
			busiestPid, busiestCount = h.PPID, counts[h.PPID]
		}
	}
	oldest := "unknown"
	if maxAge >= 0 {
		oldest = maxAge.String()
	}
	summary := fmt.Sprintf("  %d live, oldest %s", len(helpers), oldest)
	if busiestCount > 1 {
		if orphanGroups[busiestPid] {
			summary += fmt.Sprintf(", %d orphans share parent %d", busiestCount, busiestPid)
		} else {
			summary += fmt.Sprintf(", busiest client pid %d with %d helpers", busiestPid, busiestCount)
		}
	}
	return summary
}

func formatMCPHelper(h MCPHelper) string {
	age := "unknown"
	if h.Age >= 0 {
		age = h.Age.String()
	}
	flags := "ok"
	if h.Orphan || h.Stale {
		var parts []string
		if h.Orphan {
			parts = append(parts, "orphan")
		}
		if h.Stale {
			parts = append(parts, "stale")
		}
		flags = strings.Join(parts, ", ")
	}
	return fmt.Sprintf("  pid %-7d ppid %-7d age %-10s %s", h.PID, h.PPID, age, flags)
}

// ReapStaleMCPHelpers terminates the live helpers whose binary was replaced on
// disk and returns the pids it signalled — or, with a nil kill function, the pids
// it would signal.
//
// The selection is the scan `yesmem status` renders, not a name grep: a process
// counts only when argv[0] names a yesmem binary and argv[1] is "mcp". The former
// shell rule matched every command line containing "yesmem mcp" (a grep, a search
// subcommand, another installation's helper) and killed whatever it found with a
// "(deleted)" executable. Two further guards: only helpers of the current user
// (a reap under sudo is not a licence to end another account's sessions), and a
// fresh scan right before the signal, so a pid the scan saw cannot have been
// recycled meanwhile.
func ReapStaleMCPHelpers(procRoot string, now time.Time, selfPID int, kill func(pid int) error) ([]int, error) {
	helpers, err := scanMCPHelpers(procRoot, now)
	if err != nil {
		return nil, err
	}
	var reaped []int
	for _, h := range helpers {
		if !h.Stale || h.PID <= 1 || h.PID == selfPID {
			continue
		}
		if !ownedByCurrentUser(procRoot, h.PID) {
			continue
		}
		if !stillStaleHelper(procRoot, h.PID, selfPID) {
			continue
		}
		if kill != nil {
			if err := kill(h.PID); err != nil {
				continue
			}
		}
		reaped = append(reaped, h.PID)
	}
	return reaped, nil
}

// stillStaleHelper re-reads one process entry: the scan that selected it finished
// some time ago, and only a second look can tell whether the pid still names a
// stale helper. One pid, not a second full sweep — the reap may be looking at
// several candidates.
func stillStaleHelper(procRoot string, pid, selfPID int) bool {
	if pid <= 1 || pid == selfPID {
		return false
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if !isMCPHelperArgs(procCmdline(filepath.Join(dir, "cmdline"))) {
		return false
	}
	target, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return false
	}
	return strings.HasSuffix(target, " (deleted)")
}

// ownedByCurrentUser reports whether a process belongs to the user running this
// command.
func ownedByCurrentUser(procRoot string, pid int) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(procRoot, strconv.Itoa(pid)), &st); err != nil {
		return false
	}
	return int(st.Uid) == os.Geteuid()
}
