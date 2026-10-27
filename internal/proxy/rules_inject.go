package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

const (
	rulesInjectionInterval = 40000 // tokens between re-injections

	// rulesCacheTTL bounds how long a fetched rules block is served. The proxy
	// has no signal for a CLAUDE.md/OPENCODE.md change, so a time bound is the
	// only thing that stops a stale block from living for the whole process.
	rulesCacheTTL = 5 * time.Minute
)

// rulesCacheEntry is one project's cached condensed rules block.
type rulesCacheEntry struct {
	block     string
	fetchedAt time.Time
}

// rulesInject checks if rules should be re-injected based on cumulative token count.
// Called with the current total token count of the conversation.
// Returns the rules block to inject, or "" if not yet time.
func (s *Server) rulesInject(threadID string, totalTokens int, project string) string {
	s.rulesMu.Lock()
	if s.rulesTokenCount == nil {
		s.rulesTokenCount = make(map[string]int)
	}
	if s.rulesCollapseInjected == nil {
		s.rulesCollapseInjected = make(map[string]bool)
	}

	// Reset collapse flag — normal request flow resumes
	s.rulesCollapseInjected[threadID] = false

	lastInjected := s.rulesTokenCount[threadID] // total tokens at last injection
	if lastInjected == 0 {
		// First call — set baseline, don't inject yet (briefing covers initial context)
		s.rulesTokenCount[threadID] = totalTokens
		s.pruneRulesThreads()
		s.rulesMu.Unlock()
		return ""
	}

	if totalTokens-lastInjected < rulesInjectionInterval {
		s.rulesMu.Unlock()
		return ""
	}

	// Reset to current position only after a successful fetch: a daemon outage
	// at this moment must not cost a whole injection interval.
	s.rulesMu.Unlock()

	block := s.getRulesBlock(project)
	if block == "" {
		return ""
	}

	s.rulesMu.Lock()
	if s.rulesTokenCount[threadID] != lastInjected {
		// Another request for this thread already passed the gate while this one
		// was fetching; injecting again would spend the interval twice.
		s.rulesMu.Unlock()
		return ""
	}
	s.rulesTokenCount[threadID] = totalTokens
	s.pruneRulesThreads()
	s.rulesMu.Unlock()

	return block
}

// rulesInjectAfterCollapse injects rules once after collapse, then blocks until normal inject resets the flag.
func (s *Server) rulesInjectAfterCollapse(threadID, project string) string {
	s.rulesMu.Lock()
	if s.rulesTokenCount == nil {
		s.rulesTokenCount = make(map[string]int)
	}
	if s.rulesCollapseInjected == nil {
		s.rulesCollapseInjected = make(map[string]bool)
	}
	if s.rulesCollapseInjected[threadID] {
		s.rulesMu.Unlock()
		return ""
	}
	s.rulesCollapseInjected[threadID] = true
	s.rulesTokenCount[threadID] = 0
	s.pruneRulesThreads()
	s.rulesMu.Unlock()

	block := s.getRulesBlock(project)
	if block == "" {
		// Release the claim: a failed fetch must not mute the reminder until the
		// normal inject path resets the flag again.
		s.rulesMu.Lock()
		s.rulesCollapseInjected[threadID] = false
		s.rulesMu.Unlock()
		return ""
	}

	return block
}

// pruneRulesThreads bounds the two rules maps. Must be called with rulesMu held.
// The collapse flags are a subset of the token keys (rulesInject writes the token
// entry on every path it writes a flag), so trimming the token map and dropping
// the orphans is enough. Orphans exist only after a trim here, so a call that
// removed nothing has nothing left to sweep.
func (s *Server) pruneRulesThreads() {
	if len(s.rulesTokenCount) <= maxThreadStates {
		return
	}
	removed := pruneThreadStates(s.rulesTokenCount, maxThreadStates*3/4)
	for threadID := range s.rulesCollapseInjected {
		if _, ok := s.rulesTokenCount[threadID]; !ok {
			delete(s.rulesCollapseInjected, threadID)
		}
	}
	log.Printf("[rules] pruned %d idle thread states", removed)
}

// getRulesBlock returns the project's cached rules block, fetching from the
// daemon on first call or after rulesCacheTTL.
func (s *Server) getRulesBlock(project string) string {
	s.rulesMu.RLock()
	entry, ok := s.rulesCache[project]
	s.rulesMu.RUnlock()
	if ok && entry.block != "" && time.Since(entry.fetchedAt) < rulesCacheTTL {
		return entry.block
	}

	// Fetch from daemon
	block := s.fetchRulesFromDaemon(project)
	if block == "" {
		return ""
	}

	s.rulesMu.Lock()
	if s.rulesCache == nil {
		s.rulesCache = make(map[string]rulesCacheEntry)
	}
	// Drop expired entries so the map stays bounded by the projects seen within
	// one TTL window instead of growing with every project path ever served.
	for p, e := range s.rulesCache {
		if time.Since(e.fetchedAt) >= rulesCacheTTL {
			delete(s.rulesCache, p)
		}
	}
	s.rulesCache[project] = rulesCacheEntry{block: block, fetchedAt: time.Now()}
	s.rulesMu.Unlock()

	return block
}

// fetchRulesFromDaemon queries the daemon for the condensed rules block.
func (s *Server) fetchRulesFromDaemon(project string) string {
	result, err := s.queryDaemon("get_rules_block", map[string]any{"project": project})
	if err != nil {
		return ""
	}

	var resp struct {
		Content string `json:"content"`
		Exists  bool   `json:"exists"`
	}
	if json.Unmarshal(result, &resp) != nil || !resp.Exists {
		return ""
	}

	log.Printf("[rules] loaded rules block: %d chars", len(resp.Content))
	return resp.Content
}

// formatRulesReminder wraps the condensed rules in a recognizable block.
// Fetches permanent pins from daemon and appends them.
// nonClaude controls whether the footer references CLAUDE.md (Claude Code) or OPENCODE.md (opencode/DeepSeek).
func (s *Server) formatRulesReminder(rules, project string, nonClaude bool) string {
	// Fetch permanent pins
	var pinBlock string
	if result, err := s.queryDaemon("get_pins", map[string]any{"project": project}); err == nil {
		var resp struct {
			Pins []struct {
				ID      int    `json:"id"`
				Scope   string `json:"scope"`
				Content string `json:"content"`
			} `json:"pins"`
		}
		if json.Unmarshal(result, &resp) == nil {
			var lines []string
			for _, p := range resp.Pins {
				if p.Scope == "permanent" {
					lines = append(lines, fmt.Sprintf("- [pin:%d permanent] %s", p.ID, p.Content))
				}
			}
			if len(lines) > 0 {
				pinBlock = "\n\n## Pinned Instructions\n" + joinLines(lines)
			}
		}
	}

	var rulesFile string
	if nonClaude {
		rulesFile = "OPENCODE.md (or agents.md)"
	} else {
		rulesFile = "CLAUDE.md"
	}
	footer := fmt.Sprintf("\n\n---\nThese rules are an excerpt. Additional project-specific instructions (architecture, routes, code patterns) are in your system prompt (%s). When unsure: docs_search() for details.\nTimestamps [HH:MM:SS] [msg:N] [+Δ] are embedded in every message — always reference them for time-related statements.", rulesFile)
	return fmt.Sprintf("[Rules Reminder]\n%s%s%s\n[/Rules Reminder]", rules, pinBlock, footer)
}

func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
