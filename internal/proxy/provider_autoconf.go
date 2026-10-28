package proxy

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
)

// modelsJSONEntry represents one provider entry in OpenCode's models.json cache.
type modelsJSONEntry struct {
	ID     string                     `json:"id"`
	NPM    string                     `json:"npm"`
	API    string                     `json:"api"`
	Env    []string                   `json:"env"`
	Models map[string]modelsJSONModel `json:"models"`
}

type modelsJSONModel struct {
	ID string `json:"id"`
}

// opencodeProviderBlock represents a provider section in opencode.json.
type opencodeProviderBlock struct {
	APIKey  string                  `json:"apiKey"`
	Env     []string                `json:"env"`
	Options opencodeProviderOptions `json:"options"`
}

type opencodeProviderOptions struct {
	BaseURL string `json:"baseURL"`
}

// opencodeAuthEntry represents a credential entry in opencode's auth.json.
type opencodeAuthEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// autoDiscoveredProvider maps a model ID to its upstream API endpoint.
type autoDiscoveredProvider struct {
	ModelID        string // e.g. "big-pickle"
	UpstreamURL    string // e.g. "https://opencode.ai/zen"
	ProviderID     string // e.g. "opencode"
	IsOpenAICompat bool   // uses @ai-sdk/openai-compatible
	HasCredentials bool   // user has a key for it (auth.json, env var or apiKey)
}

// well-known first-party providers where models.json has no "api" field.
var firstPartyDefaults = map[string]string{
	"openai":    "https://api.openai.com",
	"anthropic": "https://api.anthropic.com",
	"google":    "https://generativelanguage.googleapis.com",
	"groq":      "https://api.groq.com",
	"mistral":   "https://api.mistral.ai",
}

// openaiCompatibleNPM is the npm package name for OpenAI-compatible providers.
const openaiCompatibleNPM = "@ai-sdk/openai-compatible"

// loadModelsJSON reads the OpenCode models cache and returns a map of providerID → entry.
// Returns nil and no error if the file doesn't exist (OpenCode not installed).
func loadModelsJSON(path string) (map[string]modelsJSONEntry, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".cache", "opencode", "models.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // OpenCode not installed — skip silently
		}
		return nil, fmt.Errorf("read models.json: %w", err)
	}
	var entries map[string]modelsJSONEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse models.json: %w", err)
	}
	return entries, nil
}

// loadOpenCodeConfig reads opencode's config (opencode.jsonc/json/config.json —
// resolved by opencode.ConfigPath) and extracts provider configurations.
// Returns a map of providerID → provider block plus the IDs whose block did not
// decode. A single wrongly typed provider (apiKey as a number, options as a
// string) must not take the whole discovery down, so each block is decoded on
// its own; structurally broken JSON is still an error (YM-262).
func loadOpenCodeConfig(path string) (map[string]opencodeProviderBlock, []string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		path = opencode.ConfigPath(home)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // No opencode config — skip silently
		}
		return nil, nil, fmt.Errorf("read opencode config %s: %w", path, err)
	}
	// opencode config has a top-level "provider" key. The file is usually
	// opencode.jsonc; encoding/json rejects comments, so blank them first.
	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(blankJSONCNoise(data), &cfg); err != nil {
		return nil, nil, fmt.Errorf("parse opencode config %s: %w", path, err)
	}
	if cfg.Provider == nil {
		return nil, nil, nil
	}
	providers := make(map[string]opencodeProviderBlock, len(cfg.Provider))
	var skipped []string
	for providerID, raw := range cfg.Provider {
		var block opencodeProviderBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			skipped = append(skipped, providerID)
			continue
		}
		providers[providerID] = block
	}
	sort.Strings(skipped)
	return providers, skipped, nil
}

// loadOpenCodeAuth reads opencode's auth.json and returns a map of providerID → apiKey.
// Returns nil and no error if the file doesn't exist.
func loadOpenCodeAuth(path string) (map[string]string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".local", "share", "opencode", "auth.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read auth.json: %w", err)
	}
	var entries map[string]opencodeAuthEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse auth.json: %w", err)
	}
	result := make(map[string]string, len(entries))
	for providerID, entry := range entries {
		if entry.Key != "" {
			result[providerID] = entry.Key
		}
	}
	return result, nil
}

