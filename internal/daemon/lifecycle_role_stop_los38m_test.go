package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Los 38m / YM-312: the process-table side of `yesmem stop`/`restart`. The fixture
// tree keeps the test hermetic: the identity check reads cmdline and environ from
// files, so no real daemon has to run.
func writeProcFixture(t *testing.T, root string, pid int, argv, environ []string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture dir for %d: %v", pid, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatalf("fixture cmdline for %d: %v", pid, err)
	}
	if environ == nil {
		return // no environ file: the data dir is unverifiable
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(strings.Join(environ, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatalf("fixture environ for %d: %v", pid, err)
	}
}

func useProcRoot(t *testing.T, root string) {
	t.Helper()
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })
}

// writeProcFixtureStat gives a fixture process a start time, so the identity pair
// (pid, start time) can be exercised.
func writeProcFixtureStat(t *testing.T, root string, pid, startTicks int) {
	t.Helper()
	fields := make([]string, 20) // fields 3..22 of /proc/<pid>/stat
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S" // state
	fields[1] = "1" // ppid
	fields[19] = strconv.Itoa(startTicks)
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "stat"),
		[]byte(fmt.Sprintf("%d (yesmem) %s\n", pid, strings.Join(fields, " "))), 0o644); err != nil {
		t.Fatalf("fixture stat for %d: %v", pid, err)
	}
}

func writeProcFixtureBtime(t *testing.T, root string, btime int64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte(fmt.Sprintf("cpu  1 2 3 4\nbtime %d\n", btime)), 0o644); err != nil {
		t.Fatalf("fixture btime: %v", err)
	}
}

// stubRoleStopSeams records the signals and keeps the grace loop short: tests in
// this package must not run in parallel, the seams are package vars.
func stubRoleStopSeams(t *testing.T, gone func(int) bool, onSignal func(pid int, label string)) *[]string {
	t.Helper()
	var calls []string
	prevSignal, prevGone, prevWait, prevNow := daemonProcSignal, daemonProcGone, daemonProcGraceWait, daemonProcNow
	now := time.Unix(0, 0)
	daemonProcSignal = func(pid int, sig syscall.Signal) error {
		label := "TERM"
		if sig == syscall.SIGKILL {
			label = "KILL"
		}
		calls = append(calls, label+" "+strconv.Itoa(pid))
		if onSignal != nil {
			onSignal(pid, label)
		}
		return nil
	}
	daemonProcGone = gone
	daemonProcGraceWait = func(time.Duration) {}
	daemonProcNow = func() time.Time { now = now.Add(time.Second); return now }
	t.Cleanup(func() {
		daemonProcSignal, daemonProcGone, daemonProcGraceWait, daemonProcNow = prevSignal, prevGone, prevWait, prevNow
	})
	return &calls
}

// TestOwnedRolePIDsRequiresArgvAndDataDir is the narrowed identity: a name match
// is not enough, and a process of another installation is not ours to stop.
func TestOwnedRolePIDsRequiresArgvAndDataDir(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	foreignHome := t.TempDir()

	// ours: started from this HOME, no explicit data dir
	writeProcFixture(t, root, 10, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + home})
	// ours even though a stray YESMEM_DATA_DIR points elsewhere: no service binary
	// reads that variable, so it must not reclassify the process
	writeProcFixture(t, root, 11, []string{"/home/u/.local/bin/yesmem", "daemon", "--replace"}, []string{"HOME=" + home, "YESMEM_DATA_DIR=/srv/elsewhere"})
	// another installation's daemon
	writeProcFixture(t, root, 12, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + foreignHome})
	// a process that only MENTIONS the words in its arguments (the pgrep class)
	writeProcFixture(t, root, 13, []string{"/usr/bin/grep", "yesmem daemon"}, []string{"HOME=" + home})
	// a worktree test binary
	writeProcFixture(t, root, 14, []string{"/home/u/worktrees/yesmem.test", "daemon"}, []string{"HOME=" + home})
	// the other role
	writeProcFixture(t, root, 15, []string{"/home/u/.local/bin/yesmem", "proxy"}, []string{"HOME=" + home})
	// no readable environment: unverifiable
	writeProcFixture(t, root, 16, []string{"/home/u/.local/bin/yesmem", "daemon"}, nil)
	// cleared environment: also unverifiable, not "our default dir"
	writeProcFixture(t, root, 18, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{})
	// a dev build under another name is still this installation
	writeProcFixture(t, root, 17, []string{"/tmp/yesmem-dev", "daemon"}, []string{"HOME=" + home})

	got := ownedRolePIDs("daemon", dataDir, 999)
	want := []int{10, 11, 17}
	if len(got) != len(want) {
		t.Fatalf("ownedRolePIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ownedRolePIDs = %v, want %v", got, want)
		}
	}
}

// TestOwnedRolePIDsIgnoresForeignPIDFile covers the PID file: an entry that names
// a live process of another installation must not be signalled just because the
// file sits in our data dir.
func TestOwnedRolePIDsIgnoresForeignPIDFile(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProcFixture(t, root, 21, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + t.TempDir()})
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.pid"), []byte("21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubRoleStopSeams(t, func(int) bool { return false }, nil)

	if got := ownedRolePIDs("daemon", dataDir, 999); len(got) != 0 {
		t.Fatalf("ownedRolePIDs = %v, want none: a foreign PID file must not widen the target set", got)
	}
}

