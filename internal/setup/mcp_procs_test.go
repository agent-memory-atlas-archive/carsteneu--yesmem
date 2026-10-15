package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pids of the fixture tree. 100/102/121/130/300 are helpers, 200-203 must be
// ignored, 99/105/107 are live parent processes.
const (
	fixtureHelperOrphan   = 100
	fixtureHelperStale    = 102
	fixtureHelperOk       = 121
	fixtureHelperReaped   = 130
	fixtureHelperClient   = 131
	fixtureHelperLost     = 300
	fixtureParentReaper   = 107
	fixtureParentLive     = 108
	fixtureParentSystemd  = "systemd"
	fixtureParentOpencode = "opencode"
	fixtureBtime          = 1700000000
)

// writeFixtureFile creates a file below the fixture tree.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// procStatLine renders a /proc/<pid>/stat line carrying the two fields the scan
// reads: ppid (field 4) and starttime (field 22), both after the comm in
// parentheses (field 2).
func procStatLine(pid int, comm string, ppid, startTicks int) string {
	fields := make([]string, 20) // fields 3..22
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[1] = strconv.Itoa(ppid)
	fields[19] = strconv.Itoa(startTicks)
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " "))
}

// addProc creates one fake /proc/<pid> directory.
func addProc(t *testing.T, root string, pid, ppid int, cmdline []string, exe, comm string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	startTicks := map[int]int{
		fixtureHelperOrphan: 30000, // 300s uptime
		fixtureHelperStale:  2000,  // 20s
		fixtureHelperOk:     0,     // 0s
		fixtureHelperLost:   60000, // 600s
	}[pid]
	writeFixtureFile(t, filepath.Join(dir, "stat"), procStatLine(pid, comm, ppid, startTicks))
	writeFixtureFile(t, filepath.Join(dir, "cmdline"), strings.Join(cmdline, "\x00")+"\x00")
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		t.Fatalf("symlink %s: %v", dir, err)
	}
}

// buildProcFixture lays out a /proc-like tree: five yesmem-mcp helpers (orphan,
// stale, ok, adopted by the reaper, lost parent) plus processes that must not be
// listed.
func buildProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu  1 2 3 4\nbtime %d\n", fixtureBtime))

	const yesmem = "/opt/yesmem/bin/yesmem"
	mcpArgs := []string{yesmem, "mcp"}

	// Live parent processes. 107 is the system process manager that adopts
	// orphans, so a helper parented by it lost its client; 108 is a healthy
	// client with a readable comm.
	for _, pid := range []int{99, 105, fixtureParentReaper, fixtureParentLive} {
		if err := os.MkdirAll(filepath.Join(root, strconv.Itoa(pid)), 0o755); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
	}
	writeFixtureFile(t, filepath.Join(root, strconv.Itoa(fixtureParentReaper), "comm"), fixtureParentSystemd+"\n")
	writeFixtureFile(t, filepath.Join(root, strconv.Itoa(fixtureParentLive), "comm"), fixtureParentOpencode+"\n")

	addProc(t, root, fixtureHelperOrphan, 1, mcpArgs, yesmem, "yesmem")
	addProc(t, root, fixtureHelperStale, 99, mcpArgs, yesmem+" (deleted)", "yesmem (dev)")
	addProc(t, root, fixtureHelperOk, 105, mcpArgs, yesmem, "yesmem")
	addProc(t, root, fixtureHelperReaped, fixtureParentReaper, mcpArgs, yesmem, "yesmem")
	addProc(t, root, fixtureHelperClient, fixtureParentLive, mcpArgs, yesmem, "yesmem")
	// Parent 410 is not in the tree — the helper lost it.
	addProc(t, root, fixtureHelperLost, 410, mcpArgs, yesmem, "yesmem")

	// Ignored: other clients' processes and non-mcp yesmem commands.
	addProc(t, root, 200, 1, []string{"/usr/bin/opencode", "serve", "--service"}, "/usr/bin/opencode", "opencode")
	addProc(t, root, 201, 1, []string{yesmem, "daemon"}, yesmem, "yesmem")
	addProc(t, root, 202, 1, []string{yesmem, "status"}, yesmem, "yesmem")
	// Ignored: "mcp" as a grep argument next to a yesmem path.
	addProc(t, root, 203, 1, []string{"grep", "-r", "mcp", "/workspaces/yesmem"}, "/usr/bin/grep", "grep")
	// Ignored: "mcp" as an argument of another yesmem subcommand.
	addProc(t, root, 204, 1, []string{yesmem, "search", "mcp"}, yesmem, "yesmem")
	return root
}