// hasProviderCredentials checks if a provider has valid credentials configured.
// Checks opencode.json fields first, then env vars from models.json, then auth.json.
func hasProviderCredentials(providerID string, block opencodeProviderBlock, models map[string]modelsJSONEntry, auth map[string]string) bool {
	if block.APIKey != "" {
		return true
	}
	for _, envVar := range block.Env {
		if os.Getenv(envVar) != "" {
			return true
		}
	}
	// Check models.json for env var hints (used by default)
	if entry, ok := models[providerID]; ok {
		for _, envVar := range entry.Env {
			if val := os.Getenv(envVar); val != "" {
				return true
			}
		}
	}
	// Check auth.json
	if _, ok := auth[providerID]; ok {
		return true
	}
	return false
}

// discoverOpenAICompatibleProviders finds all OpenAI-compatible providers from models.json
// that are active in opencode.json. Returns model→upstream mappings.
// Also returns a list of inactive (non-OpenAI-compatible) providers for YAML comment generation.
func discoverOpenAICompatibleProviders(
	models map[string]modelsJSONEntry,
	opencode map[string]opencodeProviderBlock,
	auth map[string]string,
) (active []autoDiscoveredProvider, inactive []autoDiscoveredProvider) {
	for providerID, entry := range models {
		block, isConfigured := opencode[providerID]
		if !isConfigured {
			continue // not used by this user
		}
		// opencode Zen serves free-tier models with the client-supplied
		// "public" key — no account or auth.json entry required. Treat the
		// opencode provider as a credential-free routing target whenever
		// the user has explicitly added it to their opencode config.
		if providerID != "opencode" && !hasProviderCredentials(providerID, block, models, auth) {
			continue
		}

		upstreamURL := entry.API
		if upstreamURL == "" {
			// First-party provider — look up hardcoded default
			var ok bool
			upstreamURL, ok = firstPartyDefaults[providerID]
			if !ok {
				continue // unknown provider, skip
			}
		}
		upstreamURL = strings.TrimRight(upstreamURL, "/")

		isOpenAICompat := entry.NPM == openaiCompatibleNPM ||
			entry.NPM == "@ai-sdk/openai" ||
			entry.NPM == "@ai-sdk/mistral" ||
			(entry.NPM == "" && providerID == "openai")

		for modelID := range entry.Models {
			p := autoDiscoveredProvider{
				ModelID:        modelID,
				UpstreamURL:    upstreamURL,
				ProviderID:     providerID,
				IsOpenAICompat: isOpenAICompat,
				HasCredentials: hasProviderCredentials(providerID, block, models, auth),
			}
			if isOpenAICompat {
				active = append(active, p)
			} else {
				inactive = append(inactive, p)
			}
		}
	}
	return active, inactive
}

// sortProviders orders discovered providers so the bare-model winner is the one
// the user actually pays for: credential-backed providers first, then by
// ProviderID. opencode Zen serves many models without any credential and sorts
// alphabetically before the paid plan that carries the same model, so the plain
// alphabetical order resolved bare names to the wrong provider (YM-152).
func sortProviders(providers []autoDiscoveredProvider) {
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].HasCredentials != providers[j].HasCredentials {
			return providers[i].HasCredentials
		}
		return providers[i].ProviderID < providers[j].ProviderID
	})
}

