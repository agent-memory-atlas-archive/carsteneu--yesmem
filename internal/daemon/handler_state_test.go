package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
)

// --- Proxy State ---

func TestHandleGetProxyState_RequiresKey(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetProxyState(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing key")
	}
}

func TestHandleSetAndGetProxyState(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetProxyState(map[string]any{"key": "test_key", "value": "hello"})
	if resp.Error != "" {
		t.Fatalf("set error: %s", resp.Error)
	}

	resp = h.handleGetProxyState(map[string]any{"key": "test_key"})
	if resp.Error != "" {
		t.Fatalf("get error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["value"] != "hello" {
		t.Errorf("expected 'hello', got %q", m["value"])
	}
}

func TestHandleSetProxyState_RequiresKey(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleSetProxyState(map[string]any{"value": "x"})
	if resp.Error == "" {
		t.Fatal("expected error for missing key")
	}
}

// --- Config Overrides ---

func TestHandleSetConfig_GlobalOverride(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.token_threshold", "value": "300000"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["scope"] != "config.yaml" {
		t.Errorf("expected scope 'config.yaml', got %q", m["scope"])
	}

	// The value is in the file.
	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	fileValue, err := config.GetValue(cfgPath, "proxy.token_threshold")
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if fmt.Sprintf("%v", fileValue) != "300000" {
		t.Errorf("config.yaml proxy.token_threshold = %v, want 300000", fileValue)
	}

	// Reading reports the runtime override set_config wrote, which is what the
	// proxy applies (handleGetConfig priority: session > global override >
	// config.yaml).
	resp = h.handleGetConfig(map[string]any{"key": "proxy.token_threshold"})
	if resp.Error != "" {
		t.Fatalf("get config error: %s", resp.Error)
	}
	m = resultMap(t, resp)
	if m["value"] != "300000" {
		t.Errorf("expected '300000', got %q", m["value"])
	}
	if m["scope"] != "global" || m["source"] != "proxy_state" {
		t.Errorf("expected the global override, got scope=%q source=%q", m["scope"], m["source"])
	}
}

func TestHandleSetConfig_SessionOverride(t *testing.T) {
	h, _ := mustHandler(t)

	// Set global first (writes to config.yaml)
	h.handleSetConfig(map[string]any{"key": "proxy.token_threshold", "value": "200000"})
	// Set session-specific (writes to proxy_state only)
	h.handleSetConfig(map[string]any{"key": "proxy.token_threshold", "value": "400000", "session_id": "sess-1"})

	// Session-specific should win
	resp := h.handleGetConfig(map[string]any{"key": "proxy.token_threshold", "session_id": "sess-1"})
	m := resultMap(t, resp)
	if m["value"] != "400000" {
		t.Errorf("session override should win, got %q", m["value"])
	}
	if m["scope"] != "session:sess-1" {
		t.Errorf("expected session scope, got %q", m["scope"])
	}
	if m["source"] != "proxy_state" {
		t.Errorf("expected source 'proxy_state', got %q", m["source"])
	}

	// Without session_id, read from config.yaml
	resp = h.handleGetConfig(map[string]any{"key": "proxy.token_threshold"})
	m = resultMap(t, resp)
	if m["value"] != "200000" {
		t.Errorf("expected config.yaml value '200000', got %q", m["value"])
	}
}

func TestHandleSetConfig_AcceptsAnyKey(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleSetConfig(map[string]any{"key": "extraction.model", "value": "opus"})
	if resp.Error != "" {
		t.Fatalf("expected any key to be accepted, got error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["value"] != "opus" {
		t.Errorf("expected value 'opus', got %q", m["value"])
	}
}

func TestHandleSetConfig_RequiresKeyAndValue(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleSetConfig(map[string]any{"key": "token_threshold"})
	if resp.Error == "" {
		t.Fatal("expected error for missing value")
	}
	resp = h.handleSetConfig(map[string]any{"value": "123"})
	if resp.Error == "" {
		t.Fatal("expected error for missing key")
	}
}

// YM-96: set_config without the "proxy." prefix must land where the loader
// reads it. A bare top-level token_threshold is ignored by the loader while the
// proxy_state dual-write still applies it, so YAML and runtime diverge.
func TestHandleSetConfig_PrefixesKnownProxyKeys(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": "300000"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["key"] != "proxy.token_threshold" {
		t.Errorf("written key = %v, want proxy.token_threshold", m["key"])
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	got, err := config.GetValue(cfgPath, "proxy.token_threshold")
	if err != nil {
		t.Fatalf("read proxy.token_threshold: %v", err)
	}
	if got == nil {
		t.Fatal("proxy.token_threshold was not written")
	}
	if fmt.Sprintf("%v", got) != "300000" {
		t.Errorf("proxy.token_threshold = %v, want 300000", got)
	}
	// Reading the bare key now resolves to the proxy key by design, so only the
	// raw file shows where the value landed.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "token_threshold:") {
			t.Errorf("stray top-level token_threshold in:\n%s", data)
		}
	}

	// The runtime override the proxy reads must keep the same key.
	resp = h.handleGetConfig(map[string]any{"key": "token_threshold"})
	if m = resultMap(t, resp); m["value"] != "300000" {
		t.Errorf("runtime value = %v, want 300000", m["value"])
	}
}

