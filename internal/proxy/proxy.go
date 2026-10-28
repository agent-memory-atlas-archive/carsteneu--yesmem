package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/carsteneu/yesmem/internal/buildinfo"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
	tokenizer "github.com/qhenkart/anthropic-tokenizer-go"
)

// Config holds proxy configuration.
type Config struct {
	ListenAddr             string            // e.g. ":9099"
	TargetURL              string            // e.g. "https://api.anthropic.com"
	TokenThreshold         int               // trigger stubbing above this estimated token count
	TokenMinimumThreshold  int               // stub down to this floor
	TokenThresholds        map[string]int    // model-specific thresholds: {"opus": 180000, "haiku": 130000}
	KeepRecent             int               // number of messages to always keep unmodified
	DataDir                string            // yesmem data directory for DB access
	OpenAITargetURL        string            // upstream for OpenAI-format clients; if empty, uses TargetURL
	ProviderTargets        map[string]string // per-provider upstream URLs, e.g. {"deepseek": "https://api.deepseek.com"}
	AutoConfigureProviders bool              // enable auto-discovery of provider targets from opencode config

	// InlineReflectionEnabled gates the inline signal scan (config signals.enabled).
	InlineReflectionEnabled bool
	APIKey                  string // needed for fork API calls

	// Sawtooth cache optimization
	SawtoothEnabled bool   // use sawtooth instead of progressive decay (default: true)
	CacheTTL        string // "ephemeral" (5m) or "1h" (extended)

	// Eager stubbing of closed-turn tool_results in the fresh tail. Default
	// false: correlated with upstream 400s (cache_control landing on thinking
	// blocks) and cache-miss cascades (A/B test ecb7aee: 99% hit without it).
	EagerStubEnabled bool

	// Usage deflation: scale down input_tokens reported to CC to suppress "Context low" warning.
	// 0 = disabled, 0.7 = report 70% of actual tokens. Real values kept for internal tracking.
	UsageDeflationFactor float64

	// System prompt rewriting
	PromptUngate             bool   // strip "may or may not be relevant" disclaimer from CLAUDE.md injection
	PromptRewrite            bool   // strip "Output efficiency" + "short and concise", inject Ant-quality directives
	PromptEnhance            bool   // CLAUDE.md authority reinforcement, comment discipline, persona-based tone
	PromptToolPrefs          bool   // inject [yesmem-tool-prefs] Edit/Write preference + error-semantics warning
	PromptOutputDiscipline   bool   // inject [yesmem-output-discipline] no-preamble + no-skill-eval + exploratory-heuristic
	PromptCodingDiscipline   bool   // inject [yesmem-coding-discipline] read-before-propose + no-brute-force + no-half-finished
	PromptBeweislast         bool   // inject [yesmem-beweislast] fabrication-guard + claim-vs-proof + stance-under-challenge + tool-result-honesty + long-context-erosion
	PromptFable              bool   // inject [yesmem-fable] baseline-comparison + rollback-path + readback-check for high-stakes changes
	PromptScopeDiscipline    bool   // inject [yesmem-scope-discipline] deliver-A-not-A+B+C + adjacent-findings-separate + scope-bound-authorization
	PromptDelegationContract bool   // inject [yesmem-delegation-contract] self-contained-prompts + parallel-dispatch
	PromptClarifyFirst       bool   // inject [yesmem-clarify-first] clarify only when alternative interpretations produce materially different work
	PromptCodeToolsFirst     bool   // inject [yesmem-code-tools-first] prefer MCP code-navigation tools over Agent spawns
	PromptWikiFirst          bool   // inject [yesmem-wiki-first] check per-file wiki BEFORE editing
	PromptPatternSuggest     bool   // record repeated shell-command shapes for offline cap-suggestion analysis
	EffortFloor              string // minimum effort level: "low", "medium", "high", "max" (empty = off)
	SkillEvalInject          string // "true" = verbose eval, "silent" = internal eval only, "false" = disabled

	// Cache keepalive
	CacheKeepaliveEnabled     bool
	CacheKeepaliveMode        string // "auto", "5m", "1h"
	CacheKeepalivePings5m     int
	CacheKeepalivePings1h     int
	CacheKeepaliveMinMessages int // skip keepalive when request body has fewer messages (0 = always)

	// Forked agents
	ForkedAgentsEnabled            bool
	ForkedAgentsModel              string // full model ID
	ForkedAgentsTokenGrowthTrigger int
	ForkedAgentsMaxFailures        int
	ForkedAgentsMaxForksPerSession int
	ForkedAgentsDebug              bool

	// Quality model fallback for forked agents
	QualityModelID string

	// Cache state management
	ResetCache bool // clear persisted frozen stubs and decay state on startup

	// ModelFeatures: per-model behavioral feature gates (prefix-matched).
	// Falls back to FeatureDefaults for models not listed.
	ModelFeatures   map[string]*config.FeatureGates `yaml:"model_features"`
	FeatureDefaults *config.FeatureGates            `yaml:"feature_defaults"`

	// Prompt is the profile-aware prompt configuration (shared + per-profile
	// layers). Nil falls back to the deprecated flat Prompt* fields below.
	Prompt *config.ProxyConfig

	// Custom system prompt: shared SYSTEM.md template for OpenCode and Claude Code pipelines.
	CustomSystemPrompt CustomSystemPromptConfig
}

// CustomSystemPromptConfig controls which proxy pipelines use the SYSTEM.md template.
type CustomSystemPromptConfig struct {
	EnabledOpenCode   bool
	EnabledClaudeCode bool
	EnabledCodex      bool
	TemplatePath      string
	ModelTemplates    map[string]string // per-model overrides: modelPattern → templatePath
}

const maxAnnotations = 5000 // evict oldest when exceeded

// ANSI color codes for log output
const (
	colorReset      = "\033[0m"
	colorDim        = "\033[2m"  // gray — passthrough / no stubbing
	colorGreen      = "\033[32m" // stubbed / compressed
	colorLightGreen = "\033[92m" // FINAL without compression
	colorBlue       = "\033[34m" // injection
	colorOrange     = "\033[33m" // skip / retry
	colorYellow     = "\033[33m" // warnings
	colorRed        = "\033[31m" // errors
)

// Server is the infinite-thread proxy.
type Server struct {
	cfg                 Config
	autoProviderTargets map[string]string // auto-discovered modelID → upstreamURL, guarded by autoTargetsMu
	autoTargetsStamp    uint64            // source-file stamp autoProviderTargets was built from
	autoTargetsReady    bool              // discovery has run for autoTargetsStamp
	// providerPatchRetry holds a failed opencode baseURL patch open, so the next
	// watcher tick retries it. The request path never touches it.
	providerPatchRetry     bool
	providerPatchRetries   int    // attempts spent on the current file state
	providerPatchWarnStamp uint64 // file state the patch failure was reported for
	autoTargetsMu          sync.RWMutex
	autoRefreshMu          sync.Mutex // serializes a running discovery, not the routing reads
	httpClient             *http.Client
	logger                 *log.Logger
	version                string // build version for log tracing
	requestIdx             atomic.Int64

	// Post-hoc annotations: tool_use_id → first ~120 chars of Claude's response
	mu          sync.RWMutex
	annotations map[string]string

	// Progressive decay trackers — one per thread. Keyed by threadID, same
	// scope as narratives/loopStates/frozenStubs (YM-128).
	decayMu       sync.Mutex
	decayTrackers map[string]*DecayTracker

	// Living narrative blocks — one per thread. A single proxy-global narrative
	// leaked one thread's goal, decisions and phases into every other thread's
	// briefing; keyed by threadID, same scope as loopStates/frozenStubs.
	narrativeMu sync.RWMutex
	narratives  map[string]*Narrative

	// Pivot moments cache (from daemon)
	pivotMu     sync.RWMutex
	pivotTexts  []string
	pivotCached time.Time

	// Aggregate stats (Task #2)
	stats *ProxyStats

	// Local tokenizer for accurate token counting
	tokenizer *tokenizer.Tokenizer

	// Cached overhead tokens (system + tools), set on first request per thread
	overheadMu     sync.RWMutex
	overheadTokens int // unused, kept for interface compat

	// Hysteresis state for stubbing (Task #6)
	stubActive atomic.Bool

	// Daemon connection state — first successful connect flips to true
	daemonReady atomic.Bool

	// Retry detection (Task #7)
	retryMu         sync.RWMutex
	lastFingerprint string

	// Session start time for temporal annotation in archive blocks
	sessionStartMu   sync.RWMutex
	sessionStartTime time.Time

	// Runtime config override (set via MCP set_config tool)
	configOverrideMu            sync.RWMutex
	tokenThresholdOverrides     map[string]int // model-key → threshold, "" = global fallback
	threadThresholdOverrides    map[string]int // threadID → session-scoped threshold
	overrideRefreshMu           sync.Mutex
	lastOverrideRefresh         time.Time            // process-wide window for model/global keys
	sessionOverrideRefresh      map[string]time.Time // threadID → last session-key lookup
	sessionOverrideBudget       int                  // session-key lookups left in the current window
	sessionOverrideBudgetEnd    time.Time            // end of the current budget window
	sessionOverrideBudgetWarned bool                 // exhaustion logged for this window

	// Prompt rewrite miss logging: function+Claude Code version → last log time
	rewriteMissMu  sync.Mutex
	rewriteMissLog map[string]time.Time

	// Briefing cache — keyed by threadID. Each Claude Code session thread gets
	// its own briefing+codemap snapshot so a sawtooth refreeze on thread A
	// cannot invalidate thread B's cached message-prefix hash. The cache entry
	// remembers which project it was loaded for; switching project for the
	// same thread evicts the old entry on next read.
	briefingMu    sync.RWMutex
	briefingCache map[string]briefingEntry

	// briefingLoader is an optional test-only seam for refreshBriefing and
	// injectBriefingTurn. Nil in production → both fall back to s.loadBriefing.
	briefingLoader func(project, projectDir, threadID string) briefingData

	// channelLoader is an optional test-only seam for the channel content the
	// OpenAI-format path stashes into a message's meta. Nil in production → it
	// falls back to checkDialogMessages (see loadChannelContent).
	channelLoader func(threadID, project string) dialogResult

	// Inline reflection: track which learning IDs were injected per thread
	lastInjectedIDsMu   sync.Mutex
	lastInjectedIDs     map[string]map[int64]string // threadID → id → source ("briefing"|"associative"|"fresh")
	sessionInjectCounts map[string]map[int64]int    // threadID → learningID → injection count this session
	lastTurnInjected    map[string]map[int64]bool   // threadID → learningIDs injected in previous turn

	// Timestamp tracking: threadID → time of last response completion
	responseTsMu  sync.RWMutex
	responseTimes map[string]time.Time

	// Think reminder: per-thread request counter (replaces hook-think file-based counter)
	thinkMu       sync.Mutex
	thinkCounters map[string]int // threadID → request count

	channelMu          sync.Mutex
	channelInjectCount map[string]int // sessionID → injection turn count

	// Routing fallback warnings: one-shot per model. Makes silent misroutes
	// (model without provider_target entry → fallback upstream → cryptic
	// upstream auth errors) visible in the log. See resolveOpenAITarget.
	routingWarnedMu sync.Mutex
	routingWarned   map[string]bool

	// Prompt cache gating — enables cache_control breakpoints when requests are frequent
	cacheGate *CacheGate

	// Sawtooth cache optimization
	frozenStubs        *FrozenStubs
	collapseBoundaries *collapseBoundaries
	eagerStubMemory    *EagerStubMemory
	capsCache          *CapsCache
	sawtoothTrigger    *SawtoothTrigger
	timestampStore     *TimestampStore
	cacheStatusWriter  *CacheStatusWriter
	cacheKeepalive     *CacheKeepalive
	cacheTTLDetector   *CacheTTLDetector
	// ttlMu serializes the detection transition: recording, propagation and the
	// keepalive retrigger must not interleave, otherwise a slower goroutine
	// applies a stale state over a newer one.
	ttlMu                  sync.Mutex
	lastKeepaliveRetrigger time.Time
	// threadCWD caches the working directory per thread (for opencode: only in first message)
	threadCWDMu sync.RWMutex
	threadCWD   map[string]string // threadID → cwd

	// Threads whose plan outline has already been published for the sidebar in
	// this process. The publish costs a daemon round trip, so it runs once per
	// thread and again whenever a plan tool call was seen.
	planSyncedMu sync.Mutex
	planSynced   map[string]bool

	// Cumulative token savings from collapsing (per threadID)
	rawSavingsMu sync.Mutex
	rawSavings   map[string]int

	// Skill hint tracking: per-thread active skills
	skillTracker *skillHintTracker

	// Forked agent state: per-thread token growth + failure tracking
	forkState   *ForkState
	forkConfigs []ForkConfig
	// forkModelWarned limits the "configured model belongs to another wire family"
	// warning to one line per fork and model instead of one per request.
	forkModelWarned sync.Map

	// Custom system prompt template for OpenCode (loaded from DataDir/SYSTEM.md)
	customSystemPrompt []byte

	// Per-model template overrides (loaded from config CustomSystemPrompt.ModelTemplates)
	modelTemplates map[string][]byte

	// Rules re-injection: condensed CLAUDE.md rules injected every ~40k tokens
	rulesMu               sync.RWMutex
	rulesCache            map[string]rulesCacheEntry // project → cached condensed rules
	rulesTokenCount       map[string]int             // threadID → tokens since last rules injection
	rulesCollapseInjected map[string]bool            // threadID → true if collapse already injected rules (reset by normal inject)
	msgCounters           *msgCounters               // global per-thread msg counter, persists across collapses

	// Loop detection: per-thread warning state
	loopMu     sync.Mutex
	loopStates map[string]*LoopState // threadID → state

	// Fail counter dedupe: threadID → learningID → fingerprint of the failure
	// that was already counted. An identical error must not raise fail_count
	// again (see freshFailIDs).
	failStateMu      sync.Mutex
	failFingerprints map[string]map[int64]uint64
}

