package proxy

import (
	"encoding/json"
	"testing"
)

func TestDecayTracker_PersistAndRestore(t *testing.T) {
	persisted := make(map[string]string)
	persistFn := func(key, value string) { persisted[key] = value }

	dt1 := NewDecayTracker()
	dt1.SetPersistFunc(persistFn)

	dt1.MarkStubbed(5, 10, 0.8)
	dt1.MarkStubbed(7, 15, 0.3) // clock is 15 from here on
	dt1.SetFilePath(5, "main.go")

	dt1.Persist("thread-1")

	val, ok := persisted["decay:thread-1"]
	if !ok {
		t.Fatal("expected persist callback with key 'decay:thread-1'")
	}

	// Verify JSON structure: ages relative to the persist clock, not absolute indices
	var dp decayPersisted
	if err := json.Unmarshal([]byte(val), &dp); err != nil {
		t.Fatalf("persisted value is not valid JSON: %v", err)
	}
	if dp.V != decayPersistVersion {
		t.Errorf("expected version %d, got %d", decayPersistVersion, dp.V)
	}
	if len(dp.Ages) != 2 {
		t.Fatalf("expected 2 age entries, got %d", len(dp.Ages))
	}
	if dp.Ages["msg_5"] != 5 {
		t.Errorf("expected msg_5 age=5 (stubbed at 10, clock 15), got %d", dp.Ages["msg_5"])
	}
	if dp.Ages["msg_7"] != 0 {
		t.Errorf("expected msg_7 age=0 (stubbed at clock), got %d", dp.Ages["msg_7"])
	}
	if dp.Intensity["msg_5"] != 0.8 {
		t.Errorf("expected msg_5 intensity=0.8, got %f", dp.Intensity["msg_5"])
	}
	if dp.FilePaths["msg_5"] != "main.go" {
		t.Errorf("expected msg_5 filePath=main.go, got %s", dp.FilePaths["msg_5"])
	}

	// Restore into a new instance at the same clock: stages must match exactly.
	loadFn := func(key string) (string, bool) {
		v, ok := persisted[key]
		return v, ok
	}

	dt2 := NewDecayTracker()
	dt2.SetLoadFunc(loadFn)
	dt2.LoadFromDB("thread-1", 15)

	stage1 := dt1.GetStage(5, 20, 100, 2.5)
	stage2 := dt2.GetStage(5, 20, 100, 2.5)
	if stage1 != stage2 {
		t.Errorf("stage mismatch for msg_5: original=%d, restored=%d", stage1, stage2)
	}

	stage1 = dt1.GetStage(7, 20, 100, 2.5)
	stage2 = dt2.GetStage(7, 20, 100, 2.5)
	if stage1 != stage2 {
		t.Errorf("stage mismatch for msg_7: original=%d, restored=%d", stage1, stage2)
	}
}

// A blob written by the previous format holds absolute request indices of a
// process clock that no longer exists. Reusing them produced negative ages, so
// such a blob is ignored instead of reinterpreted as ages. The clock here is
// large enough that reusing stubbed_at=40 would yield a real (wrong) stage and
// not the stage-0 every age above the boundary could produce.
func TestDecayTracker_LegacyBlobIgnored(t *testing.T) {
	loadFn := func(key string) (string, bool) {
		return `{"stubbed_at":{"msg_5":40},"intensity":{"msg_5":0.5},"file_paths":{}}`, true
	}

	dt := NewDecayTracker()
	dt.SetLoadFunc(loadFn)
	dt.LoadFromDB("thread-legacy", 90)

	// Reinterpreted as an age it would be 90-40=50 requests old → stage 3.
	if stage := dt.GetStage(5, 90, 100, 2.5); stage != DecayStage0 {
		t.Errorf("a legacy blob must not resurrect as state, got stage %d", stage)
	}

	// Positive control: the same shape with v:2 is loaded and does decay.
	dt2 := NewDecayTracker()
	dt2.SetLoadFunc(func(key string) (string, bool) {
		return `{"v":3,"ages":{"msg_5":50},"intensity":{},"file_paths":{}}`, true
	})
	dt2.LoadFromDB("thread-legacy", 90)
	if stage := dt2.GetStage(5, 90, 100, 2.5); stage != DecayStage3 {
		t.Errorf("a v2 blob with age 50 must decay to stage 3, got stage %d", stage)
	}
}

func TestDecayTracker_RestoreInvalidJSON(t *testing.T) {
	loadFn := func(key string) (string, bool) {
		return "broken{json", true
	}

	dt := NewDecayTracker()
	dt.SetLoadFunc(loadFn)

	// Should not panic
	dt.LoadFromDB("thread-bad", 100)

	// Should return stage 0 (no data)
	stage := dt.GetStage(5, 100, 100, 2.5)
	if stage != DecayStage0 {
		t.Errorf("expected stage 0 for untracked message, got %d", stage)
	}
}

func TestDecayTracker_RestoreNotFound(t *testing.T) {
	loadFn := func(key string) (string, bool) {
		return "", false
	}

	dt := NewDecayTracker()
	dt.SetLoadFunc(loadFn)
	dt.LoadFromDB("thread-missing", 100)

	// No-op, no panic
}

func TestDecayTracker_LoadOnlyOnce(t *testing.T) {
	loadCount := 0
	loadFn := func(key string) (string, bool) {
		loadCount++
		return `{"v":3,"ages":{"msg_1":0},"intensity":{"msg_1":0.5},"file_paths":{}}`, true
	}

	dt := NewDecayTracker()
	dt.SetLoadFunc(loadFn)

	dt.LoadFromDB("thread-1", 5)
	if loadCount != 1 {
		t.Errorf("expected 1 load, got %d", loadCount)
	}

	dt.LoadFromDB("thread-1", 6)
	if loadCount != 1 {
		t.Errorf("expected still 1 load, got %d", loadCount)
	}
}

func TestDecayTracker_NoPersistWithoutCallback(t *testing.T) {
	dt := NewDecayTracker()
	dt.MarkStubbed(5, 10, 0.5)

	// Should not panic
	dt.Persist("thread-1")
}