// YM-96: nested maps below a proxy key stay addressable without the prefix.
func TestHandleSetConfig_PrefixesProxyMapKeys(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_thresholds.glm-5.2", "value": "500000"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	got, err := config.GetValue(cfgPath, "proxy.token_thresholds.glm-5.2")
	if err != nil {
		t.Fatalf("read proxy.token_thresholds.glm-5.2: %v", err)
	}
	if got == nil || fmt.Sprintf("%v", got) != "500000" {
		t.Errorf("proxy.token_thresholds.glm-5.2 = %v, want 500000", got)
	}
}

// YM-96: a key the schema does not know used to be written into a place no code
// reads. It is rejected instead, so a typo cannot look like a successful write.
func TestHandleSetConfig_RejectsUnknownKeys(t *testing.T) {
	h, _ := mustHandler(t)

	for _, key := range []string{"frobnicate", "proxy.frobnicate", "proxy.token_threshold.nested"} {
		resp := h.handleSetConfig(map[string]any{"key": key, "value": "1"})
		if resp.Error == "" {
			t.Errorf("expected error for unknown config key %q", key)
			continue
		}
		if !strings.Contains(resp.Error, strings.Split(key, ".")[0]) {
			t.Errorf("error for %q should name the rejected key, got %q", key, resp.Error)
		}
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	if got, _ := config.GetValue(cfgPath, "frobnicate"); got != nil {
		t.Errorf("unknown key was written: frobnicate = %v", got)
	}
	if got, _ := config.GetValue(cfgPath, "proxy.frobnicate"); got != nil {
		t.Errorf("unknown key was written: proxy.frobnicate = %v", got)
	}
}

// Cold review F1: a value that does not fit the field type makes Load reject the
// whole file, so set_config must refuse it instead of writing it.
func TestHandleSetConfig_RejectsMistypedValue(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": "opus=500000"})
	if resp.Error == "" {
		t.Fatal("expected an error for a non-numeric threshold")
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("config.yaml was written although the value was rejected")
	}
}

// Cold review F2: the per-model threshold override the proxy reads as
// config_override:token_threshold:<model> must stay reachable, and the file has
// to say the same thing the runtime applies.
func TestHandleSetConfig_ModelSpecificThreshold(t *testing.T) {
	for _, key := range []string{"token_threshold:deepseek", "proxy.token_threshold:deepseek"} {
		h, store := mustHandler(t)

		resp := h.handleSetConfig(map[string]any{"key": key, "value": "500000"})
		if resp.Error != "" {
			t.Fatalf("%s: set config error: %s", key, resp.Error)
		}
		if m := resultMap(t, resp); m["key"] != "proxy.token_thresholds.deepseek" {
			t.Errorf("%s: written key = %v, want proxy.token_thresholds.deepseek", key, m["key"])
		}

		cfgPath := filepath.Join(h.dataDir, "config.yaml")
		got, err := config.GetValue(cfgPath, "proxy.token_thresholds.deepseek")
		if err != nil {
			t.Fatalf("%s: read back: %v", key, err)
		}
		if got == nil || fmt.Sprintf("%v", got) != "500000" {
			t.Errorf("%s: proxy.token_thresholds.deepseek = %v, want 500000", key, got)
		}

		override, err := store.GetProxyState("config_override:token_threshold:deepseek")
		if err != nil {
			t.Fatalf("%s: read override: %v", key, err)
		}
		if override != "500000" {
			t.Errorf("%s: config_override:token_threshold:deepseek = %q, want 500000", key, override)
		}
	}
}

// Cold review round 2: a model name with a dot must survive both the YAML path
// and the state key, and the file must stay decodable.
func TestHandleSetConfig_ModelThresholdWithDottedName(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold:glm-5.2", "value": "500000"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.TokenThresholds["glm-5.2"] != 500000 {
		t.Errorf("token_thresholds = %v, want glm-5.2: 500000", cfg.Proxy.TokenThresholds)
	}

	override, err := store.GetProxyState("config_override:token_threshold:glm-5.2")
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	if override != "500000" {
		t.Errorf("state override = %q, want 500000", override)
	}
}

