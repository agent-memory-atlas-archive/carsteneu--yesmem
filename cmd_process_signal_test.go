//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"testing"
	"time"
)

// procState answers what the process table says about pid: "running" while the
// process lives, "zombie" once it is dead but not yet reaped, "gone" when the
// number holds nothing. Signal 0 cannot make that distinction, and the test below
// runs as PID 1 in a namespace, so its decoys stay zombies until the test ends.
func procState(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "gone"
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return "gone"
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) == 0 {
		return "gone"
	}
	if fields[0] == "Z" {
		return "zombie"
	}
	return "running"
}

// YM-312: `yesmem stop`/`restart` used to signal every process that merely carried
// "yesmem daemon" in its argv (pgrep -f), without checking the argv structure or
// the data dir the process belongs to. A decoy with the same argv but another
// data dir therefore died, and so would a second installation's daemon. This test
// builds exactly that pair.
//
// It signals real processes and must only ever run in a process table of its own:
//
//	go test -c -o .yesmem/tmp/signal.test . &&
//	unshare -rpf --mount-proc ./.yesmem/tmp/signal.test -test.run TestStopProcessesOnlyStopsItsOwnInstallation -test.count=1 -test.v
func TestStopProcessesOnlyStopsItsOwnInstallation(t *testing.T) {
	if os.Getpid() != 1 {
		t.Skip("needs a PID namespace; outside one this would signal host yesmem processes")
	}
	yesBin, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("no 'yes' binary to give a decoy a yesmem argv")
	}

	base := t.TempDir()
	ownHome := filepath.Join(base, "home-own")
	foreignHome := filepath.Join(base, "home-foreign")
	for _, dir := range []string{ownHome, foreignHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ownDir := filepath.Join(ownHome, ".claude", "yesmem")
	// The decoys carry the argv of a yesmem daemon (`<path>/yesmem daemon`) and
	// only differ in the home their environment names: that is the only thing a
	// yesmem service resolves its data dir from.
	spawn := func(home string) *exec.Cmd {
		cmd := exec.Command(yesBin)
		cmd.Args = []string{filepath.Join(base, "yesmem"), "daemon"}
		env := make([]string, 0, len(os.Environ())+1)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "YESMEM_DATA_DIR=") {
				continue
			}
			env = append(env, kv)
		}
		cmd.Env = append(env, "HOME="+home)
		if err := cmd.Start(); err != nil {
			t.Fatalf("decoy: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	foreign := spawn(foreignHome)
	own := spawn(ownHome)
	// Give both a moment to appear in the process table.
	deadline := time.Now().Add(2 * time.Second)
	for procState(foreign.Process.Pid) != "running" || procState(own.Process.Pid) != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("decoys did not start (foreign=%q own=%q)", procState(foreign.Process.Pid), procState(own.Process.Pid))
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopProcesses("daemon", ownDir)

	if state := procState(foreign.Process.Pid); state != "running" {
		t.Errorf("the daemon of a FOREIGN data dir was stopped (state %q): the name alone decided, not the installation", state)
	}
	stopped := false
	for i := 0; i < 100; i++ {
		if procState(own.Process.Pid) != "running" {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Error("the daemon of OUR data dir survived the stop request")
	}
}
