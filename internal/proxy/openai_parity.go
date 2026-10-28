package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

type openAIRequestContext struct {
	ReqIdx int
	// Project is the short display name; ProjectDir is the absolute path and
	// the only form safe to send to the daemon.
	Project         string
	ProjectDir      string
	ThreadID        string
	SessionID       string
	Fingerprint     string
	UserQuery       string
	UserAgent       string
	Model           string
	ToolUseIDs      []string
	Retry           bool
	EstimatedTokens int
	MessageCount    int
	Subagent        bool
	SawtoothCutoff  int    // raw index where frozen stubs end (= fresh tail start)
	SawtoothStubs   int    // number of frozen stubs in combined array
	RawMsgCount     int    // total messages before sawtooth compaction
	LoopWarning     string // stashed by loop detection, merged into TimestampMeta during entry assembly
	// ChannelID is the mailbox this request reads its channel messages under.
	// ThreadID stays the cache key; the two only differ for a client that sends
	// no session header and whose session was resolved from its process (codex),
	// because its mail is addressed to the backend session id, not to the
	// derived thread hash.
	ChannelID string
	// ThreadIsSession reports whether ThreadID names a session. It is false for
	// a client whose thread id was derived from message content, which is the
	// same string for every session of that client — such a key must not carry
	// per-session content.
	ThreadIsSession bool
	// Profile is the prompt profile this request is prepared for. It follows the
	// client, not the wire: codex and opencode both arrive over the OpenAI format.
	Profile models.PromptProfile
}

// promptProfile is the profile prompt flags are resolved for. A hand-assembled
// context without one keeps the historical codex profile.
func (c *openAIRequestContext) promptProfile() models.PromptProfile {
	if c.Profile == "" {
		return models.ProfileCodex
	}
	return c.Profile
}

// channelSession is the identity the channel inbox is read under and the one
// the injected reply target names.
func (c *openAIRequestContext) channelSession() string {
	if c.ChannelID != "" {
		return c.ChannelID
	}
	return c.ThreadID
}

func (s *Server) ensureOpenAIRuntimeState() {
	// Hand-assembled contexts build these lazily (NewServer always sets them), and
	// they must follow the measured cache lifetime, not the configured one.
	ttlName := s.effectiveCacheTTLName()
	s.mu.Lock()
	if s.annotations == nil {
		s.annotations = make(map[string]string)
	}
	// s.decayTrackers is lazily created by decayFor under decayMu; do not
	// initialize it here under s.mu (mismatched lock).
	// s.narratives is lazily created by getNarrative under narrativeMu; do not
	// initialize it here under s.mu (mismatched lock).
	if s.cacheGate == nil {
		s.cacheGate = NewCacheGate(cacheGapForTTL(ttlName))
	}
	if s.frozenStubs == nil {
		s.frozenStubs = NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL(ttlName))
	}
	if s.sawtoothTrigger == nil {
		s.sawtoothTrigger = NewSawtoothTrigger(cacheGapForTTL(ttlName), s.cfg.TokenThreshold, s.cfg.TokenMinimumThreshold)
	}
	if s.skillTracker == nil {
		s.skillTracker = newSkillHintTracker()
	}
	s.mu.Unlock()

	s.loopMu.Lock()
	if s.loopStates == nil {
		s.loopStates = make(map[string]*LoopState)
	}
	s.loopMu.Unlock()

	s.thinkMu.Lock()
	if s.thinkCounters == nil {
		s.thinkCounters = make(map[string]int)
	}
	s.thinkMu.Unlock()

	s.channelMu.Lock()
	if s.channelInjectCount == nil {
		s.channelInjectCount = make(map[string]int)
	}
	s.channelMu.Unlock()

	s.responseTsMu.Lock()
	if s.responseTimes == nil {
		s.responseTimes = make(map[string]time.Time)
	}
	s.responseTsMu.Unlock()

	s.lastInjectedIDsMu.Lock()
	if s.lastInjectedIDs == nil {
		s.lastInjectedIDs = make(map[string]map[int64]string)
	}
	if s.sessionInjectCounts == nil {
		s.sessionInjectCounts = make(map[string]map[int64]int)
	}
	if s.lastTurnInjected == nil {
		s.lastTurnInjected = make(map[string]map[int64]bool)
	}
	s.lastInjectedIDsMu.Unlock()
}

func (s *Server) prepareOpenAIRequestContext(req map[string]any, reqIdx int, headerSessionID, opencodeSessionID, userAgent string, h http.Header) openAIRequestContext {
	messages, _ := req["messages"].([]any)
	fp := requestFingerprint(messages)
	ctx := openAIRequestContext{
		ReqIdx:      reqIdx,
		Project:     extractProjectName(req),
		ProjectDir:  extractProjectPath(req),
		SessionID:   extractSessionID(req, headerSessionID, opencodeSessionID),
		Fingerprint: fp,
		UserQuery:   lastUserText(messages),
		UserAgent:   userAgent,
		ToolUseIDs:  extractToolUseIDs(messages),
		Subagent:    isOpenAIProxySubagent(h, messages, req),
	}
	// The client decides both the thread-key prefix and the prompt profile:
	// a codex request carries no session header and would otherwise be filed
	// under the opencode family along with its profile.
	prefix, profile := "opencode:", models.ProfileOpencode
	if opencodeSessionID == "" && isCodexClient(userAgent) {
		prefix, profile = "codex:", models.ProfileCodex
	}
	ctx.Profile = profile

	// The session value is client input and becomes the thread key, which is
	// printed in the "tid=" slot of every line: derived from its token form, the
	// same rule anthropicThread documents.
	if key := journalToken(opencodeSessionID); key != "" {
		ctx.ThreadID = prefix + key
		ctx.ThreadIsSession = true
		s.logger.Printf("%s %sopencode session mapped: oc:%s → %s%s",
			fmtReq(reqIdx, s.version), colorGreen, journalToken(opencodeSessionID), journalToken(ctx.ThreadID), colorReset)
	} else if key := journalToken(ctx.SessionID); key != "" {
		ctx.ThreadID = prefix + key
		ctx.ThreadIsSession = true
	} else {
		// Derived from message content: stable per client build, identical across
		// its sessions. A cache key, not an identity.
		ctx.ThreadID = prefix + DeriveThreadID(req)
	}

	ctx.Retry = s.isRetry(fp)
	return ctx
}

