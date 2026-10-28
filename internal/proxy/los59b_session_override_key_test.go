package proxy

import "testing"

// YM-427: the per-session token threshold moved from
// config_override:token_threshold:<session> to session_override:token_threshold:<session>.
//
// It had to move. The config_override namespace also carries the documented
// per-model slots, and the model segment is written from a caller-chosen name:
// set_config("token_threshold:<anything>") mirrors it into exactly that key through
// the daemon, with no ownership check to speak of. A session suffix could therefore
// be made to look like a model slot at will, which is what any guard on that key
// would have had to decide on. The session slot now sits in a family of its own,
// and the old key keeps no reader for it — that is the property these two tests
// pin.

// The old key is dead as a session slot: even a caller who writes it gets nothing.
func TestLos59bSessionOverrideIgnoresTheModelNamespace(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("config_override:token_threshold:thread-lb", "777777")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("thread-lb")

	if got := s.effectiveTokenThreshold("thread-lb", ""); got != 200000 {
		t.Fatalf("threshold = %d, want the config default 200000: the model namespace still feeds the session slot", got)
	}
}

// The new family is read, so the documented per-session override keeps working.
func TestLos59bSessionOverrideReadsItsOwnFamily(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("session_override:token_threshold:thread-lb", "150000")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000

	s.refreshConfigOverrides("thread-lb")

	if got := s.effectiveTokenThreshold("thread-lb", ""); got != 150000 {
		t.Fatalf("threshold = %d, want the session override 150000", got)
	}
}