// buildAutoProviderTargets converts discovered providers to a modelID→URL map.
// providerTargets (from config.yaml) has authority — bare modelIDs already covered
// by an exact or prefix match in providerTargets are skipped to avoid non-deterministic
// overwrites when multiple providers carry the same model (e.g. glm-5.2 in opencode and
// zai-coding-plan). Qualified providerID/modelID keys are always added (even when the
// bare key is skipped) since they are unambiguous and resolveOpenAITarget checks
// ProviderTargets first anyway. Provider order comes from sortProviders, so the same
// provider wins the bare key here as in buildModelProviderMap.
func buildAutoProviderTargets(providers []autoDiscoveredProvider, providerTargets map[string]string) map[string]string {
	sortProviders(providers)
	m := make(map[string]string, len(providers)*2)
	for _, p := range providers {
		modelLower := strings.ToLower(p.ModelID)
		qualifiedKey := strings.ToLower(p.ProviderID + "/" + p.ModelID)
		m[qualifiedKey] = p.UpstreamURL
		if coveredByProviderTargets(modelLower, providerTargets) {
			continue
		}
		if _, exists := m[modelLower]; !exists {
			m[modelLower] = p.UpstreamURL
		}
	}
	return m
}

// coveredByProviderTargets reports whether modelLower is already handled by an explicit
// provider_targets entry: exactly the rule matchProviderTarget applies, so a bare
// modelID is only skipped when routing would actually use the configured entry.
func coveredByProviderTargets(modelLower string, providerTargets map[string]string) bool {
	_, ok := matchProviderTarget(modelLower, providerTargets)
	return ok
}

// maybePatchOpenCodeBaseURL sets baseURL to localhost:9099/v1 for active OpenAI-compatible
// providers that don't already have a baseURL configured. Returns the number of providers
// actually patched. The edit is textual: comments and key order of the user's config survive.
func maybePatchOpenCodeBaseURL(path string, active []autoDiscoveredProvider) (int, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, err
		}
		path = opencode.ConfigPath(home)
	}

	// The patch is a read-modify-write on a hand-maintained file, and the watcher
	// can repeat it at runtime. The state is taken BEFORE the read and re-checked
	// before the write: a change in between then reads as a change and the patch is
	// retried, instead of overwriting the user's edit with the stale copy.
	readState, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat opencode config %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read opencode config %s: %w", path, err)
	}

	// Deduplicate: one provider may have many models
	seenProviders := make(map[string]bool)
	var providersToPatch []string
	for _, p := range active {
		if seenProviders[p.ProviderID] {
			continue
		}
		seenProviders[p.ProviderID] = true
		providersToPatch = append(providersToPatch, p.ProviderID)
	}

	out, patched, err := patchOpenCodeBaseURLs(data, providersToPatch)
	if err != nil {
		return 0, fmt.Errorf("patch opencode config %s: %w", path, err)
	}
	if patched == 0 {
		return 0, nil
	}

	// Keep the mode the user set. os.WriteFile would ignore the perm argument on
	// an existing file, so the mode has to travel with the write itself.
	mode := os.FileMode(0o600) // fallback: the config may carry provider keys
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
		if mode&0o200 == 0 {
			// A read-only config is a deliberate stop sign. The directory
			// permission would still allow a rename, so refuse explicitly
			// instead of writing past it.
			return 0, fmt.Errorf("opencode config %s is read-only (mode %v); baseURL not patched", path, mode)
		}
	}
	// One-time backup before the first patch: the config is hand-maintained and
	// the proxy writes it on startup, so there must be a way back. 0600 because
	// it holds the same provider keys as the original, and next to the path the
	// user configured (a symlink target is often a dotfiles repo where an
	// untracked copy of the keys would end up in the next commit). Restore it with
	// `cp`, not `mv`: a move would replace the symlink with a regular file.
	backupPath := path + autoconfBackupSuffix
	// The backup is yesmem's own artifact, so it is written without following a
	// symlink at its path: the rename replaces a link there instead of copying the
	// config through it to whatever the link points at. A directory cannot be
	// replaced by a rename, and treating it as "already backed up" would skip the
	// restore point silently.
	backupInfo, lerr := os.Lstat(backupPath)
	switch {
	case lerr == nil && backupInfo.IsDir():
		return 0, fmt.Errorf("backup path %s is a directory; cannot write the backup", backupPath)
	case lerr != nil && !os.IsNotExist(lerr):
		return 0, fmt.Errorf("stat opencode config backup %s: %w", backupPath, lerr)
	case lerr == nil && backupInfo.Mode().IsRegular():
		// The one-time backup is already there and must not be overwritten.
	default:
		if err := writeFileAtomicNoFollow(backupPath, data, 0o600); err != nil {
			return 0, fmt.Errorf("back up opencode config %s: %w", path, err)
		}
	}
	// A user edit that landed while this patch was being prepared must win: writing
	// the stale copy over it would silently drop the change.
	if providerConfigChangedSince(path, readState) {
		return 0, fmt.Errorf("opencode config %s changed while patching; retry", path)
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return 0, fmt.Errorf("write opencode config %s: %w", path, err)
	}
	return patched, nil
}

