package proxy

import (
	"sync"
	"testing"
)

// los26OverrideDaemon is a proxy_state stub that counts every get_proxy_state
// query. The loop prefix is deliberate: parallel loops in this package have
// collided on helper names before (fakeDaemon, isSessionUUID).
type los26OverrideDaemon struct {
	mu       sync.Mutex
	values   map[string]string
	failKeys map[string]bool
	calls    int
}

func newLos26OverrideDaemon() *los26OverrideDaemon {
	return &los26OverrideDaemon{values: map[string]string{}, failKeys: map[string]bool{}}
}

func (d *los26OverrideDaemon) set(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if value == "" {
		delete(d.values, key)
		return
	}
	d.values[key] = value
}

func (d *los26OverrideDaemon) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *los26OverrideDaemon) handler(method string, params map[string]any) string {
	if method != "get_proxy_state" {
		return "null"
	}
	key, _ := params["key"].(string)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.failKeys[key] {
		// Not JSON: the fake daemon turns this into an unusable result ("null"),
		// which is what a daemon answer the proxy cannot read looks like.
		return "unusable"
	}
	return `{"value":"` + d.values[key] + `"}`
}

// The session-scoped override belongs to its own thread. Before the fix it was
// written into the process-wide override slot, so every other session inherited
// the threshold of whichever session happened to set it last.
func TestLos26_SessionThresholdOverrideIsThreadScoped(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:thread-a", "123456")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", ""); got != 123456 {
		t.Fatalf("thread-a threshold = %d, want the session override 123456", got)
	}
	if got := s.effectiveTokenThreshold("thread-b", ""); got != 200000 {
		t.Errorf("thread-b threshold = %d, want the config default 200000 (session override leaked into the global slot)", got)
	}
}

// The refresh costs one daemon round trip per configured model. Before the fix
// it ran on every request, so a single request paid for the whole override set.
func TestLos26_OverrideRefreshIsThrottled(t *testing.T) {
	d := newLos26OverrideDaemon()

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	s.cfg.TokenThresholds = map[string]int{"opus": 180000}

	s.refreshConfigOverrides("thread-a")
	first := d.callCount()
	if first == 0 {
		t.Fatal("first refresh issued no daemon query")
	}

	for i := 0; i < 20; i++ {
		s.refreshConfigOverrides("thread-a")
	}
	if extra := d.callCount() - first; extra != 0 {
		t.Errorf("20 further requests issued %d daemon queries, want 0 extra (refresh must be throttled)", extra)
	}
}

// A withdrawn override must stop applying. Before the fix the session value
// stayed in the global slot until the proxy restarted.
func TestLos26_SessionOverrideRemovalRestoresConfigDefault(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:thread-a", "123456")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("thread-a")
	if got := s.effectiveTokenThreshold("thread-a", ""); got != 123456 {
		t.Fatalf("thread-a threshold = %d, want the session override 123456", got)
	}

	d.set("session_override:token_threshold:thread-a", "")
	s.overrideRefreshMu.Lock()
	delete(s.sessionOverrideRefresh, "thread-a")
	s.overrideRefreshMu.Unlock()
	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", ""); got != 200000 {
		t.Errorf("thread-a threshold = %d after the override was withdrawn, want the config default 200000", got)
	}
}

// The session key is per thread, so its refresh window has to be per thread too.
// A process-wide window lets whichever thread arrives first consume it and
// starve every other thread of its own override.
func TestLos26_SessionRefreshWindowIsPerThread(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:thread-a", "111111")
	d.set("session_override:token_threshold:thread-b", "222222")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("thread-a")
	s.refreshConfigOverrides("thread-b")

	if got := s.effectiveTokenThreshold("thread-a", ""); got != 111111 {
		t.Errorf("thread-a threshold = %d, want 111111", got)
	}
	if got := s.effectiveTokenThreshold("thread-b", ""); got != 222222 {
		t.Errorf("thread-b threshold = %d, want 222222 (its own session key was never read)", got)
	}
}

// set_config stores the session_id it was given. A caller that passes the bare
// backend id writes a key without the protocol prefix the proxy derives, so the
// proxy must accept that spelling instead of silently applying nothing.
func TestLos26_SessionKeyWithoutProtocolPrefixIsAccepted(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:ses_abc", "333333")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("opencode:ses_abc")

	if got := s.effectiveTokenThreshold("opencode:ses_abc", ""); got != 333333 {
		t.Errorf("threshold = %d, want 333333 from the unprefixed session key", got)
	}
}

// An unusable answer is not a withdrawal: only a present but empty value clears
// the slot.
func TestLos26_UnusableDaemonAnswerKeepsOverride(t *testing.T) {
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		return "null"
	})
	s.cfg.TokenThreshold = 200000
	s.setThreadTokenThresholdOverride("thread-a", 120000)

	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", ""); got != 120000 {
		t.Errorf("threshold = %d after an unusable daemon answer, want the kept override 120000", got)
	}
}

// A thread with a protocol prefix has two candidate keys. If the first answers
// empty and the second cannot be read, the override is not proven withdrawn: the
// readable-empty answer alone must not clear it.
func TestLos26_PartialFailureDoesNotClearOverride(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:ses_abc", "333333")
	d.failKeys["session_override:token_threshold:ses_abc"] = true

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	s.setThreadTokenThresholdOverride("opencode:ses_abc", 333333)

	s.refreshConfigOverrides("opencode:ses_abc")

	if got := s.effectiveTokenThreshold("opencode:ses_abc", ""); got != 333333 {
		t.Errorf("threshold = %d after a partial read, want the kept override 333333", got)
	}
}