// newProxyHTTPClient builds the client every forwarded upstream call uses.
//
// Redirects are not followed: a 3xx from the upstream is handed back as it is.
// The forwarded request carries the provider credentials in its headers, and
// net/http copies them to whatever host the Location names — a compromised or
// misconfigured upstream could collect them by answering with a redirect. The
// keepalive client was hardened the same way (Los 29b); this client was not.
func newProxyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig:    &tls.Config{},
			MaxIdleConns:       10,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: true,
			// Muss über dem Gateway-Request-Timeout (300s) liegen dürfen:
			// PDF-Kaltkonvertierung (docling OCR) läuft im Pre-Call-Hook,
			// bevor der erste Response-Header kommt — beobachtet bis ~96s.
			ResponseHeaderTimeout: 5 * time.Minute,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// liveRunWaiters counts the shutdown goroutines of runUntil. A waiter that is
// never released outlives the server and stays blocked on a channel nobody
// drains any more (YM-399); the counter is what makes that leak observable.
var liveRunWaiters atomic.Int64

// Run starts the proxy server and blocks until interrupted.
func Run(cfg Config) error {
	return runUntil(cfg, nil)
}

// runUntil is Run with a second shutdown trigger. A nil stop channel keeps Run's
// behaviour, where only a signal ends the server. Tests close stop instead of
// signalling the test process, which would reach every handler in it (YM-399).
func runUntil(cfg Config, stop <-chan struct{}) error {
	s := &Server{
		cfg:                   cfg,
		httpClient:            newProxyHTTPClient(),
		logger:                createLogger(cfg.DataDir),
		annotations:           make(map[string]string),
		narratives:            make(map[string]*Narrative),
		stats:                 &ProxyStats{startTime: time.Now()},
		lastInjectedIDs:       make(map[string]map[int64]string),
		version:               buildinfo.Version,
		sessionInjectCounts:   make(map[string]map[int64]int),
		lastTurnInjected:      make(map[string]map[int64]bool),
		responseTimes:         make(map[string]time.Time),
		thinkCounters:         make(map[string]int),
		channelInjectCount:    make(map[string]int),
		routingWarned:         make(map[string]bool),
		rewriteMissLog:        make(map[string]time.Time),
		cacheGate:             NewCacheGate(cacheGapForTTL(cfg.CacheTTL)),
		frozenStubs:           NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL(cfg.CacheTTL)),
		collapseBoundaries:    newCollapseBoundaries(),
		eagerStubMemory:       NewEagerStubMemory(),
		capsCache:             NewCapsCache(),
		timestampStore:        NewTimestampStore(),
		sawtoothTrigger:       NewSawtoothTrigger(cacheGapForTTL(cfg.CacheTTL), cfg.TokenThreshold, cfg.TokenMinimumThreshold),
		cacheStatusWriter:     NewCacheStatusWriter(cfg.DataDir, cfg.CacheTTL, cfg.TokenThreshold, cfg.TokenMinimumThreshold),
		cacheTTLDetector:      NewCacheTTLDetectorWithPersist(cfg.DataDir),
		skillTracker:          newSkillHintTracker(),
		rulesTokenCount:       make(map[string]int),
		rulesCollapseInjected: make(map[string]bool),
		msgCounters:           newMsgCounters(),
		loopStates:            make(map[string]*LoopState),
		forkState:             NewForkState(cfg.ForkedAgentsTokenGrowthTrigger, 60000, cfg.ForkedAgentsMaxFailures, cfg.ForkedAgentsMaxForksPerSession),
		forkConfigs:           []ForkConfig{},
	}

	// The status writer ticks every second for as long as the process lives.
	// Run returns on shutdown or on a listen error, and either way the loop must
	// not keep running (YM-396).
	defer s.cacheStatusWriter.Stop()

	// Load custom system prompt from configured path
	if cfg.CustomSystemPrompt.TemplatePath != "" &&
		(cfg.CustomSystemPrompt.EnabledOpenCode || cfg.CustomSystemPrompt.EnabledClaudeCode || cfg.CustomSystemPrompt.EnabledCodex) {
		s.customSystemPrompt = loadSystemPromptFromPath(cfg.CustomSystemPrompt.TemplatePath)
		if s.customSystemPrompt != nil {
			s.logger.Printf("[proxy] loaded custom system prompt from %s (%d bytes, opencode=%v cc=%v codex=%v)",
				cfg.CustomSystemPrompt.TemplatePath, len(s.customSystemPrompt),
				cfg.CustomSystemPrompt.EnabledOpenCode, cfg.CustomSystemPrompt.EnabledClaudeCode, cfg.CustomSystemPrompt.EnabledCodex)
		} else {
			s.logger.Printf("[proxy] WARNING: custom_system_prompt enabled but template not found at %q — feature disabled",
				cfg.CustomSystemPrompt.TemplatePath)
		}
	}

	// Load per-model template overrides
	if len(cfg.CustomSystemPrompt.ModelTemplates) > 0 {
		s.modelTemplates = make(map[string][]byte, len(cfg.CustomSystemPrompt.ModelTemplates))
		for pattern, path := range cfg.CustomSystemPrompt.ModelTemplates {
			tpl := loadSystemPromptFromPath(path)
			if tpl != nil {
				s.modelTemplates[pattern] = tpl
				s.logger.Printf("[proxy] loaded model template %q from %s (%d bytes)", pattern, path, len(tpl))
			} else {
				s.logger.Printf("[proxy] WARNING: model template %q not found at %q — skipping", pattern, path)
			}
		}
	}

	// Log persisted detection state
	if sup := s.cacheTTLDetector.Is1hSupported(); sup != nil {
		s.logger.Printf("[proxy] loaded persisted TTL detection: 1h_supported=%v", *sup)
		s.applyDetectedCacheTTL(*sup)
	}

	if cfg.CacheKeepaliveEnabled {
		s.cacheKeepalive = NewCacheKeepalive(CacheKeepaliveConfig{
			Target:          cfg.TargetURL,
			ProviderTargets: cfg.ProviderTargets,
			Mode:            cfg.CacheKeepaliveMode,
			Pings5m:         cfg.CacheKeepalivePings5m,
			Pings1h:         cfg.CacheKeepalivePings1h,
			MinMessages:     cfg.CacheKeepaliveMinMessages,
			Detector:        s.cacheTTLDetector,
			Logger:          s.logger,
			OnPing:          s.onKeepalivePing,
		})
	}

	// Wire detector + keepalive to status writer for terminal display
	s.cacheStatusWriter.SetDetector(s.cacheTTLDetector)
	if s.cacheKeepalive != nil {
		s.cacheStatusWriter.SetKeepalive(s.cacheKeepalive)
	}

	// Register forked agent configs
	if cfg.ForkedAgentsEnabled {
		model := cfg.ForkedAgentsModel
		s.forkConfigs = append(s.forkConfigs, NewExtractAndEvaluateConfig(model))
		modelDesc := model
		if modelDesc == "" {
			modelDesc = "main-thread"
		}
		s.logger.Printf("[proxy] forked agents enabled: %d configs, model=%s, trigger=%dk tokens",
			len(s.forkConfigs), journalToken(modelDesc), cfg.ForkedAgentsTokenGrowthTrigger/1000)
	}

	// Wire sawtooth token persistence via daemon RPC
	s.sawtoothTrigger.SetPersistFunc(func(key, value string) {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{"key": key, "value": value}); err != nil {
			s.logger.Printf("[sawtooth] persist failed for %s: %v", key, err)
		}
	})
	s.sawtoothTrigger.SetLoadFunc(func(key string) (string, bool) {
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil || result == nil {
			return "", false
		}
		var resp struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &resp) != nil || resp.Value == "" {
			return "", false
		}
		return resp.Value, true
	})

	// Wire frozen stubs persistence via same daemon RPC
	s.frozenStubs.SetPersistFunc(func(key, value string) {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{"key": key, "value": value}); err != nil {
			s.logger.Printf("[frozen] persist failed for %s: %v", key, err)
		}
	})
	s.frozenStubs.SetLoadFunc(func(key string) (string, bool) {
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil || result == nil {
			return "", false
		}
		var resp struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &resp) != nil || resp.Value == "" {
			return "", false
		}
		return resp.Value, true
	})

	// Wire eager-stub memory persistence via same daemon RPC.
	// Persists per-thread tool_use_id sets so stub decisions survive deploys.
	s.eagerStubMemory.SetPersistFunc(func(key, value string) {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{"key": key, "value": value}); err != nil {
			s.logger.Printf("[eagerstub] persist failed for %s: %v", key, err)
		}
	})
	s.eagerStubMemory.SetLoadFunc(func(key string) (string, bool) {
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil || result == nil {
			return "", false
		}
		var resp struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &resp) != nil || resp.Value == "" {
			return "", false
		}
		return resp.Value, true
	})

	// Wire timestamp store persistence via same daemon RPC
	s.timestampStore.SetPersistFunc(func(key, value string) {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{"key": key, "value": value}); err != nil {
			s.logger.Printf("[timestamps] persist failed for %s: %v", key, err)
		}
	})
	s.timestampStore.SetLoadFunc(func(key string) (string, bool) {
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil || result == nil {
			return "", false
		}
		var resp struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &resp) != nil || resp.Value == "" {
			return "", false
		}
		return resp.Value, true
	})

	// Clear persisted cache state if --reset-cache was passed.
	if cfg.ResetCache {
		s.los26eResetPersistedCacheState()
	}

	// Init local tokenizer (BPE-based, ~60ms)
	tok, err := tokenizer.New()
	if err != nil {
		s.logger.Printf("%s[proxy] tokenizer init failed, falling back to heuristic: %v%s", colorOrange, err, colorReset)
	} else {
		s.tokenizer = tok
		s.logger.Printf("[proxy] tokenizer initialized")
	}

	// Auto-discover provider targets from opencode config
	if cfg.AutoConfigureProviders {
		targets, patchErr := runAutoDiscovery(s.logger, cfg.ProviderTargets)
		stamp := providerSourceStamp()
		s.autoTargetsMu.Lock()
		s.autoProviderTargets, s.autoTargetsStamp, s.autoTargetsReady = targets, stamp, true
		if patchErr != nil {
			// A startup patch that failed leaves no trace in the files, so the
			// watcher would see an unchanged stamp and never try again. Keeping it
			// pending is what makes the retry happen at all.
			s.providerPatchRetry = true
			s.providerPatchWarnStamp = stamp
		}
		s.autoTargetsMu.Unlock()

		// A provider logged in while this process runs must still get its baseURL:
		// the startup patch cannot see it, and without a patched baseURL opencode
		// never routes it through the proxy (YM-326).
		watchStop := make(chan struct{})
		defer close(watchStop)
		go s.watchProviderSources(watchStop, providerSourceWatchInterval)
	} else {
		s.logger.Printf("[proxy] auto_configure_providers is disabled — skipping auto-discovery")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handleRequest)

	srv := &http.Server{
		Addr:    sanitizeListenAddr(cfg.ListenAddr),
		Handler: mux,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	// The registration owns the os/signal loop goroutine, and it must not
	// outlive the server: a listen error returns from Run long before any
	// signal arrives (YM-399).
	defer signal.Stop(done)

	// runDone releases the waiter below on every return path, not only when a
	// signal and not only when a test asks for a stop.
	runDone := make(chan struct{})
	defer close(runDone)

	// The keepalive owns per-thread timers. They belong to this server, so they
	// end with it whatever the reason for the return (YM-399).
	defer func() {
		if s.cacheKeepalive != nil {
			s.cacheKeepalive.Stop()
		}
	}()

	liveRunWaiters.Add(1)
	go func() {
		defer liveRunWaiters.Add(-1)
		select {
		case <-runDone:
			return
		case <-done:
		case <-stop:
		}
		s.logger.Println("shutting down (draining in-flight requests)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			s.logger.Printf("graceful shutdown failed: %v, forcing close", err)
			srv.Close()
		}
	}()

	s.logger.Printf("listening on %s → %s (threshold=%d, keepRecent=%d, model_thresholds=%v)",
		sanitizeListenAddr(cfg.ListenAddr), cfg.TargetURL, cfg.TokenThreshold, cfg.KeepRecent, cfg.TokenThresholds)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

// los26eResetPersistedCacheState clears the frozen and decay state families a
// --reset-cache startup must not inherit. The marker rows of invalidated
// prefixes belong to the same state: without them a reset would leave
// get_compacted_stubs answering from ranges the operator just cleared. The list
// holds the four families the flag names today, not every persisted family: the
// "sawtooth:" and "eagerstub:" rows survive on purpose, since extending the flag
// is a decision about what "--reset-cache" means, not a detail of this loop. A
// failed family is logged and the remaining ones are still attempted: one
// unreachable row must not abort the startup.
func (s *Server) los26eResetPersistedCacheState() {
	for _, prefix := range []string{"frozen:", "frozen_markers:", "decay:", "timestamps:"} {
		result, err := s.queryDaemon("delete_proxy_state_prefix", map[string]any{"prefix": prefix})
		if err != nil {
			s.logger.Printf("[reset-cache] failed to delete %s*: %v", prefix, err)
			continue
		}
		var resp struct{ Deleted int }
		json.Unmarshal(result, &resp)
		s.logger.Printf("[reset-cache] deleted %d %s* entries", resp.Deleted, prefix)
	}
}

// keepaliveRetriggerCooldown bounds how often a detection change may hand a
// thread a fresh ping phase. Pings resend the full prompt and the credential,
// so an upstream that keeps flipping the evidence must not drive them without
// limit.
const keepaliveRetriggerCooldown = 5 * time.Minute

// onKeepalivePing is the keepalive success callback: the ping carried the
// thread's prompt to the upstream and rewrote or reused its cache, so frozen
// stubs and the sawtooth pause timestamp are refreshed. ephemeral1h comes from
// the ping response; passing a constant 0 here would read a 1h-cached prefix
// rewrite as "the API does not honor 1h" and demote the detection.
func (s *Server) onKeepalivePing(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
	s.recordCacheResponse(threadID, cacheRead, cacheWrite, ephemeral1h)
	if s.frozenStubs != nil {
		s.frozenStubs.Touch(threadID)
	}
	if s.sawtoothTrigger != nil {
		s.sawtoothTrigger.TouchRequestTime(threadID)
	}
}

// recordCacheResponse feeds the TTL detector and, when the detection result
// flips, propagates the new cache lifetime to the components that hard-code it.
// Without the propagation a confirmed 1h cache still counts as cold after the
// 5 minute gap and the keepalive keeps its 5 minute phase.
func (s *Server) recordCacheResponse(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
	if s.cacheTTLDetector == nil {
		return
	}
	s.ttlMu.Lock()
	defer s.ttlMu.Unlock()

	next, changed := s.cacheTTLDetector.RecordResponseAndReportChange(threadID, cacheRead, cacheWrite, ephemeral1h)
	if !changed || next == nil {
		return
	}
	if s.logger != nil {
		s.logger.Printf("TTL detection: ephemeral_1h=%d → 1h_supported=%v", ephemeral1h, *next)
	}
	s.applyDetectedCacheTTL(*next)
	// A detection change starts a new keepalive phase, including for the thread
	// whose ping produced the evidence. The refill is rate-limited: an upstream
	// that keeps flipping the evidence must not turn the bounded ping phase into
	// an unbounded one.
	if s.cacheKeepalive != nil && time.Since(s.lastKeepaliveRetrigger) >= keepaliveRetriggerCooldown {
		s.lastKeepaliveRetrigger = time.Now()
		s.cacheKeepalive.Retrigger()
		if s.logger != nil {
			s.logger.Printf("TTL detection: keepalive retriggered (interval=%ds)", s.cacheKeepalive.Status().IntervalS)
		}
	}
}

// applyDetectedCacheTTL moves the cache-lifetime derivatives to a known
// detection result: the frozen stub TTL, the sawtooth pause threshold and the
// cache gate. The measured result wins over the configured CacheTTL: a denied
// 1h cache is a 5 minute cache no matter what the config says.
func (s *Server) applyDetectedCacheTTL(supported bool) {
	ttl := "ephemeral"
	if supported {
		ttl = "1h"
	}
	if s.frozenStubs != nil {
		s.frozenStubs.UpdateTTL(sawtoothTTLForCacheTTL(ttl))
	}
	if s.sawtoothTrigger != nil {
		s.sawtoothTrigger.SetPauseThreshold(cacheGapForTTL(ttl))
	}
	if s.cacheGate != nil {
		s.cacheGate.SetMaxGap(cacheGapForTTL(ttl))
	}
}

// effectiveCacheTTLName returns the cache lifetime the derivatives should be
// built with: the measured result when there is one, the configured value
// otherwise.
func (s *Server) effectiveCacheTTLName() string {
	if s.cacheTTLDetector != nil {
		if sup := s.cacheTTLDetector.Is1hSupported(); sup != nil {
			if *sup {
				return "1h"
			}
			return "ephemeral"
		}
	}
	return s.cfg.CacheTTL
}

// effectiveTokenThreshold returns the runtime token threshold for a thread and model.
// Priority: session override (set_config with session_id, that thread only)
// > model-specific override (MCP set_config token_threshold:<model>)
// > model-specific config (TokenThresholds) > global override (set_config token_threshold)
// > config global default.
func (s *Server) effectiveTokenThreshold(threadID, model string) int {
	// Held for the whole resolution: set_config mutates these maps from another
	// goroutine, and iterating a map that is written concurrently is a fatal
	// "concurrent map read and map write" panic, not just a race-detector hit.
	s.configOverrideMu.RLock()
	defer s.configOverrideMu.RUnlock()

	// 1. Session override (applies to this thread only, never process-wide)
	if threadID != "" {
		if sessionThreshold := s.threadThresholdOverrides[threadID]; sessionThreshold > 0 {
			return sessionThreshold
		}
	}
	overrides := s.tokenThresholdOverrides
	// 2. MCP override (model-specific)
	if len(overrides) > 0 && model != "" {
		lower := strings.ToLower(model)
		for key, threshold := range overrides {
			if key != "" && strings.Contains(lower, strings.ToLower(key)) && threshold > 0 {
				return threshold
			}
		}
	}
	// 3. Config model-specific. The positivity guard matters since the file is
	// re-read: 0 is the documented reset value of set_config, so a 0 that lands
	// in the file must fall through to the global threshold instead of turning
	// every request into a stub trigger.
	if model != "" && len(s.cfg.TokenThresholds) > 0 {
		lower := strings.ToLower(model)
		for key, threshold := range s.cfg.TokenThresholds {
			if threshold > 0 && strings.Contains(lower, strings.ToLower(key)) {
				return threshold
			}
		}
	}
	// 4. Global override fallback (key "")
	if len(overrides) > 0 {
		if globalOverride, ok := overrides[""]; ok && globalOverride > 0 {
			return globalOverride
		}
	}
	// 5. Config global default
	return s.cfg.TokenThreshold
}

// shouldTriggerSawtooth decides whether a stub-cycle must run for this thread.
// The threshold is per model: effectiveTokenThreshold already resolves the
// model-specific override chain, so the trigger must not fall back to the
// global scalar of its own struct.
func (s *Server) shouldTriggerSawtooth(threadID, model string, totalTokens int) TriggerReason {
	if s.sawtoothTrigger == nil {
		return TriggerNone
	}
	return s.sawtoothTrigger.ShouldTrigger(threadID, totalTokens, s.effectiveTokenThreshold(threadID, model))
}

// frozenPrefixExpired reports whether existing frozen stubs must be rebuilt.
// It logs the numbers the decision was made on: the frozen prefix, the fresh
// tail and the request overhead on one side, the raw client history and the
// threshold on the other.
func (s *Server) frozenPrefixExpired(reqIdx int, proj, threadID, model string, totalTokens, frozenTokens, freshTokens, overhead int) bool {
	threshold := s.effectiveTokenThreshold(threadID, model)
	combinedTokens := frozenTokens + freshTokens + overhead
	if !shouldInvalidateFrozen(combinedTokens, threshold) {
		return false
	}
	if s.logger != nil {
		s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: frozen prefix expired (combined=%dk > %dk threshold; raw history=%dk, %dk frozen + %dk fresh + %dk overhead)",
			reqIdx, journalToken(proj), journalToken(threadID), combinedTokens/1000, threshold/1000, totalTokens/1000, frozenTokens/1000, freshTokens/1000, overhead/1000)
	}
	return true
}