// providerConfigChangedSince reports whether path no longer matches the state
// recorded before the patch was prepared. An unreadable file counts as changed:
// the patch must not overwrite something it cannot verify.
func providerConfigChangedSince(path string, before os.FileInfo) bool {
	now, err := os.Stat(path)
	if err != nil {
		return true
	}
	return now.ModTime() != before.ModTime() || now.Size() != before.Size()
}

// autoconfBackupSuffix marks the copy of opencode's config taken before the first
// automatic baseURL patch.
const autoconfBackupSuffix = ".yesmem-backup"

// runAutoDiscovery loads models.json and opencode.json, discovers active providers,
// builds autoProviderTargets, and patches opencode.json baseURLs.
// providerTargets (from config.yaml) is passed through so buildAutoProviderTargets can
// skip bare modelIDs already covered by explicit config. Returns the auto-discovered
// modelID→URL map (nil on any non-fatal error) together with the patch error, so the
// caller can keep the patch pending and retry it.
func runAutoDiscovery(logger *log.Logger, providerTargets map[string]string) (map[string]string, error) {
	targets, active, _ := discoverProviderTargets(logger, providerTargets)
	if len(active) == 0 {
		return nil, nil
	}

	// Patch opencode.json. The count must come from the patch itself: discovery
	// rebuilt its own provider list, so a length taken elsewhere reported
	// providers that were never touched (YM-145).
	patched, patchErr := maybePatchOpenCodeBaseURL("", active)
	if patchErr != nil {
		logger.Printf("[autoconf] WARNING: failed to patch opencode config: %v", patchErr)
		// Discovery still resolves upstreams inside the proxy, but a provider
		// without baseURL keeps talking to its upstream directly: opencode only
		// routes through the proxy once the config points there.
	} else if patched > 0 {
		logger.Printf("[autoconf] patched opencode config: set baseURL for %d providers", patched)
	}

	return targets, patchErr
}

// providerSourceWatchInterval bounds how often the running proxy re-checks the
// provider source files for a provider that was logged in after start.
const providerSourceWatchInterval = time.Minute

// providerPatchMaxRetries bounds how often a failed config patch is retried for
// the same file state. A read-only config is a deliberate stop sign, so the proxy
// must not walk into it every minute for the life of the process.
const providerPatchMaxRetries = 5

// watchProviderSources re-applies the baseURL patch when opencode's provider
// files change. The startup patch only covers providers that were already logged
// in; a provider added later (opencode auth login) would otherwise keep talking to
// its upstream directly for the whole life of this process, because only a
// patched baseURL routes it through the proxy (YM-326).
//
// The write stays off the request path, which is read-only (YM-262); this
// goroutine is the only other writer. One opencode restart stays unavoidable: it
// reads its config at startup, so the fresh baseURL only routes afterwards.
func (s *Server) watchProviderSources(stop <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.refreshProviderAutoconf()
		}
	}
}

