package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/daemon"
	"github.com/carsteneu/yesmem/internal/proxy"
)

func runProxy() {
	dataDir := yesmemDataDir()
	cfg, _ := config.Load(filepath.Join(dataDir, "config.yaml"))

	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	port := fs.Int("port", 0, "listen port (overrides config)")
	threshold := fs.Int("threshold", 0, "token threshold for stubbing (overrides config)")
	target := fs.String("target", "", "target API URL (overrides config)")
	openaiTarget := fs.String("openai-target", "", "OpenAI upstream URL (overrides config)")
	keepRecent := fs.Int("keep-recent", 0, "messages to always keep (overrides config)")
	resetCache := fs.Bool("reset-cache", false, "clear persisted frozen stubs and decay state")
	fs.Parse(os.Args[2:])

	pcfg := buildProxyConfig(cfg, dataDir)

	// CLI overrides
	if *port > 0 {
		pcfg.ListenAddr = fmt.Sprintf(":%d", *port)
	}
	if *threshold > 0 {
		pcfg.TokenThreshold = *threshold
	}
	if *target != "" {
		pcfg.TargetURL = *target
	}
	if *openaiTarget != "" {
		pcfg.OpenAITargetURL = *openaiTarget
	}
	if *keepRecent > 0 {
		pcfg.KeepRecent = *keepRecent
	}
	if *resetCache {
		pcfg.ResetCache = true
	}

	// Defaults
	if pcfg.ListenAddr == "" {
		pcfg.ListenAddr = ":9099"
	}
	if pcfg.TargetURL == "" {
		pcfg.TargetURL = "https://api.anthropic.com"
	}
	if pcfg.TokenThreshold == 0 {
		pcfg.TokenThreshold = 250000
	}
	if pcfg.TokenMinimumThreshold == 0 {
		pcfg.TokenMinimumThreshold = 100000
	}
	if pcfg.KeepRecent == 0 {
		pcfg.KeepRecent = 10
	}
	if pcfg.OpenAITargetURL == "" {
		pcfg.OpenAITargetURL = "https://api.openai.com"
	}

	if err := proxy.Run(pcfg); err != nil {
		log.Fatalf("proxy: %v", err)
	}
}

func runStop() {
	target := "all"
	if len(os.Args) > 2 {
		target = os.Args[2] // "daemon", "proxy", or "all"
	}

	dataDir := yesmemDataDir()
	stopped := 0

	stop := func(role string) {
		n, _ := stopProcesses(role, dataDir)
		stopped += n
	}
	switch target {
	case "daemon":
		stop("daemon")
	case "proxy":
		stop("proxy")
	default: // "all"
		stop("daemon")
		stop("proxy")
	}

	if stopped == 0 {
		fmt.Fprintf(os.Stderr, "No yesmem %s processes found.\n", target)
	} else {
		fmt.Fprintf(os.Stderr, "Stopped %d process(es).\n", stopped)
	}
}

// stopProcesses stops this installation's instances of role and reports how many it
// ended, plus whether a service manager ended the process instead of a signal.
// Naming the role is all it decides: which processes carry it is daemon.StopRole's
// question, and that one verifies the argv structure and the data dir (or, where the
// process table cannot be read, the launchd agent or the data dir's PID file)
// instead of trusting a name match.
func stopProcesses(role, dataDir string) (stopped int, byService bool) {
	pids, stoppedByService, err := daemon.StopRole(role, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", role, err)
	}
	for _, pid := range pids {
		fmt.Fprintf(os.Stderr, "  signalled PID %d (%s)\n", pid, role)
	}
	if stoppedByService {
		fmt.Fprintf(os.Stderr, "  stopped the %s service\n", role)
		return 1, true
	}
	return len(pids), false
}

