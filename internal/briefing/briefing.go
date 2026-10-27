package briefing

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Generator builds narrative briefing text from stored data.
type Generator struct {
	store          *storage.Store
	maxSessions    int
	maxPerCategory int
	dedupThreshold float64
	unfinishedTTL  int // days; 0 = no limit
	otherProjects  int // days a neighbouring project may be idle; 0 = no limit
	maxTokens      int // briefing.max_tokens; 0 = the 20000-byte default
	userProfile    bool
	strings        Strings
	recovery       *RecoveryConfig
	skipUnfinished bool
	codeScanner    *codescan.CachedScanner
	codeGraph      *codescan.CodeGraph // built during briefing, available for MCP tools
	codeMapText    string              // built during briefing, appended post-refine
	sourceAgent    string              // claude (default), codex, opencode — drives profile-aware wording
	deadlineHits   []DeadlineHit       // deadline items that reached the rendered open-work list
}

// DeadlineHit is a deadline item that reached the rendered open-work list.
// Content identifies the item in the delivered text: refinement may rewrite or
// drop the open-work block, and an item that never reaches the delivered text
// must not silence its deadline.
type DeadlineHit struct {
	ID      int64
	Content string
}

// New creates a briefing generator with auto-loaded strings.
func New(store *storage.Store, maxSessions int) *Generator {
	if maxSessions <= 0 {
		maxSessions = 3
	}
	return &Generator{
		store:          store,
		maxSessions:    maxSessions,
		maxPerCategory: 3,
		dedupThreshold: 0.4,
		userProfile:    true,
		strings:        resolveDefaultStrings(),
	}
}

// SetMaxPerCategory overrides the default max items per category.
func (g *Generator) SetMaxPerCategory(n int) {
	if n > 0 {
		g.maxPerCategory = n
	}
}

// SetDedupThreshold overrides the default dedup similarity threshold.
func (g *Generator) SetDedupThreshold(t float64) {
	if t > 0 {
		g.dedupThreshold = t
	}
}

// SetUnfinishedTTL sets the max age in days for unfinished items. 0 = no limit.
func (g *Generator) SetUnfinishedTTL(days int) {
	g.unfinishedTTL = days
}

// SetOtherProjectsDays bounds how long ago a neighbouring project may have been
// active to still appear in the gap awareness section. 0 = no limit.
func (g *Generator) SetOtherProjectsDays(days int) {
	g.otherProjects = days
}

// SetMaxTokens caps the rendered briefing. Bytes are derived at 4 per token,
// the ratio the previous hardcoded 20000-byte limit was built on. 0 = default.
func (g *Generator) SetMaxTokens(tokens int) {
	g.maxTokens = tokens
}

// SetUserProfile enables or disables the user profile section in the briefing.
func (g *Generator) SetUserProfile(enabled bool) {
	g.userProfile = enabled
}

// SetSkipUnfinished disables loading of unfinished/open-work items.
// Use for agent sessions where todo nudges are not appropriate.
func (g *Generator) SetSkipUnfinished(skip bool) {
	g.skipUnfinished = skip
}

// SetSourceAgent sets the agent origin for profile-aware wording.
// Default is "claude" (backward compatible). Use "opencode" or "codex" for neutral wording.
func (g *Generator) SetSourceAgent(agent string) {
	g.sourceAgent = models.NormalizeSourceAgent(agent)
	if !g.profile().IsClaude() {
		g.strings.AgentName = "Agent"
	}
}

// profile returns the PromptProfile for the generator's source agent.
func (g *Generator) profile() models.PromptProfile {
	return models.SourceAgentToProfile(g.sourceAgent)
}

// SetStrings sets translated UI strings for the briefing.
func (g *Generator) SetStrings(s Strings) {
	g.strings = s
}

// CodeGraph returns the code graph built during the last Generate() call.
// Returns nil if no briefing has been generated yet or the project had no source files.
func (g *Generator) CodeGraph() *codescan.CodeGraph {
	return g.codeGraph
}

// CodeMap returns the rendered code map built during Generate.
// Append post-refine to bypass LLM compression.
func (g *Generator) CodeMap() string {
	return g.codeMapText
}

// DeadlineHits returns the deadline items that reached the rendered open-work
// list during the last Generate call.
func (g *Generator) DeadlineHits() []DeadlineHit {
	return g.deadlineHits
}