// refreshProviderAutoconf patches the opencode config and refreshes the routes
// when the provider source files changed since the last run. It reports whether a
// provider block was patched. Unreadable files keep the known routes and patch
// nothing, exactly like the request path.
func (s *Server) refreshProviderAutoconf() bool {
	stamp := providerSourceStamp()
	s.autoTargetsMu.RLock()
	ready, current := s.autoTargetsReady, s.autoTargetsStamp
	retry := s.providerPatchRetry
	s.autoTargetsMu.RUnlock()
	if ready && current == stamp && !retry {
		return false
	}

	// Same serialization as the request path: a tick and a burst of requests must
	// not parse models.json (megabytes) once per goroutine.
	s.autoRefreshMu.Lock()
	defer s.autoRefreshMu.Unlock()
	s.autoTargetsMu.RLock()
	done := s.autoTargetsReady && s.autoTargetsStamp == stamp && !s.providerPatchRetry
	s.autoTargetsMu.RUnlock()
	if done {
		return false
	}

	targets, active, err := discoverProviderTargets(s.logger, s.cfg.ProviderTargets)
	if err != nil {
		return false
	}

	// A fresh file state deserves a fresh budget of attempts.
	s.autoTargetsMu.Lock()
	if s.providerPatchWarnStamp != stamp {
		s.providerPatchRetries = 0
	}
	s.autoTargetsMu.Unlock()

	patched := 0
	patchErr := error(nil)
	if p, patchFault := maybePatchOpenCodeBaseURL("", active); patchFault != nil {
		patchErr = patchFault
		s.autoTargetsMu.RLock()
		warned := s.providerPatchWarnStamp == stamp
		s.autoTargetsMu.RUnlock()
		if !warned {
			s.logger.Printf("[autoconf] WARNING: failed to patch opencode config (will retry): %v", patchFault)
		}
	} else {
		patched = p
	}

	s.autoTargetsMu.Lock()
	s.autoProviderTargets = targets
	if patchErr != nil {
		// Keep the stamp so the next tick sees the same file state, and keep the
		// patch pending so a transient failure (full disk, NFS hiccup, temporary
		// permissions) is not forgotten for the life of the process.
		s.autoTargetsStamp = stamp
		s.providerPatchRetries++
		s.providerPatchRetry = s.providerPatchRetries < providerPatchMaxRetries
		s.providerPatchWarnStamp = stamp
	} else {
		// The routes belong to the file state discovered above, but a patch rewrote
		// opencode.json, so the stamp is taken again; otherwise the next tick would
		// mistake its own write for a fresh login.
		s.autoTargetsStamp = providerSourceStamp()
		s.providerPatchRetry = false
		s.providerPatchRetries = 0
	}
	s.autoTargetsReady = true
	exhausted := patchErr != nil && !s.providerPatchRetry
	s.autoTargetsMu.Unlock()

	if exhausted {
		s.logger.Printf("[autoconf] WARNING: gave up patching opencode config after %d attempts (%v); the provider files have to change before it is tried again", providerPatchMaxRetries, patchErr)
	}
	if patched > 0 {
		s.logger.Printf("[autoconf] patched opencode config after a provider change: set baseURL for %d provider(s); opencode must be restarted to route them through the proxy", patched)
	}
	return patched > 0
}

