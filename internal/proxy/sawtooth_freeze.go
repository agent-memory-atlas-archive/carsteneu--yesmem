package proxy

// freezeStubsAndInjectBreakpoint stores the current req["messages"] as the
// frozen prefix for threadID and injects a cache breakpoint at the boundary.
//
// Embedded cache_control markers are stripped from the prefix BEFORE storing
// so that the stored frozen.Messages is byte-clean. This guarantees that
// subsequent RESTORE turns produce byte-identical outgoing prefix bytes,
// which is the precondition for Anthropic prompt cache hits across turns
// post-sawtooth.
//
// Pure data-plane: callers handle logging based on returned values.
//
//	pairRepairs        = orphaned tool_results removed before the store
//	frozenCount        = number of messages stored as frozen prefix
//	strippedBreakpoints = embedded cache_control entries removed pre-store
//	breakpointInjected  = whether a boundary breakpoint was added
func (s *Server) freezeStubsAndInjectBreakpoint(req map[string]any, threadID string, cutoff int, boundary any, tokenEstimate int, rawTokenEstimate int) (frozenCount int, strippedBreakpoints int, breakpointInjected bool, pairRepairs int) {
	finalMessages, _ := req["messages"].([]any)
	if len(finalMessages) == 0 {
		return 0, 0, false, 0
	}
	// YM-411: repair before storing. The pairing repair used to run only on the
	// outgoing request, after this same array had been frozen, so every later
	// request rebuilt its prefix from the unfixed copy and removed the same
	// orphan again (50 repairs at one unchanged boundary in the audit). Fixing it
	// here persists the repair in the stored bytes: one repair per boundary
	// instead of one per request. Synthesis stays out — see
	// validateToolPairsWith: an invented tool_result inside the stored prefix
	// would outlive the real one.
	if repaired, orphans := validateToolPairsWith(finalMessages, s.logger, false); orphans > 0 {
		finalMessages = repaired
		req["messages"] = repaired
		pairRepairs = orphans
		// The caller measured the prefix before this repair. Storing that number
		// with the shorter payload would overstate the frozen prefix and pull the
		// next prefix expiry forward, i.e. cost a cache miss for bytes that are no
		// longer there.
		tokenEstimate = s.countMessageTokens(finalMessages)
	}
	strippedBreakpoints = StripMessagesCacheControl(req, 0, len(finalMessages))
	finalMessages, _ = req["messages"].([]any)
	s.frozenStubs.Store(threadID, finalMessages, cutoff, boundary, tokenEstimate, rawTokenEstimate)
	breakpointInjected = InjectFrozenStubCacheBreakpoint(req, len(finalMessages))
	return len(finalMessages), strippedBreakpoints, breakpointInjected, pairRepairs
}

// snapshotFrozenPrefix refreshes the stored frozen prefix with the bytes the
// request currently carries for that range, and returns how many messages it
// covers (0 when nothing changed).
//
// Both request pipelines call it after their per-message annotation stage and
// before any stage that inserts turns at the head: such an insert shifts the
// frozen range to a higher offset, so slicing afterwards would store injected
// turns and break cache continuity. UpdateMessages refuses a length change and
// compares the prefix structurally, so an unchanged prefix costs one walk over
// the stored values and no daemon write.
//
// Embedded cache_control markers are stripped from the stored copy, matching the
// invariant freezeStubsAndInjectBreakpoint establishes: the stored prefix is
// byte-clean, and the pipeline re-injects exactly one breakpoint per request.
// The strip runs on a copy, so the outgoing request keeps its breakpoints. That
// path also runs on the OpenAI-parity pipeline, which has no restore-side strip.
func (s *Server) snapshotFrozenPrefix(req map[string]any, threadID string) int {
	if s.frozenStubs == nil || threadID == "" {
		return 0
	}
	frozenLen := s.frozenStubs.LengthFor(threadID)
	if frozenLen == 0 {
		return 0
	}
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) < frozenLen {
		return 0
	}
	snapshot := msgs[:frozenLen]
	if messagesHaveCacheControl(snapshot) {
		snapshot = deepCopyMessages(snapshot)
		if snapshot == nil {
			return 0
		}
		stripCacheControlFromMessages(snapshot, 0, len(snapshot))
	}
	if !s.frozenStubs.UpdateMessages(threadID, snapshot) {
		return 0
	}
	return frozenLen
}
