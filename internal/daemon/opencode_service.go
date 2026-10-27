package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
)

// A detached OpenCode server still inherits the daemon's systemd cgroup.
// Stopping yesmem then kills the shared server and every session's shell,
// including a deployment that was supposed to restart yesmem afterwards.
// Explicit --server prevents the agent TUI from starting a shared server in
// our cgroup. New servers are launched by the user manager in their own unit.
var prepareOpencodeAgentServer = defaultPrepareOpencodeAgentServer

var opencodeServiceStartMu sync.Mutex

func processControlGroup(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && (parts[1] == "" || parts[1] == "name=systemd") {
			return parts[2], nil
		}
	}
	return "", nil
}

func serviceControlGroup(group string) bool {
	return strings.HasSuffix(filepath.Base(group), ".service")
}

func opencodeServerConnection(svc opencode.Service, daemonGroup, serverGroup string) ([]string, []string, error) {
	if serverGroup == daemonGroup || strings.HasPrefix(serverGroup, daemonGroup+"/") {
		return nil, nil, fmt.Errorf("OpenCode server PID %d belongs to the daemon's service; restart that shared server outside yesmem before spawning agents", svc.PID)
	}
	return []string{"--server", svc.URL}, []string{"OPENCODE_PASSWORD=" + svc.Password}, nil
}

func defaultPrepareOpencodeAgentServer(bin string) ([]string, []string, error) {
	group, err := processControlGroup(os.Getpid())
	if err != nil || !serviceControlGroup(group) {
		// Non-systemd hosts and interactive daemons have no service stop that
		// can sweep up a shared server. Preserve their existing startup path.
		return nil, nil, nil
	}
	help, err := runHelpOutput(bin)
	if err != nil {
		return nil, nil, fmt.Errorf("probe OpenCode server support: %w", err)
	}
	if !strings.Contains(help, "--server ") {
		return nil, nil, nil // v1 has no shared background service
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	opencodeServiceStartMu.Lock()
	defer opencodeServiceStartMu.Unlock()
	svc, err := opencode.FirstLiveService(home)
	if err != nil {
		if err := startIndependentOpencodeServer(bin); err != nil {
			return nil, nil, err
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			svc, err = opencode.FirstLiveService(home)
			if err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("independent OpenCode server did not become ready: %w", err)
		}
	}
	serverGroup, err := processControlGroup(svc.PID)
	if err != nil {
		return nil, nil, fmt.Errorf("check OpenCode server ownership: %w", err)
	}
	return opencodeServerConnection(svc, group, serverGroup)
}

func startIndependentOpencodeServer(bin string) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"--user", "--quiet", "--collect", "--property=Type=exec",
		"--unit=yesmem-opencode-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	// systemd supplies the home and runtime dir. Preserve the executable search
	// path and proxy address the server's tools need, but not agent identity.
	for _, key := range []string{"PATH", "ANTHROPIC_BASE_URL", "DISPLAY", "XAUTHORITY"} {
		if value, ok := os.LookupEnv(key); ok {
			args = append(args, "--setenv="+key+"="+value)
		}
	}
	args = append(args, "--", path, "serve", "--service")
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("start independent OpenCode server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