// TestIsMCPHelperArgs covers the argv anchor. main.go dispatches exclusively on
// os.Args[1], so a helper is exactly "yesmem mcp" — accepting "mcp" at any
// position listed "yesmem search mcp" as a helper, and the status section then
// reports such a row as a possibly orphaned helper.
func TestIsMCPHelperArgs(t *testing.T) {
	const yesmem = "/opt/yesmem/bin/yesmem"
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"mcp as first argument", []string{yesmem, "mcp"}, true},
		{"mcp with extra arguments", []string{yesmem, "mcp", "--verbose"}, true},
		{"mcp as a later argument", []string{yesmem, "search", "mcp"}, false},
		{"mcp as a search query", []string{yesmem, "search", "mcp", "helper"}, false},
		{"daemon", []string{yesmem, "daemon"}, false},
		{"status", []string{yesmem, "status"}, false},
		{"no arguments", []string{yesmem}, false},
		{"foreign executable", []string{"/usr/bin/grep", "-r", "mcp", "/workspaces/yesmem"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMCPHelperArgs(tc.args); got != tc.want {
				t.Errorf("isMCPHelperArgs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestScanMCPHelpersFindsLiveHelpers(t *testing.T) {
	root := buildProcFixture(t)
	now := time.Unix(fixtureBtime+1000, 0)

	got, err := scanMCPHelpers(root, now)
	if err != nil {
		t.Fatalf("scanMCPHelpers: %v", err)
	}

	want := []MCPHelper{
		{PID: fixtureHelperOrphan, PPID: 1, Age: 700 * time.Second, Orphan: true},
		{PID: fixtureHelperStale, PPID: 99, Age: 980 * time.Second, Stale: true},
		{PID: fixtureHelperOk, PPID: 105, Age: 1000 * time.Second},
		{PID: fixtureHelperReaped, PPID: fixtureParentReaper, Age: 1000 * time.Second, Orphan: true},
		{PID: fixtureHelperClient, PPID: fixtureParentLive, Age: 1000 * time.Second},
		{PID: fixtureHelperLost, PPID: 410, Age: 400 * time.Second, Orphan: true},
	}
	if len(got) != len(want) {
		t.Fatalf("helpers = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("helper[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestScanMCPHelpersReportsMissingProcRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "no-proc-here")

	if _, err := scanMCPHelpers(root, time.Now()); err == nil {
		t.Fatal("scanMCPHelpers on a missing proc root returned no error")
	}
}

func TestScanMCPHelpersReportsUnknownAgeWithoutBtime(t *testing.T) {
	root := buildProcFixture(t)
	writeFixtureFile(t, filepath.Join(root, "stat"), "cpu  1 2 3 4\n")

	got, err := scanMCPHelpers(root, time.Unix(fixtureBtime+1000, 0))
	if err != nil {
		t.Fatalf("scanMCPHelpers: %v", err)
	}
	for _, h := range got {
		if h.Age != -1 {
			t.Errorf("helper %d age = %v, want -1 (unknown)", h.PID, h.Age)
		}
	}
}

func TestScanMCPHelpersClampsNegativeAge(t *testing.T) {
	root := buildProcFixture(t)
	// The clock reads before some helpers "started" (300s/20s/600s uptime vs.
	// 10s of boot time): clamping keeps 0 distinct from the -1 unknown marker
	// for those, while helpers starting at uptime 0 keep their real 10s.
	got, err := scanMCPHelpers(root, time.Unix(fixtureBtime+10, 0))
	if err != nil {
		t.Fatalf("scanMCPHelpers: %v", err)
	}
	want := map[int]time.Duration{
		fixtureHelperOrphan: 0,
		fixtureHelperStale:  0,
		fixtureHelperOk:     10 * time.Second,
		fixtureHelperReaped: 10 * time.Second,
		fixtureHelperClient: 10 * time.Second,
		fixtureHelperLost:   0,
	}
	for _, h := range got {
		if h.Age != want[h.PID] {
			t.Errorf("helper %d age = %v, want %v", h.PID, h.Age, want[h.PID])
		}
	}
}

func TestScanMCPHelpersBoundsCmdlineRead(t *testing.T) {
	root := buildProcFixture(t)
	// A hostile process can present a multi-megabyte argv. The scan reads only
	// the head of it, so an "mcp" argument beyond the bound is not seen — the
	// accepted trade-off for a bounded read.
	dir := filepath.Join(root, "401")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pad := strings.Repeat("x", maxCmdlineBytes+100)
	writeFixtureFile(t, filepath.Join(dir, "cmdline"), "/opt/yesmem/bin/yesmem\x00"+pad+"\x00mcp\x00")
	writeFixtureFile(t, filepath.Join(dir, "stat"), procStatLine(401, "yesmem", 1, 0))
	if err := os.Symlink("/opt/yesmem/bin/yesmem", filepath.Join(dir, "exe")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := scanMCPHelpers(root, time.Unix(fixtureBtime+1000, 0))
	if err != nil {
		t.Fatalf("scanMCPHelpers: %v", err)
	}
	for _, h := range got {
		if h.PID == 401 {
			t.Fatal("helper beyond the cmdline read bound was detected")
		}
	}
}

func TestScanMCPHelpersTruncatesAgeToSeconds(t *testing.T) {
	root := buildProcFixture(t)
	// Half a second past the boot time plus 1000s: the age must not carry the
	// sub-second noise the wall clock would add.
	got, err := scanMCPHelpers(root, time.Unix(fixtureBtime+1000, 500_000_000))
	if err != nil {
		t.Fatalf("scanMCPHelpers: %v", err)
	}
	want := map[int]time.Duration{
		fixtureHelperOrphan: 700 * time.Second,
		fixtureHelperStale:  980 * time.Second,
		fixtureHelperOk:     1000 * time.Second,
		fixtureHelperReaped: 1000 * time.Second,
		fixtureHelperClient: 1000 * time.Second,
		fixtureHelperLost:   400 * time.Second,
	}
	for _, h := range got {
		if h.Age != want[h.PID] {
			t.Errorf("helper %d age = %v, want %v", h.PID, h.Age, want[h.PID])
		}
	}
}

func TestFormatMCPHelperSection(t *testing.T) {
	tests := []struct {
		name    string
		helpers []MCPHelper
		err     error
		want    []string
	}{
		{
			name: "unavailable",
			err:  errors.New("open /proc: no such file or directory"),
			want: []string{"  unavailable (open /proc: no such file or directory)"},
		},
		{
			name: "none",
			want: []string{"  none"},
		},
		{
			name: "orphan stale ok",
			helpers: []MCPHelper{
				{PID: 100, PPID: 1, Age: 700 * time.Second, Orphan: true},
				{PID: 102, PPID: 99, Age: 980 * time.Second, Stale: true},
				{PID: 121, PPID: 105, Age: 1000 * time.Second},
			},
			want: []string{
				"  3 live, oldest 16m40s",
				"  pid 100     ppid 1       age 11m40s     orphan",
				"  pid 102     ppid 99      age 16m20s     stale",
				"  pid 121     ppid 105     age 16m40s     ok",
			},
		},
		{
			name: "orphan and stale",
			helpers: []MCPHelper{
				{PID: 300, PPID: 410, Age: 400 * time.Second, Stale: true, Orphan: true},
				{PID: 301, PPID: 105, Age: -1},
			},
			want: []string{
				"  2 live, oldest 6m40s",
				"  pid 300     ppid 410     age 6m40s      orphan, stale",
				"  pid 301     ppid 105     age unknown    ok",
			},
		},
		{
			name: "busiest parent",
			helpers: []MCPHelper{
				{PID: 5, PPID: 99, Age: time.Minute},
				{PID: 6, PPID: 99, Age: 120 * time.Second},
				{PID: 7, PPID: 105, Age: 30 * time.Second},
			},
			want: []string{
				"  3 live, oldest 2m0s, busiest client pid 99 with 2 helpers",
				"  pid 5       ppid 99      age 1m0s       ok",
				"  pid 6       ppid 99      age 2m0s       ok",
				"  pid 7       ppid 105     age 30s        ok",
			},
		},
		{
			name: "orphan group",
			helpers: []MCPHelper{
				{PID: 11, PPID: 1, Age: 0, Orphan: true},
				{PID: 12, PPID: 1, Age: 0, Orphan: true},
			},
			want: []string{
				"  2 live, oldest 0s, 2 orphans share parent 1",
				"  pid 11      ppid 1       age 0s         orphan",
				"  pid 12      ppid 1       age 0s         orphan",
			},
		},
		{
			name: "unknown age only",
			helpers: []MCPHelper{
				{PID: 9, PPID: 105, Age: -1},
			},
			want: []string{
				"  1 live, oldest unknown",
				"  pid 9       ppid 105     age unknown    ok",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatMCPHelperSection(test.helpers, test.err)
			if len(got) != len(test.want) {
				t.Fatalf("lines = %q, want %q", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Errorf("line[%d] = %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}