// daemonStillAnswers reports whether something accepts a connection on the daemon
// socket.
func daemonStillAnswers(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func runRestart() {
	dataDir := yesmemDataDir()

	fmt.Fprintf(os.Stderr, "Stopping all yesmem processes...\n")
	stopProcesses("daemon", dataDir)

	// Wait for processes to die
	time.Sleep(1 * time.Second)

	sockPath := filepath.Join(dataDir, "daemon.sock")
	pidPath := filepath.Join(dataDir, "daemon.pid")
	if daemonStillAnswers(sockPath) {
		// On a platform where the instance could not be verified (no /proc) the
		// stop above did nothing. Starting a second daemon on the same database
		// would be worse than not restarting: say so and leave the state alone,
		// including the proxy this restart has not touched yet.
		fmt.Fprintf(os.Stderr, "A daemon still answers on %s and could not be verified as this installation's. Not starting a second instance.\n", sockPath)
		os.Exit(1)
	}
	_, proxyStoppedByService := stopProcesses("proxy", dataDir)

	// Clean up stale socket and PID file
	os.Remove(sockPath)
	os.Remove(pidPath)

	fmt.Fprintf(os.Stderr, "Starting daemon...\n")
	// Where a service manager owns the process, it has to start it again: a daemon
	// spawned here would live outside its supervision, and a kept-alive agent would
	// fight it. The proxy comes back the same way, but only if this restart took it
	// out of the domain: starting an agent the user had unloaded would be a change
	// nobody asked for. Without a manager the flow below is the only path, as before.
	if startedByService, err := daemon.StartRole("daemon"); err != nil {
		log.Fatalf("restart: %v", err)
	} else if startedByService {
		if proxyStoppedByService {
			if _, proxyErr := daemon.StartRole("proxy"); proxyErr != nil {
				fmt.Fprintf(os.Stderr, "  proxy: %v\n", proxyErr)
			}
		}
		if waitForDaemonSocket(sockPath) {
			fmt.Fprintf(os.Stderr, "Daemon ready (started by the service manager).\n")
		} else {
			fmt.Fprintf(os.Stderr, "Daemon started by the service manager but socket not yet ready. Check logs.\n")
		}
		return
	}

	exe, _ := os.Executable()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("restart: %v", err)
	}
	// Detach — don't wait for daemon
	cmd.Process.Release()

	// Wait for socket to appear
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(sockPath); err == nil {
			fmt.Fprintf(os.Stderr, "Daemon ready (PID %d).\n", cmd.Process.Pid)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "Daemon started (PID %d) but socket not yet ready. Check logs.\n", cmd.Process.Pid)
}

// waitForDaemonSocket polls for the daemon socket. Used where the pid of the new
// daemon is not ours to report (a service manager started it).
func waitForDaemonSocket(sockPath string) bool {
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(sockPath); err == nil {
			return true
		}
	}
	return false
}

// ensureProxyRunning starts the proxy if ANTHROPIC_BASE_URL points to
// localhost:9099 and no proxy is listening. Prefers systemd, falls back to direct start.
func ensureProxyRunning() {
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" || !strings.Contains(baseURL, "localhost:9099") {
		return // proxy not configured
	}

	// Quick check: is something listening on :9099?
	conn, err := net.DialTimeout("tcp", "localhost:9099", 500*time.Millisecond)
	if err == nil {
		conn.Close()
		return // already running
	}

	// Try systemd first (Linux)
	if runtime.GOOS == "linux" {
		if err := exec.Command("systemctl", "--user", "start", "yesmem-proxy").Run(); err == nil {
			time.Sleep(300 * time.Millisecond)
			return
		}
	}

	// Try launchd (macOS)
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		plist := filepath.Join(home, "Library", "LaunchAgents", "com.yesmem.proxy.plist")
		if err := exec.Command("launchctl", "load", plist).Run(); err == nil {
			time.Sleep(300 * time.Millisecond)
			return
		}
	}

	// Fallback: direct start
	binaryPath, _ := os.Executable()
	cmd := exec.Command(binaryPath, "proxy")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return // best-effort, don't fail the hook
	}
	cmd.Process.Release()
	time.Sleep(200 * time.Millisecond)
}