func (s *Server) runOpenAIParityPipeline(req map[string]any, ctx *openAIRequestContext) {
	if ctx == nil {
		return
	}

	model, _ := req["model"].(string)

	s.ensureOpenAIRuntimeState()

	s.sessionStartMu.Lock()
	if s.sessionStartTime.IsZero() {
		s.sessionStartTime = time.Now()
	}
	s.sessionStartMu.Unlock()

	s.refreshConfigOverrides(ctx.ThreadID)
	if s.sawtoothTrigger != nil && ctx.ThreadID != "" {
		s.sawtoothTrigger.ClearSentEstimate(ctx.ThreadID)
	}

	messages, _ := req["messages"].([]any)
	if len(messages) <= 1 && !strings.Contains(ctx.UserAgent, "opencode") {
		AppendSystemBlock(req, "yesmem-identity", "The first user message contains your briefing — written by yourself, from your memory. This is not an external document. This is you, remembering.")
	}

	// Resolve profile-aware prompt flags for the client behind this request.
	// Claude-only injectors (CLAUDE.md, REPL tools, Opus/Sonnet/Haiku refs) are gated out.
	pf := s.getPromptFlags(ctx.promptProfile())

	// prompt_ungate: strip CLAUDE.md subordination disclaimer (safe for all profiles).
	if pf.Ungate {
		StripCLAUDEMDDisclaimer(req)
	}

	// prompt_rewrite: strip + inject (agent-neutral, safe for all profiles)
	if pf.Rewrite {
		if !StripOutputEfficiency(req) {
			s.logRewriteMiss("StripOutputEfficiency", ctx.UserAgent)
		}
		if !StripToneBrevity(req) {
			s.logRewriteMiss("StripToneBrevity", ctx.UserAgent)
		}
		InjectAntDirectives(req)
	}
	// prompt_enhance: CLAUDE.md authority is Claude-only — skip InjectCLAUDEMDAuthority,
	// but persona tone is shared and safe for all profiles.
	if pf.Enhance {
		if raw, err := s.queryDaemon("get_persona", nil); err == nil {
			var persona map[string]any
			if json.Unmarshal(raw, &persona) == nil {
				if traits, ok := persona["traits"].(map[string]any); ok {
					if verbosity, ok := traits["verbosity"].(string); ok {
						InjectPersonaTone(req, verbosity)
					}
				}
			}
		}
	}

	// Shared discipline blocks (agent-neutral wording, safe for all profiles).
	if pf.OutputDiscipline {
		if strings.Contains(strings.ToLower(ctx.Model), "deepseek") {
			InjectDeepSeekOutputDiscipline(req)
		} else {
			InjectOutputDiscipline(req)
		}
	}
	if pf.CodingDiscipline {
		InjectCodingDiscipline(req)
	}
	if pf.Beweislast {
		InjectBeweislast(req)
	}
	if pf.Fable {
		InjectFable(req)
	}
	if pf.ScopeDiscipline {
		InjectScopeDiscipline(req)
	}
	if pf.ClarifyFirst {
		InjectClarifyFirst(req)
	}
	if pf.CodeToolsFirst {
		InjectCodeToolsFirst(req, extractWorkingDirectory(req))
	}
	if pf.WikiFirst {
		wd := extractWorkingDirectory(req)
		InjectWikiFirst(req, wd, s.wikiDir(wd))
	}
	// Claude-only blocks are gated via profile (Pf.Codex never has ToolPrefs/DelegationContract set):
	//   InjectDelegationContract → only in Claude profile (mentions Opus/Sonnet/Haiku)

	// Inject opencode capability catalog (active caps with execute_cap instructions)
	s.injectOpencodeCapabilitiesCatalog(req, ctx.ThreadID, daemonProject(ctx.Project, ctx.ProjectDir))

	messages, _ = req["messages"].([]any)
	overhead := s.measureOverhead(req)
	totalTokens := s.estimateTotalTokens(ctx.ThreadID, messages, overhead)
	ctx.EstimatedTokens = totalTokens
	ctx.MessageCount = len(messages)

	if !ctx.Retry && s.cfg.DataDir != "" {
		turnProject := daemonProject(ctx.Project, ctx.ProjectDir)
		if turnProject == "" {
			turnProject = "__global__"
		}
		go s.queryDaemon("increment_turn", map[string]any{"project": turnProject})
	}

	s.runOpenAIInlineReflection(ctx, messages)

	if ci := detectGitCommit(messages); ci != nil && s.cfg.DataDir != "" {
		workdir := extractWorkingDirectory(req)
		if s.logger != nil {
			s.logger.Printf("[req %d %s tid=%s] git commit detected: %s %s", ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), ci.Hash, ci.Message)
		}
		go s.queryDaemon("invalidate_on_commit", map[string]any{
			"hash": ci.Hash, "project": daemonProject(ctx.Project, ctx.ProjectDir), "workdir": workdir,
		})
	}

	if ctx.Subagent {
		finalMessages, _ := req["messages"].([]any)
		ctx.EstimatedTokens = s.estimateTotalTokens(ctx.ThreadID, finalMessages, s.measureOverhead(req))
		ctx.MessageCount = len(finalMessages)
		return
	}

	// Loop detection: check for repeating tool-call patterns and stash warning.
	// The warning is merged into the last user message's TimestampMeta during
	// annotateOpenAIMessageMetadata — cache-safe via freeze-once-replay.
	// Mirrors the placement in handleMessages: after the subagent decision, so a
	// subagent cannot raise a warning counter whose warning is never injected.
	if ctx.ThreadID != "" && !ctx.Retry && isFeatureEnabled(&s.cfg, model, "loop_warning") {
		s.loopMu.Lock()
		loopState := s.loopStates[ctx.ThreadID]
		if loopState == nil {
			loopState = &LoopState{}
			s.loopStates[ctx.ThreadID] = loopState
		}
		s.loopMu.Unlock()

		if warning, level := CheckLoopAndFormat(messages, loopState); warning != "" {
			ctx.LoopWarning = warning
			if s.logger != nil {
				s.logger.Printf("%s[req %d %s tid=%s] LOOP: level %d warning stashed (cached via TimestampMeta)%s", colorOrange, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), level, colorReset)
			}
		}
	}

	briefingText := ""
	// Briefing as system block, gated per-model via FeatureGates.
	if isFeatureEnabled(&s.cfg, model, "briefing") && len(messages) <= 6 {
		cwd := extractWorkingDirectory(req)
		if data := s.loadBriefing(ctx.Project, cwd, ctx.ThreadID); data.Text != "" {
			briefingText = data.Text
			AppendSystemBlock(req, "yesmem-briefing", data.Text)
			s.logger.Printf("[req %d] %sOpenAI pipeline: briefing injected (%db) project=%s%s",
				ctx.ReqIdx, colorBlue, len(data.Text), journalToken(ctx.Project), colorReset)

			if data.CodeMap != "" {
				AppendSystemBlock(req, "yesmem-codemap", data.CodeMap)
				s.logger.Printf("[req %d] %sOpenAI pipeline: codemap injected (%db) project=%s%s",
					ctx.ReqIdx, colorBlue, len(data.CodeMap), journalToken(ctx.Project), colorReset)
			}
		}
	}

	ctx.RawMsgCount = len(messages)

	if s.cfg.SawtoothEnabled && s.frozenStubs != nil && s.sawtoothTrigger != nil {
		frozen := s.frozenStubs.Get(ctx.ThreadID, messages)
		if frozen != nil {
			freshMessages := messages[frozen.Cutoff:]
			freshTokens := s.countMessageTokens(freshMessages)
			combinedTokens := frozen.Tokens + freshTokens + overhead
			if combinedTokens > s.effectiveTokenThreshold(ctx.ThreadID, model) {
				s.invalidateThreadCaches(ctx.ThreadID, ctx.Project, extractWorkingDirectory(req), model)
				frozen = nil
			} else {
				combined := make([]any, 0, len(frozen.Messages)+len(freshMessages))
				combined = append(combined, frozen.Messages...)
				combined = append(combined, freshMessages...)
				req["messages"] = combined
				s.sawtoothTrigger.SetSentEstimate(ctx.ThreadID, combinedTokens)
				ctx.SawtoothCutoff = frozen.Cutoff
				ctx.SawtoothStubs = len(frozen.Messages)
			}
		}
		if frozen == nil {
			triggerReason := s.shouldTriggerSawtooth(ctx.ThreadID, model, totalTokens)
			if triggerReason != TriggerNone {
				// Decay ages follow this thread's own requests, not the proxy-wide
				// counter (YM-275), the same rule the Anthropic path applies. The
				// clock advances only where the age is read: a request that never
				// stubs would otherwise occupy one of the bounded tracker slots.
				decayIdx := s.decayFor(ctx.ThreadID).Tick()
				s.setRawEstimate(ctx.ThreadID, totalTokens)
				_ = s.runStubCycle(messages, req, ctx.ReqIdx, decayIdx, ctx.Project, ctx.ProjectDir, ctx.ThreadID, overhead, totalTokens, ctx.UserQuery, ctx.Retry)
				finalMessages, _ := req["messages"].([]any)
				if len(finalMessages) > 0 {
					cutoff := len(messages)
					var boundaryMsg any
					if cutoff > 0 {
						boundaryMsg = messages[cutoff-1]
					}
					frozenCount, _, _, _ := s.freezeStubsAndInjectBreakpoint(req, ctx.ThreadID, cutoff, boundaryMsg, s.countMessageTokens(finalMessages), totalTokens)
					ctx.SawtoothCutoff = cutoff
					// The freeze drops orphaned tool_results before storing, so the
					// stub count comes from its result, not from the array captured
					// above: SawtoothStubs feeds the [msg:N] offset.
					ctx.SawtoothStubs = frozenCount
				}
			}
		}
	} else if s.shouldStub(totalTokens, model) {
		decayIdx := s.decayFor(ctx.ThreadID).Tick()
		s.setRawEstimate(ctx.ThreadID, totalTokens)
		s.runStubCycle(messages, req, ctx.ReqIdx, decayIdx, ctx.Project, ctx.ProjectDir, ctx.ThreadID, overhead, totalTokens, ctx.UserQuery, ctx.Retry)
	}

	messages, _ = req["messages"].([]any)

	assocContext := ""
	_ = messages // keep for when injections are re-enabled

	/*
		// DISABLED: cache-incompatible injections for DeepSeek/OpenAI path.
		// Re-enable when injection-state-per-message store is implemented.
		//
		// These inserts land in the middle of the message array, which invalidates
		// the cached prefix behind them; that is why they are off while the same
		// content is injected through the per-message store instead (TimestampMeta,
		// see annotateOpenAIMessageMetadata). The channel content this block used to
		// carry moved there in YM-59, which serves the opencode sessions on a
		// non-Anthropic provider — they key the pipeline by their own session id.
		// A codex request carries no session header, so it is keyed by a derived
		// thread hash instead and its mail (addressed to codex:<tid>) is not
		// matched; that gap is reported, not fixed here.

		if isFeatureEnabled(&s.cfg, model, "rules_reminder") {
			if rulesBlock := s.rulesInject(ctx.ThreadID, totalTokens, daemonProject(ctx.Project, ctx.ProjectDir)); rulesBlock != "" {
				req["messages"] = injectAssociativeContext(messages, s.formatRulesReminder(rulesBlock, daemonProject(ctx.Project, ctx.ProjectDir), true), s.cfg.SawtoothEnabled)
				messages, _ = req["messages"].([]any)
				s.logger.Printf("%s[req %d %s tid=%s] rules reminder injected%s", colorBlue, ctx.ReqIdx, ctx.Project, ctx.ThreadID, colorReset)
			}
		}

		planCallSeen := s.detectPlanToolCall(messages, ctx.ThreadID, totalTokens)
		if isFeatureEnabled(&s.cfg, model, "plan_checkpoint") {
			if checkpoint := s.planCheckpointInject(ctx.ThreadID, totalTokens); checkpoint != "" {
				req["messages"] = injectAssociativeContext(messages, checkpoint, s.cfg.SawtoothEnabled)
				messages, _ = req["messages"].([]any)
				s.logger.Printf("%s[req %d %s tid=%s] plan checkpoint injected%s", colorBlue, ctx.ReqIdx, ctx.Project, ctx.ThreadID, colorReset)
			}
		}
		s.syncPlanSnapshot(ctx.ThreadID, planCallSeen)

		if isFeatureEnabled(&s.cfg, model, "skill_eval") && ctx.ThreadID != "" && ctx.Project != "" && isUserInputTurn(messages) && s.skillTracker != nil {
			s.syncSkillActivations(messages, daemonProject(ctx.Project, ctx.ProjectDir), ctx.ThreadID)
			skillEval := buildSkillEvalBlock(s.cfg.SkillEvalInject)
			if skillEval != "" {
				req["messages"] = injectAssociativeContext(messages, skillEval, s.cfg.SawtoothEnabled)
			}
			messages, _ = req["messages"].([]any)
			s.logger.Printf("%s[req %d %s tid=%s] skill-eval injected%s", colorBlue, ctx.ReqIdx, ctx.Project, ctx.ThreadID, colorReset)
		}

		if isFeatureEnabled(&s.cfg, model, "think_reminder") && ctx.ThreadID != "" {
			// NOTE: buildThinkReminder accepts sessionID param (unused internally).
			// ctx.ThreadID is prefix + journalToken(ctx.SessionID), so the two are
			// the same value only for ids that are tokens already (YM-447).
			// ctx.SessionID stays the raw client value on purpose: the daemon and
			// the SESSION_ID hint key on what the client registered.
			thinkReminder := s.buildThinkReminder(ctx.ThreadID, ctx.SessionID, true)
			if thinkReminder != "" {
				req["messages"] = injectAssociativeContext(messages, thinkReminder, s.cfg.SawtoothEnabled)
				messages, _ = req["messages"].([]any)
			}

			dr := s.checkDialogMessages(ctx.ThreadID, daemonProject(ctx.Project, ctx.ProjectDir))
			if dr.Extra != "" {
				if ctx.SessionID != "" {
					dr.Extra = fmt.Sprintf("SESSION_ID: %s\n", ctx.SessionID) + dr.Extra
				}
				if len(messages) > 0 {
					lastMsg, _ := messages[len(messages)-1].(map[string]any)
					if lastMsg != nil && lastMsg["role"] == "user" {
						messages = append(messages, map[string]any{
							"role":    "assistant",
							"content": "\u200b",
						})
					}
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": "<channel source=\"yesmem-dialog\">\n" + dr.Extra + "\n</channel>",
					})
					req["messages"] = messages
					if dr.HasUnread && dr.SessionID != "" && s.shouldMarkChannelRead(dr) {
						s.markDialogRead(dr)
					}
				}
			}
		}
	*/

	s.annotateOpenAIMessageMetadata(req, ctx, messages, model, totalTokens)

	if !s.cfg.SawtoothEnabled && s.cacheGate != nil && s.cacheGate.ShouldCache() {
		if n := InjectCacheBreakpoints(req, s.logger); n > 0 && s.logger != nil {
			s.logger.Printf("[req %d %s] prompt cache: %d breakpoints injected", ctx.ReqIdx, journalToken(ctx.Project), n)
		}
	}
	if n := NormalizeCacheTTL(req, s.cfg.CacheTTL); n > 0 && s.logger != nil {
		s.logger.Printf("[req %d %s] cache TTL normalized: %d blocks", ctx.ReqIdx, journalToken(ctx.Project), n)
	}
	if n := EnforceCacheBreakpointLimit(req, maxCacheBreakpoints); n > 0 && s.logger != nil {
		s.logger.Printf("[req %d %s] prompt cache: trimmed %d surplus breakpoints", ctx.ReqIdx, journalToken(ctx.Project), n)
	}

	if msgs, ok := req["messages"].([]any); ok {
		if fixed, orphans := validateToolPairs(msgs, s.logger); orphans > 0 {
			req["messages"] = fixed
			if s.logger != nil {
				s.logger.Printf("%s[req %d %s tid=%s] validate: repaired %d tool pairing issue(s)%s", colorOrange, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), orphans, colorReset)
			}
		}
	}

	s.trackOpenAIInjectedLearnings(ctx.ThreadID, briefingText, assocContext)

	finalMessages, _ := req["messages"].([]any)
	ctx.EstimatedTokens = s.measureOverhead(req) + s.countMessageTokens(finalMessages)
}