// discoverProviderTargets reads OpenCode's provider files and returns the
// discovered model→upstream map together with the active providers. Read-only: it
// never rewrites the OpenCode config, so the request path may call it to pick up
// new credentials (YM-262). error means "the files could not be read"; an empty
// result with no error means "there is nothing to route", which the caller must
// treat as a fact and not as a failure.
func discoverProviderTargets(logger *log.Logger, providerTargets map[string]string) (map[string]string, []autoDiscoveredProvider, error) {
	models, err := loadModelsJSON("")
	if err != nil {
		logger.Printf("[autoconf] WARNING: failed to load models.json: %v", err)
		return nil, nil, err
	}
	if models == nil {
		// A missing cache file says nothing about credentials: OpenCode may be
		// reinstalling or refreshing it. Treated as an error so the caller keeps
		// the routes it already had.
		logger.Printf("[autoconf] models.json not found — auto-discovery skipped (known routes are kept)")
		return nil, nil, fmt.Errorf("models.json not found")
	}

	oc, skipped, err := loadOpenCodeConfig("")
	if err != nil {
		logger.Printf("[autoconf] WARNING: failed to load opencode.json: %v", err)
		return nil, nil, err
	}
	if oc == nil {
		logger.Printf("[autoconf] opencode.json not found — auto-discovery skipped (known routes are kept)")
		return nil, nil, fmt.Errorf("opencode.json not found")
	}
	if len(skipped) > 0 {
		logger.Printf("[autoconf] WARNING: %d provider block(s) unreadable and skipped (%s) — their models stay unrouted",
			len(skipped), strings.Join(skipped, ", "))
	}

	auth, err := loadOpenCodeAuth("")
	if err != nil {
		// An unreadable auth.json would make every credential-backed provider
		// disappear, which reads exactly like a logout. That is an error.
		logger.Printf("[autoconf] WARNING: failed to load auth.json: %v", err)
		return nil, nil, err
	}

	active, inactive := discoverOpenAICompatibleProviders(models, oc, auth)

	if len(active) == 0 {
		logger.Printf("[autoconf] no active OpenAI-compatible providers found")
		return nil, nil, nil
	}

	// Log discovered providers
	seen := make(map[string]bool)
	var names []string
	for _, p := range active {
		if !seen[p.ProviderID] {
			seen[p.ProviderID] = true
			names = append(names, p.ProviderID)
		}
	}
	logger.Printf("[autoconf] discovered %d providers (%s) with %d total models",
		len(names), strings.Join(names, ", "), len(active))

	// Log inactive providers (for future reference)
	if len(inactive) > 0 {
		seen = make(map[string]bool)
		names = names[:0]
		for _, p := range inactive {
			if !seen[p.ProviderID] {
				seen[p.ProviderID] = true
				names = append(names, p.ProviderID)
			}
		}
		logger.Printf("[autoconf] %d non-OpenAI-compatible providers skipped (%s)",
			len(names), strings.Join(names, ", "))
	}

	return buildAutoProviderTargets(active, providerTargets), active, nil
}

// providerSourceFiles lists the files the discovery result derives from.
func providerSourceFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".cache", "opencode", "models.json"),
		opencode.ConfigPath(home),
		filepath.Join(home, ".local", "share", "opencode", "auth.json"),
	}
}

// providerSourceStamp fingerprints those files by modification time and size. A
// discovery result is only valid for the file state it was built from: a
// credential added after start (opencode auth login) has to reach routing and
// spawn resolution without a process restart (YM-262). The stamp is a cache hint,
// not a correctness boundary: a rewrite that keeps both size and modification
// time within the file system's timestamp granularity goes unnoticed until the
// next change.
func providerSourceStamp() uint64 {
	h := fnv.New64a()
	for _, path := range providerSourceFiles() {
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(h, "%s|missing;", path)
			continue
		}
		fmt.Fprintf(h, "%s|%d|%d;", path, info.ModTime().UnixNano(), info.Size())
	}
	return h.Sum64()
}

// BuildModelProviderMap returns a map of lowercase bare modelID → providerID,
// for resolving bare model names (e.g. "glm-5.2") to "providerID/modelID" strings
// (e.g. "zai-coding-plan/glm-5.2"). Used by the daemon when spawning agents to avoid
// hardcoding provider prefixes.
//
// All discovered providers are included, including coding-plan variants. When
// several providers carry the same modelID, a credential-backed provider wins
// over a credential-free one (opencode Zen), then the alphabetically-first
// ProviderID. Users must resolve the remaining ambiguity explicitly via
// "provider/model".
//
// Returns nil on any non-fatal error (no models.json, no opencode.json, etc.).
// Callers on a hot path should use BuildModelProviderMapCached.
func BuildModelProviderMap() map[string]string {
	value, _ := buildModelProviderMapErr()
	return value
}

// buildModelProviderMapErr is BuildModelProviderMap with the distinction the
// cache needs: an error means "the files could not be read", nil without an error
// means "there is nothing to resolve" (YM-262).
func buildModelProviderMapErr() (map[string]string, error) {
	models, err := loadModelsJSON("")
	if err != nil {
		return nil, err
	}
	if models == nil {
		return nil, fmt.Errorf("models.json not found")
	}
	oc, skipped, err := loadOpenCodeConfig("")
	if err != nil {
		return nil, err
	}
	if oc == nil {
		return nil, fmt.Errorf("opencode.json not found")
	}
	if len(skipped) > 0 {
		log.Printf("[autoconf] %d provider block(s) unreadable and skipped for model resolution (%s)",
			len(skipped), strings.Join(skipped, ", "))
	}
	auth, err := loadOpenCodeAuth("")
	if err != nil {
		return nil, err
	}
	active, _ := discoverOpenAICompatibleProviders(models, oc, auth)
	if len(active) == 0 {
		return nil, nil
	}
	return buildModelProviderMap(active), nil
}