// buildProxyConfig maps the loaded configuration onto the proxy's runtime
// config. It is separate from runProxy so a test can assert the mapping without
// starting the proxy: the profile layers once went missing exactly here and no
// test noticed.
func buildProxyConfig(cfg *config.Config, dataDir string) proxy.Config {
	return proxy.Config{
		ListenAddr:             cfg.Proxy.Listen,
		TargetURL:              cfg.Proxy.Target,
		TokenThreshold:         cfg.Proxy.TokenThreshold,
		TokenMinimumThreshold:  cfg.Proxy.TokenMinimumThreshold,
		TokenThresholds:        cfg.Proxy.TokenThresholds,
		KeepRecent:             cfg.Proxy.KeepRecent,
		DataDir:                dataDir,
		OpenAITargetURL:        cfg.Proxy.OpenAITarget,
		AutoConfigureProviders: cfg.Proxy.AutoConfigureProviders,
		ProviderTargets:        cfg.Proxy.ProviderTargets,
		// Inline reflection (config signals.enabled)
		InlineReflectionEnabled:   cfg.Signals.Enabled,
		APIKey:                    cfg.API.APIKey,
		SawtoothEnabled:           cfg.Proxy.SawtoothEnabled,
		CacheTTL:                  cfg.Proxy.CacheTTL,
		UsageDeflationFactor:      cfg.Proxy.UsageDeflationFactor,
		PromptUngate:              cfg.Proxy.PromptUngate,
		PromptRewrite:             cfg.Proxy.PromptRewrite,
		PromptEnhance:             cfg.Proxy.PromptEnhance,
		PromptToolPrefs:           cfg.Proxy.PromptToolPrefs,
		PromptOutputDiscipline:    cfg.Proxy.PromptOutputDiscipline,
		PromptCodingDiscipline:    cfg.Proxy.PromptCodingDiscipline,
		PromptBeweislast:          cfg.Proxy.PromptBeweislast,
		PromptFable:               cfg.Proxy.PromptFable,
		PromptScopeDiscipline:     cfg.Proxy.PromptScopeDiscipline,
		PromptDelegationContract:  cfg.Proxy.PromptDelegationContract,
		PromptClarifyFirst:        cfg.Proxy.PromptClarifyFirst,
		PromptCodeToolsFirst:      cfg.Proxy.PromptCodeToolsFirst,
		PromptWikiFirst:           cfg.Proxy.PromptWikiFirst,
		PromptPatternSuggest:      cfg.Proxy.PromptPatternSuggest,
		EffortFloor:               cfg.Proxy.EffortFloor,
		SkillEvalInject:           cfg.Proxy.SkillEvalInject,
		CacheKeepaliveEnabled:     cfg.Proxy.CacheKeepaliveEnabled,
		CacheKeepaliveMode:        cfg.Proxy.CacheKeepaliveMode,
		CacheKeepalivePings5m:     cfg.Proxy.CacheKeepalivePings5m,
		CacheKeepalivePings1h:     cfg.Proxy.CacheKeepalivePings1h,
		CacheKeepaliveMinMessages: cfg.Proxy.CacheKeepaliveMinMessages,
		EagerStubEnabled:          cfg.Proxy.EagerStubEnabled,
		// Forked agents
		ForkedAgentsEnabled:            cfg.ForkedAgents.Enabled,
		ForkedAgentsModel:              cfg.ForkedAgentsModelID(),
		ForkedAgentsTokenGrowthTrigger: cfg.ForkedAgents.TokenGrowthTrigger,
		ForkedAgentsMaxFailures:        3,
		ForkedAgentsMaxForksPerSession: cfg.ForkedAgents.MaxForksPerSession,
		ForkedAgentsDebug:              cfg.ForkedAgents.Debug,
		QualityModelID:                 cfg.QualityModelID(),
		// Per-model feature gates
		ModelFeatures:   cfg.Proxy.ModelFeatures,
		FeatureDefaults: cfg.Proxy.FeatureDefaults,
		// Profile-aware prompt layers (shared_prompt + per-client profiles).
		Prompt: &cfg.Proxy,
		CustomSystemPrompt: proxy.CustomSystemPromptConfig{
			EnabledOpenCode:   cfg.Proxy.CustomSystemPrompt.EnabledOpenCode,
			EnabledClaudeCode: cfg.Proxy.CustomSystemPrompt.EnabledClaudeCode,
			EnabledCodex:      cfg.Proxy.CustomSystemPrompt.EnabledCodex,
			TemplatePath:      cfg.Proxy.CustomSystemPrompt.TemplatePath,
			ModelTemplates:    cfg.Proxy.CustomSystemPrompt.ModelTemplates,
		},
	}
}