// SetTokenThresholdOverride sets a runtime override for the token threshold per model key.
// Pass threshold=0 to reset the override for that key. Key "" is the global fallback.
func (s *Server) SetTokenThresholdOverride(modelKey string, threshold int) {
	s.configOverrideMu.Lock()
	if s.tokenThresholdOverrides == nil {
		s.tokenThresholdOverrides = make(map[string]int)
	}
	if threshold <= 0 {
		delete(s.tokenThresholdOverrides, modelKey)
	} else {
		s.tokenThresholdOverrides[modelKey] = threshold
	}
	s.configOverrideMu.Unlock()

	// Propagate global threshold to sawtooth trigger so runtime overrides take effect
	if modelKey == "" && s.sawtoothTrigger != nil {
		if threshold > 0 {
			s.sawtoothTrigger.SetTokenThreshold(threshold)
		} else {
			s.sawtoothTrigger.SetTokenThreshold(s.cfg.TokenThreshold)
		}
	}
}

// configOverrideRefreshInterval bounds how often the proxy re-reads the token
// threshold overrides from the daemon. A refresh costs one round trip per
// configured model; running it per request made every request pay for the whole
// override set. The price of the throttle is that a set_config reaches the
// proxy within this interval instead of on the next request.
const configOverrideRefreshInterval = 5 * time.Second

// sessionOverrideRefreshCap bounds the per-thread refresh bookkeeping. Clearing
// it only costs each thread one extra lookup; the alternative would be a map
// that grows with every session the proxy ever saw.
const sessionOverrideRefreshCap = 4096

// sessionOverrideRefreshBudget bounds the daemon reads this path may spend per
// window. The per-thread window is not a bound: the thread id comes from the
// client, so a caller that invents one id per request never waits and pays one
// round trip per request (two, when the id carries a backend prefix). The budget
// only caps the rate; a thread that loses the race keeps its last known
// override, because a refresh is a hint, not a state transition.
//
// The value covers a busy swarm: subagent threads refresh like main threads, so
// 32 slots per 5s starved legitimate work. 256 slots stay a hard ceiling for the
// invented-id case (about 100 daemon reads per second worst case) while a few
// hundred concurrent threads fit.
const sessionOverrideRefreshBudget = 256

// refreshConfigOverrides loads runtime config overrides from daemon proxy_state.
// The session-scoped key belongs to its own thread: it lands in the per-thread
// slot, never in the process-wide one.
func (s *Server) refreshConfigOverrides(threadID string) {
	s.refreshSessionOverride(threadID)
	s.refreshSharedOverrides()
}

// refreshSessionOverride reads this thread's own override key. Throttled per
// thread, not per process: a process-wide window would let whichever thread
// arrives first consume it and starve every other thread of its own key.
func (s *Server) refreshSessionOverride(threadID string) {
	if threadID == "" {
		return
	}
	s.overrideRefreshMu.Lock()
	if last, ok := s.sessionOverrideRefresh[threadID]; ok && time.Since(last) < configOverrideRefreshInterval {
		s.overrideRefreshMu.Unlock()
		return
	}
	if !s.acquireSessionOverrideBudgetLocked() {
		s.overrideRefreshMu.Unlock()
		return
	}
	if s.sessionOverrideRefresh == nil || len(s.sessionOverrideRefresh) >= sessionOverrideRefreshCap {
		s.sessionOverrideRefresh = make(map[string]time.Time)
	}
	s.sessionOverrideRefresh[threadID] = time.Now()
	s.overrideRefreshMu.Unlock()

	for i, key := range sessionOverrideKeys(threadID) {
		value, ok := s.readTokenThresholdOverride(key)
		if !ok {
			// One unusable answer is enough to keep the last known override: the
			// key space was not readable in full, so a withdrawal is not proven.
			return
		}
		if value > 0 {
			if i > 0 && s.logger != nil {
				s.logger.Printf("[config] session threshold %d read from unprefixed key %q for %s", value, key, journalToken(threadID))
			}
			s.setThreadTokenThresholdOverride(threadID, value)
			return
		}
	}
	// Every candidate answered readably, so the override is gone. (An unreachable
	// daemon never gets here: its answer is not readable.)
	s.setThreadTokenThresholdOverride(threadID, 0)
}

// acquireSessionOverrideBudgetLocked spends one slot of the process-wide
// refresh budget and reports whether a slot was available. The caller holds
// overrideRefreshMu. A denied refresh costs one map lookup and leaves the last
// known override in place until the next window.
func (s *Server) acquireSessionOverrideBudgetLocked() bool {
	now := time.Now()
	if now.After(s.sessionOverrideBudgetEnd) {
		s.sessionOverrideBudgetEnd = now.Add(configOverrideRefreshInterval)
		s.sessionOverrideBudget = sessionOverrideRefreshBudget
		s.sessionOverrideBudgetWarned = false
	}
	if s.sessionOverrideBudget <= 0 {
		// One line per window: a silent denial would hide a set_config that
		// never reaches its session.
		if !s.sessionOverrideBudgetWarned && s.logger != nil {
			s.sessionOverrideBudgetWarned = true
			s.logger.Printf("[config] session override refresh budget exhausted (%d per %s); overrides stay at their last known value until the next window", sessionOverrideRefreshBudget, configOverrideRefreshInterval)
		}
		return false
	}
	s.sessionOverrideBudget--
	return true
}

// sessionOverrideKeyPrefix is where a per-session runtime override lives. It is a
// family of its own rather than a suffix of config_override: the threshold
// namespace also carries the per-model slots (cmd_config mirrors them through the
// same RPC, and set_config's per-model alias writes them from a caller-chosen model
// name), so a session suffix there cannot be told apart from a model name. The
// daemon refused a written session slot in that namespace for exactly that reason
// (YM-427); with a separate family the key itself says which of the two it is.
const sessionOverrideKeyPrefix = config.SessionOverrideStateKey + ":"

// sessionOverrideKeys lists the proxy_state keys that can carry this thread's
// session override. set_config stores the session_id it was given verbatim,
// while the proxy derives "opencode:<id>" for opencode sessions; a caller that
// passes the bare backend id would otherwise write a key nobody reads.
func sessionOverrideKeys(threadID string) []string {
	keys := []string{sessionOverrideKeyPrefix + threadID}
	if _, rest, ok := strings.Cut(threadID, ":"); ok && rest != "" {
		keys = append(keys, sessionOverrideKeyPrefix+rest)
	}
	return keys
}