// modelProviderCache holds the provider map for agent spawns. It is rebuilt when
// the files behind it change, so a credential added after start is visible on the
// next spawn without a daemon restart (YM-262).
var modelProviderCache struct {
	mu    sync.Mutex
	stamp uint64
	ready bool
	value map[string]string
}

// BuildModelProviderMapCached is BuildModelProviderMap for callers that run
// repeatedly: it reuses the last result until the source files change, and keeps
// it when the files cannot be read (a transient unreadable file must not drop
// working resolutions).
func BuildModelProviderMapCached() map[string]string {
	stamp := providerSourceStamp()
	modelProviderCache.mu.Lock()
	defer modelProviderCache.mu.Unlock()
	if modelProviderCache.ready && modelProviderCache.stamp == stamp {
		return modelProviderCache.value
	}
	value, err := buildModelProviderMapErr()
	modelProviderCache.stamp = stamp
	modelProviderCache.ready = true
	if err == nil {
		modelProviderCache.value = value
	} else {
		// Silent staleness would make "my bare model does not resolve" ungreppable.
		log.Printf("[autoconf] WARNING: provider map refresh failed, keeping the previous one: %v", err)
	}
	return modelProviderCache.value
}

// buildModelProviderMap converts discovered providers to a lowercase
// modelID → providerID map. Order comes from sortProviders: credential-backed
// providers before credential-free ones, then ProviderID alphabetically (the
// discovery order itself is non-deterministic because it iterates maps).
func buildModelProviderMap(providers []autoDiscoveredProvider) map[string]string {
	sortProviders(providers)
	m := make(map[string]string, len(providers))
	for _, p := range providers {
		key := strings.ToLower(p.ModelID)
		if _, exists := m[key]; !exists {
			m[key] = p.ProviderID
		}
	}
	return m
}

// maxModelEntries bounds the list handed to a run. Every sandboxed run carries it,
// so an unbounded list from an unusually large config would push the child's
// environment past the exec limit and no run would start.
const maxModelEntries = 256

// OpencodeModelEntries lists the "provider/model" pairs the opencode config in
// configDir declares, plus "<provider>/* (default)" for a provider that names no
// models of its own. It is sorted and free of duplicates.
//
// A bundled cap shows this list for its /models command. The daemon computes it
// so the opencode config directory can stay masked from a sandboxed run (YM-303):
// that config carries plugin paths and mcpServers, which the next opencode start
// executes. configDir is the directory the caller masks, so a host with a custom
// XDG_CONFIG_HOME gets the list from the same place; empty means the home default.
// A missing or unreadable config yields nil, which the caller treats as "no list
// to hand over".
func OpencodeModelEntries(configDir string) []string {
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		configDir = filepath.Join(home, ".config", "opencode")
	}
	data, err := os.ReadFile(opencode.ConfigPathInDir(configDir))
	if err != nil {
		return nil
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]json.RawMessage `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(blankJSONCNoise(data), &cfg); err != nil {
		return nil
	}
	entries := make([]string, 0, len(cfg.Provider))
	for provider, block := range cfg.Provider {
		if len(block.Models) == 0 {
			entries = append(entries, provider+"/* (default)")
			continue
		}
		for model := range block.Models {
			entries = append(entries, provider+"/"+model)
		}
	}
	sort.Strings(entries)
	out := entries[:0]
	for i, entry := range entries {
		if i > 0 && entries[i-1] == entry {
			continue
		}
		out = append(out, entry)
	}
	if len(out) > maxModelEntries {
		out = out[:maxModelEntries]
	}
	return out
}