// isOpenAIProxySubagent is the OpenAI wire's entry into the shared verdict. It
// exists for the protocol fact alone: these shapes carry no thinking field, so
// the thinking fallback cannot speak here (see subagentVerdict). Everything else
// is the same predicate, including the header set, the marker precedence and the
// empty-history rule, so a client with a declaration is classified the same on
// both endpoints.
func isOpenAIProxySubagent(h http.Header, messages []any, req map[string]any) bool {
	return subagentVerdict(h, messages, req, false)
}

func (s *Server) runOpenAIInlineReflection(ctx *openAIRequestContext, messages []any) {
	if ctx == nil || ctx.ThreadID == "" || ctx.Retry || s.cfg.DataDir == "" {
		return
	}

	s.lastInjectedIDsMu.Lock()
	prevInjected := s.lastInjectedIDs[ctx.ThreadID]
	s.lastInjectedIDsMu.Unlock()
	if len(prevInjected) == 0 {
		return
	}

	signals := s.scanInlineSignals(messages)

	var confirmedUsed []int64
	for _, id := range signals.UsedIDs {
		if _, ok := prevInjected[id]; ok {
			confirmedUsed = append(confirmedUsed, id)
		}
	}
	if len(confirmedUsed) > 0 {
		idsAny := make([]any, len(confirmedUsed))
		for i, id := range confirmedUsed {
			idsAny[i] = id
		}
		go s.queryDaemon("increment_use", map[string]any{"ids": idsAny})
	}

	usedSet := make(map[int64]bool, len(confirmedUsed))
	for _, id := range confirmedUsed {
		usedSet[id] = true
	}
	var noiseIDs []int64
	for id, source := range prevInjected {
		if usedSet[id] {
			continue
		}
		if source == "associative" || source == "fresh" {
			noiseIDs = append(noiseIDs, id)
		}
	}
	// Only a finished turn proves an injected learning went unused: the scan must
	// have read an answer, and the trailing user message must not be a tool result.
	if signals.Scanned && !isToolContinuation(messages) && len(noiseIDs) > 0 {
		idsAny := make([]any, len(noiseIDs))
		for i, id := range noiseIDs {
			idsAny[i] = id
		}
		go s.queryDaemon("increment_noise", map[string]any{"ids": idsAny})
	}

	if signals.ToolErrorText != "" && len(confirmedUsed) > 0 {
		if freshIDs := s.freshFailIDs(ctx.ThreadID, confirmedUsed, signals.ToolErrorText); len(freshIDs) > 0 {
			failAny := make([]any, len(freshIDs))
			for i, id := range freshIDs {
				failAny[i] = id
			}
			go s.queryDaemon("increment_fail", map[string]any{"ids": failAny})
			if s.logger != nil {
				s.logger.Printf("[inline-reflection] error feedback: %d used learnings → fail_count++", len(freshIDs))
			}
		}
	}

	for _, topic := range signals.GapTopics {
		go s.queryDaemon("track_gap", map[string]any{"topic": topic, "project": daemonProject(ctx.Project, ctx.ProjectDir)})
	}
	for _, desc := range signals.Contradictions {
		go s.queryDaemon("flag_contradiction", map[string]any{"description": desc, "project": daemonProject(ctx.Project, ctx.ProjectDir), "thread_id": ctx.ThreadID})
	}

	if s.logger != nil && (len(confirmedUsed) > 0 || len(signals.GapTopics) > 0 || len(signals.Contradictions) > 0 || len(noiseIDs) > 0) {
		s.logger.Printf("[req %d %s tid=%s] inline-reflection: injected=%d used=%d noise=%d gaps=%d contradictions=%d",
			ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), len(prevInjected), len(confirmedUsed), len(noiseIDs), len(signals.GapTopics), len(signals.Contradictions))
	}
}

