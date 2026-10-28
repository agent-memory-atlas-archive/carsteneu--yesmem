package proxy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// A failed daemon call must not consume the thread's one-shot sync. Observed on
// 28.10.2026: the deploy restarted the daemon, the first request of the session
// hit it while it was down, the thread was marked as synced anyway, and the plan
// never reached the status file for the rest of the session.
func TestPlanSnapshotRetriesAfterAFailedFetch(t *testing.T) {
	resp := planResponse("the plan", StatusPlanItem{Content: "step", Status: "pending"})
	calls := 0
	s := fakeDaemon(t, func(method string, _ map[string]any) string {
		if method != "get_plan" {
			return ""
		}
		calls++
		raw, err := json.Marshal(*resp)
		if err != nil {
			return ""
		}
		return string(raw)
	})
	s.cacheStatusWriter = NewCacheStatusWriter(s.cfg.DataDir, "ephemeral", 250000, 100000)
	t.Cleanup(s.cacheStatusWriter.Stop)
	s.cacheStatusWriter.Update(time.Now(), 1000, 0, 0, "t-retry")

	// The daemon is not answering: no socket in this data dir.
	reachable := s.cfg.DataDir
	s.cfg.DataDir = t.TempDir()
	s.syncPlanSnapshot("t-retry", false)
	if calls != 0 {
		t.Fatalf("the stub must not have been reached, got %d calls", calls)
	}

	// The daemon is back.
	s.cfg.DataDir = reachable
	s.syncPlanSnapshot("t-retry", false)

	if calls != 1 {
		t.Fatalf("expected a retry after the failed fetch, got %d fetches", calls)
	}
	s.cacheStatusWriter.writeStatus()
	data, err := os.ReadFile(CacheStatusPath(s.cfg.DataDir, "t-retry"))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(string(data), `"plan"`) {
		t.Errorf("the outline never reached the status file: %s", data)
	}
}

// A successful answer still syncs once per thread, not once per request.
func TestPlanSnapshotStillSyncsOnceAfterASuccessfulFetch(t *testing.T) {
	resp := planResponse("the plan", StatusPlanItem{Content: "step", Status: "pending"})
	calls := 0
	s := fakeDaemon(t, func(method string, _ map[string]any) string {
		if method != "get_plan" {
			return ""
		}
		calls++
		raw, err := json.Marshal(*resp)
		if err != nil {
			return ""
		}
		return string(raw)
	})
	s.cacheStatusWriter = NewCacheStatusWriter(s.cfg.DataDir, "ephemeral", 250000, 100000)
	t.Cleanup(s.cacheStatusWriter.Stop)
	// The writer has seen this thread, so the publish lands and the answer can be
	// cached.
	s.cacheStatusWriter.Update(time.Now(), 1000, 0, 0, "t-once")

	s.syncPlanSnapshot("t-once", false)
	s.syncPlanSnapshot("t-once", false)
	if calls != 1 {
		t.Errorf("expected one fetch for a thread that answered, got %d", calls)
	}
}

// A plan call runs inside Code Mode, so the assistant message carries one
// tool_use named "execute" whose code calls the plan tool. Reading only the tool
// names misses it, and the snapshot then waits for the next request that happens
// to look at the plan again.
func TestDetectPlanToolCallSeesCodeModeExecute(t *testing.T) {
	s := &Server{}
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "execute",
					"input": map[string]any{"code": `await tools.yesmem.set_plan({items:[{content:"a",status:"pending"}]})`},
				},
			},
		},
	}

	if !s.detectPlanToolCall(messages, "t-code-mode", 0) {
		t.Fatal("a set_plan call inside Code Mode must count as a plan call")
	}
}

// The namespaced spellings must count too.
func TestDetectPlanToolCallSeesNamespacedTools(t *testing.T) {
	s := &Server{}
	for _, name := range []string{"yesmem_set_plan", "tools.yesmem.update_plan", "set_plan"} {
		messages := []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "name": name, "input": map[string]any{}},
				},
			},
		}
		if !s.detectPlanToolCall(messages, "t-namespaced", 0) {
			t.Errorf("%s must count as a plan call", name)
		}
	}
}

// The status writer only writes into a thread it has already seen, and the first
// request of a session publishes the outline before the writer has counted that
// request. The snapshot is therefore dropped; the thread must not be marked as
// synced, or the outline never reaches the file for the rest of the session.
func TestPlanSnapshotRetriesWhenTheWriterHasNotSeenTheThread(t *testing.T) {
	resp := planResponse("the plan", StatusPlanItem{Content: "step", Status: "pending"})
	calls := 0
	s := fakeDaemon(t, func(method string, _ map[string]any) string {
		if method != "get_plan" {
			return ""
		}
		calls++
		raw, err := json.Marshal(*resp)
		if err != nil {
			return ""
		}
		return string(raw)
	})
	s.cacheStatusWriter = NewCacheStatusWriter(s.cfg.DataDir, "ephemeral", 250000, 100000)
	t.Cleanup(s.cacheStatusWriter.Stop)

	// No Update for this thread yet: the writer does not know it, so the snapshot
	// cannot land.
	s.syncPlanSnapshot("t-early", false)

	// The request is counted now, as the response side does it.
	s.cacheStatusWriter.Update(time.Now(), 1000, 0, 0, "t-early")
	s.syncPlanSnapshot("t-early", false)

	if calls != 2 {
		t.Fatalf("expected a retry once the writer knows the thread, got %d fetches", calls)
	}
	s.cacheStatusWriter.writeStatus()
	data, err := os.ReadFile(CacheStatusPath(s.cfg.DataDir, "t-early"))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(string(data), `"plan"`) {
		t.Errorf("the outline never reached the status file: %s", data)
	}
}
