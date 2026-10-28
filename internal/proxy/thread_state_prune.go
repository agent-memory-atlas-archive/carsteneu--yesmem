package proxy

// Per-thread state of the proxy is keyed by the thread id, and the thread id is
// either a client header or a hash over client content. Both are caller-driven:
// a client that invents one id per request, or simply a proxy that serves many
// sessions over weeks, grows every one of these maps without a bound. Only the
// maps in checkpoint_counter.go, rules_inject.go and cache_ttl_detect.go had a
// cap; the rest did not, and loopStates had no delete path at all.
//
// The sweep runs on the existing pruner (every 50th request, both wire formats).
// It drops arbitrary entries of the map being trimmed — the same trade-off the
// capped maps already document: what they hold is a per-thread baseline or a
// cache, so a dropped entry costs one re-derived value, not history.

// timestampEntriesPerThread bounds the per-message annotations of one thread.
// The newest msg:N survive, so the messages a running session still annotates
// keep their ids and only old, no longer replayed entries go.
const timestampEntriesPerThread = 2000

// pruneThreadKeyed trims a thread-keyed map to keep entries. Map iteration order
// is randomized, so the survivors are arbitrary.
func pruneThreadKeyed[V any](m map[string]V, keep int) int {
	removed := 0
	for k := range m {
		if len(m) <= keep {
			break
		}
		delete(m, k)
		removed++
	}
	return removed
}

// pruneLoopStates trims the loop states to keep entries.
//
// A state with a pending escalation survives while plain candidates are left:
// dropping it loses the escalation level, and the next signal starts the ladder
// at the first warning again, so an agent stuck in the same loop never gets past
// level one. Only when the map cannot be trimmed below keep without touching
// escalated states do they go too: an unbounded, caller-keyed map is the worse
// trade (see maxThreadStates).
func pruneLoopStates(m map[string]*LoopState, keep int) int {
	removed := 0
	for k, st := range m {
		if len(m) <= keep {
			break
		}
		if st != nil && st.escalated() {
			continue
		}
		delete(m, k)
		removed++
	}
	for k := range m {
		if len(m) <= keep {
			break
		}
		delete(m, k)
		removed++
	}
	return removed
}

// pruneThreadStateMaps applies the caps to every per-thread map of the server.
// It is the single entry point, so a new map has one place to be registered.
func (s *Server) pruneThreadStateMaps() {
	if s.timestampStore != nil {
		s.timestampStore.CapState(maxThreadStates, timestampEntriesPerThread)
	}

	s.loopMu.Lock()
	pruneLoopStates(s.loopStates, maxThreadStates)
	s.loopMu.Unlock()

	s.lastInjectedIDsMu.Lock()
	pruneThreadKeyed(s.lastInjectedIDs, maxThreadStates)
	pruneThreadKeyed(s.sessionInjectCounts, maxThreadStates)
	pruneThreadKeyed(s.lastTurnInjected, maxThreadStates)
	s.lastInjectedIDsMu.Unlock()

	s.responseTsMu.Lock()
	pruneThreadKeyed(s.responseTimes, maxThreadStates)
	s.responseTsMu.Unlock()

	s.thinkMu.Lock()
	pruneThreadKeyed(s.thinkCounters, maxThreadStates)
	s.thinkMu.Unlock()

	s.channelMu.Lock()
	pruneThreadKeyed(s.channelInjectCount, maxThreadStates)
	s.channelMu.Unlock()

	s.configOverrideMu.Lock()
	// A dropped override is a session setting, not a cache: the thread falls
	// back to the global threshold. It is capped anyway — an unbounded map with
	// a client-chosen key is the worse trade.
	pruneThreadKeyed(s.threadThresholdOverrides, maxThreadStates)
	s.configOverrideMu.Unlock()

	s.threadCWDMu.Lock()
	pruneThreadKeyed(s.threadCWD, maxThreadStates)
	s.threadCWDMu.Unlock()

	s.rawSavingsMu.Lock()
	pruneThreadKeyed(s.rawSavings, maxThreadStates)
	s.rawSavingsMu.Unlock()

	s.failStateMu.Lock()
	pruneThreadKeyed(s.failFingerprints, maxThreadStates)
	s.failStateMu.Unlock()

	if s.msgCounters != nil {
		s.msgCounters.prune(maxThreadStates)
	}
	if s.forkState != nil {
		s.forkState.pruneThreads(maxThreadStates)
	}
	if s.cacheStatusWriter != nil {
		s.cacheStatusWriter.pruneThreads(maxThreadStates)
	}
	if s.eagerStubMemory != nil {
		s.eagerStubMemory.pruneThreads(maxThreadStates)
	}
}