func (s *Server) trackOpenAIInjectedLearnings(threadID, briefingText, assocContext string) {
	if threadID == "" {
		return
	}

	currentIDs := make(map[int64]string)
	for _, m := range idPattern.FindAllStringSubmatch(briefingText, -1) {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			currentIDs[id] = "briefing"
		}
	}
	for _, m := range idPattern.FindAllStringSubmatch(assocContext, -1) {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			currentIDs[id] = "associative"
		}
	}
	if len(currentIDs) == 0 {
		return
	}

	s.lastInjectedIDsMu.Lock()
	existing := s.lastInjectedIDs[threadID]
	if existing == nil {
		existing = make(map[int64]string)
	}
	for id, source := range currentIDs {
		existing[id] = source
	}
	s.lastInjectedIDs[threadID] = existing
	s.lastInjectedIDsMu.Unlock()
}

func (s *Server) annotateOpenAIMessageMetadata(req map[string]any, ctx *openAIRequestContext, messages []any, model string, totalTokens int) {
	threadID := ctx.ThreadID
	if threadID == "" {
		return
	}
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}

	if s.timestampStore == nil {
		return
	}

	if ctx.ThreadIsSession {
		s.timestampStore.Load(threadID)
	} else {
		// A derived key may still hold another session's frozen content from
		// before the write gate existed (or from a build that had no gate).
		s.timestampStore.LoadScrubbed(threadID)
	}

	msgOffset := ctx.SawtoothCutoff - ctx.SawtoothStubs

	s.logger.Printf("[req %d %s] timestamps: rawTotal=%d, postSawtooth=%d, cutoff=%d, stubs=%d, offset=%d",
		ctx.ReqIdx, journalToken(ctx.Project), ctx.RawMsgCount, len(msgs), ctx.SawtoothCutoff, ctx.SawtoothStubs, msgOffset)

	// Pre-store assistant timestamp from previous response
	s.responseTsMu.RLock()
	respTime, hasRespTime := s.responseTimes[threadID]
	s.responseTsMu.RUnlock()
	if hasRespTime {
		respStr := shortWeekday(respTime.Weekday()) + " " + respTime.Format("2006-01-02 15:04:05")
		for j := len(msgs) - 2; j >= 0; j-- {
			prevMsg, _ := msgs[j].(map[string]any)
			if prevMsg == nil || prevMsg["role"] != "assistant" {
				continue
			}
			if !msgHasTextBlock(prevMsg) {
				continue
			}
			prevMsgN := msgOffset + j + 1
			if _, exists := s.timestampStore.Get(threadID, prevMsgN); !exists {
				s.timestampStore.Store(threadID, prevMsgN, &TimestampMeta{Timestamp: respStr})
			}
			break
		}
	}

	// Inject [msg:N] (+ timestamp/delta if stored) on all previous messages
	if isFeatureEnabled(&s.cfg, model, "timestamps") {
		if n := InjectTimestamps(s.timestampStore, threadID, msgs, len(msgs)-1, msgOffset, ctx.SawtoothStubs); n > 0 {
			s.logger.Printf("[req %d %s] timestamps: injected %d", ctx.ReqIdx, journalToken(ctx.Project), n)
		}
	}

	// Skill activation tracking runs regardless of the skill_eval gate: the
	// tracker is what re-injects skill content after a context collapse
	// (buildSkillBlocksForThread), so it must not depend on whether the
	// evaluation instruction is enabled for this model. Placement mirrors the
	// Anthropic path. The guard is ThreadIsSession, the same field the channel
	// block below reads (there only to choose where the block rides): a codex
	// thread id is derived from message content, so tracking under it would
	// replay one session's skills into another. The Anthropic path applies the
	// same condition (anthropicThread reports whether the key names a session),
	// and detectSkillActivations reads both wire shapes, so the two paths track
	// the same set of activations.
	if ctx.ThreadIsSession && ctx.ThreadID != "" && ctx.Project != "" && isUserInputTurn(messages) && s.skillTracker != nil {
		s.syncSkillActivations(messages, daemonProject(ctx.Project, ctx.ProjectDir), ctx.ThreadID)
	}

	// Current (last) user message: full timestamp + delta + msg:N from raw count
	lastMsg, _ := msgs[len(msgs)-1].(map[string]any)
	if lastMsg != nil {
		role, _ := lastMsg["role"].(string)
		if role == "user" || role == "tool" {
			if !msgHasMetaPrefix(lastMsg) {
				now := time.Now()
				nowStr := shortWeekday(now.Weekday()) + " " + now.Format("2006-01-02 15:04:05")
				entry := &TimestampMeta{Timestamp: nowStr}
				if hasRespTime {
					entry.Delta = formatDelta(now.Sub(respTime))
				}
				// Stable injects frozen once per msg:N (idempotent via TimestampStore).
				//
				// The per-session part of them is frozen ONLY under a session-keyed
				// thread id: a derived id is the same string for every session of that
				// client, so a frozen rules reminder, associative context, plan
				// checkpoint, docs hint or loop warning would be replayed into a
				// session it never belonged to. On a derived key the same content
				// rides the tail message of this request instead (see meta below),
				// which keeps the feature without storing it per thread.
				perSession := ctx.ThreadIsSession
				var tail []string
				if isFeatureEnabled(&s.cfg, model, "think_reminder") && ctx.ThreadID != "" && lastUserTextLen(messages) >= resolveThinkReminderMinChars(&s.cfg, model) {
					if tr := s.buildThinkReminder(ctx.ThreadID, ctx.SessionID, true); tr != "" {
						if perSession {
							entry.ThinkReminder = tr
						} else {
							tail = append(tail, "[think-reminder] "+tr)
						}
					}
				}
				if isFeatureEnabled(&s.cfg, model, "skill_eval") && ctx.ThreadID != "" && ctx.Project != "" && isUserInputTurn(messages) && s.skillTracker != nil {
					entry.SkillEval = buildSkillEvalBlock(s.cfg.SkillEvalInject)
				}
				if isFeatureEnabled(&s.cfg, model, "rules_reminder") {
					if rulesBlock := s.rulesInject(ctx.ThreadID, totalTokens, daemonProject(ctx.Project, ctx.ProjectDir)); rulesBlock != "" {
						rules := s.formatRulesReminder(rulesBlock, daemonProject(ctx.Project, ctx.ProjectDir), true)
						if perSession {
							entry.Rules = rules
						} else {
							tail = append(tail, "[rules] "+rules)
						}
					}
				}
				if isFeatureEnabled(&s.cfg, model, "assoc_context") && ctx.UserQuery != "" {
					if ac := s.findAssociativeContextFor(ctx.UserQuery, daemonProject(ctx.Project, ctx.ProjectDir), ctx.ThreadID, messages); ac != "" {
						if perSession {
							entry.AssocContext = ac
						} else {
							tail = append(tail, "[assoc-context] "+ac)
						}
					}
				}
				if ctx.LoopWarning != "" {
					if perSession {
						entry.LoopWarning = ctx.LoopWarning
					} else {
						tail = append(tail, "[loop-warning] "+ctx.LoopWarning)
					}
				}
				// Plan checkpoint: detect plan tool calls (resets counter), inject reminder if interval fires.
				// The sidebar snapshot is synced regardless of the gate: it reads a different channel.
				planCallSeen := s.detectPlanToolCall(messages, ctx.ThreadID, totalTokens)
				if isFeatureEnabled(&s.cfg, model, "plan_checkpoint") {
					if checkpoint := s.planCheckpointInject(ctx.ThreadID, totalTokens); checkpoint != "" {
						if perSession {
							entry.PlanCheckpoint = checkpoint
							if s.logger != nil {
								s.logger.Printf("%s[req %d %s tid=%s] plan checkpoint stashed (cached via TimestampMeta)%s", colorBlue, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), colorReset)
							}
						} else {
							tail = append(tail, "[plan-checkpoint] "+checkpoint)
						}
					}
				}
				s.syncPlanSnapshot(ctx.ThreadID, planCallSeen)

				// Plan auto-activation: a read plan file without an active plan.
				// The Anthropic path appends a system block here; this path freezes
				// the nudge into the message instead, or the insert would invalidate
				// the cached prefix behind it. A silent daemon is not "no active
				// plan" — the frozen text would repeat for the rest of the thread.
				planNudge := ""
				if planFile := detectPlanFileRead(messages); planFile != "" {
					if activePlan, _, ok := s.getActivePlan(ctx.ThreadID); ok && shouldNudgePlan(planFile, activePlan) {
						name := planNudgeName(planFile)
						planNudge = planNudgeText(name)
						if s.logger != nil {
							s.logger.Printf("%s[req %d %s tid=%q] plan nudge for %s (session-keyed=%t)%s", colorBlue, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), name, ctx.ThreadIsSession, colorReset)
						}
					}
				}
				// Freeze only when the thread id names a session. A codex thread id
				// is derived from message content, so every codex session shares it
				// and a frozen nudge would be replayed into a session that never read
				// the file — the same reason the channel block below is not frozen
				// there. Those requests carry the nudge on the tail message instead.
				freezePlanNudge := ctx.ThreadIsSession
				if freezePlanNudge && planNudge != "" {
					entry.PlanNudge = planNudge
				}

				// Docs hint: inject docs-available reminder if interval fires
				if docsHint := s.docsHintInject(ctx.ThreadID, totalTokens, daemonProject(ctx.Project, ctx.ProjectDir)); docsHint != "" {
					if perSession {
						entry.DocsHint = docsHint
						if s.logger != nil {
							s.logger.Printf("%s[req %d %s tid=%s] docs hint stashed (cached via TimestampMeta)%s", colorBlue, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), colorReset)
						}
					} else {
						tail = append(tail, "[docs-hint] "+docsHint)
					}
				}

				// Channel messages and broadcasts this session has not seen. This
				// is the store the disabled mid-array block was waiting for: the
				// content is frozen into the message once and replayed verbatim,
				// so the cached prefix of everything after it stays valid (YM-59).
				//
				// Read and mark are two calls, and this reader does not claim a
				// row: a second consumer that selects between them can carry a
				// copy as well. That window is argued on the sending side
				// (send_opencode.go:198-240): the claim there closes it for the
				// opencode push, whose rows this reader competes for. A codex
				// address has no push at all (sendViaOpencodeAPI refuses every
				// non-opencode address), so no second consumer exists for it.
				// The mark itself is throttled to the second injection
				// (shouldMarkChannelRead), so the same mail can ride one more
				// turn after the first — the same as on the Anthropic path.
				// Rows the heartbeat only pinged (read = 1, delivered = 0) stay in
				// the selection on purpose: that is mail nobody has shown yet.
				//
				// The address is the one the message was stored under — for codex
				// its backend session id, for the header-carrying harnesses their
				// own id (see openAIRequestContext.channelSession).
				channelID := ctx.channelSession()
				channelBlock := ""
				if ch := s.loadChannelContent(channelID, daemonProject(ctx.Project, ctx.ProjectDir)); ch.Extra != "" {
					channelBlock = "SESSION_ID: " + channelID + "\n" + ch.Extra
					if ch.HasUnread && ch.SessionID != "" && s.shouldMarkChannelRead(ch) {
						s.markDialogRead(ch)
					}
					if s.logger != nil {
						s.logger.Printf("%s[req %d %s tid=%s] channel content stashed (cached via TimestampMeta)%s", colorBlue, ctx.ReqIdx, journalToken(ctx.Project), journalToken(ctx.ThreadID), colorReset)
					}
				}
				// Freeze the block only when the thread id names a session. A
				// codex thread id is derived from message content (DeriveThreadID),
				// so every codex session shares it and a frozen mailbox would be
				// replayed into another session's prompt from the per-message
				// store — including its SESSION_ID line. There the block
				// rides the tail message of this request instead.
				freezeChannel := ctx.ThreadIsSession
				if freezeChannel {
					entry.Channel = channelBlock
				}

				s.timestampStore.Store(threadID, ctx.RawMsgCount, entry)
				meta := BuildMeta(ctx.RawMsgCount, entry)
				if !freezeChannel && channelBlock != "" {
					meta += "\n[channel] " + channelBlock
				}
				if planNudge != "" && !freezePlanNudge {
					meta += "\n[plan-nudge] " + planNudge
				}
				if len(tail) > 0 {
					// The tail block rides the last message of this request. It is
					// never stored in the entry, so the next request decides anew
					// whether the interval fired. It does reach the frozen-prefix
					// snapshot below, which is the frozen message set itself
					// (UpdateMessages → Get → frozen.Messages). That a replayed tail
					// of a derived thread does not reach another session's prompt
					// rests on two guards, not on the snapshot being harmless: a
					// message inside the frozen range keeps its meta prefix, and
					// msgHasMetaPrefix skips the rebuild, so the block is written
					// once, into the request that decided it.
					meta += "\n" + strings.Join(tail, "\n")
				}
				if isFeatureEnabled(&s.cfg, model, "timestamps") || entry.LoopWarning != "" || entry.PlanCheckpoint != "" || entry.PlanNudge != "" || planNudge != "" || entry.DocsHint != "" || entry.Channel != "" || channelBlock != "" || len(tail) > 0 {
					prependMeta(lastMsg, meta)
				}
			}
		}
	}

	// Last assistant message: timestamp from stored response time
	if hasRespTime {
		for i := len(msgs) - 1; i >= 0; i-- {
			msg, _ := msgs[i].(map[string]any)
			if msg != nil && msg["role"] == "assistant" && msgHasTextBlock(msg) {
				if !msgHasMetaPrefix(msg) {
					prependMeta(msg, fmt.Sprintf("[%s] [msg:%d]", respTime.Format("2006-01-02 15:04:05"), msgOffset+i+1))
				}
				break
			}
		}
	}

	s.timestampStore.Persist(threadID)

	// Mirror the Anthropic path: the annotation pass can touch a message inside
	// the frozen range (the newest assistant turn lies there whenever the tail
	// holds only user messages), so the stored prefix must follow the wire bytes.
	if n := s.snapshotFrozenPrefix(req, threadID); n > 0 {
		s.logger.Printf("[req %d %s tid=%s] FROZEN-SNAPSHOT: refreshed %d pre-inject messages", ctx.ReqIdx, journalToken(ctx.Project), journalToken(threadID), n)
	}
}

