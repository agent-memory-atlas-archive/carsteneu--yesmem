package proxy

import (
	"math"
	"strings"
	"testing"
)

// los26ePrefix builds a frozen prefix of the size a long thread reaches: about
// 1.2 MB of JSON, the payload the audit measured at roughly 20 ms per request.
func los26ePrefix(messages int) []any {
	body := strings.Repeat("token ", 120)
	msgs := make([]any, 0, messages)
	for i := 0; i < messages; i++ {
		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": body},
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_" + strings.Repeat("x", i%7),
					"name":  "bash",
					"input": map[string]any{"command": "ls -la"},
				},
			},
		})
	}
	return msgs
}

// The snapshot runs on every request and compares the stored prefix against the
// bytes the request carries. Marshalling the prefix for that comparison costs
// megabytes of allocation per turn; the untouched case has to be answered
// without touching the heap at all.
func TestLos26e_SnapshotComparesIdenticalPrefixWithoutAllocating(t *testing.T) {
	f := NewFrozenStubs()
	persists := 0
	f.SetPersistFunc(func(key, value string) { persists++ })

	msgs := los26ePrefix(1400)
	f.Store("t1", msgs, len(msgs), msgs[len(msgs)-1], 1, 2)
	persists = 0 // Store persists the initial prefix; only the update counts

	// Store keeps a deep copy, so every round hands UpdateMessages a structurally
	// equal but distinct slice, exactly like a freshly decoded request does.
	allocs := testing.AllocsPerRun(5, func() {
		if f.UpdateMessages("t1", msgs) {
			t.Error("an identical prefix was reported as changed")
		}
	})
	if allocs > 0 {
		t.Errorf("comparing an unchanged prefix allocated %.0f times per call, want 0", allocs)
	}
	if persists != 0 {
		t.Errorf("an unchanged prefix was persisted %d times, want 0", persists)
	}
}

// The cheap comparison must not become blind: a single changed value deep inside
// the prefix still has to be reported and persisted.
func TestLos26e_SnapshotDetectsChangeDeepInsidePrefix(t *testing.T) {
	f := NewFrozenStubs()
	var persists int
	f.SetPersistFunc(func(key, value string) { persists++ })

	msgs := los26ePrefix(200)
	f.Store("t1", msgs, len(msgs), msgs[len(msgs)-1], 1, 2)
	persists = 0 // Store persists the initial prefix; only the update counts

	changed := los26ePrefix(200)
	input := changed[150].(map[string]any)["content"].([]any)[1].(map[string]any)["input"].(map[string]any)
	input["command"] = "ls -la /tmp"

	if !f.UpdateMessages("t1", changed) {
		t.Fatal("a rewritten value deep inside the prefix was not reported as a change")
	}
	if persists != 1 {
		t.Errorf("changed prefix persisted %d times, want 1", persists)
	}
}

// -0.0 and 0.0 are equal in Go, but they marshal to different bytes. Reading
// them as equal would keep the stored prefix stale against the wire: the next
// restore turn replays "-0" while the client sent "0", and the prompt cache
// breaks without any code path noticing.
func TestLos26e_SnapshotSeesSignedZeroAsChange(t *testing.T) {
	f := NewFrozenStubs()
	persists := 0
	f.SetPersistFunc(func(key, value string) { persists++ })

	seed := []any{map[string]any{"n": 0.0}}
	f.Store("t1", seed, 1, seed[0], 1, 2)
	persists = 0 // Store persists the initial prefix; only the update counts

	negZero := math.Float64frombits(1 << 63)
	if !f.UpdateMessages("t1", []any{map[string]any{"n": negZero}}) {
		t.Fatal("a prefix whose only difference is the sign of zero was reported as unchanged")
	}
	if persists != 1 {
		t.Errorf("changed prefix persisted %d times, want 1", persists)
	}
}

// A value the comparison does not know must count as different, never as equal:
// the fallback then compares the marshalled bytes, which keeps the decision
// exact even when the walk gives up.
func TestLos26e_SnapshotFallsBackToBytesForUnhandledTypes(t *testing.T) {
	f := NewFrozenStubs()
	var persists int
	f.SetPersistFunc(func(key, value string) { persists++ })

	seed := []any{map[string]any{"n": float64(1)}}
	f.Store("t1", seed, 1, seed[0], 1, 2)
	persists = 0 // Store persists the initial prefix; only the update counts

	// int(1) marshals to the same bytes as the stored float64(1). The walk does
	// not handle int and must not call the values equal, but the byte comparison
	// still suppresses the write.
	if f.UpdateMessages("t1", []any{map[string]any{"n": int(1)}}) {
		t.Error("a byte-identical payload was reported as changed")
	}
	if persists != 0 {
		t.Errorf("byte-identical payload persisted %d times, want 0", persists)
	}
}