// MarkDeadlineHits starts the 12h cooldown for those deadline items of the last
// Generate call that are present in the delivered text. Marking must happen on
// delivery, not during Generate: background generations feed the refine cache and
// never reach a session. The text check covers the refine step, which may rewrite
// or drop the open-work block, and the 20KB truncation.
func (g *Generator) MarkDeadlineHits(delivered string) {
	var ids []int64
	for _, h := range g.deadlineHits {
		if h.Content != "" && strings.Contains(delivered, h.Content) {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := g.store.UpdateLastHitAt(ids); err != nil {
		log.Printf("[briefing] mark deadline hits failed: %v", err)
	}
}

// Generate produces a narrative briefing for the given project directory.
func (g *Generator) Generate(projectDir string) string {
	return g.generateResolved(projectDir, g.resolveProject(projectDir))
}

// GenerateForKey renders the briefing for a project key that is already resolved.
// A caller holding the row identity (the refresh loop) must render that key, not the
// project a re-resolution would produce: Generate resolves through OwningProject,
// which climbs to the nearest ancestor that carries sessions. Which rows the refresh
// loop hands over is decided by ListProjectsWithRepositoryRoots, whose gate only
// keeps roots that OwningProject resolves to themselves, so key and read path agree.
func (g *Generator) GenerateForKey(projectShort string) string {
	return g.generateResolved(projectShort, projectShort)
}

func (g *Generator) generateResolved(projectDir, projectShort string) string {
	g.deadlineHits = nil

	// Load, deduplicate, and score all learnings
	allLearnings := g.loadLearnings(projectShort)
	// Enrich with session fixation ratios before scoring
	g.store.EnrichSessionFixationScores(allLearnings)
	// Sort + limit BEFORE dedup — Deduplicate is O(n²) and becomes very slow with 2000+ learnings.
	// Score first so we keep the most relevant learnings, then dedup within that set.
	ctx := models.QueryContext{Project: projectShort}
	models.ContextualScoreAndSort(allLearnings, ctx)
	const maxBeforeDedup = 400
	if len(allLearnings) > maxBeforeDedup {
		allLearnings = allLearnings[:maxBeforeDedup]
	}
	allLearnings = Deduplicate(allLearnings, g.dedupThreshold)
	allLearnings = filterFaded(allLearnings, fadeThreshold)

	// Load other data
	sessions := g.loadSessions(projectShort)
	profile := g.loadProfile(projectShort)
	unfinished := g.loadUnfinished(projectShort)
	totalSessions := g.countTotalSessions()
	narratives := g.loadNarratives(projectShort)
	docSources := g.loadDocSources(projectShort)

	s := g.strings

	// Render sections — greeting+narratives FIRST for relationship framing
	var b strings.Builder
	b.WriteString(g.renderAwakening(s, allLearnings, totalSessions, narratives, sessions, projectShort, docSources))

	// Latest state brief — resolved local-first with global fallback. The
	// line carries the brief's fixed DATE (never a relative time): identical
	// DB state must render identical bytes, or the briefing prefix busts the
	// prompt cache across regenerations.
	if brief := g.loadLatestStateBrief(projectShort); brief != nil {
		b.WriteString(fmt.Sprintf("\nState brief #%d (%s): get_learnings(id=%d)\n", brief.ID, brief.CreatedAt.Format("2006-01-02"), brief.ID))
	}

	// User profile (full Entwicklerprofil) and persona directive are not mutually
	// exclusive (D8): the profile describes the person, the directive carries the
	// hard working rules. Profile first, then a short directive; when no profile
	// exists the directive keeps its full limit and the preference learnings are
	// rendered as well (a synthesized profile already contains them).
	hasProfile := false
	if g.userProfile {
		if userProfile := g.loadUserProfile(); userProfile != "" {
			b.WriteString("\n" + userProfile + "\n")
			hasProfile = true
		}
	}
	directiveLimit := 10
	if hasProfile {
		directiveLimit = 5
	}
	if personaDirective := g.loadPersonaDirective(); personaDirective != "" {
		re := regexp.MustCompile(`^\d+ Sessions`)
		personaDirective = re.ReplaceAllString(personaDirective, fmt.Sprintf("%d Sessions", totalSessions))
		b.WriteString(FormatPersonaDirectiveLimited(personaDirective, directiveLimit))
		b.WriteString("\n")
	}
	if !hasProfile {
		b.WriteString(g.renderPerson(s, allLearnings))
	}
	b.WriteString(g.renderKnowledge(s, allLearnings))
	b.WriteString(g.renderCaps(s, allLearnings))

	milestones := g.loadMilestones(narratives)
	b.WriteString(g.renderMilestones(s, milestones))

	b.WriteString(g.renderProject(s, projectShort, profile, sessions))
	absenceHours := calcAbsenceHours(sessions)
	// Merge deadline triggers into unfinished items, track urgency reasons.
	// Gated behind skipUnfinished — agent sessions suppress all open-work including deadline items.
	triggerReasons := make(map[int64]string) // learning ID → urgency reason
	if !g.skipUnfinished {
		triggers := checkDeadlineTriggers(g.store, projectShort, s)
		for _, tm := range triggers {
			triggerReasons[tm.Learning.ID] = tm.Reason
			if !containsLearning(unfinished, tm.Learning.ID) {
				unfinished = append(unfinished, tm.Learning)
			}
		}
	}
	b.WriteString(g.renderOpenWork(s, unfinished, absenceHours, triggerReasons))

	gapData := g.loadGapAwareness(projectShort)
	b.WriteString(g.renderGapAwareness(s, gapData))

	// Recurrence alerts — wiederkehrende Muster
	recurrenceAlerts := g.loadRecurrenceAlerts(projectShort)
	if len(recurrenceAlerts) > 0 {
		b.WriteString("\n")
		b.WriteString(s.Recurrence)
		b.WriteString("\n")
		for _, a := range recurrenceAlerts {
			b.WriteString(fmt.Sprintf("• %s\n", a.Content))
		}
	}

	// Knowledge Index Phase A — enriched briefing sections
	b.WriteString(g.renderKnowledgeIndex(s, projectDir, projectShort, docSources))

	result := b.String()

	// Hard limit from briefing.max_tokens (~5000 tokens at 4 bytes each).
	// Prevents briefing from consuming too much context.
	maxBytes := g.maxTokens * 4
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	if len(result) > maxBytes {
		// The marker is appended after the cut, so the result is maxBytes plus
		// the marker (the same shape the previous fixed 20000-byte limit had).
		result = truncateBytes(result, maxBytes) + "\n... (briefing truncated)"
	}

	return result
}

// truncateBytes cuts text to at most maxBytes without splitting a UTF-8 rune.
// The result may be shorter than maxBytes by up to three bytes. Callers append
// their own marker, so the returned string carries no suffix of its own.
func truncateBytes(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// resolveProject resolves the project a directory belongs to: the directory
// itself when a session carries it, else the nearest ancestor that does, else the
// path as given. The rule lives in storage.OwningProject, so pin lookups and the
// briefing resolve a cwd to the same project.
func (g *Generator) resolveProject(projectDir string) string {
	if g.store == nil {
		return models.ProjectShortFromPath(projectDir)
	}
	return g.store.OwningProject(projectDir)
}

// ProjectKey exposes the resolved project identifier used by Generate.
// Callers running project-scoped lookups after Generate (refine cache, pins,
// unfinished count) must key them the same way, which is the project path —
// sessions.project_short, learnings.project and refined_briefings.project all
// store the path, never a basename.
func (g *Generator) ProjectKey(projectDir string) string {
	return g.resolveProject(projectDir)
}

// loadLearnings fetches project-specific and global learnings.
func (g *Generator) loadLearnings(projectShort string) []models.Learning {
	// ActiveLearningsForProject adds the rows written below the project path: a
	// repository whose work happens in worktree checkouts has no row under its own
	// path, and the canonical scope alone would leave the briefing without content.
	learnings, _ := g.store.ActiveLearningsForProject(projectShort, "", 0)
	return learnings
}

// loadSessions fetches sessions for the current project.
func (g *Generator) loadSessions(projectShort string) []models.Session {
	if projectShort == "" {
		return nil
	}
	sessions, _ := g.store.ListSessions(projectShort, 30)
	return sessions
}

// loadProfile fetches the project profile, limited to maxLines.
func (g *Generator) loadProfile(projectShort string) string {
	if projectShort == "" {
		return ""
	}
	profile, err := g.store.GetProjectProfile(projectShort)
	if err != nil || profile.ProfileText == "" {
		return ""
	}
	// Limit to 5 lines for compact briefing
	lines := strings.Split(profile.ProfileText, "\n")
	if len(lines) > 5 {
		lines = lines[:5]
	}
	return strings.Join(lines, "\n")
}

// loadUnfinished fetches unfinished work items for the current project only.
// Items older than unfinishedTTL days are excluded (0 = no limit).
func (g *Generator) loadUnfinished(projectShort string) []models.Learning {
	if g.skipUnfinished {
		return nil
	}
	if projectShort == "" {
		return nil
	}
	unfinished, _ := g.store.GetActiveLearnings("unfinished", projectShort, "", "", 0)
	if g.unfinishedTTL <= 0 || len(unfinished) == 0 {
		return unfinished
	}
	cutoff := time.Now().AddDate(0, 0, -g.unfinishedTTL)
	filtered := unfinished[:0]
	for _, u := range unfinished {
		if u.CreatedAt.After(cutoff) {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

// countTotalSessions returns the total number of sessions across all projects.
func (g *Generator) countTotalSessions() int {
	projects, err := g.store.ListProjects()
	if err != nil {
		return 0
	}
	total := 0
	for _, p := range projects {
		total += p.SessionCount
	}
	return total
}

// loadPersonaDirective fetches the cached persona directive for the default user.
func (g *Generator) loadPersonaDirective() string {
	directive, err := g.store.GetPersonaDirective("default")
	if err != nil || directive == nil {
		return ""
	}
	return directive.Directive
}

// loadUserProfile fetches the synthesized user profile.
func (g *Generator) loadUserProfile() string {
	directive, err := g.store.GetPersonaDirective("user_profile")
	if err != nil || directive == nil {
		return ""
	}
	return directive.Directive
}

// loadNarratives fetches recent session narratives for the current project.
// Narratives within 1h of each other are consolidated into a single entry.
func (g *Generator) loadNarratives(projectShort string) []NarrativeSummary {
	if projectShort == "" {
		return nil
	}
	// Fetch more than needed so consolidation still yields enough
	learnings, err := g.store.GetRecentNarratives(projectShort, 10)
	if err != nil {
		log.Printf("[briefing] loadNarratives failed for %q: %v", projectShort, err)
		return nil
	}
	if len(learnings) == 0 {
		return nil
	}

	// No consolidation — show individual narratives (newest first).
	// Limit to 3 most recent.
	if len(learnings) > 3 {
		learnings = learnings[:3]
	}

	var result []NarrativeSummary
	for _, l := range learnings {
		content := l.Content
		if len(content) > 1500 {
			content = content[:1500] + "..."
		}

		result = append(result, NarrativeSummary{
			Ago:       relativeTime(g.strings, l.CreatedAt),
			Flavor:    l.SessionFlavor,
			Content:   content,
			SessionID: l.SessionID,
		})
	}
	return result
}

func (g *Generator) renderAwakening(s Strings, learnings []models.Learning, totalSessions int, narratives []NarrativeSummary, sessions []models.Session, projectShort string, docSources []DocSourceSummary) string {
	// Extract pulse lines
	for i := range narratives {
		narratives[i].Pulse = extractPulse(narratives[i].Content)
	}

	data := AwakeningData{
		HasLearnings:  len(learnings) > 0,
		TotalSessions: totalSessions,
		AwakeningText: fmt.Sprintf(s.AwakeningNarrative, totalSessions, totalSessions),
	}

	if len(narratives) > 0 {
		latest := narratives[0]
		data.LatestNarrative = &latest
		if len(narratives) > 1 {
			// Truncate flavors at sentence boundary for compact display
			older := make([]NarrativeSummary, len(narratives)-1)
			copy(older, narratives[1:])
			for i := range older {
				if older[i].Flavor != "" {
					older[i].Flavor = truncateAtSentence(older[i].Flavor, 120)
				}
			}
			data.OlderNarratives = older
		}
	}

	// Load clusters from DB
	data.StrongClusters, data.WeakClusters = g.loadClusters(projectShort)

	// Doc sources
	data.DocSources = docSources

	// Compute time gaps from session start times (last 90 days only)
	cutoff := time.Now().AddDate(0, 0, -90)
	var startTimes []time.Time
	for _, sess := range sessions {
		if !sess.StartedAt.IsZero() && sess.StartedAt.After(cutoff) {
			startTimes = append(startTimes, sess.StartedAt)
		}
	}
	data.TimeGaps = computeTimeGaps(s, startTimes)

	return renderTemplate("awakening", tmplAwakening, s, data)
}

// loadClusters fetches learning clusters and splits them into strong (confidence >= 0.5) and weak.
func (g *Generator) loadClusters(projectShort string) (strong, weak []ClusterSummary) {
	clusters, err := g.store.GetLearningClusters(projectShort)
	if err != nil {
		log.Printf("[briefing] loadClusters failed for %q: %v", projectShort, err)
		return nil, nil
	}
	if len(clusters) == 0 {
		return nil, nil
	}

	for _, c := range clusters {
		summary := ClusterSummary{
			Label:       c.Label,
			Count:       c.LearningCount,
			RecencyNote: g.recencyNote(c.AvgRecencyDays),
			Confidence:  c.Confidence,
		}
		if c.Confidence >= 0.5 {
			strong = append(strong, summary)
		} else {
			weak = append(weak, summary)
		}
	}
	return strong, weak
}

// recencyNote returns a translated recency description.
func (g *Generator) recencyNote(avgDays float64) string {
	s := g.strings
	switch {
	case avgDays < 7:
		return s.RecencyFresh
	case avgDays < 30:
		return s.RecencyWeeks
	case avgDays < 90:
		return s.RecencyMonth
	default:
		return s.RecencyOlder
	}
}

func (g *Generator) renderPerson(s Strings, learnings []models.Learning) string {
	var items []string
	for _, l := range learnings {
		if l.Category == "preference" || l.Category == "relationship" {
			items = append(items, RewriteToPersonalTone(l.Content))
		}
	}
	if len(items) == 0 {
		return ""
	}
	if len(items) > g.maxPerCategory {
		items = items[:g.maxPerCategory]
	}
	return renderTemplate("person", tmplPerson, s, PersonData{Items: items})
}

func (g *Generator) renderKnowledge(s Strings, learnings []models.Learning) string {
	groups := map[string][]models.Learning{}
	for _, l := range learnings {
		groups[l.Category] = append(groups[l.Category], l)
	}

	// With relevance scoring, all categories get equal space.
	// The score already prioritizes gotchas via categoryWeight.
	max := g.maxPerCategory

	gotchas, moreGotchas := limitLearningsTruncated(groups["gotcha"], max, 120)
	decisions, moreDecisions := limitLearningsTruncated(groups["decision"], max, 120)
	patterns, morePatterns := limitLearningsTruncated(groups["pattern"], max, 120)
	teachings, moreTeachings := limitLearningsTruncated(groups["explicit_teaching"], max, 120)
	facts, moreFacts := limitFacts(groups["fact"], max, 120)
	pivots, morePivots := limitLearnings(groups["pivot_moment"], 5)

	return renderTemplate("knowledge", tmplKnowledge, s, KnowledgeData{
		Gotchas:       gotchas,
		MoreGotchas:   moreGotchas,
		Decisions:     decisions,
		MoreDecisions: moreDecisions,
		Patterns:      patterns,
		MorePatterns:  morePatterns,
		Teachings:     teachings,
		MoreTeachings: moreTeachings,
		Facts:         facts,
		MoreFacts:     moreFacts,
		Pivots:        pivots,
		MorePivots:    morePivots,
	})
}

func (g *Generator) renderCaps(s Strings, learnings []models.Learning) string {
	var items []string
	for _, l := range learnings {
		if l.Category != "cap" {
			continue
		}
		line := l.Content
		if len(l.Keywords) > 0 {
			line += " (" + strings.Join(l.Keywords, ", ") + ")"
		}
		items = append(items, line)
		if len(items) >= g.maxPerCategory {
			break
		}
	}
	if len(items) == 0 {
		return ""
	}
	return renderTemplate("capabilities", tmplCaps, s, CapsData{Items: items})
}

func (g *Generator) renderProject(s Strings, projectShort, profile string, sessions []models.Session) string {
	if projectShort == "" || len(sessions) == 0 {
		return ""
	}

	limit := g.maxSessions
	if limit > len(sessions) {
		limit = len(sessions)
	}

	// Bulk-load subagent counts for shown sessions
	sessionIDs := make([]string, limit)
	for i := 0; i < limit; i++ {
		sessionIDs[i] = sessions[i].ID
	}
	subagentCounts, _ := g.store.GetSubagentCounts(sessionIDs)
	marksCounts, _ := g.store.GetLearningsCounts(sessionIDs)

	var summaries []SessionSummary
	for i := 0; i < limit; i++ {
		sess := sessions[i]
		msg := sess.FirstMessage
		if len(msg) > 80 {
			msg = msg[:80] + "..."
		}
		summaries = append(summaries, SessionSummary{
			Ago:           relativeTime(s, sess.StartedAt),
			FirstMessage:  msg,
			Branch:        sess.GitBranch,
			SubagentCount: subagentCounts[sess.ID],
			Marks:         marksCounts[sess.ID],
		})
	}

	return renderTemplate("project", tmplProject, s, ProjectData{
		Name:          projectShort,
		DisplayName:   models.DisplayNameOfShort(projectShort),
		Profile:       profile,
		Sessions:      summaries,
		TotalSessions: len(sessions),
		ShownCount:    limit,
	})
}

// loadLatestStateBrief returns the Zustandsbrief for this project: the
// project's own active head, or the global fallback. See
// Store.GetLatestStateBrief for the marker convention.
func (g *Generator) loadLatestStateBrief(projectShort string) *models.Learning {
	brief, err := g.store.GetLatestStateBrief(projectShort)
	if err != nil {
		return nil
	}
	return brief
}

// loadRecurrenceAlerts fetches active recurrence alerts for the current project.
func (g *Generator) loadRecurrenceAlerts(projectShort string) []models.Learning {
	alerts, err := g.store.GetLearningsByCategory("recurrence_alert", projectShort, 3)
	if err != nil {
		return nil
	}
	return alerts
}

// loadMetamemory returns a rendered self-assessment of knowledge quality.
func (g *Generator) loadMetamemory(projectShort string) string {
	db := g.store.DB()
	if db == nil {
		return ""
	}

	projFilter := "AND (canonical_project = ? OR canonical_project = '')"
	base := "FROM learnings WHERE superseded_by IS NULL " + projFilter

	var total, solid, noisy, expired int
	db.QueryRow("SELECT COUNT(*) "+base, projectShort).Scan(&total)
	db.QueryRow("SELECT COUNT(*) "+base+" AND save_count >= 3", projectShort).Scan(&solid)
	db.QueryRow("SELECT COUNT(*) "+base+" AND noise_count > 3", projectShort).Scan(&noisy)
	// julianday() on both sides: valid_until is written as datetime('now') by the
	// supersede paths but as RFC3339 by legacy rows, and a lexical comparison puts
	// the RFC3339 'T' above the space separator, hiding a same-day expiry.
	db.QueryRow("SELECT COUNT(*) "+base+" AND valid_until IS NOT NULL AND julianday(valid_until) < julianday('now')", projectShort).Scan(&expired)

	// Only render if there's something to say
	if solid == 0 && noisy == 0 && expired == 0 {
		return ""
	}

	return metamemoryText(g.strings, total, solid, noisy, expired)
}

// metamemoryText renders the knowledge-quality self-assessment.
func metamemoryText(s Strings, total, solid, noisy, expired int) string {
	var sb strings.Builder
	sb.WriteString(s.MetamemoryHeader + "\n")
	if solid > 0 {
		sb.WriteString(fmt.Sprintf(s.MetamemoryProvenFmt, solid, total) + "\n")
	}
	if noisy > 0 {
		sb.WriteString(fmt.Sprintf(s.MetamemoryFragileFmt, noisy) + "\n")
	}
	if expired > 0 {
		sb.WriteString(fmt.Sprintf(s.MetamemoryOutdatedFmt, expired) + "\n")
	}
	return sb.String()
}

// GenerateMetamemory is a public wrapper for loadMetamemory, intended for
// post-refine injection so the metamemory block survives LLM refinement.
func (g *Generator) GenerateMetamemory(projectShort string) string {
	return g.loadMetamemory(projectShort)
}

// loadGapAwareness computes what knowledge exists but isn't shown in the briefing.
func (g *Generator) loadGapAwareness(projectShort string) *GapAwarenessData {
	counts, err := g.store.GetLearningCounts(projectShort)
	if err != nil {
		counts = nil
	}
	overflow := make(map[string]int)
	for cat, count := range counts {
		if extra := count - g.maxPerCategory; extra > 0 {
			overflow[cat] = extra
		}
	}

	projects, _ := g.store.ListProjects()
	var others []ProjectGap
	moreCount := 0
	for _, p := range projects {
		if p.ProjectShort == projectShort || p.ProjectShort == "" {
			continue
		}
		if p.SessionCount < 5 {
			continue
		}
		lastActive, parseErr := time.Parse(time.RFC3339, p.LastActive)
		daysAgo := int(time.Since(lastActive).Hours() / 24)
		// An unparsable last_active is unknown age, not infinite age: dropping
		// the project would hide it from every briefing instead of listing it.
		if g.otherProjects > 0 && parseErr == nil && daysAgo > g.otherProjects {
			continue
		}
		if len(others) < 5 {
			others = append(others, ProjectGap{
				Name:         p.ProjectShort,
				SessionCount: p.SessionCount,
				DaysAgo:      daysAgo,
			})
		} else {
			moreCount++
		}
	}

	// Load knowledge gaps
	var knowledgeGaps []KnowledgeGapSummary
	if activeGaps, err := g.store.GetActiveGaps(projectShort, 3); err == nil {
		for _, gap := range activeGaps {
			daysAgo := int(time.Since(gap.FirstSeen).Hours() / 24)
			knowledgeGaps = append(knowledgeGaps, KnowledgeGapSummary{
				Topic:    gap.Topic,
				HitCount: gap.HitCount,
				DaysAgo:  daysAgo,
			})
		}
	}

	if len(overflow) == 0 && len(others) == 0 && len(knowledgeGaps) == 0 {
		return nil
	}

	return &GapAwarenessData{
		ProjectShort:  projectShort,
		Overflow:      overflow,
		OtherProjects: others,
		MoreCount:     moreCount,
		KnowledgeGaps: knowledgeGaps,
	}
}

func (g *Generator) renderGapAwareness(s Strings, data *GapAwarenessData) string {
	if data == nil {
		return ""
	}
	// Pre-compute DaysAgoText for each project gap
	for i := range data.OtherProjects {
		if data.OtherProjects[i].DaysAgo > 0 {
			data.OtherProjects[i].DaysAgoText = fmt.Sprintf(s.LabelDaysAgoFmt, data.OtherProjects[i].DaysAgo)
		}
	}
	return renderTemplate("gap", tmplGapAwareness, s, data)
}

// extractPulse finds and returns a "Puls: ..." or "Pulse: ..." line from
// narrative content. Both spellings are accepted: the german narrative prompt
// used "Puls:", the english one emits "Pulse:".
func extractPulse(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Puls:") || strings.HasPrefix(trimmed, "Pulse:") {
			return trimmed
		}
	}
	return ""
}

// loadDocSources fetches documentation sources visible to this project:
// project-scoped sources plus truly global ones (empty project). It must never
// fall back to "all sources" — that leaked every other project's docs into the
// briefing's documentation index.
func (g *Generator) loadDocSources(projectShort string) []DocSourceSummary {
	if projectShort == "" {
		return nil
	}
	sources, err := g.store.ListDocSourcesForProject(projectShort)
	if err != nil || len(sources) == 0 {
		return nil
	}
	var result []DocSourceSummary
	seen := make(map[string]bool, len(sources))
	for _, s := range sources {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		result = append(result, DocSourceSummary{
			Name:        s.Name,
			Version:     s.Version,
			ChunkCount:  s.ChunkCount,
			TriggerExts: s.TriggerExtensions,
			DocType:     s.DocType,
		})
	}
	return result
}

// loadMilestones fetches the most emotionally impactful sessions across all time.
// Results are sorted chronologically (oldest first).
func (g *Generator) loadMilestones(narratives []NarrativeSummary) []MilestoneItem {
	// Exclude sessions already shown in narratives
	excludeIDs := make(map[string]bool)
	for _, n := range narratives {
		if n.SessionID != "" {
			excludeIDs[n.SessionID] = true
		}
	}

	learnings, err := g.store.GetMilestoneNarratives(10 + len(excludeIDs))
	if err != nil || len(learnings) == 0 {
		return nil
	}

	// Sort chronologically (oldest first) — query returns by intensity
	sort.Slice(learnings, func(i, j int) bool {
		return learnings[i].CreatedAt.Before(learnings[j].CreatedAt)
	})

	var items []MilestoneItem
	for _, l := range learnings {
		if l.SessionFlavor == "" || excludeIDs[l.SessionID] {
			continue
		}
		items = append(items, MilestoneItem{
			Ago:            relativeTime(g.strings, l.CreatedAt),
			Flavor:         truncateAtSentence(l.SessionFlavor, 300),
			IntensityLabel: g.intensityLabel(l.EmotionalIntensity),
		})
		if len(items) >= 3 {
			break
		}
	}
	return items
}

// intensityLabel converts a 0.0-1.0 intensity to a translated label.
func (g *Generator) intensityLabel(intensity float64) string {
	s := g.strings
	switch {
	case intensity >= 0.8:
		return s.IntensityBreakthrough
	case intensity >= 0.6:
		return s.IntensityIntense
	case intensity >= 0.4:
		return s.IntensityVivid
	default:
		return s.IntensityCalm
	}
}

// truncateAtSentence truncates text at the last sentence boundary before maxLen.
func truncateAtSentence(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	// Find last sentence-ending punctuation before maxLen
	lastEnd := -1
	for i, r := range runes[:maxLen] {
		if r == '.' || r == '!' || r == '?' || r == '—' {
			lastEnd = i
		}
	}
	if lastEnd > maxLen/3 { // only use if we keep at least 1/3 of the text
		return string(runes[:lastEnd+1])
	}
	// Fallback: cut at last space
	for i := maxLen - 1; i > maxLen/2; i-- {
		if runes[i] == ' ' {
			return string(runes[:i]) + "..."
		}
	}
	return string(runes[:maxLen]) + "..."
}

func (g *Generator) renderMilestones(s Strings, items []MilestoneItem) string {
	if len(items) == 0 {
		return ""
	}
	return renderTemplate("milestones", tmplMilestones, s, MilestoneData{Items: items})
}

func (g *Generator) renderOpenWork(s Strings, unfinished []models.Learning, absenceHours float64, triggerReasons map[int64]string) string {
	if len(unfinished) == 0 {
		return ""
	}

	isReminder := absenceHours >= 4

	// Count ideas and stale before filtering them out
	ideaCount := 0
	staleCount := 0
	const capIdeaThreshold = 3
	var capIdeas []models.Learning
	var actionable []models.Learning
	for _, l := range unfinished {
		switch l.TaskType {
		case "idea":
			ideaCount++
		case "stale":
			staleCount++
		case "cap_idea":
			// Codex: cap suggestions are surfaced separately from normal open tasks.
			if l.MatchCount >= capIdeaThreshold {
				capIdeas = append(capIdeas, l)
			}
		default: // "task", "blocked", "" (legacy)
			actionable = append(actionable, l)
		}
	}
	unfinished = actionable

	// Filter by importance when in reminder mode. Triggered deadlines survive:
	// an approaching deadline is urgent by definition, whatever its importance.
	if isReminder {
		var filtered []models.Learning
		for _, l := range unfinished {
			if l.Importance >= 3 {
				filtered = append(filtered, l)
				continue
			}
			if _, triggered := triggerReasons[l.ID]; triggered {
				filtered = append(filtered, l)
			}
		}
		unfinished = filtered
		if len(unfinished) == 0 && ideaCount == 0 && staleCount == 0 && len(capIdeas) == 0 {
			return ""
		}
	}

	// Prioritize deadline items — sort triggered items to front so they survive the limit
	sort.SliceStable(unfinished, func(i, j int) bool {
		_, iHasTrigger := triggerReasons[unfinished[i].ID]
		_, jHasTrigger := triggerReasons[unfinished[j].ID]
		if iHasTrigger != jHasTrigger {
			return iHasTrigger // triggered items first
		}
		return false // preserve existing order
	})

	// Limit: 5 items when long absence, 3 otherwise
	limit := 3
	if absenceHours >= 24 {
		limit = 5
	}
	if len(unfinished) > limit {
		unfinished = unfinished[:limit]
	}

	var items []OpenWorkItem
	for _, l := range unfinished {
		deadline := ""
		if reason, ok := triggerReasons[l.ID]; ok {
			// urgency-based: "morgen", "heute"; only a deadline that actually
			// reaches the rendered list may start the 12h cooldown.
			deadline = reason
			g.deadlineHits = append(g.deadlineHits, DeadlineHit{ID: l.ID, Content: l.Content})
		} else {
			deadline = formatDeadline(s, l.TriggerRule) // static, e.g. "Sat 28 March"
		}
		items = append(items, OpenWorkItem{
			Project:  l.Project,
			Content:  l.Content,
			Deadline: deadline,
		})
	}

	capIdeasBlock := ""
	if len(capIdeas) > 0 {
		capIdeasBlock = capIdeasText(s, capIdeas)
	}

	data := OpenWorkData{
		Items:      items,
		IsReminder: isReminder,
		IdeaCount:  ideaCount,
		StaleCount: staleCount,
	}
	if isReminder {
		data.AbsenceNote = formatAbsence(s, absenceHours)
	}

	result := capIdeasBlock + renderTemplate("openwork", tmplOpenWork, s, data)
	return result
}

// capIdeasText renders the cap suggestions block (Codex: kept visible even when
// no regular open-work item remains). The layout newlines and the
// remember(...)/resolve(...) calls stay here: a translated strings.yaml must
// never be able to rewrite a tool argument or collapse the block's spacing.
func capIdeasText(s Strings, capIdeas []models.Learning) string {
	var b strings.Builder
	b.WriteString("\n" + s.CapIdeasHeader + "\n\n")
	for _, c := range capIdeas {
		fmt.Fprintf(&b, "- [#%d] %s - seen %dx\n", c.ID, c.Content, c.MatchCount)
	}
	fmt.Fprintf(&b, "\n%s: `remember(text=\"<intent>\", category=\"unfinished\", task_type=\"task\", supersedes=<ID>)`\n", s.CapIdeasConfirmLabel)
	fmt.Fprintf(&b, "%s: `resolve(learning_id=<ID>)` — %s\n", s.CapIdeasDismissLabel, s.CapIdeasDismissNote)
	return b.String()
}

// calcAbsenceHours returns hours since the last session started.
// Returns 0 if no sessions exist (first session ever).
func calcAbsenceHours(sessions []models.Session) float64 {
	if len(sessions) == 0 {
		return 0
	}
	if sessions[0].StartedAt.IsZero() {
		return 0
	}
	return time.Since(sessions[0].StartedAt).Hours()
}

// formatAbsence returns the translated absence note, e.g. "2 days away".
func formatAbsence(s Strings, hours float64) string {
	switch {
	case hours < 24:
		return fmt.Sprintf(s.AbsenceHoursFmt, int(hours))
	case hours < 48:
		return s.AbsenceYesterday
	default:
		return fmt.Sprintf(s.AbsenceDaysFmt, int(hours/24))
	}
}

// formatDeadline extracts a human-readable deadline from a trigger_rule.
// Returns "" if trigger is not a deadline format.
func formatDeadline(s Strings, triggerRule string) string {
	if !strings.HasPrefix(triggerRule, "deadline:") {
		return ""
	}
	dateStr := strings.TrimPrefix(triggerRule, "deadline:")
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	// Layout, weekday and month come from the translated data, so the rendered
	// date follows the installation language instead of a fixed day.month order.
	layout := s.DeadlineDateFormat
	if layout == "" {
		layout = DefaultStrings().DeadlineDateFormat
	}
	return fmt.Sprintf(layout, weekdayShort(s, t.Weekday()), t.Day(), monthName(s, t.Month()))
}