// extractModelFromBody returns the "model" field from a JSON request body.
func extractModelFromBody(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &req)
	return req.Model
}

// stripProviderPrefix returns the model name with any "providerID/" prefix
// removed. opencode uses "providerID/modelID" internally but upstream APIs
// (DeepSeek, OpenAI, Anthropic, opencode.ai/zen, etc.) expect bare model
// names without the prefix. The heuristic is: any "/" in the model string is
// treated as the opencode prefix separator (last "/" wins). Real upstream
// model IDs do not contain "/", so this is safe for current providers; if a
// future provider uses paths in model IDs, gate stripping behind an allowlist.
func stripProviderPrefix(model string) string {
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

// rewriteModelInBody returns a copy of body with the "model" field replaced.
// Uses json.Decoder.UseNumber to preserve integer precision for non-model
// fields (e.g. seed values > 2^53). Falls back to returning body unchanged
// on any parse error — we never corrupt the request just because we couldn't
// rewrite it.
func rewriteModelInBody(body []byte, newModel string) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return body
	}
	req["model"] = newModel
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// stripProviderPrefixFromBody returns body with the model's provider prefix
// removed. If the body has no model field or the model has no "/", the
// original body is returned unchanged.
func stripProviderPrefixFromBody(body []byte) []byte {
	model := extractModelFromBody(body)
	stripped := stripProviderPrefix(model)
	if stripped == model {
		return body
	}
	return rewriteModelInBody(body, stripped)
}