// refreshSharedOverrides reads the model-specific and global override keys.
// They are thread-independent, so one process-wide window is enough.
func (s *Server) refreshSharedOverrides() {
	if !s.acquireSharedOverrideWindow() {
		return
	}
	// 2. Model-specific overrides (e.g. config_override:token_threshold:deepseek)
	for modelKey := range s.thresholdModelKeys() {
		s.refreshSharedKey("config_override:token_threshold:"+modelKey, modelKey)
	}
	// 3. Global override (fallback)
	s.refreshSharedKey("config_override:token_threshold", "")
}

// thresholdModelKeys returns the model keys one refresh window has to visit: the
// ones config.yaml names plus the ones already carrying an override.
//
// The map used to be a snapshot from process start, which made a per-model
// override invisible for every model the file did not name at that moment:
// set_config token_threshold:<model> persisted the model into the file and wrote
// its runtime slot, and the proxy never visited that slot. The file is the
// persistent store for exactly this key, so the window re-reads it.
//
// The second half of the set is what makes a withdrawal reachable: a model that
// left the file still has its slot visited, and an empty slot clears the entry.
//
// A file that cannot be read or parsed keeps the in-memory map. It is not
// evidence that the operator dropped the thresholds, and config.Load would
// report exactly that by falling back to defaults.
func (s *Server) thresholdModelKeys() map[string]int {
	if s.cfg.DataDir != "" {
		path := filepath.Join(s.cfg.DataDir, "config.yaml")
		if thresholds, err := config.LoadProxyTokenThresholds(path); err == nil {
			s.configOverrideMu.Lock()
			s.cfg.TokenThresholds = thresholds
			s.configOverrideMu.Unlock()
		}
	}
	s.configOverrideMu.RLock()
	defer s.configOverrideMu.RUnlock()
	keys := make(map[string]int, len(s.cfg.TokenThresholds)+len(s.tokenThresholdOverrides))
	for modelKey := range s.cfg.TokenThresholds {
		keys[modelKey] = 0
	}
	for modelKey := range s.tokenThresholdOverrides {
		keys[modelKey] = 0
	}
	return keys
}

// refreshSharedKey applies one model-scoped or global override key. A key that
// was read successfully clears its slot when it carries no value: a withdrawn
// override must stop applying.
func (s *Server) refreshSharedKey(key, modelKey string) {
	value, ok := s.readTokenThresholdOverride(key)
	if !ok {
		return
	}
	s.SetTokenThresholdOverride(modelKey, value)
}

func (s *Server) acquireSharedOverrideWindow() bool {
	s.overrideRefreshMu.Lock()
	defer s.overrideRefreshMu.Unlock()
	if !s.lastOverrideRefresh.IsZero() && time.Since(s.lastOverrideRefresh) < configOverrideRefreshInterval {
		return false
	}
	s.lastOverrideRefresh = time.Now()
	return true
}

// readTokenThresholdOverride returns the threshold stored under one key. The
// second return value reports whether the answer was usable: a query error or a
// malformed answer must leave the last known override in place, because a daemon
// outage is not evidence that the operator removed it.
func (s *Server) readTokenThresholdOverride(key string) (int, bool) {
	result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
	if err != nil || result == nil {
		return 0, false
	}
	// Pointer, not string: a missing "value" field must stay distinguishable
	// from a present but empty one (a daemon answer of null decodes without error
	// into any struct and would otherwise read as a withdrawal).
	var resp struct {
		Value *string `json:"value"`
	}
	if json.Unmarshal(result, &resp) != nil || resp.Value == nil {
		return 0, false
	}
	threshold, convErr := strconv.Atoi(*resp.Value)
	if convErr != nil || threshold <= 0 {
		return 0, true
	}
	return threshold, true
}

// setThreadTokenThresholdOverride stores a session-scoped threshold for one
// thread. A non-positive threshold removes the entry, so a session that sets no
// override never accumulates state here.
func (s *Server) setThreadTokenThresholdOverride(threadID string, threshold int) {
	if threadID == "" {
		return
	}
	s.configOverrideMu.Lock()
	defer s.configOverrideMu.Unlock()
	if threshold <= 0 {
		delete(s.threadThresholdOverrides, threadID)
		return
	}
	if s.threadThresholdOverrides == nil {
		s.threadThresholdOverrides = make(map[string]int)
	}
	s.threadThresholdOverrides[threadID] = threshold
}

// queryDaemon opens a fresh unix socket connection per call (thread-safe, no import cycle).
// ensureFrozenPersisted checks if frozen stubs for a thread are in the DB.
// If missing (e.g. daemon was unreachable after deploy), re-persists them.
// Runs asynchronously to avoid blocking the request path.
func (s *Server) ensureFrozenPersisted(threadID string) {
	go func() {
		key := "frozen:" + threadID
		result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": key})
		if err != nil {
			return // daemon still unreachable, will retry next request
		}
		var resp struct{ Value string }
		if json.Unmarshal(result, &resp) == nil && resp.Value != "" {
			return // already persisted
		}
		// Not in DB — trigger re-persist from in-memory state
		s.frozenStubs.Persist(threadID)
		s.decayFor(threadID).Persist(threadID)
		s.logger.Printf("[frozen] re-persisted stubs for %s (was missing from DB)", journalToken(threadID))
	}()
}

func (s *Server) queryDaemon(method string, params map[string]any) (json.RawMessage, error) {
	if s.cfg.DataDir == "" {
		return nil, fmt.Errorf("no data dir configured")
	}
	// A bare basename resolves on the daemon side only by luck: once two
	// indexed projects share it, resolution is refused and the call returns
	// empty. Make that visible here instead of at the call site.
	if name, bad := relativeProjectParam(params); bad {
		s.logger.Printf("%s[daemon] WARN: %s sent relative project %q — expected absolute path%s",
			colorOrange, method, journalToken(name), colorReset)
	}
	sockPath := filepath.Join(s.cfg.DataDir, "daemon.sock")

	// On cold start (daemon not yet confirmed ready), retry longer to survive daemon restart
	maxAttempts := 3
	if !s.daemonReady.Load() {
		maxAttempts = 30 // 30 x 500ms = 15s max wait for daemon startup
	}

	var conn net.Conn
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		conn, err = net.DialTimeout("unix", sockPath, 3*time.Second)
		if err == nil {
			s.daemonReady.Store(true)
			break
		}
		// A socket file without a listener is a daemon that is gone, not one that
		// is still starting: ECONNREFUSED cannot turn into a connection by waiting,
		// and the 15s cold-start budget on every RPC would stall the proxy
		// (audit 16.10.2026, YM-338).
		if errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		if attempt < maxAttempts-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	defer conn.Close()
	// Briefing generation can take 10-20s (LLM refinement + scanner);
	// other RPCs (search, learnings) are fast and fine with 5s.
	deadline := 5 * time.Second
	if method == "generate_briefing" {
		deadline = 30 * time.Second
	}
	conn.SetDeadline(time.Now().Add(deadline))

	// JSON-RPC style request/response (matches daemon.SocketServer protocol)
	type request struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params,omitempty"`
	}
	type response struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
	}

	if err := json.NewEncoder(conn).Encode(request{Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("daemon: %s", resp.Error)
	}
	return resp.Result, nil
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if isResponsesPath(r) {
		s.handleResponses(w, r)
		return
	}
	if isOpenAIPath(r) {
		s.handleOpenAICompletions(w, r)
		return
	}
	if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/messages") &&
		!strings.Contains(r.URL.Path, "/batches") &&
		!strings.Contains(r.URL.Path, "/count_tokens") {
		s.handleMessages(w, r)
		return
	}
	s.passthrough(w, r)
}