// TestStopOwnedRoleSignalsOnlyItsOwn walks the whole stop path with a recorder
// instead of signals.
func TestStopOwnedRoleSignalsOnlyItsOwn(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	writeProcFixture(t, root, 31, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + home})
	writeProcFixture(t, root, 32, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + t.TempDir()})
	calls := stubRoleStopSeams(t, func(int) bool { return true }, nil)

	signalled := StopOwnedRole("daemon", dataDir)

	if len(signalled) != 1 || signalled[0] != 31 {
		t.Fatalf("signalled = %v, want [31]", signalled)
	}
	if len(*calls) != 1 || (*calls)[0] != "TERM 31" {
		t.Fatalf("signals = %v, want [TERM 31]", *calls)
	}
}

// TestStopOwnedRoleRefusesKillAfterIdentityLoss is the PID-reuse guard: a process
// that stopped identifying as ours during the grace period must not get SIGKILL.
func TestStopOwnedRoleRefusesKillAfterIdentityLoss(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	writeProcFixture(t, root, 41, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + home})
	calls := stubRoleStopSeams(t, func(int) bool { return false }, func(pid int, label string) {
		if label == "TERM" {
			// The number is recycled: the fixture entry no longer resolves.
			os.RemoveAll(filepath.Join(root, strconv.Itoa(pid)))
		}
	})

	StopOwnedRole("daemon", dataDir)

	for _, call := range *calls {
		if strings.HasPrefix(call, "KILL") {
			t.Fatalf("signals = %v: SIGKILL must follow a positive identity check", *calls)
		}
	}
}

// TestOwnedRolePIDsWithoutAProcTableStopsNothing: on a platform without /proc
// (macOS) the identity check has nothing to read. It must then stop nothing rather
// than fall back to the name match it replaced.
func TestOwnedRolePIDsWithoutAProcTableStopsNothing(t *testing.T) {
	useProcRoot(t, filepath.Join(t.TempDir(), "missing"))
	calls := stubRoleStopSeams(t, func(int) bool { return false }, nil)

	if pids := StopOwnedRole("daemon", filepath.Join(t.TempDir(), "data")); len(pids) != 0 {
		t.Fatalf("StopOwnedRole signalled %v without a process table", pids)
	}
	if len(*calls) != 0 {
		t.Fatalf("signals = %v, want none", *calls)
	}
}

// TestStopOwnedRoleRefusesKillWhenTheStartTimeChanges is the PID-reuse window: the
// number survives, the identity does not. The start time is what tells the two
// apart when a recycled pid even keeps argv and data dir plausible.
func TestStopOwnedRoleRefusesKillWhenTheStartTimeChanges(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	writeProcFixtureBtime(t, root, 1700000000)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	writeProcFixture(t, root, 51, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + home})
	writeProcFixtureStat(t, root, 51, 1000)

	calls := stubRoleStopSeams(t, func(int) bool { return false }, func(pid int, label string) {
		if label == "TERM" {
			// The number is recycled onto a process that started later.
			writeProcFixtureStat(t, root, pid, 2000)
		}
	})

	StopOwnedRole("daemon", dataDir)

	for _, call := range *calls {
		if strings.HasPrefix(call, "KILL") {
			t.Fatalf("signals = %v: a pid whose start time changed must not get SIGKILL", *calls)
		}
	}
}

// TestEffectiveDataDirResolution pins the two ways a running process can name its
// data dir.
func TestEffectiveDataDirResolution(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		want    string
	}{
		{"below own HOME", []string{"HOME=/home/u"}, "/home/u/.claude/yesmem"},
		{"stray override is ignored", []string{"YESMEM_DATA_DIR=/tmp/other", "HOME=/home/u"}, "/home/u/.claude/yesmem"},
		{"missing HOME falls back", []string{"PATH=/usr/bin"}, "/fallback/.claude/yesmem"},
	}
	for _, tc := range cases {
		if got := effectiveDataDir(tc.environ, "/fallback"); got != tc.want {
			t.Errorf("%s: effectiveDataDir = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStopOwnedRoleChecksIdentityBeforeTheFirstSignal covers the check-to-use
// window nobody sees in the SIGKILL test: the scan selects the process, and by the
// time SIGTERM would go out the pid already names something else. No signal at all
// may leave the daemon.
func TestStopOwnedRoleChecksIdentityBeforeTheFirstSignal(t *testing.T) {
	root := t.TempDir()
	useProcRoot(t, root)
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	writeProcFixture(t, root, 61, []string{"/home/u/.local/bin/yesmem", "daemon"}, []string{"HOME=" + home})

	// First look (the scan) dates the process; every later look finds another one.
	looks := 0
	prevStart := procStartTime
	procStartTime = func(int) (time.Time, bool) {
		looks++
		if looks == 1 {
			return time.Unix(1700001000, 0), true
		}
		return time.Unix(1700009999, 0), true
	}
	t.Cleanup(func() { procStartTime = prevStart })

	calls := stubRoleStopSeams(t, func(int) bool { return true }, nil)
	StopOwnedRole("daemon", dataDir)

	if len(*calls) != 0 {
		t.Fatalf("signals = %v, want none: the identity check before SIGTERM did not run", *calls)
	}
}