// Cold review round 2, finding 3: the schema spelling of a per-model threshold
// must write the state key the proxy actually reads.
func TestHandleSetConfig_ThresholdsPathWritesReadStateKey(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_thresholds.deepseek", "value": "500000"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}

	override, err := store.GetProxyState("config_override:token_threshold:deepseek")
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	if override != "500000" {
		t.Errorf("config_override:token_threshold:deepseek = %q, want 500000", override)
	}
	if dead, _ := store.GetProxyState("config_override:token_thresholds.deepseek"); dead != "" {
		t.Errorf("wrote an override key the proxy never reads: %q", dead)
	}
}

// Cold review round 2, finding 5: the runtime override must carry the value that
// was stored in the file, not the raw request string.
func TestHandleSetConfig_DualWriteStoresCoercedValue(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": " 300000 "})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}
	if m := resultMap(t, resp); m["value"] != "300000" {
		t.Errorf("response value = %v, want the coerced 300000", m["value"])
	}

	override, err := store.GetProxyState("config_override:token_threshold")
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	if override != "300000" {
		t.Errorf("state override = %q, want the coerced 300000", override)
	}
}

// --- Compacted Stubs ---

func TestHandleGetCompactedStubs_RequiresThreadID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetCompactedStubs(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing thread_id")
	}
}

func TestHandleGetCompactedStubs_FallbackToSessionID(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SaveCompactedBlock("s-fallback", 0, 10, "test block"); err != nil {
		t.Fatalf("seed legacy block: %v", err)
	}
	resp := h.handleGetCompactedStubs(map[string]any{"session_id": "s-fallback"})
	if resp.Error != "" {
		t.Fatalf("expected fallback to session_id, got error: %s", resp.Error)
	}
}

func TestHandleGetCompactedStubs_WithRange(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SaveCompactedBlock("t-range", 0, 50, "block 1"); err != nil {
		t.Fatalf("seed block 1: %v", err)
	}
	if err := store.SaveCompactedBlock("t-range", 51, 100, "block 2"); err != nil {
		t.Fatalf("seed block 2: %v", err)
	}

	// Only block in range 0-50
	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-range", "from_idx": float64(0), "to_idx": float64(50)})
	if resp.Error != "" {
		t.Fatalf("range query error: %s", resp.Error)
	}
}