// pruneIdleThreadState runs the periodic TTL eviction of per-thread proxy
// state. Every request family has to call it: the OpenAI and Responses handlers
// dispatch before handleMessages, so a prune that lived only in the Anthropic
// handler never saw the threads of the other families and their state grew for
// the lifetime of the process.
func (s *Server) pruneIdleThreadState(reqIdx int) {
	if s.sawtoothTrigger != nil {
		if n := s.sawtoothTrigger.Prune(sawtoothStateTTL); n > 0 {
			s.logger.Printf("[req %d] SAWTOOTH: pruned state of %d idle threads", reqIdx, n)
		}
	}
	if s.cfg.SawtoothEnabled && s.frozenStubs != nil {
		if n := s.frozenStubs.Evict(); n > 0 {
			s.logger.Printf("[req %d] SAWTOOTH: evicted %d stale frozen stubs", reqIdx, n)
		}
	}
	s.pruneThreadStateMaps()
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	reqIdx := int(s.requestIdx.Add(1))

	// Track session start time (first request sets it)
	s.sessionStartMu.Lock()
	if s.sessionStartTime.IsZero() {
		s.sessionStartTime = time.Now()
	}
	s.sessionStartMu.Unlock()

	// Sawtooth: periodic TTL eviction of frozen stubs and idle trigger state.
	// The trigger state load runs independently of SawtoothEnabled (token
	// estimation reads it), so its pruning must not be gated on the feature.
	if reqIdx%50 == 0 {
		s.pruneIdleThreadState(reqIdx)
	}

	// Task #9: Bypass — skip all proxy logic
	if isBypassed(r.Header) {
		s.logger.Printf("%s[req %d] bypass active, passthrough%s", colorDim, reqIdx, colorReset)
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadGateway)
			return
		}
		s.forwardRaw(w, r, body, reqIdx)
		return
	}

	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		s.logger.Printf("[req %d] read body error: %v", reqIdx, err)
		http.Error(w, "failed to read request body", http.StatusBadGateway)
		return
	}

	// Parse JSON
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		s.logger.Printf("%s[req %d] JSON parse error, passing through: %v%s", colorDim, reqIdx, err, colorReset)
		s.forwardRaw(w, r, body, reqIdx)
		return
	}

	messages, ok := req["messages"].([]any)
	if !ok {
		s.forwardRaw(w, r, body, reqIdx)
		return
	}

	req["messages"] = messages

	// Count ALL raw messages BEFORE sawtooth — this gives the correct total msg:N
	// for the entire session (CC sends all messages, proxy compacts later).
	rawMsgCount := len(messages)

	// Thread ID: CC header > opencode session header > CC metadata > derived hash.
	// The hash is a cache key shared by every session of one client build, so
	// anything that acts on behalf of the session is gated on threadIsSession.
	threadID, threadIsSession := anthropicThread(req, r.Header)
	if ocSessionID := opencodeSessionID(r.Header); ocSessionID != "" {
		s.logger.Printf("[req %d] %sopencode session=%s tid=%s%s", reqIdx, colorGreen, journalToken(ocSessionID), journalToken(threadID), colorReset)
	}
	proj := extractProjectName(req)
	// projPath is what goes to the daemon; proj stays the short log/display form.
	projPath := extractProjectPath(req)
	model, _ := req["model"].(string)

	// Persist current model under session-keyed proxy_state so handleWhoami
	// can return it. Best-effort: failure is non-fatal (log only).
	if threadID != "" && model != "" {
		if _, err := s.queryDaemon("set_proxy_state", map[string]any{
			"key":   "session_model:" + threadID,
			"value": model,
		}); err != nil {
			s.logger.Printf("[whoami-model] persist failed for %s: %v", journalToken(threadID), err)
		}
	}

	// Refresh runtime config overrides from daemon (per-session or global)
	s.refreshConfigOverrides(threadID)

	// Drop a calibration sample left over from an earlier request: this request
	// either sets its own below or must not be measured at all.
	if s.sawtoothTrigger != nil && threadID != "" {
		s.sawtoothTrigger.ClearSentEstimate(threadID)
	}

	// Track whether req has been modified and needs re-serialization
	needsReserialization := false

	// Message count at request entry (used for actual-based token estimation)
	msgCount := len(messages)

	// Log message count + cache breakpoint positions for diagnostics
	s.logger.Printf("req %d: %d messages total", reqIdx, msgCount)
	logCacheBreakpointLocations(req, s.logger)

	// Pre-modification dump: capture the raw request before any proxy changes.
	if debugDumpsEnabled() {
		if preBody, err := json.Marshal(req); err == nil {
			if p := writeDebugDump(s.cfg.DataDir, threadID, dumpKindPre, reqIdx, preBody); p != "" {
				s.logger.Printf("[req %d] pre-dump %dk to %s", reqIdx, len(preBody)/1024, p)
			}
		}
	}

	// Cache bug mitigations (CC Bun fork issues):
	// Bug 1: Normalize cch= sentinel patterns in message content to prevent
	// Bun's string replacement from corrupting the wrong occurrence.
	if SanitizeBillingSentinel(messages) {
		s.logger.Printf("[req %d] CACHE: sanitized billing sentinel in message content", reqIdx)
		needsReserialization = true
	}
	// Bug 2: Normalize system[0] billing hash to a fixed value so the cache
	// prefix stays stable across fresh/resume session boundaries.
	if NormalizeBillingHeader(req) {
		s.logger.Printf("[req %d] CACHE: normalized billing header cch= hash", reqIdx)
		needsReserialization = true
	}

	// Profile-aware prompt flags. The profile follows the client that owns the
	// thread, not the wire: opencode and codex speak /v1/messages as well (shared
	// + profile layers). Deriving it from threadID matters when a request carries
	// both session headers (a wrapper forwarding a Claude session): the Claude
	// header wins the thread resolution, so a profile taken from the opencode
	// header alone would give that Claude thread the opencode dialect.
	profile := requestProfile(threadID, r.Header.Get("User-Agent"))
	pfInject := s.getPromptFlags(profile)

	// CC custom system prompt replacement: must run before strip/rewrite so replaced blocks
	// don't trigger false-positive strip/rewrite calls.
	s.applyCCSystemPrompt(req)

	// Strip role:system messages unconditionally — must run on every request.
	// StripReminders (inside runStubCycle) only runs on long conversations.
	if filtered := FilterSystemRoleMessages(messages); len(filtered) != len(messages) {
		messages = filtered
		req["messages"] = messages
		needsReserialization = true
	}

	// prompt_ungate: strip the CLAUDE.md subordination disclaimer so user instructions carry full authority.
	if pfInject.Ungate {
		if StripCLAUDEMDDisclaimer(req) {
			s.logger.Printf("[req %d] SYSTEM: stripped CLAUDE.md disclaimer", reqIdx)
			needsReserialization = true
		}
	}

	// prompt_rewrite: strip output-throttling directives, rewrite quality caps, inject Ant-quality directives
	if pfInject.Rewrite {
		// When CC custom system prompt is active, the system prompt has been replaced with SYSTEM.md.
		// Strip/rewrite calls would not find their target strings — skip to avoid log noise.
		if !s.cfg.CustomSystemPrompt.EnabledClaudeCode {
			userAgent := r.Header.Get("User-Agent")
			if StripOutputEfficiency(req) {
				s.logger.Printf("[req %d] REWRITE: stripped Output efficiency section", reqIdx)
				needsReserialization = true
			} else {
				s.logRewriteMiss("StripOutputEfficiency", userAgent)
			}
			if StripToneBrevity(req) {
				s.logger.Printf("[req %d] REWRITE: stripped 'short and concise' from Tone", reqIdx)
				needsReserialization = true
			} else {
				s.logRewriteMiss("StripToneBrevity", userAgent)
			}
			if RewriteGoldPlating(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten gold-plating directive", reqIdx)
				needsReserialization = true
			}
			if RewriteErrorHandling(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten error handling directive", reqIdx)
				needsReserialization = true
			}
			if RewriteThreeLinesRule(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten three-lines rule", reqIdx)
				needsReserialization = true
			}
			if RewriteSubagentCompleteness(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten subagent completeness", reqIdx)
				needsReserialization = true
			}
			if RewriteExploreAgentSpeed(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten explore agent speed bias", reqIdx)
				needsReserialization = true
			}
			if RewriteSubagentCodeSuppression(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten subagent code suppression", reqIdx)
				needsReserialization = true
			}
			if RewriteScopeMatching(req) {
				s.logger.Printf("[req %d] REWRITE: rewritten scope matching", reqIdx)
				needsReserialization = true
			}
		} // end !EnabledClaudeCode guard
		InjectAntDirectives(req)
		s.logger.Printf("[req %d] REWRITE: injected Ant-quality directives", reqIdx)
		needsReserialization = true
	}

	// prompt_enhance: CLAUDE.md authority + comment discipline + persona tone
	if pfInject.Enhance {
		InjectCLAUDEMDAuthority(req)
		if raw, err := s.queryDaemon("get_persona", nil); err == nil {
			var persona map[string]any
			if json.Unmarshal(raw, &persona) == nil {
				if traits, ok := persona["traits"].(map[string]any); ok {
					if verbosity, ok := traits["verbosity"].(string); ok {
						InjectPersonaTone(req, verbosity)
					}
				}
			}
		}
		s.logger.Printf("[req %d] ENHANCE: injected authority + persona tone", reqIdx)
		needsReserialization = true
	}

	// yesmem directive blocks: restore guidance Anthropic dropped 2026-03→04.
	// Profile-aware via getPromptFlags(profile) — shared + profile layers merged.
	pf := s.getPromptFlags(profile)
	if pf.ToolPrefs || pf.OutputDiscipline || pf.CodingDiscipline ||
		pf.Beweislast || pf.ScopeDiscipline || pf.DelegationContract ||
		pf.ClarifyFirst {
		if pf.ToolPrefs {
			InjectClaudeToolPrefs(req)
		}
		if pf.OutputDiscipline {
			InjectOutputDiscipline(req)
		}
		if pf.CodingDiscipline {
			InjectCodingDiscipline(req)
		}
		if pf.Beweislast {
			InjectBeweislast(req)
		}
		if pf.Fable {
			InjectFable(req)
		}
		if pf.ScopeDiscipline {
			InjectScopeDiscipline(req)
		}
		if pf.DelegationContract {
			InjectDelegationContract(req)
		}
		if pf.ClarifyFirst {
			InjectClarifyFirst(req)
		}
		if pf.CodeToolsFirst {
			InjectCodeToolsFirst(req, proj)
		}
		if pf.WikiFirst {
			InjectWikiFirst(req, proj, s.wikiDir(daemonProject(proj, projPath)))
		}
		needsReserialization = true
		s.logger.Printf("[req %d] DIRECTIVES: injected yesmem-* discipline blocks", reqIdx)
	}

	// Turn-sequence recording: hash tool types from previous assistant turn, send to daemon.
	// The gate is the resolved profile, not the deprecated flat field: the flat
	// fields only reach the Claude profile (claudeLegacyFlags), and a
	// prompt_profile layer must be able to state the opposite. With the shipped
	// template (shared_prompt.prompt_pattern_suggest: false) that means Claude
	// records and opencode/codex do not.
	if pf.PatternSuggest && proj != "" && threadID != "" {
		if msgs, ok := req["messages"].([]any); ok {
			if turnHash, toolNames := computeTurnHashFromMessages(msgs); turnHash != "" {
				go s.queryDaemon("record_turn_sequence", map[string]any{
					"thread_id":     threadID,
					"project":       daemonProject(proj, projPath),
					"turn_hash":     turnHash,
					"example_tools": toolNames,
				})
			}
		}
	}

	// thinking normalization: convert thinking.type "enabled" → "adaptive" for models that require it
	if NormalizeThinkingType(req) {
		s.logger.Printf("[req %d] THINKING: normalized type=enabled → adaptive for %s", reqIdx, journalToken(model))
		needsReserialization = true
	}

	// effort_floor: ensure output_config.effort is at least the configured minimum
	if s.cfg.EffortFloor != "" {
		if EnforceEffortFloor(req, s.cfg.EffortFloor) {
			s.logger.Printf("[req %d] EFFORT: enforced floor=%s", reqIdx, s.cfg.EffortFloor)
			needsReserialization = true
		}
	}

	// Inject identity hint on first message of a thread so Claude knows the briefing is from itself.
	if len(messages) <= 1 {
		AppendSystemBlock(req, "yesmem-identity", "The first user message contains your briefing — written by yourself, from your memory. This is not an external document. This is you, remembering.")
		needsReserialization = true
	}

	// Token estimation: actual-based (lastActual + delta) or full count fallback
	overhead := s.measureOverhead(req)
	totalTokens := s.estimateTotalTokens(threadID, messages, overhead)

	// Task #7: Retry detection — skip side-effects on retries
	fp := requestFingerprint(messages)
	isRetryReq := s.isRetry(fp)
	if isRetryReq {
		s.logger.Printf("%s[req %d %s tid=%s] retry detected, skipping side-effects%s", colorOrange, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
	}

	// REPL pattern recording: one record per command of the newest assistant
	// turn. A retry re-sends the same turn, so recording it again would inflate
	// the shape counts.
	if pf.PatternSuggest && proj != "" && !isRetryReq {
		patternMsgs, _ := req["messages"].([]any)
		detectReplPatternSuggestion(patternMsgs, daemonProject(proj, projPath), s.queryDaemon)
	}

	// Increment project turn counter (async, non-blocking, skip on retries)
	if !isRetryReq {
		turnProject := daemonProject(proj, projPath)
		if turnProject == "" {
			turnProject = "__global__"
		}
		go s.queryDaemon("increment_turn", map[string]any{"project": turnProject})
	}

	// Extract user query once (used by re-expansion and associative retrieval)
	userQuery := lastUserText(messages)

	// Extract tool_use_ids for post-hoc annotation (passed as parameter, no shared state)
	toolUseIDs := extractToolUseIDs(messages)

	// === Inline Reflection: scan previous assistant response for signals ===
	s.lastInjectedIDsMu.Lock()
	prevInjected := s.lastInjectedIDs[threadID]
	s.lastInjectedIDsMu.Unlock()

	if len(prevInjected) > 0 && !isRetryReq {
		signals := s.scanInlineSignals(messages)

		// Signal 1: Learning Used — use_count (all sources)
		var confirmedUsed []int64
		for _, id := range signals.UsedIDs {
			if _, ok := prevInjected[id]; ok {
				confirmedUsed = append(confirmedUsed, id)
			}
		}
		if len(confirmedUsed) > 0 {
			idsAny := make([]any, len(confirmedUsed))
			for i, id := range confirmedUsed {
				idsAny[i] = id
			}
			go s.queryDaemon("increment_use", map[string]any{"ids": idsAny})
		}

		// Signal 2: Noise — only for associative and fresh injections.
		// Briefing learnings are injected every turn, so "not used this turn"
		// is expected and not noise. Only contextual injections that were
		// ignored are real noise signals.
		usedSet := make(map[int64]bool, len(confirmedUsed))
		for _, id := range confirmedUsed {
			usedSet[id] = true
		}
		var noiseIDs []int64
		for id, source := range prevInjected {
			if usedSet[id] {
				continue
			}
			if source == "associative" || source == "fresh" {
				noiseIDs = append(noiseIDs, id)
			}
			// briefing: skip — not being used in one turn is expected
		}
		// Only a finished turn proves an injected learning went unused: the scan
		// must have read an answer, and the trailing user message must not be a
		// tool result — then the assistant message read is an intermediate step.
		if signals.Scanned && !isToolContinuation(messages) && len(noiseIDs) > 0 {
			idsAny := make([]any, len(noiseIDs))
			for i, id := range noiseIDs {
				idsAny[i] = id
			}
			go s.queryDaemon("increment_noise", map[string]any{"ids": idsAny})
		}

		// Signal 3: Error Feedback — fail_count on used learnings when tool errors occurred.
		// If Claude referenced a learning and the resulting action still failed,
		// the learning didn't prevent the error. Only bump on USED ids (not all injected),
		// and only once per distinct failure: an error loop repeats the same tool
		// error every turn, and counting each repetition would block the learning.
		if signals.ToolErrorText != "" && len(confirmedUsed) > 0 {
			if freshIDs := s.freshFailIDs(threadID, confirmedUsed, signals.ToolErrorText); len(freshIDs) > 0 {
				failAny := make([]any, len(freshIDs))
				for i, id := range freshIDs {
					failAny[i] = id
				}
				go s.queryDaemon("increment_fail", map[string]any{"ids": failAny})
				if s.logger != nil {
					s.logger.Printf("[inline-reflection] error feedback: %d used learnings → fail_count++", len(freshIDs))
				}
			}
		}

		// Signal 4: Knowledge Gaps
		for _, topic := range signals.GapTopics {
			go s.queryDaemon("track_gap", map[string]any{"topic": topic, "project": daemonProject(proj, projPath)})
		}

		// Signal 3: Contradictions
		for _, desc := range signals.Contradictions {
			go s.queryDaemon("flag_contradiction", map[string]any{"description": desc, "project": daemonProject(proj, projPath), "thread_id": threadID})
		}

		if len(confirmedUsed) > 0 || len(signals.GapTopics) > 0 || len(signals.Contradictions) > 0 {
			s.logger.Printf("[req %d %s tid=%s] inline-reflection: injected=%d used=%d noise=%d gaps=%d contradictions=%d",
				reqIdx, journalToken(proj), journalToken(threadID), len(prevInjected), len(confirmedUsed), len(noiseIDs), len(signals.GapTopics), len(signals.Contradictions))
		}
	}

	// Commit detection: check for git commit in tool_result, trigger async stale-learning evaluation
	if ci := detectGitCommit(messages); ci != nil {
		workdir := extractWorkingDirectory(req)
		s.logger.Printf("[req %d %s tid=%s] git commit detected: %s %s", reqIdx, journalToken(proj), journalToken(threadID), ci.Hash, ci.Message)
		go s.queryDaemon("invalidate_on_commit", map[string]any{
			"hash": ci.Hash, "project": daemonProject(proj, projPath), "workdir": workdir,
		})
	}

	// Task #4: Subagent detection — skip all proxy logic for short conversations
	if isSubagentRequest(r.Header, messages, req) {
		// Inject docs hint for subagents if reference docs are indexed
		if hint := s.getDocsHint(daemonProject(proj, projPath)); hint != "" {
			modified := injectDocsHintForSubagent(messages, hint)
			req["messages"] = modified
			if newBody, err := json.Marshal(req); err == nil {
				body = newBody
			}
			s.logger.Printf("%s[req %d %s tid=%s] %d messages (subagent, docs-hint injected)%s", colorOrange, reqIdx, journalToken(proj), journalToken(threadID), len(messages), colorReset)
		} else {
			s.logger.Printf("%s[req %d %s tid=%s] %d messages (subagent, passthrough)%s", colorOrange, reqIdx, journalToken(proj), journalToken(threadID), len(messages), colorReset)
		}
		s.forwardWithAnnotation(w, r, body, reqIdx, toolUseIDs, proj, projPath, threadID, msgCount, isDeclaredSubagent(r.Header, req))
		return
	}

	// Loop detection: check for repeating tool-call patterns and stash warning.
	// The warning is merged into the last user message's TimestampMeta during
	// entry assembly below — cache-safe via freeze-once-replay through TimestampStore.
	// It runs after the subagent decision: a subagent returns before the warning
	// is injected, so detecting there would raise a counter that never resets.
	loopWarning := ""
	if threadID != "" && !isRetryReq && isFeatureEnabled(&s.cfg, model, "loop_warning") {
		s.loopMu.Lock()
		loopState := s.loopStates[threadID]
		if loopState == nil {
			loopState = &LoopState{}
			s.loopStates[threadID] = loopState
		}
		s.loopMu.Unlock()

		if warning, level := CheckLoopAndFormat(messages, loopState); warning != "" {
			loopWarning = warning
			s.logger.Printf("%s[req %d %s tid=%s] LOOP: level %d warning stashed (cached via TimestampMeta)%s", colorOrange, reqIdx, journalToken(proj), journalToken(threadID), level, colorReset)
		}
	}

	// === Sawtooth Cache Optimization ===
	// When enabled: use frozen stubs between stub-cycles for cache hits.
	// When disabled: fall back to progressive decay on every request above threshold.

	sawtoothCutoff := 0 // raw index where fresh tail starts (0 = no frozen stubs)
	sawtoothStubs := 0  // number of frozen stubs in combined array

	// Advance this thread's own request clock: the decay ages are measured in the
	// thread's own turns, not in the proxy-wide counter, so a quiet thread's stubs
	// do not age with other threads' traffic (YM-275). On the sawtooth path the
	// persisted state is rebased onto that clock.
	decayTracker := s.decayFor(threadID)
	decayIdx := decayTracker.Tick()
	if s.cfg.SawtoothEnabled {
		decayTracker.LoadFromDB(threadID, decayIdx)

		// Check for existing frozen stubs first
		frozen := s.frozenStubs.Get(threadID, messages)
		if frozen != nil {
			sawtoothCutoff = frozen.Cutoff
			sawtoothStubs = len(frozen.Messages)
			// Estimate combined token count: frozen prefix + fresh tail + overhead
			freshMessages := messages[frozen.Cutoff:]
			freshTokens := s.countMessageTokens(freshMessages)
			combinedTokens := frozen.Tokens + freshTokens + overhead

			if s.frozenPrefixExpired(reqIdx, proj, threadID, model, totalTokens, frozen.Tokens, freshTokens, overhead) {
				// Fresh tail grew too large — invalidate and re-stub
				s.invalidateThreadCaches(threadID, proj, extractWorkingDirectory(req), model)
				frozen = nil // fall through to trigger check below
			} else {
				// Use frozen prefix + fresh tail
				combined := make([]any, 0, len(frozen.Messages)+len(freshMessages))
				combined = append(combined, frozen.Messages...)
				combined = append(combined, freshMessages...)
				// Eager-stub large tool_results in fresh tail (turn already closed by a real user prompt)
				beforeEager := s.countMessageTokens(combined[len(frozen.Messages):])
				var stubSticky, stubFresh int
				if s.cfg.EagerStubEnabled {
					combined = EagerStubToolResults(combined, len(frozen.Messages), s.countTokens, WithStubMemory(s.eagerStubMemory, threadID), WithStubCounters(&stubSticky, &stubFresh))
				}
				afterEager := s.countMessageTokens(combined[len(frozen.Messages):])
				if beforeEager != afterEager {
					s.logger.Printf("[req %d %s tid=%s] EAGER-STUB: fresh %dk → %dk (saved %dk) [sticky=%d fresh=%d]", reqIdx, journalToken(proj), journalToken(threadID), beforeEager/1000, afterEager/1000, (beforeEager-afterEager)/1000, stubSticky, stubFresh)
				}
				req["messages"] = combined
				needsReserialization = true
				// Strip embedded CC breakpoints from frozen stubs — they waste cache slots.
				// InjectFrozenStubCacheBreakpoint adds exactly one below.
				if n := StripMessagesCacheControl(req, 0, len(frozen.Messages)); n > 0 {
					s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: stripped %d embedded breakpoints from frozen stubs", reqIdx, journalToken(proj), journalToken(threadID), n)
				}
				s.logger.Printf("%s[req %d %s tid=%s] SAWTOOTH: frozen prefix (%d stubs, %dk msg) + %d fresh (%dk msg) + %dk overhead = %dk total%s",
					colorGreen, reqIdx, journalToken(proj), journalToken(threadID), len(frozen.Messages), frozen.Tokens/1000, len(freshMessages), freshTokens/1000, overhead/1000, combinedTokens/1000, colorReset)
				rawMsgTokens := s.countMessageTokens(messages)
				s.setRawEstimate(threadID, rawMsgTokens+overhead)
				if s.sawtoothTrigger != nil {
					s.sawtoothTrigger.SetSentEstimate(threadID, frozen.Tokens+afterEager+overhead)
				}
				s.logger.Printf("[req %d %s tid=%s] RAW ESTIMATE: msgTokens=%dk + overhead=%dk = %dk (messages=%d)", reqIdx, journalToken(proj), journalToken(threadID), rawMsgTokens/1000, overhead/1000, (rawMsgTokens+overhead)/1000, len(messages))
				if InjectFrozenStubCacheBreakpoint(req, len(frozen.Messages)) {
					s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: frozen stub cache breakpoint at messages[%d]",
						reqIdx, journalToken(proj), journalToken(threadID), len(frozen.Messages)-1)
				}

				// Ensure frozen stubs are persisted (may have failed on first request after deploy)
				s.ensureFrozenPersisted(threadID)
			}
		}
		if frozen == nil {
			// No frozen stubs — check trigger
			triggerReason := s.shouldTriggerSawtooth(threadID, model, totalTokens)
			if triggerReason != TriggerNone {
				s.logger.Printf("[req %d %s tid=%s] SAWTOOTH stub-cycle triggered: %s (tokens=%dk)",
					reqIdx, journalToken(proj), journalToken(threadID), triggerReason, totalTokens/1000)

				// Refresh briefing during stub-cycle — disabled (briefing now via SessionStart hook)
				// s.refreshBriefing(proj)
				// if s.briefingText != "" {
				// 	UpsertSystemBlockCached(req, "yesmem-briefing", s.briefingText)
				// }

				// Run full stub pipeline
				s.setRawEstimate(threadID, totalTokens)
				_ = s.runStubCycle(messages, req, reqIdx, decayIdx, proj, projPath, threadID, overhead, totalTokens, userQuery, isRetryReq)

				// Freeze the result
				finalMessages, _ := req["messages"].([]any)
				if finalMessages != nil && len(finalMessages) > 0 {
					// Boundary = last message of original that maps to last stub
					cutoff := len(messages)
					sawtoothCutoff = cutoff
					var boundaryMsg any
					if cutoff > 0 {
						boundaryMsg = messages[cutoff-1]
					}
					frozenTokens := s.countMessageTokens(finalMessages)
					frozenCount, stripped, breakpointInjected, pairRepairs := s.freezeStubsAndInjectBreakpoint(req, threadID, cutoff, boundaryMsg, frozenTokens, totalTokens)
					s.decayFor(threadID).Persist(threadID)
					if pairRepairs > 0 {
						s.logger.Printf("%s[req %d %s tid=%s] SAWTOOTH: removed %d orphaned tool_result(s) before freezing, cutoff=%d%s",
							colorOrange, reqIdx, journalToken(proj), journalToken(threadID), pairRepairs, cutoff, colorReset)
					}
					if stripped > 0 {
						s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: stripped %d embedded breakpoints before freeze",
							reqIdx, journalToken(proj), journalToken(threadID), stripped)
					}
					s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: frozen %d messages at cutoff=%d (~%dk tokens)",
						reqIdx, journalToken(proj), journalToken(threadID), frozenCount, cutoff, frozenTokens/1000)
					if breakpointInjected {
						s.logger.Printf("[req %d %s tid=%s] SAWTOOTH: frozen stub cache breakpoint at messages[%d]",
							reqIdx, journalToken(proj), journalToken(threadID), frozenCount-1)
					}
				}
				needsReserialization = true
			} else {
				// No trigger, no frozen stubs — eager-stub closed-turn tool_results to delay first collapse
				beforeEager := s.countMessageTokens(messages)
				var stubSticky, stubFresh int
				if s.cfg.EagerStubEnabled {
					messages = EagerStubToolResults(messages, 0, s.countTokens, WithStubMemory(s.eagerStubMemory, threadID), WithStubCounters(&stubSticky, &stubFresh))
				}
				afterEager := s.countMessageTokens(messages)
				if beforeEager != afterEager {
					s.logger.Printf("[req %d %s tid=%s] EAGER-STUB: %dk → %dk (saved %dk) [sticky=%d fresh=%d]", reqIdx, journalToken(proj), journalToken(threadID), beforeEager/1000, afterEager/1000, (beforeEager-afterEager)/1000, stubSticky, stubFresh)
					req["messages"] = messages
					needsReserialization = true
				}
				if s.sawtoothTrigger != nil {
					s.sawtoothTrigger.SetSentEstimate(threadID, afterEager+overhead)
				}
				s.logger.Printf("%s[req %d %s tid=%s] %d messages, ~%dk tokens (sawtooth: no trigger)%s",
					colorDim, reqIdx, journalToken(proj), journalToken(threadID), len(messages), totalTokens/1000, colorReset)
			}
		}
	} else {
		// Legacy path: progressive decay
		if s.shouldStub(totalTokens, model) {
			s.setRawEstimate(threadID, totalTokens)
			_ = s.runStubCycle(messages, req, reqIdx, decayIdx, proj, projPath, threadID, overhead, totalTokens, userQuery, isRetryReq)
			needsReserialization = true
		}
	}

	// Persistent timestamp injection: re-injects stored timestamps on ALL previous user messages.
	// msg:N is computed from position (counting user messages in raw array), NOT from a counter.
	// The injected data is deterministic (same bytes every turn) = cache-safe after initial miss.
	if threadID != "" {
		if currentMsgs, ok := req["messages"].([]any); ok && len(currentMsgs) > 0 {
			// A derived key may still hold another session's frozen content from
			// before the write gate existed: the store is persisted as
			// timestamps:<id> and survives a restart. InjectTimestamps would render
			// it back into this session, so the read side is closed on load, the
			// same way the OpenAI path does it.
			if threadIsSession {
				s.timestampStore.Load(threadID)
			} else {
				s.timestampStore.LoadScrubbed(threadID)
			}

			// Offset = cutoff - stubsCount: maps combined array index to raw position.
			// Raw position of message at combined index I = cutoff + (I - stubs).
			// msgN = rawPos + 1 = cutoff + (I - stubs) + 1 = (cutoff - stubs) + I + 1.
			msgOffset := sawtoothCutoff - sawtoothStubs

			s.logger.Printf("[req %d %s] timestamps: rawTotal=%d, postSawtooth=%d, cutoff=%d, stubs=%d, offset=%d", reqIdx, journalToken(proj), rawMsgCount, len(currentMsgs), sawtoothCutoff, sawtoothStubs, msgOffset)

			// Pre-store assistant timestamp BEFORE inject loop so it's available immediately
			s.responseTsMu.RLock()
			respTime, hasRespTime := s.responseTimes[threadID]
			s.responseTsMu.RUnlock()
			if hasRespTime {
				respStr := shortWeekday(respTime.Weekday()) + " " + respTime.Format("2006-01-02 15:04:05")
				for j := len(currentMsgs) - 2; j >= 0; j-- {
					prevMsg, _ := currentMsgs[j].(map[string]any)
					if prevMsg == nil || prevMsg["role"] != "assistant" {
						continue
					}
					if !msgHasTextBlock(prevMsg) {
						s.logger.Printf("[req %d %s] timestamps: skip assistant[%d] no text block", reqIdx, journalToken(proj), j)
						continue // skip tool-use-only assistant messages
					}
					prevMsgN := msgOffset + j + 1
					if _, exists := s.timestampStore.Get(threadID, prevMsgN); !exists {
						s.timestampStore.Store(threadID, prevMsgN, &TimestampMeta{Timestamp: respStr})
						s.logger.Printf("[req %d %s] timestamps: stored assistant msg:%d ts=%s", reqIdx, journalToken(proj), prevMsgN, respStr)
					} else {
						s.logger.Printf("[req %d %s] timestamps: assistant msg:%d already has ts", reqIdx, journalToken(proj), prevMsgN)
					}
					break
				}
			} else {
				s.logger.Printf("[req %d %s] timestamps: no responseTimes for thread", reqIdx, journalToken(proj))
			}

			// Inject [msg:N] (+ timestamp/delta if stored) on all PREVIOUS messages
			if isFeatureEnabled(&s.cfg, model, "timestamps") {
				if n := InjectTimestamps(s.timestampStore, threadID, currentMsgs, len(currentMsgs)-1, msgOffset, sawtoothStubs); n > 0 {
					needsReserialization = true
					s.logger.Printf("[req %d %s] timestamps: injected %d", reqIdx, journalToken(proj), n)
				}
			}

			// Current (last) user message: full timestamp + delta + msg:N from raw count
			// Loop warning (if stashed above) is merged here — cache-safe via freeze-once-replay.
			lastMsg, _ := currentMsgs[len(currentMsgs)-1].(map[string]any)
			if lastMsg != nil && (lastMsg["role"] == "user" || lastMsg["role"] == "tool") && !msgHasMetaPrefix(lastMsg) {
				now := time.Now()
				nowStr := shortWeekday(now.Weekday()) + " " + now.Format("2006-01-02 15:04:05")
				entry := &TimestampMeta{Timestamp: nowStr}
				if hasRespTime {
					entry.Delta = formatDelta(now.Sub(respTime))
				}

				// Session content may only be frozen under an id that names a
				// session. A derived id is the same string for every session of a
				// client, so a frozen loop warning, plan checkpoint or docs hint
				// would be replayed into a session that never saw it. On a derived
				// key the same text rides the tail message of this request instead
				// (the OpenAI path does the same).
				var tail []string
				if threadIsSession {
					entry.LoopWarning = loopWarning
				} else if loopWarning != "" {
					tail = append(tail, "[loop-warning] "+loopWarning)
				}

				// Plan checkpoint: detect plan tool calls (resets counter), inject reminder if interval fires.
				// The sidebar snapshot is synced regardless of the gate: it reads a different channel.
				planCallSeen := s.detectPlanToolCall(messages, threadID, totalTokens)
				if isFeatureEnabled(&s.cfg, model, "plan_checkpoint") {
					if checkpoint := s.planCheckpointInject(threadID, totalTokens); checkpoint != "" {
						if threadIsSession {
							entry.PlanCheckpoint = checkpoint
							s.logger.Printf("%s[req %d %s tid=%s] plan checkpoint stashed (cached via TimestampMeta)%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
						} else {
							tail = append(tail, "[plan-checkpoint] "+checkpoint)
						}
					}
				}
				s.syncPlanSnapshot(threadID, planCallSeen)

				// Docs hint: inject docs-available reminder if interval fires
				if docsHint := s.docsHintInject(threadID, totalTokens, daemonProject(proj, projPath)); docsHint != "" {
					if threadIsSession {
						entry.DocsHint = docsHint
						s.logger.Printf("%s[req %d %s tid=%s] docs hint stashed (cached via TimestampMeta)%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
					} else {
						tail = append(tail, "[docs-hint] "+docsHint)
					}
				}

				s.timestampStore.Store(threadID, rawMsgCount, entry)
				meta := BuildMeta(rawMsgCount, entry)
				if len(tail) > 0 {
					meta += "\n" + strings.Join(tail, "\n")
				}
				if isFeatureEnabled(&s.cfg, model, "timestamps") || entry.LoopWarning != "" || entry.PlanCheckpoint != "" || entry.DocsHint != "" || len(tail) > 0 {
					prependMeta(lastMsg, meta)
				}
				needsReserialization = true
			}

			s.timestampStore.Persist(threadID)
		}
	}

	// Associative context injection (for ALL requests, not just stubbed ones)
	// Re-enabled with higher thresholds (55/75, max 1) after disabling Fresh Remember.
	// Gated per model feature (assoc_context), like the OpenAI-parity path: a
	// model configured with the feature off must not receive it on either wire.
	var assocContext string
	if isFeatureEnabled(&s.cfg, model, "assoc_context") {
		assocContext = s.findAssociativeContextFor(userQuery, daemonProject(proj, projPath), threadID, messages)
	}
	if assocContext != "" {
		currentMessages, _ := req["messages"].([]any)
		if currentMessages == nil {
			currentMessages = messages
		}
		req["messages"] = injectAssociativeContext(currentMessages, assocContext, s.cfg.SawtoothEnabled)
		needsReserialization = true
		s.logger.Printf("%s[req %d %s tid=%s] associative context injected%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
	}

	// Doc-chunk context injection: separate search path for indexed docs (not skills)
	docContext := s.findDocContextFor(userQuery, daemonProject(proj, projPath), messages)
	if docContext != "" {
		currentMessages, _ := req["messages"].([]any)
		if currentMessages == nil {
			currentMessages = messages
		}
		req["messages"] = injectAssociativeContext(currentMessages, docContext, s.cfg.SawtoothEnabled)
		needsReserialization = true
		s.logger.Printf("%s[req %d %s tid=%s] doc context injected%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
	}

	// Fresh Remember Injection: DISABLED
	// Causes echo-loop: Claude saves via remember(), sees it again next turn as
	// [yesmem fresh memory], reacts to own output instead of user's request.
	// Recovery after /clear is handled by recovery.go (briefing), not here.
	// Subagents get their own briefing. No remaining use-case.
	// remItems := s.popRecentRemember()
	var remItems []recentLearningItem

	// Rules re-injection: inject condensed CLAUDE.md rules every ~40k output tokens
	// Gated per model feature (rules_reminder), like the OpenAI-parity path.
	if isFeatureEnabled(&s.cfg, model, "rules_reminder") {
		if rulesBlock := s.rulesInject(threadID, totalTokens, daemonProject(proj, projPath)); rulesBlock != "" {
			currentMessages, _ := req["messages"].([]any)
			if currentMessages == nil {
				currentMessages = messages
			}
			req["messages"] = injectAssociativeContext(currentMessages, s.formatRulesReminder(rulesBlock, daemonProject(proj, projPath), false), s.cfg.SawtoothEnabled)
			needsReserialization = true
			s.logger.Printf("%s[req %d %s tid=%s] rules reminder injected%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), colorReset)
		}
	}

	// Plan auto-activation: nudge Claude to call set_plan() when reading a plan file
	if planFile := detectPlanFileRead(messages); planFile != "" {
		// A silent daemon is not "no active plan": the injected instruction would
		// claim something this request never learned.
		if activePlan, _, ok := s.getActivePlan(threadID); ok && shouldNudgePlan(planFile, activePlan) {
			name := planNudgeName(planFile)
			AppendSystemBlock(req, "plan-nudge", planNudgeText(name))
			needsReserialization = true
			s.logger.Printf("%s[req %d %s tid=%s] plan nudge injected for %s%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), name, colorReset)
		}
	}

	// Plan checkpoint and docs hint are now injected via TimestampMeta freeze-once
	// in the current-message annotation block above. The old injectAssociativeContext
	// calls were removed as part of the cache-safe migration (see merge 58bca90a).

	if len(remItems) > 0 {
		var lines []string
		for _, item := range remItems {
			if item.ID > 0 {
				lines = append(lines, fmt.Sprintf("- [ID:%d] %s", item.ID, truncateStr(item.Text, 200)))
			} else {
				lines = append(lines, "- "+truncateStr(item.Text, 200))
			}
		}
		remContext := fmt.Sprintf("[yesmem fresh memory]\nGerade gelernt:\n%s\n%s\n[/yesmem fresh memory]", strings.Join(lines, "\n"), InlineReflectionHint)
		currentMessages, _ := req["messages"].([]any)
		if currentMessages == nil {
			currentMessages = messages
		}
		req["messages"] = injectAssociativeContext(currentMessages, remContext, s.cfg.SawtoothEnabled)
		needsReserialization = true
		s.logger.Printf("%s[req %d %s tid=%s] fresh remember injected (%d items)%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), len(remItems), colorReset)
	}

	// Skill activation tracking runs regardless of the skill_eval gate: the
	// tracker is what re-injects skill content after a context collapse
	// (buildSkillBlocksForThread), so it must not depend on whether the
	// evaluation instruction is enabled for this model. The guard is
	// threadIsSession, the same rule the OpenAI path applies: a derived thread
	// key is shared by every session of one client build, so tracking under it
	// would replay one session's skills into another and spend a daemon RPC on
	// a key that names no session.
	if threadIsSession && threadID != "" && proj != "" && isUserInputTurn(messages) && s.skillTracker != nil {
		s.syncSkillActivations(messages, daemonProject(proj, projPath), threadID)
	}

	// Skill eval injection: mandatory evaluation instruction for real user input only.
	// Gated per model feature (skill_eval) exactly like the OpenAI-parity path, so a
	// model configured with skill_eval: false gets no block on either wire.
	if isFeatureEnabled(&s.cfg, model, "skill_eval") && threadID != "" && proj != "" && isUserInputTurn(messages) && s.skillTracker != nil {
		// Inject mandatory skill evaluation block into last user message
		skillEval := buildSkillEvalBlock(s.cfg.SkillEvalInject)
		currentMessages, _ := req["messages"].([]any)
		if currentMessages == nil {
			currentMessages = messages
		}
		if skillEval != "" {
			req["messages"] = injectAssociativeContext(currentMessages, skillEval, s.cfg.SawtoothEnabled)
			needsReserialization = true
			s.logger.Printf("%s[req %d %s tid=%s] skill-eval injected (mode=%s)%s", colorBlue, reqIdx, journalToken(proj), journalToken(threadID), s.cfg.SkillEvalInject, colorReset)
		}
	}

	// === Collect injected learning IDs for next turn's inline reflection ===
	if threadID != "" {
		currentIDs := make(map[int64]string) // id → source
		// Source 1: Briefing learnings (cached, same every turn)
		briefingSnapshot, _, _ := s.getCachedBriefing(threadID, proj)
		if briefingSnapshot != "" {
			for _, m := range idPattern.FindAllStringSubmatch(briefingSnapshot, -1) {
				if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					currentIDs[id] = "briefing"
				}
			}
		}
		// Source 2: Associative context (contextual, per-turn)
		if assocContext != "" {
			for _, m := range idPattern.FindAllStringSubmatch(assocContext, -1) {
				if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					currentIDs[id] = "associative" // overwrites briefing if both — associative is the stronger signal
				}
			}
		}
		// Source 3: Fresh remember items (just learned)
		for _, item := range remItems {
			if item.ID > 0 {
				currentIDs[item.ID] = "fresh"
			}
		}
		s.lastInjectedIDsMu.Lock()
		if len(currentIDs) > 0 {
			// Accumulate IDs across turns — don't overwrite.
			// Claude may reference an ID from turn 30 in turn 50.
			existing := s.lastInjectedIDs[threadID]
			if existing == nil {
				existing = make(map[int64]string)
			}
			for id, source := range currentIDs {
				existing[id] = source
			}
			s.lastInjectedIDs[threadID] = existing
		}
		s.lastInjectedIDsMu.Unlock()
	}

	// Think reminder injection — only for real user turns, not tool continuations
	if threadID != "" {
		currentMessages, _ := req["messages"].([]any)
		if currentMessages == nil {
			currentMessages = messages
		}
		if isFeatureEnabled(&s.cfg, model, "think_reminder") && lastUserHasText(currentMessages) && lastUserTextLen(currentMessages) >= resolveThinkReminderMinChars(&s.cfg, model) {
			thinkReminder := s.buildThinkReminder(threadID, threadID, false)
			if thinkReminder != "" {
				req["messages"] = injectAssociativeContext(currentMessages, thinkReminder, s.cfg.SawtoothEnabled)
				needsReserialization = true
			}
		}

		// Refresh frozen-prefix snapshot with the post-timestamps bytes that
		// will appear in the wire body for the frozen range. This MUST run
		// BEFORE injectBriefingTurn / injectCodeMapTurn / injectCapabilitiesTurn:
		// those stages prepend or insert turns into req["messages"], which
		// shifts the frozen range to a higher offset. Slicing
		// req["messages"][:frozenLen] AFTER inject would capture the injected
		// turns at the head and corrupt the stored frozen prefix, breaking
		// cache continuity for 5-9 turns after every emergency-sawtooth.
		if n := s.snapshotFrozenPrefix(req, threadID); n > 0 {
			s.logger.Printf("[req %d %s tid=%s] FROZEN-SNAPSHOT: refreshed %d pre-inject messages", reqIdx, journalToken(proj), journalToken(threadID), n)
		}

		// Briefing injection: prepend user/assistant turn pair at beginning of messages.
		// Static per session → stable prefix → cacheable. Must be before dialog injection.
		// Gated per model feature (briefing), like the OpenAI-parity path.
		if isFeatureEnabled(&s.cfg, model, "briefing") && s.injectBriefingTurn(req, reqIdx, proj, threadID) {
			needsReserialization = true
		}

		// Code Map injection: insert after briefing turn pair.
		if s.injectCodeMapTurn(req, reqIdx, proj, threadID) {
			needsReserialization = true
		}

		// Active capabilities injection: insert user/assistant pair directly
		// before the last user message. Fresh per turn (no cache) so
		// activate/deactivate takes effect immediately. Must run AFTER
		// briefing/codeMap (both prepend at beginning) and BEFORE dialog
		// injection (which appends at end) to preserve cache for the
		// briefing prefix and keep the injection close to the real user
		// turn.
		if s.injectCapabilitiesTurn(req, threadID, profile) {
			needsReserialization = true
		}

		// Dialog injection: append at end of messages with pseudo-padding for alternation.
		// Always works regardless of tool_result chains or conversation state.
		dr := s.checkDialogMessages(threadID, daemonProject(proj, projPath))
		if dr.Extra != "" {
			// Prepend session ID for agent-to-agent communication
			dr.Extra = fmt.Sprintf("SESSION_ID: %s\n", threadID) + dr.Extra
			injected := false
			if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
				// Pad with empty assistant if last message is user (maintains alternation)
				lastMsg, _ := msgs[len(msgs)-1].(map[string]any)
				if lastMsg != nil && lastMsg["role"] == "user" {
					msgs = append(msgs, map[string]any{
						"role":    "assistant",
						"content": "​", // zero-width space — invisible padding
					})
				}
				// Append dialog as new user message (becomes the last message Claude sees)
				msgs = append(msgs, map[string]any{
					"role":    "user",
					"content": "<channel source=\"yesmem-dialog\">\n" + dr.Extra + "\n</channel>",
				})
				req["messages"] = msgs
				needsReserialization = true
				injected = true
			}
			// Mark as read after 3 injection turns — long enough for Claude to process,
			// resilient against fast ping-pong sessions (time-based was broken for <30s turns).
			if injected && dr.HasUnread && dr.SessionID != "" {
				if s.shouldMarkChannelRead(dr) {
					s.markDialogRead(dr)
				}
			}
			s.logger.Printf("[dialog] injection injected=%v extra_len=%d", injected, len(dr.Extra))
		}
	}

	// The learning signals are collected from the response itself (inline
	// reflection, see scanInlineSignals) — no tool is injected for them.

	// Legacy assistant annotation: timestamp on last assistant message after cache breakpoint.
	// User timestamps are now handled by TimestampStore (injected earlier in the pipeline).
	if threadID != "" {
		if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
			// Find last cache_control breakpoint in messages
			cacheIdx := -1
			for i, m := range msgs {
				msg, _ := m.(map[string]any)
				if msg == nil {
					continue
				}
				if _, has := msg["cache_control"]; has {
					cacheIdx = i
				}
				if content, ok := msg["content"].([]any); ok {
					for _, block := range content {
						if b, ok := block.(map[string]any); ok {
							if _, has := b["cache_control"]; has {
								cacheIdx = i
							}
						}
					}
				}
			}
			startIdx := cacheIdx + 1
			if startIdx < 0 {
				startIdx = 0
			}
			s.responseTsMu.RLock()
			respTime, hasRespTime := s.responseTimes[threadID]
			s.responseTsMu.RUnlock()
			if hasRespTime {
				for i := len(msgs) - 1; i >= startIdx; i-- {
					msg, _ := msgs[i].(map[string]any)
					if msg != nil && msg["role"] == "assistant" && msgHasTextBlock(msg) {
						meta := fmt.Sprintf("[%s] [msg:%d]", respTime.Format("2006-01-02 15:04:05"), i)
						prependMeta(msg, meta)
						needsReserialization = true
						break
					}
				}
			}
		}
	}

	// Prompt Caching: inject cache_control breakpoints on system + tools
	// Sawtooth: disabled (all 4 slots are used by CC system (2) + briefing (1) + CC messages (1))
	// Legacy: count existing breakpoints and fill up remaining slots.
	if !s.cfg.SawtoothEnabled && s.cacheGate != nil && s.cacheGate.ShouldCache() {
		if n := InjectCacheBreakpoints(req, s.logger); n > 0 {
			needsReserialization = true
			s.logger.Printf("[req %d %s] prompt cache: %d breakpoints injected", reqIdx, journalToken(proj), n)
		} else {
			s.logger.Printf("[req %d %s] prompt cache: no budget (4/4 already set)", reqIdx, journalToken(proj))
		}
	}

	// Final: upgrade cache TTL + enforce breakpoint limit (must run AFTER all injections + InjectCacheBreakpoints)
	// Shift the last messages-breakpoint from unstable user message to stable assistant message.
	// Text user messages change between turns (timestamps, reminders, structure); assistant messages don't.
	// Tool-result messages are excluded — they are stable cache anchors.
	if ShiftMessageBreakpoint(req) {
		needsReserialization = true
	}
	// Normalize cache_control TTL across all blocks to prevent Anthropic from
	// rejecting requests with increasing TTLs in block order. Auto-detects 1h
	// when any existing block already carries it (config, Claude Code
	// passthrough on --resume, or stale frozen prefix from an earlier turn).
	if n := NormalizeCacheTTL(req, s.cfg.CacheTTL); n > 0 {
		needsReserialization = true
		s.logger.Printf("[req %d %s] cache TTL normalized: %d blocks", reqIdx, journalToken(proj), n)
	}
	if n := EnforceCacheBreakpointLimit(req, maxCacheBreakpoints); n > 0 {
		needsReserialization = true
		s.logger.Printf("[req %d %s] prompt cache: trimmed %d surplus breakpoints", reqIdx, journalToken(proj), n)
	}

	// Pre-flight: validate tool_use/tool_result pairing before forwarding.
	// Repairs orphans created by Stubify/Collapse/Injection pipeline.
	if msgs, ok := req["messages"].([]any); ok {
		repaired, orphans := validateToolPairs(msgs, s.logger)
		if orphans > 0 {
			req["messages"] = repaired
			s.logger.Printf("%s[req %d %s tid=%s] validate: repaired %d tool pairing issue(s)%s", colorOrange, reqIdx, journalToken(proj), journalToken(threadID), orphans, colorReset)
			needsReserialization = true
		}
	}

	// Early-out: skip re-serialization when nothing changed
	if !needsReserialization {
		s.logger.Printf("%s[req %d %s tid=%s] %d messages, ~%dk tokens (no stubbing, threshold=%dk)%s", colorDim, reqIdx, journalToken(proj), journalToken(threadID), len(messages), totalTokens/1000, s.effectiveTokenThreshold(threadID, model)/1000, colorReset)
		s.forwardWithAnnotation(w, r, body, reqIdx, toolUseIDs, proj, projPath, threadID, msgCount, false, totalTokens)
		return
	}

	// Re-serialize
	body, err = json.Marshal(req)
	if err != nil {
		s.logger.Printf("%s[req %d %s tid=%s] re-marshal error: %v%s", colorRed, reqIdx, journalToken(proj), journalToken(threadID), err, colorReset)
		http.Error(w, "internal proxy error", http.StatusInternalServerError)
		return
	}

	// Debug: dump final request body to file (enable with YESMEM_PROXY_DEBUG=1)
	if needsReserialization && debugDumpsEnabled() {
		if p := writeDebugDump(s.cfg.DataDir, threadID, dumpKindPost, reqIdx, body); p != "" {
			s.logger.Printf("[req %d] dumped %dk to %s", reqIdx, len(body)/1024, p)
		}
	}

	// Task #7: Mark this request fingerprint for retry detection
	if !isRetryReq {
		s.markRequest(fp)
	}

	// Forward to Anthropic with SSE annotation extraction
	// Use original msgCount (pre-injection) so actual-based estimation stays consistent across turns
	// Use actual-based totalTokens as estimate when available (more accurate than full recount)
	finalEstimate := totalTokens
	if s.sawtoothTrigger == nil || s.sawtoothTrigger.GetLastTokens(threadID) == 0 {
		// First request: no actual yet, do a full recount of post-injection messages
		finalMsgs, _ := req["messages"].([]any)
		finalEstimate = s.measureOverhead(req) + s.countMessageTokens(finalMsgs)
	}
	s.logger.Printf("[req %d %s tid=%s] FINAL: ~%dk estimated (after all injections)", reqIdx, journalToken(proj), journalToken(threadID), finalEstimate/1000)
	if raw := s.getRawEstimate(threadID); raw > 0 {
		s.cacheStatusWriter.UpdateRawForThread(threadID, raw)
	}
	s.forwardWithAnnotation(w, r, body, reqIdx, toolUseIDs, proj, projPath, threadID, msgCount, false, finalEstimate)
}

// getPromptFlags resolves the effective prompt flags for a profile, falling back
// to the Server's legacy flat config if no prompt layers are configured
// (backward compatibility with configs that predate shared_prompt).
func (s *Server) getPromptFlags(profile models.PromptProfile) *config.PromptFlags {
	if s.cfg.Prompt != nil {
		return s.cfg.Prompt.EffectivePromptFlags(profile)
	}
	// Legacy path: use flat fields as Claude defaults.
	return &config.PromptFlags{
		Ungate:             s.cfg.PromptUngate,
		Rewrite:            s.cfg.PromptRewrite,
		Enhance:            s.cfg.PromptEnhance,
		ToolPrefs:          s.cfg.PromptToolPrefs,
		OutputDiscipline:   s.cfg.PromptOutputDiscipline,
		CodingDiscipline:   s.cfg.PromptCodingDiscipline,
		Beweislast:         s.cfg.PromptBeweislast,
		Fable:              s.cfg.PromptFable,
		ScopeDiscipline:    s.cfg.PromptScopeDiscipline,
		DelegationContract: s.cfg.PromptDelegationContract,
		ClarifyFirst:       s.cfg.PromptClarifyFirst,
		CodeToolsFirst:     s.cfg.PromptCodeToolsFirst,
		WikiFirst:          s.cfg.PromptWikiFirst,
		PatternSuggest:     s.cfg.PromptPatternSuggest,
	}
}

// getThreadCWD returns the cached working directory for a thread.
func (s *Server) getThreadCWD(threadID string) string {
	s.threadCWDMu.RLock()
	defer s.threadCWDMu.RUnlock()
	if s.threadCWD == nil {
		return ""
	}
	return s.threadCWD[threadID]
}

// setThreadCWD caches the working directory for a thread.
func (s *Server) setThreadCWD(threadID, cwd string) {
	s.threadCWDMu.Lock()
	defer s.threadCWDMu.Unlock()
	if s.threadCWD == nil {
		s.threadCWD = make(map[string]string)
	}
	s.threadCWD[threadID] = cwd
}

// matchProviderTarget returns the ProviderTargets URL for model. Keys match as
// prefixes ("deepseek" covers "deepseek-v4-pro") and the longest matching prefix
// wins: ranging the map and taking the first hit routed a model with overlapping
// keys ("deepseek" and "deepseek-v4-flash-free") to different upstreams between
// calls (PRX-P45). An exact model key is the longest possible prefix, so it keeps
// winning. Two prefixes of equal length are necessarily the same text, so only
// case-only duplicate keys can tie; those break on the smaller URL to stay
// deterministic even for a pathological config.
func matchProviderTarget(model string, targets map[string]string) (string, bool) {
	modelLower := strings.ToLower(model)
	bestKey, bestURL := "", ""
	for key, url := range targets {
		if key == "" || url == "" {
			continue
		}
		keyLower := strings.ToLower(key)
		if !strings.HasPrefix(modelLower, keyLower) {
			continue
		}
		url = strings.TrimRight(url, "/")
		if url == "" {
			continue
		}
		if bestKey != "" {
			if len(keyLower) < len(bestKey) || (len(keyLower) == len(bestKey) && url >= bestURL) {
				continue
			}
		}
		bestKey, bestURL = keyLower, url
	}
	if bestKey == "" {
		return "", false
	}
	return bestURL, true
}

// resolveOpenAITarget returns the upstream URL for an OpenAI-format request.
// Checks ProviderTargets by prefix match (e.g. "deepseek" matches "deepseek-v4-pro",
// longest prefix wins), then falls back to the auto-discovered target, then to
// OpenAITargetURL.
func (s *Server) resolveOpenAITarget(model string) string {
	if url, ok := matchProviderTarget(model, s.cfg.ProviderTargets); ok {
		return url
	}
	// Auto-discovered targets (secondary — explicit provider_targets takes priority)
	if targets := s.discoveredTargets(); len(targets) > 0 {
		if url, ok := targets[strings.ToLower(model)]; ok && url != "" {
			return url
		}
	}

	if s.cfg.OpenAITargetURL != "" {
		s.warnRoutingFallback(model, s.cfg.OpenAITargetURL)
		return s.cfg.OpenAITargetURL
	}
	s.warnRoutingFallback(model, s.cfg.TargetURL)
	return s.cfg.TargetURL
}

// discoveredTargets returns the auto-discovered model→upstream map and rebuilds it
// when the files behind it changed (a fresh credential from "opencode auth login"
// must route without a restart). The refresh is read-only: patching the OpenCode
// config stays a startup action, so the request path never writes (YM-262).
func (s *Server) discoveredTargets() map[string]string {
	refresh := s.cfg.AutoConfigureProviders
	var stamp uint64
	if refresh {
		stamp = providerSourceStamp()
	}
	s.autoTargetsMu.RLock()
	targets, current, ready := s.autoProviderTargets, s.autoTargetsStamp, s.autoTargetsReady
	s.autoTargetsMu.RUnlock()
	if !refresh || (ready && current == stamp) {
		return targets
	}

	// Serialize the discovery itself: a burst of requests that all see the old
	// stamp must not parse models.json (megabytes) once per goroutine. The routing
	// reads stay on autoTargetsMu, this lock only guards the refresh work.
	s.autoRefreshMu.Lock()
	defer s.autoRefreshMu.Unlock()
	s.autoTargetsMu.RLock()
	done := s.autoTargetsReady && s.autoTargetsStamp == stamp
	s.autoTargetsMu.RUnlock()
	if done {
		return s.readAutoTargets()
	}

	fresh, _, err := discoverProviderTargets(s.logger, s.cfg.ProviderTargets)

	s.autoTargetsMu.Lock()
	defer s.autoTargetsMu.Unlock()
	s.autoTargetsStamp = stamp
	s.autoTargetsReady = true
	if err == nil {
		// A valid empty result must clear the routes: after "opencode auth
		// logout" the provider is gone. An unreadable file keeps them.
		s.autoProviderTargets = fresh
	}
	return s.autoProviderTargets
}

// readAutoTargets reads the current map without attempting a refresh. It takes
// the read lock itself, so it must not be called from inside a write lock.
func (s *Server) readAutoTargets() map[string]string {
	s.autoTargetsMu.RLock()
	defer s.autoTargetsMu.RUnlock()
	return s.autoProviderTargets
}

// warnRoutingFallback logs a one-shot warning per model when its OpenAI-format
// request resolves via the fallback target instead of an explicit or
// auto-discovered provider route. Makes silent misroutes (e.g. glm-5.3 hit the
// OpenAI target and surfaced at the TUI as a keys-related error) greppable.
// Flood-guarded: one warning per model per process lifetime.
func (s *Server) warnRoutingFallback(model, target string) {
	s.routingWarnedMu.Lock()
	defer s.routingWarnedMu.Unlock()
	if s.routingWarned == nil {
		s.routingWarned = make(map[string]bool)
	}
	if s.routingWarned[model] {
		return
	}
	s.routingWarned[model] = true
	log.Printf("[routing] model %s has no provider route — falling back to %s. Add a provider_targets entry if this is unintentional.", journalToken(model), target)
}

// resolveAnthropicTarget returns the upstream URL for an Anthropic-format request.
// Checks ProviderTargets by prefix match (e.g. "deepseek" matches "deepseek-v4-pro",
// longest prefix wins), then falls back to TargetURL.
func (s *Server) resolveAnthropicTarget(model string) string {
	if url, ok := matchProviderTarget(model, s.cfg.ProviderTargets); ok {
		return url
	}
	return s.cfg.TargetURL
}

// bytesPerToken is the approximation factor for token estimation.
// Anthropic recommends ~3.5 chars/token; we use 3.6 for slight overestimation
// (triggers stubbing a bit earlier = safer for context budget).
const bytesPerToken = 3.6

// TokenEstimateFunc is the central token estimation function.
// All stubbing/budget decisions use this. Receives extracted text content
// (not JSON wire format) so the tokenizer counts actual content tokens.
type TokenEstimateFunc func(text string) int
