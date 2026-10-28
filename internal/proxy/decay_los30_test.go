package proxy

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
)

// YM-128: one tracker instance served every thread. State keyed only by message
// index meant a message at index 5 inherited the stub age of another thread's
// message 5, and every thread persisted the whole tracker, so the stored blobs
// were byte-identical.
func TestDecayFor_IsPerThread(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	a := s.decayFor("thread-a")
	b := s.decayFor("thread-b")
	if a == b {
		t.Fatal("threads share one decay tracker")
	}
	if again := s.decayFor("thread-a"); again != a {
		t.Error("decayFor must return the same tracker for the same thread")
	}

	// Thread A stubs its message 5 at request 10.
	a.MarkStubbed(5, 10, 0)

	// Thread B is a different thread; its message 5 was never stubbed.
	if stage := b.GetStage(5, 60, 100, 2.5); stage != DecayStage0 {
		t.Fatalf("thread B inherits thread A's stub age: stage=%d, want 0 for an unstubbed message", stage)
	}
	// …and A still sees its own stub decay normally.
	if stage := a.GetStage(5, 60, 100, 2.5); stage != DecayStage3 {
		t.Fatalf("thread A lost its own stub age: stage=%d, want 3", stage)
	}
}

// YM-128: each thread persists only its own entries. With a shared tracker
// every blob was byte-identical, so restoring one thread restored all of them.
func TestDecayFor_PersistedBlobsArePerThread(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	persisted := map[string]string{}
	persist := func(key, value string) { persisted[key] = value }

	a := s.decayFor("thread-a")
	a.SetPersistFunc(persist)
	b := s.decayFor("thread-b")
	b.SetPersistFunc(persist)

	a.MarkStubbed(1, 5, 0)
	b.MarkStubbed(9, 6, 0)
	a.Persist("thread-a")
	b.Persist("thread-b")

	if persisted["decay:thread-a"] == "" {
		t.Fatal("thread-a persisted nothing")
	}
	if persisted["decay:thread-a"] == persisted["decay:thread-b"] {
		t.Fatal("both threads persisted the same blob")
	}
	if strings.Contains(persisted["decay:thread-a"], "msg_9") {
		t.Errorf("thread-a's blob carries thread-b's message 9: %s", persisted["decay:thread-a"])
	}
	if strings.Contains(persisted["decay:thread-b"], "msg_1\"") {
		t.Errorf("thread-b's blob carries thread-a's message 1: %s", persisted["decay:thread-b"])
	}
}

// The blob lives in proxy_state, which anything with socket access can write.
// Implausible ages must not clear or pin a stub forever.
func TestDecayTracker_RejectsImplausibleAges(t *testing.T) {
	dt := NewDecayTracker()
	dt.SetLoadFunc(func(key string) (string, bool) {
		return `{"v":3,"ages":{"msg_1":-5,"msg_2":9223372036854775807,"msg_3":20},"intensity":{},"file_paths":{}}`, true
	})
	dt.LoadFromDB("thread-tampered", 100)

	if stage := dt.GetStage(1, 100, 100, 2.5); stage != DecayStage0 {
		t.Errorf("negative age was accepted (stage %d)", stage)
	}
	if stage := dt.GetStage(2, 100, 100, 2.5); stage != DecayStage0 {
		t.Errorf("absurd age was accepted (stage %d) — the stub would be cleared permanently", stage)
	}
	if stage := dt.GetStage(3, 100, 100, 2.5); stage != DecayStage2 {
		t.Errorf("plausible age 20 must still load (stage %d, want 2: 5/15/50 boundaries)", stage)
	}
}

// The thread id comes from a client header, so the per-thread map must be
// bounded: beyond the cap a thread keeps no state instead of growing forever.
func TestDecayFor_IsBounded(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	for i := 0; i < maxDecayTrackers+10; i++ {
		s.decayFor(fmt.Sprintf("thread-%d", i))
	}
	if got := len(s.decayTrackers); got > maxDecayTrackers {
		t.Fatalf("decayTrackers grew past the cap: %d", got)
	}

	// An over-cap thread still gets a usable tracker (state never shared).
	a := s.decayFor("thread-over-cap")
	b := s.decayFor("thread-over-cap")
	if a == nil || b == nil {
		t.Fatal("over-cap thread got no tracker")
	}
	if a == b {
		t.Error("over-cap tracker must not be cached/shared")
	}
}

// YM-128: stubbedAt was persisted as an absolute request index while a restarted
// proxy starts its counter at 1. The restored age went negative, GetStage
// returned stage 0 for good and progressive decay never resumed.
func TestDecayTracker_AgeSurvivesRestart(t *testing.T) {
	persisted := map[string]string{}
	dt1 := NewDecayTracker()
	dt1.SetPersistFunc(func(k, v string) { persisted[k] = v })

	dt1.MarkStubbed(5, 50, 0)
	// At request 70 the stub is 20 requests old: 5 <= 20 < 50 → stage 2.
	want := dt1.GetStage(5, 70, 100, 2.5)
	if want != DecayStage2 {
		t.Fatalf("fixture broken: want stage 2, got %d", want)
	}
	dt1.Persist("thread-1")

	// New process: the global request counter starts again at 2.
	dt2 := NewDecayTracker()
	dt2.SetLoadFunc(func(k string) (string, bool) { v, ok := persisted[k]; return v, ok })
	dt2.LoadFromDB("thread-1", 2)

	if got := dt2.GetStage(5, 2, 100, 2.5); got != want {
		t.Fatalf("after restart the stub lost its age: got stage %d, want %d", got, want)
	}
}