func TestHandleGetCompactedStubs_FromFrozenStubs(t *testing.T) {
	h, store := mustHandler(t)

	// Simulate a frozen stub snapshot with an archive block
	frozenJSON := `{
		"messages": [{
			"role": "user",
			"content": "Something before archive"
		}, {
			"role": "user",
			"content": "[Archiv: Messages 1-725 (725 msgs) — get_compacted_stubs('t-frozen', 1, 725) zum Reinzoomen]\nTools: bash(238), read(57), edit(43)\nFiles: daemon(8), proxy(3)"
		}],
		"cutoff": 726,
		"boundary_hash": "abc123",
		"prefix_hash": "def456",
		"tokens": 45000,
		"raw_tokens": 120000
	}`
	store.SetProxyState("frozen:t-frozen", frozenJSON)

	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-frozen"})
	if resp.Error != "" {
		t.Fatalf("get from frozen: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) == 0 {
		t.Fatal("expected at least 1 block from frozen stubs")
	}
	block := blocks[0].(map[string]any)
	if start, ok := block["start_idx"].(float64); !ok || start != 1 {
		t.Fatalf("expected start_idx=1, got %v", block["start_idx"])
	}
	if end, ok := block["end_idx"].(float64); !ok || end != 725 {
		t.Fatalf("expected end_idx=725, got %v", block["end_idx"])
	}
	content, _ := block["content"].(string)
	if content == "" {
		t.Fatal("expected non-empty content")
	}
}

func TestHandleGetCompactedStubs_FrozenWithRange(t *testing.T) {
	h, store := mustHandler(t)

	frozenJSON := `{
		"messages": [{
			"role": "user",
			"content": "[Archiv: Messages 100-199 (100 msgs) — x]"
		}],
		"cutoff": 200,
		"tokens": 5000
	}`
	store.SetProxyState("frozen:t-range2", frozenJSON)

	// Range fully overlapping: should return block
	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-range2", "from_idx": float64(50), "to_idx": float64(150)})
	if resp.Error != "" {
		t.Fatalf("range query: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) == 0 {
		t.Fatal("expected block in overlapping range")
	}

	// Range outside: should return empty
	resp = h.handleGetCompactedStubs(map[string]any{"thread_id": "t-range2", "from_idx": float64(300), "to_idx": float64(400)})
	if resp.Error != "" {
		t.Fatalf("range query outside: %s", resp.Error)
	}
	blocks = resultSlice(t, resp)
	if len(blocks) != 0 {
		t.Fatal("expected 0 blocks for non-overlapping range")
	}
}

func TestHandleGetCompactedStubs_ContentFormat(t *testing.T) {
	h, store := mustHandler(t)

	frozenJSON := `{
		"messages": [{"role":"user","content":"[Archiv: Messages 10-20 (11 msgs) — get_compacted_stubs('t-fmt', 10, 20) zum Reinzoomen]\nTools: bash(5)"}],
		"cutoff": 21,
		"tokens": 5000
	}`
	store.SetProxyState("frozen:t-fmt", frozenJSON)

	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-fmt"})
	if resp.Error != "" {
		t.Fatalf("get: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0].(map[string]any)
	if start, ok := b["start_idx"].(float64); !ok || start != 10 {
		t.Fatalf("expected start_idx=10, got %v", b["start_idx"])
	}
	content, _ := b["content"].(string)
	if content == "" {
		t.Fatal("content is empty")
	}
	// Verify content mentions the range and frozen tokens
	if !strings.Contains(content, "Messages 10-20") {
		t.Fatalf("content missing range: %s", content)
	}
	if !strings.Contains(content, "frozen_tokens=5000") {
		t.Fatalf("content missing frozen_tokens: %s", content)
	}
}

func TestHandleGetCompactedStubs_MultipleMarkers(t *testing.T) {
	h, store := mustHandler(t)

	frozenJSON := `{
		"messages": [
			{"role":"user","content":"[Archiv: Messages 1-100 (100 msgs) — stage-2]"},
			{"role":"user","content":"[Archiv: Messages 101-200 (100 msgs) — stage-3]"}
		],
		"cutoff": 201,
		"tokens": 8000
	}`
	store.SetProxyState("frozen:t-multi", frozenJSON)

	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-multi"})
	if resp.Error != "" {
		t.Fatalf("get: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestHandleGetCompactedStubs_FallbackToCompactedBlocks(t *testing.T) {
	h, store := mustHandler(t)

	// Seed a row the way the pre-frozen-prefix proxy left it behind: the table is
	// a legacy read path, no RPC writes it any more.
	if err := store.SaveCompactedBlock("t-legacy", 0, 50, "legacy archive"); err != nil {
		t.Fatalf("seed legacy block: %v", err)
	}

	// No frozen stubs — should fall back to compacted_blocks
	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-legacy"})
	if resp.Error != "" {
		t.Fatalf("get fallback: %s", resp.Error)
	}
	blocks := resultSlice(t, resp)
	if len(blocks) == 0 {
		t.Fatal("expected blocks from legacy fallback")
	}
	content, _ := blocks[0].(map[string]any)["content"].(string)
	if content != "legacy archive" {
		t.Fatalf("expected legacy content, got: %s", content)
	}
}

func TestHandleGetCompactedStubs_MalformedJSON(t *testing.T) {
	h, store := mustHandler(t)

	store.SetProxyState("frozen:t-bad", `{not valid json`)

	resp := h.handleGetCompactedStubs(map[string]any{"thread_id": "t-bad"})
	if resp.Error == "" {
		t.Fatal("expected error for malformed JSON")
	}
}

// --- Expand Context ---

func TestHandleExpandContext_RequiresQueryOrRange(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleExpandContext(map[string]any{"thread_id": "t-1"})
	if resp.Error == "" {
		t.Fatal("expected error when neither query nor message_range given")
	}
}

func TestHandleExpandContext_MessageRange(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SaveCompactedBlock("t-expand", 10, 20, "context block"); err != nil {
		t.Fatalf("seed legacy block: %v", err)
	}

	resp := h.handleExpandContext(map[string]any{"thread_id": "t-expand", "message_range": "10-20"})
	if resp.Error != "" {
		t.Fatalf("expand error: %s", resp.Error)
	}
}

func TestHandleExpandContext_InvalidRange(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleExpandContext(map[string]any{"thread_id": "t-1", "message_range": "invalid"})
	if resp.Error == "" {
		t.Fatal("expected error for invalid range format")
	}
}

// --- Pins ---

func TestHandlePin_CreateAndGet(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handlePin(map[string]any{"content": "Always use TDD", "scope": "permanent", "project": "yesmem"})
	if resp.Error != "" {
		t.Fatalf("pin error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["scope"] != "permanent" {
		t.Errorf("expected permanent scope, got %q", m["scope"])
	}

	resp = h.handleGetPins(map[string]any{"project": "yesmem"})
	if resp.Error != "" {
		t.Fatalf("get pins error: %s", resp.Error)
	}
}

func TestHandlePin_DefaultsToSession(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handlePin(map[string]any{"content": "session pin"})
	if resp.Error != "" {
		t.Fatalf("pin error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["scope"] != "session" {
		t.Errorf("expected default session scope, got %q", m["scope"])
	}
}

func TestHandlePin_RequiresContent(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handlePin(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing content")
	}
}

func TestHandlePin_RejectsInvalidScope(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handlePin(map[string]any{"content": "x", "scope": "invalid"})
	if resp.Error == "" {
		t.Fatal("expected error for invalid scope")
	}
}

func TestHandleUnpin(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handlePin(map[string]any{"content": "to unpin", "scope": "session"})
	m := resultMap(t, resp)
	pinID := m["id"].(float64)

	resp = h.handleUnpin(map[string]any{"id": pinID, "scope": "session"})
	if resp.Error != "" {
		t.Fatalf("unpin error: %s", resp.Error)
	}
}

func TestHandleUnpin_RequiresID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleUnpin(map[string]any{"scope": "session"})
	if resp.Error == "" {
		t.Fatal("expected error for missing id")
	}
}

// --- Gaps ---

func TestHandleTrackAndGetGaps(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleTrackGap(map[string]any{"topic": "proxy caching", "project": "yesmem"})
	if resp.Error != "" {
		t.Fatalf("track gap error: %s", resp.Error)
	}

	resp = h.handleGetActiveGaps(map[string]any{"project": "yesmem"})
	if resp.Error != "" {
		t.Fatalf("get gaps error: %s", resp.Error)
	}
}

func TestHandleTrackGap_RequiresTopic(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleTrackGap(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing topic")
	}
}

func TestHandleResolveGap(t *testing.T) {
	h, _ := mustHandler(t)
	h.handleTrackGap(map[string]any{"topic": "to-resolve", "project": "test"})

	resp := h.handleResolveGap(map[string]any{"topic": "to-resolve", "project": "test"})
	if resp.Error != "" {
		t.Fatalf("resolve gap error: %s", resp.Error)
	}
}

func TestHandleResolveGap_RequiresTopic(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleResolveGap(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing topic")
	}
}

// --- PopRecentRemember ---

func TestHandlePopRecentRemember_Empty(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handlePopRecentRemember()
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

func TestHandlePopRecentRemember_ReturnsAndClears(t *testing.T) {
	h, _ := mustHandler(t)

	h.recentRememberMu.Lock()
	h.recentRemembered = []recentLearning{{ID: 1, Text: "test item"}}
	h.recentRememberMu.Unlock()

	resp := h.handlePopRecentRemember()
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}

	// Second call should be empty
	resp = h.handlePopRecentRemember()
	m := resultMap(t, resp)
	items := m["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected empty after pop, got %d items", len(items))
	}
}

// --- Index Status ---

func TestHandleIndexStatus_NoProgress(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleIndexStatus()
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["running"] != false {
		t.Error("expected running=false when no IndexProgress set")
	}
}

func TestHandleIndexStatus_WithProgress(t *testing.T) {
	h, _ := mustHandler(t)
	h.IndexProgress = func() (total, done, skipped int, running bool) {
		return 100, 42, 3, true
	}
	resp := h.handleIndexStatus()
	m := resultMap(t, resp)
	if m["total"] != float64(100) {
		t.Errorf("expected total=100, got %v", m["total"])
	}
	if m["done"] != float64(42) {
		t.Errorf("expected done=42, got %v", m["done"])
	}
	if m["running"] != true {
		t.Error("expected running=true")
	}
}

// --- Coverage & Project Profile ---

func TestHandleGetCoverage(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetCoverage(map[string]any{"project": "yesmem"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

func TestHandleGetProjectProfile_NotFound(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetProjectProfile(map[string]any{"project": "nonexistent"})
	if resp.Error != "" {
		t.Fatalf("should not error on missing profile: %s", resp.Error)
	}
}

func TestHandleGetSelfFeedback(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetSelfFeedback(map[string]any{"days": float64(7)})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

// --- Related To File ---

func TestHandleRelatedToFile(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleRelatedToFile(map[string]any{"path": "internal/proxy/proxy.go"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
}

// --- Track Session End ---

func TestHandleTrackSessionEnd_Success(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.handleTrackSessionEnd(map[string]any{
		"project":    "/home/testuser/projects/myapp",
		"session_id": "test-session-abc",
		"reason":     "clear",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	// Verify it was tracked and handed to the next briefing
	sid, reason, err := s.GetPendingRecovery("/home/testuser/projects/myapp", time.Hour)
	if err != nil {
		t.Fatalf("GetPendingRecovery: %v", err)
	}
	if sid != "test-session-abc" {
		t.Errorf("expected session_id='test-session-abc', got %q", sid)
	}
	if reason != "clear" {
		t.Errorf("expected reason='clear', got %q", reason)
	}
}

func TestHandleTrackSessionEnd_ClearsPinsOnClear(t *testing.T) {
	h, s := mustHandler(t)

	// Create a session-scoped pin. Project identifier must match the full path
	// passed to handleTrackSessionEnd, since ResolveProjectShort returns the
	// cleaned absolute path verbatim under the full-path-as-identifier convention.
	s.PinLearning("session", "/home/testuser/projects/myapp", "test pin", "test", "")

	resp := h.handleTrackSessionEnd(map[string]any{
		"project":    "/home/testuser/projects/myapp",
		"session_id": "sess-1",
		"reason":     "clear",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	// Session pins should be cleared
	pins, _ := s.GetPinnedLearnings("session", "/home/testuser/projects/myapp")
	if len(pins) != 0 {
		t.Errorf("expected 0 session pins after clear, got %d", len(pins))
	}
}

func TestHandleTrackSessionEnd_CompactKeepsPins(t *testing.T) {
	h, s := mustHandler(t)

	s.PinLearning("session", "/home/testuser/projects/myapp", "test pin", "test", "")

	resp := h.handleTrackSessionEnd(map[string]any{
		"project":    "/home/testuser/projects/myapp",
		"session_id": "sess-2",
		"reason":     "compact",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	// Session pins should survive compact
	pins, _ := s.GetPinnedLearnings("session", "/home/testuser/projects/myapp")
	if len(pins) != 1 {
		t.Errorf("expected 1 session pin after compact, got %d", len(pins))
	}
}

// YM-120: session pins without a project must not survive /clear.
func TestHandleTrackSessionEnd_ClearRemovesProjectlessSessionPins(t *testing.T) {
	h, s := mustFileHandler(t)

	const project = "/home/testuser/projects/myapp"
	if _, err := s.PinLearning("session", "", "projectless session pin", "test", "sess-3"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := s.PinLearning("session", project, "project session pin", "test", "sess-3"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := s.PinLearning("permanent", "", "permanent pin", "test", ""); err != nil {
		t.Fatalf("pin: %v", err)
	}

	resp := h.handleTrackSessionEnd(map[string]any{
		"project":    project,
		"session_id": "sess-3",
		"reason":     "clear",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	sessionPins, _ := s.GetPinnedLearnings("session", project)
	if len(sessionPins) != 0 {
		t.Errorf("expected 0 session pins after clear (project and project-less), got %d", len(sessionPins))
	}

	permanentPins, _ := s.GetPinnedLearnings("permanent", project)
	if len(permanentPins) != 1 {
		t.Errorf("permanent pins must survive /clear, got %d", len(permanentPins))
	}
}

// YM-121: unpin without a scope finds a pin that only exists in the permanent table.
func TestHandleUnpin_WithoutScopeFindsPermanentPin(t *testing.T) {
	h, s := mustFileHandler(t)

	const project = "/home/testuser/projects/myapp"
	id, err := s.PinLearning("permanent", project, "permanent pin", "test", "")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	resp := h.handleUnpin(map[string]any{"id": float64(id)})
	if resp.Error != "" {
		t.Fatalf("unpin error: %s", resp.Error)
	}

	permPins, _ := s.GetPinnedLearnings("permanent", project)
	if len(permPins) != 0 {
		t.Errorf("permanent pin must be removed when unpinning without a scope, got %d", len(permPins))
	}

	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["scope"] != "permanent" {
		t.Errorf("scope: got %v, want permanent", result["scope"])
	}
}

// YM-121: an id that exists in both tables must not silently delete two pins.
func TestHandleUnpin_AmbiguousIdRequiresScope(t *testing.T) {
	h, s := mustFileHandler(t)

	const project = "/home/testuser/projects/myapp"
	permID, err := s.PinLearning("permanent", project, "permanent pin", "test", "")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	sessID, err := s.PinLearning("session", project, "session pin", "test", "")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	// The two tables keep separate id sequences, so the same id exists twice.
	if permID != sessID {
		t.Fatalf("precondition: expected colliding ids, got permanent=%d session=%d", permID, sessID)
	}

	resp := h.handleUnpin(map[string]any{"id": float64(permID)})
	if resp.Error == "" {
		t.Fatal("an ambiguous id must be reported instead of deleting two pins")
	}
	if !strings.Contains(resp.Error, "scope") {
		t.Errorf("error should ask for a scope, got %q", resp.Error)
	}

	permPins, _ := s.GetPinnedLearnings("permanent", project)
	if len(permPins) != 1 {
		t.Errorf("permanent pin must survive an ambiguous unpin, got %d", len(permPins))
	}
	sessPins, _ := s.GetPinnedLearnings("session", project)
	if len(sessPins) != 1 {
		t.Errorf("session pin must survive an ambiguous unpin, got %d", len(sessPins))
	}

	// The same call succeeds once the scope names the target.
	resp = h.handleUnpin(map[string]any{"id": float64(permID), "scope": "permanent"})
	if resp.Error != "" {
		t.Fatalf("scoped unpin error: %s", resp.Error)
	}
	permPins, _ = s.GetPinnedLearnings("permanent", project)
	if len(permPins) != 0 {
		t.Errorf("scoped unpin must remove the permanent pin, got %d", len(permPins))
	}
	sessPins, _ = s.GetPinnedLearnings("session", project)
	if len(sessPins) != 1 {
		t.Errorf("session pin must stay untouched, got %d", len(sessPins))
	}
}

// YM-121: unpin must report when nothing was deleted instead of claiming success.
func TestHandleUnpin_ReportsNothingDeleted(t *testing.T) {
	h, _ := mustFileHandler(t)

	resp := h.handleUnpin(map[string]any{"id": float64(4242)})
	if resp.Error == "" {
		t.Error("unpin without scope must report that no pin was deleted")
	}

	resp = h.handleUnpin(map[string]any{"id": float64(4242), "scope": "permanent"})
	if resp.Error == "" {
		t.Error("unpin with scope must report that no pin was deleted")
	}
}

func TestHandleTrackSessionEnd_RequiresFields(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleTrackSessionEnd(map[string]any{})
	if resp.Error == "" {
		t.Fatal("expected error for missing fields")
	}

	resp = h.handleTrackSessionEnd(map[string]any{"project": "/tmp", "session_id": "x"})
	if resp.Error == "" {
		t.Fatal("expected error for missing reason")
	}
}

func TestHandleTrackSessionEnd_ViaDispatch(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.Handle(Request{
		Method: "track_session_end",
		Params: map[string]any{
			"project":    "/home/testuser/projects/myapp",
			"session_id": "dispatch-test",
			"reason":     "clear",
		},
	})
	if resp.Error != "" {
		t.Fatalf("dispatch error: %s", resp.Error)
	}
}

// --- Test helpers ---

func resultMap(t *testing.T, resp Response) map[string]any {
	t.Helper()
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, string(b))
	}
	return m
}

func resultSlice(t *testing.T, resp Response) []any {
	t.Helper()
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var s []any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, string(b))
	}
	return s
}

// Ensure recentLearning is accessible for tests.
var _ = recentLearning{}
var _ = sync.Mutex{}

// Cold review round 3, NEW-1: "0"/"1" reach the handler as int64 through
// CoerceValue, so the bool branch must accept them on the MCP path too.
func TestHandleSetConfig_BoolZeroOne(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.sawtooth_enabled", "value": "0"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}
	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.SawtoothEnabled {
		t.Error("sawtooth_enabled = true, want false after setting 0")
	}

	resp = h.handleSetConfig(map[string]any{"key": "proxy.sawtooth_enabled", "value": "1"})
	if resp.Error != "" {
		t.Fatalf("set config error: %s", resp.Error)
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Proxy.SawtoothEnabled {
		t.Error("sawtooth_enabled = false, want true after setting 1")
	}
}

// Cold review round 3, VORBESTEHEND-1: the per-session override must land in the
// slot the proxy reads, for both spellings, and the read must follow.
func TestHandleSetConfig_SessionOverrideUsesProxySlot(t *testing.T) {
	h, store := mustHandler(t)

	for _, key := range []string{"token_threshold", "proxy.token_threshold"} {
		if resp := h.handleSetConfig(map[string]any{"key": key, "value": "400000", "session_id": "sess-7"}); resp.Error != "" {
			t.Fatalf("%s: set session error: %s", key, resp.Error)
		}
		override, err := store.GetProxyState("session_override:token_threshold:sess-7")
		if err != nil {
			t.Fatalf("%s: read override: %v", key, err)
		}
		if override != "400000" {
			t.Errorf("%s: session_override:token_threshold:sess-7 = %q, want 400000", key, override)
		}
	}
}

// Cold review round 4: the model-suffix form must read from the file, not only
// from the runtime slot it wrote.
func TestHandleGetConfig_ModelThresholdAliasReadsFile(t *testing.T) {
	h, _ := mustHandler(t)

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  token_thresholds:\n    deepseek: 500000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resp := h.handleGetConfig(map[string]any{"key": "token_threshold:deepseek"})
	if resp.Error != "" {
		t.Fatalf("get config error: %s", resp.Error)
	}
	if m := resultMap(t, resp); m["value"] != "500000" {
		t.Errorf("get(token_threshold:deepseek) = %v, want 500000", m["value"])
	}
}

// Cold review round 4: the session branch must validate and coerce like the
// global one, otherwise a typo looks like an active override.
func TestHandleSetConfig_SessionValidatesLikeGlobal(t *testing.T) {
	h, store := mustHandler(t)

	for _, key := range []string{"frobnicate", "proxy.token_thresholds."} {
		resp := h.handleSetConfig(map[string]any{"key": key, "value": "1", "session_id": "s1"})
		if resp.Error == "" {
			t.Errorf("%q: session override accepted an invalid key", key)
		}
	}

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": " 300000 ", "session_id": "s2"})
	if resp.Error != "" {
		t.Fatalf("set session error: %s", resp.Error)
	}
	override, err := store.GetProxyState("session_override:token_threshold:s2")
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	if override != "300000" {
		t.Errorf("session override = %q, want the coerced 300000", override)
	}
}

// YM-227 (2): a pin created through the dispatch path must be stored under the
// canonical project path, because that is the value /clear compares against.
// A short-name pin would be invisible to the briefing and survive every clear.
func TestHandlePin_DispatchStoresCanonicalProjectPath(t *testing.T) {
	h, s := mustFileHandler(t)
	const project = "/home/testuser/projects/myapp"
	if err := s.UpsertSession(&models.Session{ID: "sess-pin", Project: project, StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	resp := h.Handle(Request{Method: "pin", Params: map[string]any{
		"content":     "short name pin",
		"scope":       "session",
		"project":     "myapp",
		"_cwd":        project,
		"_session_id": "sess-pin",
	}})
	if resp.Error != "" {
		t.Fatalf("pin error: %s", resp.Error)
	}

	pins, _ := s.GetPinnedLearnings("session", project)
	if len(pins) != 1 {
		t.Fatalf("pin not stored under the canonical project path %q, got %d pins", project, len(pins))
	}

	resp = h.Handle(Request{Method: "track_session_end", Params: map[string]any{
		"project":    project,
		"session_id": "sess-pin",
		"reason":     "clear",
	}})
	if resp.Error != "" {
		t.Fatalf("track_session_end error: %s", resp.Error)
	}

	pins, _ = s.GetPinnedLearnings("session", project)
	if len(pins) != 0 {
		t.Errorf("session pin survived /clear, got %d pins", len(pins))
	}
}

// YM-227: under opencode v2 the env-derived _session_id names whichever session
// the shared service created last. It must not be stored as the pin's owner —
// the pin would then die with an unrelated session's /clear — and with no
// provable session the pin has to stay project-bound so its own /clear reaches it.
func TestHandlePin_DoesNotTrustEnvDerivedSession(t *testing.T) {
	h, s := mustFileHandler(t)
	const project = "/home/testuser/projects/myapp"

	resp := h.Handle(Request{Method: "pin", Params: map[string]any{
		"content":            "tui pin",
		"scope":              "session",
		"project":            "",
		"_cwd":               project,
		"_session_id":        "sess-foreign",
		"_session_id_source": "env",
		"_source_agent":      "opencode",
		"_caller_pid":        float64(1),
	}})
	if resp.Error != "" {
		t.Fatalf("pin error: %s", resp.Error)
	}

	// A project-less pin attributed to the env-derived id would carry project '' and
	// session "sess-foreign", so the clear below could never reach it.
	pins, _ := s.GetPinnedLearnings("session", project)
	if len(pins) != 1 {
		t.Fatalf("expected the pin to be stored under the caller's project, got %d pins", len(pins))
	}

	resp = h.Handle(Request{Method: "track_session_end", Params: map[string]any{
		"project":    project,
		"session_id": "sess-own",
		"reason":     "clear",
	}})
	if resp.Error != "" {
		t.Fatalf("track_session_end error: %s", resp.Error)
	}
	pins, _ = s.GetPinnedLearnings("session", project)
	if len(pins) != 0 {
		t.Errorf("project-bound pin survived the project's /clear, got %d pins", len(pins))
	}
}

// YM-227: the cwd fallback exists for session pins that would otherwise be
// impossible to clear. A permanent pin is global by design ("visible in every
// turn") and must not be bound to one project.
func TestHandlePin_PermanentPinStaysGlobalWithoutSession(t *testing.T) {
	h, s := mustFileHandler(t)
	const project = "/home/testuser/projects/myapp"

	resp := h.Handle(Request{Method: "pin", Params: map[string]any{
		"content":            "global permanent pin",
		"scope":              "permanent",
		"project":            "",
		"_cwd":               project,
		"_session_id":        "sess-foreign",
		"_session_id_source": "env",
		"_source_agent":      "opencode",
		"_caller_pid":        float64(1),
	}})
	if resp.Error != "" {
		t.Fatalf("pin error: %s", resp.Error)
	}

	pins, _ := s.GetPinnedLearnings("permanent", project)
	if len(pins) != 1 {
		t.Fatalf("permanent pin not visible for the project, got %d pins", len(pins))
	}
	if pins[0].Project != "" {
		t.Errorf("permanent pin was bound to project %q, want global", pins[0].Project)
	}
}