// upstreamErrForLog keeps the cause of a failed request but drops the query:
// the URL is assembled from the client's own request, and a key passed there
// would otherwise reach the journal.
func upstreamErrForLog(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if u, perr := url.Parse(uerr.URL); perr == nil {
			u.RawQuery = ""
			return oneLineForJournal(fmt.Sprintf("%s %s: %v", uerr.Op, u.String(), uerr.Err))
		}
	}
	return oneLineForJournal(err.Error())
}

func (s *Server) forwardOpenAIWithTracking(w http.ResponseWriter, origReq *http.Request, body []byte, reqIdx int, toolUseIDs []string, project string, threadID string, estimatedTokens, msgCount int, isSubagent bool, parser openAIUsageParser) {
	// Debug: dump forwarded request body for inspection (enable with YESMEM_PROXY_DEBUG=1)
	if p := writeDebugDump(s.cfg.DataDir, threadID, dumpKindBody, reqIdx, body); p != "" {
		s.logger.Printf("[req %d] body-dump %dk to %s", reqIdx, len(body)/1024, p)
	}

	model := extractModelFromBody(body)
	targetURL := s.resolveOpenAITarget(model)
	if targetURL == "" {
		targetURL = "https://api.openai.com"
	}

	// Strip provider prefix from model name. opencode sends "providerID/modelID"
	// but upstream APIs expect bare model names. resolveOpenAITarget above keeps
	// the prefixed name for routing; the rewrite below only affects the forwarded
	// JSON body. Also update model so tracking reflects what the upstream sees.
	if stripped := stripProviderPrefix(model); stripped != model {
		body = rewriteModelInBody(body, stripped)
		model = stripped
	}
	// When the upstream target has its own path component (e.g. https://api.z.ai/api/paas/v4),
	// strip /v1 from the request URI to avoid double path segments (v4/v1/chat/completions).
	// For plain host targets (e.g. https://api.deepseek.com), /v1 is correct.
	requestURI := origReq.URL.RequestURI()
	if idx := strings.Index(targetURL, "://"); idx >= 0 && strings.Contains(targetURL[idx+3:], "/") {
		requestURI = strings.TrimPrefix(requestURI, "/v1")
	}

	proxyReq, err := http.NewRequestWithContext(origReq.Context(), origReq.Method, targetURL+requestURI, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "create request: "+err.Error(), http.StatusBadGateway)
		return
	}

	for key, vals := range origReq.Header {
		for _, v := range vals {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("Content-Length", strconv.Itoa(len(body)))
	proxyReq.Header.Del("Connection")
	proxyReq.Header.Del("Accept-Encoding")

	resp, err := s.httpClient.Do(proxyReq)
	if err != nil {
		// The client answer is not the diagnostic proxy_health reads: without a
		// journal line, every provider failure on this path (deepseek, z.ai, glm)
		// stays invisible in the health report. An abandoned request is the one
		// exception: the caller went away, the provider did not fail, and the
		// health check would count it as an API error.
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("%supstream error: %s%s", colorRed, upstreamErrForLog(err), colorReset)
		}
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody := resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gzReader, gzErr := gzip.NewReader(resp.Body)
		if gzErr == nil {
			responseBody = gzReader
			defer gzReader.Close()
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
		}
	}

	// Parse rate-limit headers before forwarding to client
	rlInfo := ParseRateLimitHeaders(resp.Header)
	var rlJSON string
	if rlInfo != nil {
		if b, err := json.Marshal(rlInfo); err == nil {
			rlJSON = string(b)
		}
	}

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	isSub, parentThread := s.subagentStreamInfo(origReq, body)

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		// Non-SSE: notify stream start
		if threadID != "" {
			go s.trackStreamState(threadID, true, 0, isSub, project, parentThread)
		}

		bodyBytes, readErr := io.ReadAll(responseBody)
		if readErr != nil {
			// A body that breaks off is a failed call like any other. The wording
			// is the marker proxy_health reads: "upstream error:" counts it as a
			// provider failure, while a bare "read error:" with an i/o timeout
			// lands in the daemon-timeout bucket.
			s.logger.Printf("%supstream error: read from upstream: %v%s", colorRed, readErr, colorReset)
			if threadID != "" {
				go s.trackStreamState(threadID, false, 0, isSub, project, parentThread)
			}
			return
		}
		_, _ = w.Write(bodyBytes)

		// Same line shape as the Anthropic forwarder, so proxy_health counts an
		// upstream status on either path. An empty body is logged too: the status
		// alone already says the call failed.
		if resp.StatusCode != 200 {
			s.logger.Printf("%s[req %d tid=%s] upstream %d: %s%s", colorRed, reqIdx, journalToken(threadID), resp.StatusCode, upstreamBodyForLog(bodyBytes), colorReset)
		}

		if threadID != "" {
			go s.trackStreamState(threadID, false, int64(len(bodyBytes)), isSub, project, parentThread)
		}

		s.trackOpenAINonStreamingUsage(reqIdx, bodyBytes, threadID, project, estimatedTokens, msgCount)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	reader := bufio.NewReaderSize(responseBody, 8192)
	usage := &UsageTracker{}
	var textAccum strings.Builder
	var textDone bool

	// Stream tracking for SSE path
	var streamStarted bool
	var totalClientBytes int64

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmedLine := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmedLine, []byte("data: ")) {
				// Notify stream start on first SSE event
				if !streamStarted && threadID != "" {
					streamStarted = true
					go s.trackStreamState(threadID, true, 0, isSub, project, parentThread)
				}

				data := bytes.TrimSpace(bytes.TrimPrefix(trimmedLine, []byte("data: ")))
				if parser != nil {
					parser.ParseUsage(usage, data)
					parser.ParseAnnotation(data, &textAccum, &textDone, 120)
				}
			}
			if _, writeErr := w.Write(line); writeErr != nil {
				if threadID != "" {
					go s.trackStreamState(threadID, false, totalClientBytes, isSub, project, parentThread)
				}
				return
			}
			totalClientBytes += int64(len(line))
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	// Notify daemon that stream ended
	if threadID != "" {
		go s.trackStreamState(threadID, false, totalClientBytes, isSub, project, parentThread)
	}

	var fwdModel struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &fwdModel)
	s.finalizeOpenAIUsage(reqIdx, threadID, project, estimatedTokens, msgCount, usage, fwdModel.Model, rlJSON)

	// Fire forked agents (async, fire-and-forget) — same logic as proxy_forward.go.
	// A subagent request never reaches the fork state: it shares the parent's
	// thread ID, so its tokens must not move the parent's baseline and its
	// responses must not make the parent look cache-proven. isSub (stream
	// attribution) knows fewer markers than ctx.Subagent, so the broader verdict
	// is the one that counts here. Accepted consequence: a request that the
	// pipeline already handles as a subagent (ctx.Subagent, e.g. a Haiku model)
	// does not fork either, which matches the rest of the pipeline (see the
	// early return in runOpenAIParityPipeline).
	if !isSubagent && usage.Complete && usage.CacheReadInputTokens > 0 && !s.forkState.IsDisabled(threadID) && isRealUserSession(threadID) {
		totalTokens := usage.TotalInputTokens()
		cacheRatio := 0.0
		if totalTokens > 0 {
			cacheRatio = float64(usage.CacheReadInputTokens) / float64(totalTokens)
		}
		hasCache := cacheRatio > 0.9
		cacheProven := s.forkState.IsCacheProven(threadID)
		if hasCache {
			s.forkState.MarkCacheWarm(threadID)
		}
		if hasCache && cacheProven && s.forkState.ShouldFork(threadID, totalTokens, hasCache, isSub) {
			// The fork keeps the client's wire family: chat-completions bodies go
			// out as they are, the Responses-shaped body of the /v1/responses
			// adapter is translated to chat completions first. Cloning the
			// Anthropic intermediate instead would send the body of one family to
			// the endpoint and credentials of another (YM-262). Both the clone and
			// the translation run in the goroutine: the client already has its
			// response.
			forkCtx := ForkContext{
				AssistantResponse: textAccum.String(),
				ThreadID:          threadID,
				Project:           project,
				ReqIdx:            reqIdx,
				TokensUsed:        totalTokens,
				CacheReadTokens:   usage.CacheReadInputTokens,
				MessageCount:      msgCount,
				LastExtractedIdx:  reqIdx - 2,
				SessionID:         threadID,
				AuthHeader:        origReq.Header,
				SourceTarget:      targetURL,
			}
			go func() {
				forkBody, forkFormat, err := forkCloneFor(body)
				if err != nil {
					s.logger.Printf("%s[req %d %s] fork: no cloneable body: %v%s",
						colorOrange, reqIdx, journalToken(s.version), err, colorReset)
					// ShouldFork marked this thread as pending; without a release
					// no later request would ever fork again (fork_state.go).
					s.forkState.RecordFailure(threadID)
					return
				}
				forkCtx.OriginalBody, forkCtx.SourceFormat = forkBody, forkFormat
				s.fireForkedAgents(forkCtx, s.forkConfigs)
			}()
		}
	}

	// NOTE: No keepalive reset here — OpenAI-format body would be rejected by Anthropic API.
	// TTL detection is handled in finalizeOpenAIUsage via s.recordCacheResponse, which
	// also moves the cache-lifetime derivatives to the detected result.

	if usage.Complete && textAccum.Len() > 0 && len(toolUseIDs) > 0 {
		annotation := strings.TrimSpace(textAccum.String())
		if len([]rune(annotation)) > 120 {
			annotation = string([]rune(annotation)[:120])
		}
		s.mu.Lock()
		if len(s.annotations) > maxAnnotations {
			s.evictOldAnnotations()
		}
		for _, id := range toolUseIDs {
			if _, exists := s.annotations[id]; !exists {
				s.annotations[id] = annotation
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) trackOpenAINonStreamingUsage(reqIdx int, body []byte, threadID string, project string, estimatedTokens, msgCount int) {
	var parsed struct {
		Model string         `json:"model"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Usage == nil {
		return
	}

	u := &UsageTracker{Complete: true}
	var hasDeepSeekCache bool
	if v, ok := parsed.Usage["prompt_tokens"].(float64); ok {
		u.InputTokens = int(v)
	}
	if v, ok := parsed.Usage["completion_tokens"].(float64); ok {
		u.OutputTokens = int(v)
	}
	if v, ok := parsed.Usage["prompt_cache_hit_tokens"].(float64); ok {
		u.CacheReadInputTokens = int(v)
		hasDeepSeekCache = true
	}
	if v, ok := parsed.Usage["prompt_cache_miss_tokens"].(float64); ok {
		u.CacheMissTokens = int(v)
	}
	// When cache breakdown is available, use miss as uncached input
	// so TotalInputTokens = miss + hit = prompt_tokens (no double-counting).
	if hasDeepSeekCache {
		u.InputTokens = u.CacheMissTokens
	}
	if u.InputTokens == 0 {
		if v, ok := parsed.Usage["input_tokens"].(float64); ok {
			u.InputTokens = int(v)
		}
	}
	if u.OutputTokens == 0 {
		if v, ok := parsed.Usage["output_tokens"].(float64); ok {
			u.OutputTokens = int(v)
		}
	}
	if u.InputTokens == 0 {
		if total, ok := parsed.Usage["total_tokens"].(float64); ok && u.OutputTokens > 0 {
			u.InputTokens = int(total) - u.OutputTokens
		}
	}
	if details, ok := parsed.Usage["input_tokens_details"].(map[string]any); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			u.CacheReadInputTokens = int(v)
		}
	}
	s.finalizeOpenAIUsage(reqIdx, threadID, project, estimatedTokens, msgCount, u, parsed.Model, "")
}

func (s *Server) finalizeOpenAIUsage(reqIdx int, threadID string, project string, estimatedTokens, msgCount int, usage *UsageTracker, model string, rlJSON string) {
	if usage == nil || (usage.TotalInputTokens() == 0 && usage.OutputTokens == 0) {
		return
	}

	if reqIdx > 0 {
		s.logger.Printf("%s %s", fmtReq(reqIdx, s.version), usage.LogLine(reqIdx, 0, estimatedTokens, threadID))
	}
	if s.cfg.SawtoothEnabled && threadID != "" && s.sawtoothTrigger != nil {
		s.sawtoothTrigger.UpdateAfterResponse(threadID, usage.TotalInputTokens(), msgCount)
	}

	if threadID != "" && s.cfg.DataDir != "" {
		go s.queryDaemon("_track_usage", map[string]any{
			"thread_id":          threadID,
			"project":            project,
			"input_tokens":       usage.TotalInputTokens(),
			"output_tokens":      usage.OutputTokens,
			"cache_read_tokens":  usage.CacheReadInputTokens,
			"cache_write_tokens": usage.CacheCreationInputTokens,
		})
	}
	// Persist rate-limit snapshot (independent of threadID)
	if rlJSON != "" && s.cfg.DataDir != "" {
		go s.queryDaemon("_persist_rate_limits", map[string]any{"rate_limits": rlJSON})
	}

	if s.cacheStatusWriter != nil {
		s.cacheStatusWriter.Update(time.Now(), usage.TotalInputTokens(), usage.CacheReadInputTokens, usage.CacheCreationInputTokens, threadID)
		s.cacheStatusWriter.UpdateThresholdForThread(threadID, s.effectiveTokenThreshold(threadID, model))
		if raw := s.getRawEstimate(threadID); raw > 0 {
			s.cacheStatusWriter.UpdateRawForThread(threadID, raw)
		}
	}

	// Feed TTL detector with cache hit/miss signal
	if s.cacheTTLDetector != nil {
		s.recordCacheResponse(threadID, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, 0)
	}

	if threadID != "" {
		s.responseTsMu.Lock()
		if s.responseTimes == nil {
			s.responseTimes = make(map[string]time.Time)
		}
		s.responseTimes[threadID] = time.Now()
		s.responseTsMu.Unlock()
	}

	if s.cfg.DataDir != "" {
		go func(u *UsageTracker) {
			s.queryDaemon("track_proxy_usage", map[string]any{
				"input_tokens":          u.InputTokens,
				"output_tokens":         u.OutputTokens,
				"cache_read_tokens":     u.CacheReadInputTokens,
				"cache_creation_tokens": u.CacheCreationInputTokens,
			})
		}(usage)
	}
}

type openAIUsageParser interface {
	ParseUsage(usage *UsageTracker, data []byte)
	ParseAnnotation(data []byte, accum *strings.Builder, done *bool, maxLen int)
}

type chatCompletionsParser struct{}

func (chatCompletionsParser) ParseUsage(usage *UsageTracker, data []byte) {
	if usage == nil || len(data) == 0 || data[0] != '{' {
		return
	}
	var chunk struct {
		Usage *OpenAIUsage `json:"usage"`
	}
	if json.Unmarshal(data, &chunk) != nil || chunk.Usage == nil {
		return
	}
	if chunk.Usage.CompletionTokens > 0 {
		usage.OutputTokens = chunk.Usage.CompletionTokens
	}

	// DeepSeek reports cache as prompt_cache_hit_tokens + prompt_cache_miss_tokens.
	// prompt_tokens = hit + miss (total). We map hit→CacheReadInputTokens,
	// miss→CacheMissTokens. InputTokens is set to miss (uncached portion)
	// so TotalInputTokens() = miss + hit = prompt_tokens without double-counting.
	//
	// We track presence of the hit field to distinguish DeepSeek-cache-aware
	// responses from providers that don't report cache at all.
	hasDeepSeekCache := chunk.Usage.PromptCacheHitTokens > 0
	if chunk.Usage.PromptCacheHitTokens > 0 {
		usage.CacheReadInputTokens = chunk.Usage.PromptCacheHitTokens
	}
	// Always store miss (even if zero) so the LogLine format can trigger
	// on (CacheReadInputTokens > 0 && CacheCreationInputTokens == 0).
	usage.CacheMissTokens = chunk.Usage.PromptCacheMissTokens

	if hasDeepSeekCache {
		// InputTokens = uncached portion (miss).  If API returned miss=0
		// (all cached) this correctly sets InputTokens=0.
		usage.InputTokens = usage.CacheMissTokens
	} else if chunk.Usage.PromptTokens > 0 {
		usage.InputTokens = chunk.Usage.PromptTokens
	} else if chunk.Usage.TotalTokens > 0 && usage.OutputTokens > 0 {
		usage.InputTokens = chunk.Usage.TotalTokens - usage.OutputTokens
	}
	if chunk.Usage.TotalTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		usage.Complete = true
	}
}

func (chatCompletionsParser) ParseAnnotation(data []byte, accum *strings.Builder, done *bool, maxLen int) {
	if *done || len(data) == 0 || data[0] != '{' {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			accum.WriteString(choice.Delta.Content)
			if accum.Len() >= maxLen {
				*done = true
				return
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			*done = true
		}
	}
}

type responsesParser struct{}

func (responsesParser) ParseUsage(usage *UsageTracker, data []byte) {
	if usage == nil || len(data) == 0 || data[0] != '{' {
		return
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				InputDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil || event.Type != "response.completed" {
		return
	}
	usage.InputTokens = event.Response.Usage.InputTokens
	usage.OutputTokens = event.Response.Usage.OutputTokens
	usage.CacheReadInputTokens = event.Response.Usage.InputDetails.CachedTokens
	usage.Complete = true
}

func (responsesParser) ParseAnnotation(data []byte, accum *strings.Builder, done *bool, maxLen int) {
	if *done || len(data) == 0 || data[0] != '{' {
		return
	}
	var event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if event.Type == "response.content_part.delta" && event.Delta.Type == "text_delta" {
		accum.WriteString(event.Delta.Text)
		if accum.Len() >= maxLen {
			*done = true
		}
	}
	if event.Type == "response.completed" && accum.Len() > 0 {
		*done = true
	}
}
