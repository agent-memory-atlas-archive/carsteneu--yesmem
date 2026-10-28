package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/hints"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// HookInput represents the JSON Claude Code sends on stdin for hook events.
type HookInput struct {
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	// ToolResponse carries what the tool returned on PostToolUse. A Bash
	// response is {stdout, stderr, interrupted, isImage}: no exit status, so it
	// can report the commit git created but not prove that the command succeeded.
	ToolResponse json.RawMessage `json:"tool_response"`
	// DurationMS is how long the tool took. It bounds the window the commit has
	// to fall into, so a long or chained command cannot widen it.
	DurationMS int `json:"duration_ms"`
}

// BashInput represents the tool_input for Bash tool calls.
type BashInput struct {
	Command string `json:"command"`
}

// FileInput represents the tool_input for Edit/Write tool calls.
type FileInput struct {
	FilePath string `json:"file_path"`
}

// REPLInput represents the tool_input for REPL tool calls (Claude Code internal).
// REPL executes JavaScript that can invoke sh(), put(), Edit(), Write(), etc.,
// so the full code body is the relevant surface for rule evaluation.
type REPLInput struct {
	Code string `json:"code"`
}

// WebFetchInput represents the tool_input for WebFetch tool calls.
type WebFetchInput struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

// buildWebFetchKeywords extracts matchable keywords from a WebFetch URL.
// Only uses the host — the tool name "webfetch" is excluded because it's
// always present for any WebFetch call (like "bash" for Bash), making it
// useless as a discriminator and causing false positive matches.
func buildWebFetchKeywords(rawURL string) []string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return extractKeywords(u.Host)
	}
	return nil
}

// matchedGotcha holds a gotcha with its match score for sorting.
type matchedGotcha struct {
	learning models.Learning
	score    int
	effScore float64
}

// blockThreshold is the fail_count at which a gotcha escalates from warn to hard block.
const blockThreshold = 5

// codeNavIndexer is the lookup the code-nav guard asks: it must answer for a file
// and for a directory operand, because a recursive grep and a find name a
// directory (YM-409). The indirection exists so a test can bind exactly the
// predicate the hook uses instead of a stub that answers anything.
func codeNavIndexer(store *storage.Store) func(string, string) bool {
	return store.IsTreeInCodeIndex
}

// codeNavCommands lists the shell lines one tool call submits to the guard: the
// Bash command itself, or every sh() body a REPL call contains. A REPL runs
// JavaScript, whose shell reach is its sh() calls, so the bodies are checked one
// by one.
func codeNavCommands(toolName, inputStr string) []string {
	switch toolName {
	case "Bash":
		return []string{inputStr}
	case "REPL":
		return ParseREPLNavCommands(inputStr)
	}
	return nil
}

// mcpToolPrefix is how Claude Code qualifies the yesmem MCP tools in the hook
// payload: the dialog call arrives as "mcp__yesmem__send_to", never bare (YM-158).
const mcpToolPrefix = "mcp__yesmem__"

// fileToolPath reads the target file of a file-mutating tool. NotebookEdit names
// it in notebook_path, the other tools in file_path.
func fileToolPath(toolName string, raw json.RawMessage) (string, bool) {
	if toolName == "NotebookEdit" {
		var nb struct {
			NotebookPath string `json:"notebook_path"`
		}
		if json.Unmarshal(raw, &nb) != nil || nb.NotebookPath == "" {
			return "", false
		}
		return nb.NotebookPath, true
	}
	var file FileInput
	if json.Unmarshal(raw, &file) != nil || file.FilePath == "" {
		return "", false
	}
	return file.FilePath, true
}

// RunCheck reads PreToolUse JSON from stdin, queries gotchas, outputs warning.
// Supports Bash, Edit, and Write tool calls.
// Gotchas with fail_count >= blockThreshold are hard-blocked (exit 2).
func RunCheck(dataDir string) {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var hook HookInput
	if json.Unmarshal(input, &hook) != nil {
		return
	}

	// Parse tool input to get inputStr (for save_count) and keywords (for matching).
	// MCP tools arrive qualified ("mcp__yesmem__send_to"), so the name is stripped
	// once here and every comparison below works on the bare name (YM-158).
	toolName := strings.TrimPrefix(hook.ToolName, mcpToolPrefix)
	var keywords []string
	var inputStr string
	var fileBase string
	var protectedHint string
	// codeNavHint carries the nudge-mode advice into every context this call may
	// produce: code_nav_mode "nudge" suggests, it does not block (YM-436).
	var codeNavHint string
	isFileOp := false
	switch toolName {
	case "send_to", "start_dialog", "end_dialog":
		// Dialog tools: register session_id so daemon knows who's calling
		// PreToolUse fires BEFORE the MCP call → daemon has the ID when send_to arrives
		if hook.SessionID != "" {
			RegisterPID(dataDir, hook.SessionID, os.Getppid())
			WritePIDFile(dataDir, hook.SessionID, os.Getppid())
		}
		emitReminder("")
		return
	case "Bash":
		var bash BashInput
		if json.Unmarshal(hook.ToolInput, &bash) != nil || bash.Command == "" {
			return
		}
		// The shell reaches the installation's own files too, and a redirect, a tee,
		// a copy or a move replaces their whole content just like a Write does. The
		// guard cannot see that from the command's keywords, so it asks the write
		// detector before any gotcha work (YM-310).
		if target, whole, ok := bashProtectedWrite(bash.Command, hook.CWD); ok {
			switch protectedWriteAction(target, whole) {
			case protectedBlock:
				blockEdit(target)
				return
			case protectedWarn:
				// The warning does not end the call: the same command may still carry an
				// escalated gotcha, and a hint must not preempt a block.
				protectedHint = settingsEditWarning(target)
			}
		}
		inputStr = bash.Command
		keywords = extractKeywords(bash.Command)
	case "REPL":
		var repl REPLInput
		if json.Unmarshal(hook.ToolInput, &repl) != nil || repl.Code == "" {
			return
		}
		// REPL runs JavaScript whose sh() reaches the shell, so a shell write to one
		// of the installation's own files is the write a Bash call performs. The
		// bodies are checked one by one: the first blocking write ends the call, a
		// warning is kept and the scan continues (YM-342). The REPL's own Write(),
		// Edit() and put() helpers are outside this check, as the ticket scopes the
		// REPL half to sh().
		for _, cmd := range los31eReplShellCommands(repl.Code) {
			target, whole, ok := bashProtectedWrite(cmd, hook.CWD)
			if !ok {
				continue
			}
			switch protectedWriteAction(target, whole) {
			case protectedBlock:
				blockEdit(target)
				return
			case protectedWarn:
				protectedHint = settingsEditWarning(target)
			}
		}
		inputStr = repl.Code
		keywords = extractKeywords(repl.Code)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		filePath, ok := fileToolPath(toolName, hook.ToolInput)
		if !ok {
			return
		}
		// The installation's own settings file: a whole-file Write drops whichever
		// yesmem keys the caller did not reproduce, a surgical edit can only touch
		// what it targets. Block the first, warn on the second (YM-269). MultiEdit is
		// the Edit case — its edits are old_string/new_string pairs — while a
		// NotebookEdit addresses a cell of its notebook. The target is resolved first
		// so a relative file_path cannot walk around the guard, and the resolved path
		// is what the messages echo (YM-310: both tool names fell into the default
		// branch and saw neither guard).
		target := resolveHookPath(filePath, hook.CWD)
		switch protectedWriteAction(target, toolName == "Write") {
		case protectedBlock:
			blockEdit(target)
			return
		case protectedWarn:
			// The warning does not end the call, for the same reason the Bash branch
			// keeps going: the same edit may still carry a gotcha, and a hint must
			// not preempt it (YM-342). The save-count bookkeeping that follows is the
			// same every Edit does.
			protectedHint = settingsEditWarning(target)
		}
		inputStr = filePath
		keywords = extractPathKeywords(filePath)
		fileBase = filepath.Base(filePath)
		isFileOp = true
	case "WebFetch":
		var wf WebFetchInput
		if json.Unmarshal(hook.ToolInput, &wf) != nil || wf.URL == "" {
			return
		}
		if u, err := url.Parse(wf.URL); err == nil && u.Host != "" {
			inputStr = "WebFetch " + u.Host
		} else {
			inputStr = "WebFetch"
		}
		keywords = buildWebFetchKeywords(wf.URL)
	default:
		return
	}

	// emit carries the protected-file hint and the code-nav nudge into every context
	// this call may produce. The hints are set for a shell write that only warns or
	// for a navigation that nudge mode does not block; the gotcha work below still
	// runs, so the same command can reach a hard block (YM-310, YM-436).
	emit := func(text string) {
		prefix := protectedHint
		if codeNavHint != "" {
			if prefix != "" {
				prefix += "\n---\n"
			}
			prefix += codeNavHint
		}
		switch {
		case prefix == "":
			emitReminder(text)
		case text == "":
			emitReminder(prefix)
		default:
			emitReminder(prefix + "\n---\n" + text)
		}
	}

	dbPath := filepath.Join(dataDir, "yesmem.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		emit("")
		return
	}
	defer store.Close()

	// Code-nav detection: block shell navigation of indexed project files. The
	// question is about the command and the index alone, so it runs before the
	// gotcha lookup and before the empty-keyword gate: a project without an active
	// gotcha returned above it and never blocked, and a call-site search such as
	// `grep -n x .` produces no keyword at all yet is exactly this navigation
	// (YM-431).
	// Only the shell tools reach these questions: a file op or a WebFetch must not
	// pay for a config read, an index key and a dismissal query.
	if cmds := codeNavCommands(toolName, inputStr); len(cmds) > 0 {
		codeNavMode := "block"
		cfgPath := filepath.Join(dataDir, "config.yaml")
		if cfg, err := config.Load(cfgPath); err == nil && cfg.Proxy.CodeNavMode != "" {
			codeNavMode = cfg.Proxy.CodeNavMode
		}
		// The value is hand-written in config.yaml, so a stray space or capital must
		// not silently fall through to the block branch: that is the trap this mode
		// exists to remove (YM-436). An unknown value still blocks.
		codeNavMode = strings.ToLower(strings.TrimSpace(codeNavMode))
		// The dismissal lifts the block, not the advice: a session that dismissed
		// navigation would otherwise get no hint in this layer while the opencode
		// plugin still delivers one — the split YM-442 closes (YM-442 review).
		if codeNavMode != "off" && (codeNavMode == "nudge" || !store.CodeNavDismissed(hook.SessionID)) {
			indexed := codeNavIndexer(store)
			project := projectFromCWD(hook.CWD)
			for _, cmd := range cmds {
				// nudge is the documented middle setting: the caller wants the advice
				// and keeps the call. Blocking here made the value do the opposite of
				// its name (YM-436); it also ended the call before the gotcha work
				// below, which a hint must not do.
				if codeNavMode == "nudge" {
					if hint, ok := NavHint(cmd, hook.CWD, project, indexed); ok && codeNavHint == "" {
						codeNavHint = hint
					}
					continue
				}
				if reason, block := CheckCodeNav(cmd, hook.CWD, project, hook.SessionID, indexed, false); block {
					blockCodeNav(reason)
					return
				}
			}
		}
	}

	if len(keywords) == 0 {
		emit("")
		return
	}

	// Check if previous gotcha prevented an error (save_count heuristic). Every tool
	// that reaches this point consumes the one-shot state, as before; whether it may
	// credit a save is decided inside checkSaveCount. The earlier returns above
	// (empty keywords, unreadable store, no active gotchas) skip the consumption, which
	// is the same gap they always had, and a code-nav block now returns before this
	// point as well: a blocked command is no approach change to credit (YM-431).
	if inputStr != "" {
		checkSaveCount(store, hook.SessionID, toolName, hashInput(inputStr))
	}

	// Derive canonical project from cwd for gotcha scoping (worktree-aware).
	// The code-nav check above keys its lookup by codescan.ScanCacheKey instead —
	// two stores, two key rules; they agree only at a repository root.
	gotchaProject := ""
	if hook.CWD != "" {
		gotchaProject = models.CanonicalProject(hook.CWD)
	}

	gotchas, err := store.GetActiveLearnings("gotcha", gotchaProject, "", "", 0)
	if err != nil || len(gotchas) == 0 {
		emit("")
		return
	}

	// Batch-load V2 junction data (entities + actions) for all gotchas
	ids := make([]int64, len(gotchas))
	for i, g := range gotchas {
		ids[i] = g.ID
	}
	entitiesMap := store.BatchLoadEntities(ids)
	actionsMap := store.BatchLoadActions(ids)
	for i := range gotchas {
		if ents, ok := entitiesMap[gotchas[i].ID]; ok {
			gotchas[i].Entities = ents
		}
		if acts, ok := actionsMap[gotchas[i].ID]; ok {
			gotchas[i].Actions = acts
		}
	}

	// Split matches into project-specific and global buckets
	// File ops use lower threshold: 1 match with filename (contains ".") suffices
	var projectMatches, globalMatches []matchedGotcha
	for _, g := range gotchas {
		// Skip gotchas with destructive actions that aren't relevant to this tool type.
		if !isRelevantForTool(&g, toolName) {
			continue
		}
		score := matchScore(keywords, g.Content)
		decay := injectionDecay(g.InjectCount, g.UseCount, g.SaveCount)
		effScore := float64(score) * decay
		matched := false
		if isFileOp {
			if isBashOriginAutoLearned(&g) {
				// A Bash failure transcript only mentions a file incidentally — the
				// pending Write/Edit is not running the failed command, so the file
				// name alone must not select it (issue #151). Require overlap that
				// does not come from the file name itself.
				matched = effScore >= 2.0 && hasNonFilenameKeywordMatch(keywords, g.Content, fileBase)
			} else {
				matched = effScore >= 2.0 || (effScore >= 1.0 && hasFileKeywordMatch(keywords, g.Content, fileBase))
			}
		} else {
			matched = effScore >= 2.0 || (effScore >= 1.0 && hasLongKeywordMatch(keywords, g.Content))
		}
		// V2: entity/action matching (much more precise)
		if !matched && matchesV2Metadata(&g, keywords) {
			matched = true
		}
		if !matched {
			continue
		}
		if effScore < 2.0 {
			effScore = 2.0
		}
		mg := matchedGotcha{learning: g, score: score, effScore: effScore}
		if gotchaProject != "" && models.ProjectMatches(g.CanonicalProject, gotchaProject) {
			projectMatches = append(projectMatches, mg)
		} else {
			globalMatches = append(globalMatches, mg)
		}
	}

	// Limits: 3 project + 2 global, if 0 project → 3 global
	maxProject := 3
	maxGlobal := 2
	if len(projectMatches) == 0 {
		maxGlobal = 3
	}

	sort.Slice(projectMatches, func(i, j int) bool { return projectMatches[i].effScore > projectMatches[j].effScore })
	sort.Slice(globalMatches, func(i, j int) bool { return globalMatches[i].effScore > globalMatches[j].effScore })
	if len(projectMatches) > maxProject {
		projectMatches = projectMatches[:maxProject]
	}
	if len(globalMatches) > maxGlobal {
		globalMatches = globalMatches[:maxGlobal]
	}

	// Filter out old info gotchas that are already in the session-start briefing.
	// Keep: (1) failure-based (FailCount>0), (2) new since session start, (3) file-specific (entity matches input),
	// (4) bypass if no session data.
	if hook.SessionID != "" {
		if sess, err := store.GetSession(hook.SessionID); err == nil && !sess.StartedAt.IsZero() {
			projectMatches = filterAlreadyBriefedGotchas(projectMatches, sess.StartedAt, inputStr)
			globalMatches = filterAlreadyBriefedGotchas(globalMatches, sess.StartedAt, "")
		}
	}

	if len(projectMatches) == 0 && len(globalMatches) == 0 {
		emit("")
		return
	}

	// Check for block-worthy gotchas (fail_count >= threshold, auto-learned,
	// not imported; see findBlockableGotcha)
	allMatches := append(projectMatches, globalMatches...)

	// A hard block decides the pending call, it is not an injection: the
	// per-session cap below must not hide it. Otherwise an escalated gotcha stopped
	// blocking as soon as it had been injected three times, and the command it
	// warns about ran silently. The cap was also the only automatic limit on
	// repetition, so an escalated gotcha now blocks every matching call until it is
	// resolved or superseded; that is what an escalation means (YM-342).
	if bg := findBlockableGotcha(allMatches); bg != nil {
		store.IncrementMatchCounts([]int64{bg.learning.ID})
		store.IncrementInjectCounts([]int64{bg.learning.ID})
		blockGotchaCall(bg.learning)
		return
	}

	// Per-session cap: skip gotchas already shown 3+ times this session.
	// Reset on session restart (new SessionID → new keys). Global injectionDecay
	// still handles cross-session staleness independently.
	if hook.SessionID != "" && len(allMatches) > 0 {
		var capped []matchedGotcha
		for _, mg := range allMatches {
			key := fmt.Sprintf("gotcha_count:%s:%d", hook.SessionID, mg.learning.ID)
			countStr, _ := store.GetProxyState(key)
			count, _ := strconv.Atoi(countStr)
			if count >= 3 {
				continue
			}
			capped = append(capped, mg)
		}
		allMatches = capped
	}

	// Build tiered output: top-1 full text, rest as summary
	text, injectedIDs, matchedIDs := buildGotchaOutput(allMatches)

	// Bump counts: match for all, inject only for fully injected top match
	allIDs := make([]int64, 0, len(injectedIDs)+len(matchedIDs))
	allIDs = append(allIDs, injectedIDs...)
	allIDs = append(allIDs, matchedIDs...)
	store.IncrementMatchCounts(allIDs)
	store.IncrementInjectCounts(injectedIDs)

	// Persist state for save_count heuristic. Only an eligible tool creates a
	// pending state; the next call consumes it regardless of its own eligibility,
	// so the one-shot adjacency is the same as before.
	if len(injectedIDs) > 0 && saveCountEligible(toolName) {
		idsJSON, _ := json.Marshal(injectedIDs)
		store.SetProxyState(saveCountStateKey("last_gotcha_ids", hook.SessionID), string(idsJSON))
		store.SetProxyState(saveCountStateKey("last_gotcha_tool", hook.SessionID), toolName)
		store.SetProxyState(saveCountStateKey("last_gotcha_input_hash", hook.SessionID), hashInput(inputStr))
	} else {
		store.SetProxyState(saveCountStateKey("last_gotcha_ids", hook.SessionID), "")
	}

	// Per-session cap: increment injection counter for each injected gotcha. This
	// counter is not part of the save-count heuristic and runs for every tool.
	for _, id := range injectedIDs {
		key := fmt.Sprintf("gotcha_count:%s:%d", hook.SessionID, id)
		countStr, _ := store.GetProxyState(key)
		count, _ := strconv.Atoi(countStr)
		store.SetProxyState(key, strconv.Itoa(count+1))
	}

	emit(text)
}

// hashInput returns a deterministic 16-char hex hash of the input string.
func hashInput(input string) string {
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:8])
}

// saveCountStateKey namespaces the one-shot save-count state by session. The keys
// were global, so two parallel sessions read each other's pending gotcha and the
// one that ran a different command afterwards counted a save for a gotcha it never
// saw (YM-161). An empty session id keeps the key shared, as it was before.
func saveCountStateKey(name, sessionID string) string {
	if sessionID == "" {
		return name
	}
	return name + ":" + sessionID
}

// saveCountEligible reports whether a changed input for this tool means the agent
// revised its approach after seeing a gotcha. Two tools are excluded because their
// input says nothing about the approach: WebFetch carries only the host, so
// fetching an unrelated site looked like a changed approach (the audit measured 322
// rows, 141 of them carrying a save from a plain host switch, YM-269), and REPL
// carries its own code body on every call, so a different body is the normal case
// rather than an answer to the warning (YM-310). The heuristic asks whether the
// warning changed what we did; neither a different URL nor a different script
// answers that question.
func saveCountEligible(toolName string) bool {
	switch strings.ToLower(toolName) {
	case "webfetch", "repl":
		return false
	}
	return true
}

// checkSaveCount checks if a previous gotcha warning prevented an error.
// If the same tool type was used but with a DIFFERENT input hash, the user
// changed their approach after seeing the warning → save_count++.
// State is always cleared after check (one-shot).
func checkSaveCount(store *storage.Store, sessionID, toolName, inputHash string) {
	idsKey := saveCountStateKey("last_gotcha_ids", sessionID)
	prevIDs, _ := store.GetProxyState(idsKey)
	if prevIDs == "" || prevIDs == "[]" {
		return
	}
	prevTool, _ := store.GetProxyState(saveCountStateKey("last_gotcha_tool", sessionID))
	prevHash, _ := store.GetProxyState(saveCountStateKey("last_gotcha_input_hash", sessionID))

	// Same tool type but different input → user changed approach → save!
	if toolName == prevTool && inputHash != prevHash && saveCountEligible(toolName) {
		var ids []int64
		json.Unmarshal([]byte(prevIDs), &ids)
		if len(ids) > 0 {
			store.IncrementSaveCounts(ids)
		}
	}
	// Clear after check (one-shot — regardless of outcome)
	store.SetProxyState(idsKey, "")
}

func buildGotchaOutput(matches []matchedGotcha) (text string, injectedIDs []int64, matchedIDs []int64) {
	if len(matches) == 0 {
		return "", nil, nil
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].effScore > matches[j].effScore })
	top := matches[0]
	injectedIDs = append(injectedIDs, top.learning.ID)
	var buf strings.Builder
	buf.WriteString("YesMem Gotchas:\n- ")
	buf.WriteString(top.learning.Content)
	if len(matches) > 1 {
		rest := matches[1:]
		for _, mg := range rest {
			matchedIDs = append(matchedIDs, mg.learning.ID)
		}
		buf.WriteString(fmt.Sprintf("\n+%d more — use query_facts(keyword=\"...\") for details", len(rest)))
	}
	return buf.String(), injectedIDs, matchedIDs
}

func emitReminder(gotchaText string) {
	if gotchaText == "" {
		return
	}
	tsHint := hints.NextTimestampHint()
	text := gotchaText + "\n---\n" + tsHint
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PreToolUse",
			"additionalContext": text,
		},
	}
	jsonOut, _ := json.Marshal(out)
	fmt.Print(string(jsonOut))
}

// protectedFiles lists auto-generated files that must not be edited directly.
// Fix the source (learnings via remember/supersede) instead. The list carries the
// default name; a target renamed through config.yaml (output_file) is not known
// here and stays unprotected (YM-342 scope).
var protectedFiles = []string{
	"yesmem-ops.md",
}

// protectedFileDir is the directory the generated files live in, relative to the
// project: claudemd writes <projectDir>/.claude/<output_file>. A file of the same
// name elsewhere is not the generated one (YM-342).
const protectedFileDir = ".claude"

// los31eReplShellCommands returns the shell commands a REPL code body runs through
// sh(). Only a literal call outside a comment or another literal counts: a quoted
// or commented example is documentation, and a hard block must not grow out of one
// (the Bash path excludes a quoted command for the same reason). Regular
// expressions get their own state, because a pattern may carry a quote or "/*".
// The body is read with its backslash escapes resolved, so a quoted path inside it
// still names its file.
//
// Shapes that stay out of reach and are accepted as gaps: a body that is a
// variable (sh(cmd)), a body built by concatenation (sh("echo x > " + p), where
// the scanner reads only the first literal even though two literals would be
// statically resolvable), a call inside the ${...} expression of a template
// literal, which the scanner treats as text, and a pattern right after a closing
// parenthesis ("if (x) /re/"), which reads as division and lets the scan drift.
// The first, third and fourth cannot be judged without running the code.
// ParseREPLNavCommands narrows the same call shape to navigation commands; the
// write guard needs every body (YM-342).
func los31eReplShellCommands(code string) []string {
	var cmds []string
	for i := 0; i < len(code); {
		switch c := code[i]; {
		case c == '/' && i+1 < len(code) && code[i+1] == '/':
			end := strings.IndexByte(code[i:], '\n')
			if end < 0 {
				return cmds
			}
			i += end + 1
		case c == '/' && i+1 < len(code) && code[i+1] == '*':
			end := strings.Index(code[i+2:], "*/")
			if end < 0 {
				// An unterminated block comment hides the rest of the body, which is
				// what the language says as well; there is no code left to check.
				return cmds
			}
			i += end + 4
		case c == '/':
			// A regular expression may carry a quote or "/*" in its pattern, and without
			// its own state a pattern like /['"]/ desynchronizes the scan and swallows
			// the call (YM-342).
			if los31eRegexAt(code, i) {
				if end := los31eSkipRegex(code, i); end > i {
					i = end
					continue
				}
			}
			i++
		case c == '\'' || c == '"' || c == '`':
			// An unmatched quote is not a literal the scanner understands; resync one
			// byte later instead of abandoning the rest of the body, so a stray
			// apostrophe cannot switch the guard off.
			if end := los31eSkipJSString(code, i); end > i {
				i = end
			} else {
				i++
			}
		case c == 's':
			if body, next, ok := los31eShCallAt(code, i); ok {
				cmds = append(cmds, body)
				i = next
				continue
			}
			i++
		default:
			i++
		}
	}
	return cmds
}

// los31eRegexAt reports whether the "/" at i opens a regular expression rather
// than a division. ECMAScript decides that from the previous token; this is the
// usual punctuation-and-keyword heuristic, and it only has to protect the scan from
// reading a pattern's quotes as code, so an unusual spelling may still be missed in
// the conservative direction.
//
// A closing parenthesis is deliberately not in the set: "if (x) /re/" and
// "(a + b) / 2" are the same bytes up to the slash, and the division reading is the
// one that keeps scanning the rest, which is what a guard needs (YM-342).
func los31eRegexAt(code string, i int) bool {
	j := los31eSkipBackTrivia(code, i)
	if j < 0 {
		return true // a leading "/" opens the body, so it is a pattern
	}
	// The pattern opens a line, so a statement starts here and the slash cannot
	// divide. That also covers "return // note" on the line before (YM-342).
	if strings.ContainsRune(code[j+1:i], '\n') {
		return true
	}
	if los31eNameRune(rune(code[j])) {
		start := j
		for start > 0 && isWordByte(code[start-1]) {
			start--
		}
		// A word behind a dot is a property, not a keyword: "obj.do / 2" divides.
		// The dot may stand behind trivia (YM-342).
		if p := los31eSkipBackTrivia(code, start); p >= 0 && code[p] == '.' {
			return false
		}
		word := code[start : j+1]
		for _, kw := range los31eRegexKeywords {
			if word == kw {
				return true
			}
		}
		return false
	}
	// A doubled sign is a postfix increment or decrement, so the slash after it
	// divides: "t++ / 2" (YM-342).
	if (code[j] == '+' || code[j] == '-') && j > 0 && code[j-1] == code[j] {
		return false
	}
	return strings.ContainsRune("(,=:[!&|?{};+-*%~^<>", rune(code[j]))
}

// los31eSkipBackTrivia returns the offset of the last byte before i that is
// neither whitespace nor part of a block comment, or -1 when only trivia precedes
// i. Line comments need no handling: the forward scan never reaches a slash inside
// one (YM-342).
func los31eSkipBackTrivia(code string, i int) int {
	for {
		for i > 0 {
			r, size := utf8.DecodeLastRuneInString(code[:i])
			if !unicode.IsSpace(r) {
				break
			}
			i -= size
		}
		if i >= 2 && code[i-1] == '/' && code[i-2] == '*' {
			open := strings.LastIndex(code[:i-2], "/*")
			if open < 0 {
				return -1
			}
			i = open
			continue
		}
		return i - 1
	}
}

// los31eRegexKeywords are the words after which a "/" cannot divide (return /re/,
// typeof /re/).
var los31eRegexKeywords = []string{
	"return", "typeof", "instanceof", "in", "of", "case", "do", "else",
	"yield", "await", "void", "delete", "new", "throw",
}

// los31eSkipRegex returns the offset just behind the pattern that starts at i, or
// -1 when it does not close on the same line. A slash inside a character class
// and an escaped slash do not end the pattern.
func los31eSkipRegex(code string, i int) int {
	inClass := false
	for j := i + 1; j < len(code); j++ {
		switch code[j] {
		case '\\':
			j++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '\n':
			return -1
		case '/':
			if !inClass {
				return j + 1
			}
		}
	}
	return -1
}

// los31eSkipJSString returns the offset just behind the string literal that starts
// at i, or -1 for an unterminated literal. A backslash escapes the next byte, so
// \" does not close the string. A single and a double quoted literal end at the
// line, which keeps a misread quote from swallowing the rest of the body; only a
// template literal spans lines (YM-342).
func los31eSkipJSString(code string, i int) int {
	quote := code[i]
	for j := i + 1; j < len(code); j++ {
		switch code[j] {
		case '\\':
			j++
		case '\n':
			if quote != '`' {
				return -1
			}
		case quote:
			return j + 1
		}
	}
	return -1
}

// los31eShCallAt reads a "sh(<literal>)" call whose identifier starts at i. It
// requires a word boundary on both sides of the name, so "push(" and "shell(" do
// not qualify, and a member of another object does not either: "obj.sh(" is not the
// shell, while the named globals (globalThis, window, self, this) carry the same
// function and do count (YM-342). It only accepts a quoted body, because the shell
// command of a variable cannot be judged without running the code. The optional
// call sh?.(...) is the same call in ECMAScript.
func los31eShCallAt(code string, i int) (body string, next int, ok bool) {
	if !los31eShCallee(code, i) {
		return "", 0, false
	}
	j := i + 2
	if j > len(code) {
		return "", 0, false
	}
	if j < len(code) {
		r, _ := utf8.DecodeRuneInString(code[j:])
		if los31eNameRune(r) {
			return "", 0, false
		}
	}
	j = los31eSkipTrivia(code, j)
	if j+1 < len(code) && code[j] == '?' && code[j+1] == '.' {
		j = los31eSkipTrivia(code, j+2)
	}
	if j >= len(code) || code[j] != '(' {
		return "", 0, false
	}
	// Redundant grouping around the body is still the same literal:
	// sh(("echo x > f")) (YM-342).
	for j < len(code) && code[j] == '(' {
		j = los31eSkipTrivia(code, j+1)
	}
	if j >= len(code) || (code[j] != '"' && code[j] != '\'' && code[j] != '`') {
		return "", 0, false
	}
	quote := code[j]
	j++
	var b strings.Builder
	for j < len(code) {
		c := code[j]
		if c == '\\' && j+1 < len(code) {
			// A backslash before a line break is a line continuation in JavaScript:
			// both bytes disappear, so the shell sees one word (YM-375). Writing the
			// break alone would leave a command separator where the shell joins. U+2028
			// and U+2029 are line terminators too (ECMAScript LineTerminatorSequence).
			next := code[j+1]
			if next == '\n' || next == '\r' {
				j += 2
				if next == '\r' && j < len(code) && code[j] == '\n' {
					j++
				}
				continue
			}
			if next == 0xE2 && j+3 < len(code) && code[j+2] == 0x80 && (code[j+3] == 0xA8 || code[j+3] == 0xA9) {
				j += 4
				continue
			}
			b.WriteByte(next)
			j += 2
			continue
		}
		if c == quote {
			return b.String(), j + 1, true
		}
		b.WriteByte(c)
		j++
	}
	return "", 0, false
}

// los31eSkipSpace returns the offset of the first rune that is not whitespace.
// ECMAScript accepts every Unicode space, the non-breaking space included, so an
// ASCII-only test would miss the call (YM-342).
func los31eSkipSpace(code string, i int) int {
	for i < len(code) {
		r, size := utf8.DecodeRuneInString(code[i:])
		if !unicode.IsSpace(r) {
			return i
		}
		i += size
	}
	return i
}

// los31eSkipTrivia returns the offset of the first byte that is neither whitespace
// nor a comment. A comment between the name and its body is legal ECMAScript, and
// without this the call reads as bodyless and the guard stays silent (YM-342).
func los31eSkipTrivia(code string, i int) int {
	for i < len(code) {
		i = los31eSkipSpace(code, i)
		switch {
		case i+1 < len(code) && code[i] == '/' && code[i+1] == '/':
			end := strings.IndexByte(code[i:], '\n')
			if end < 0 {
				return len(code)
			}
			i += end + 1
		case i+1 < len(code) && code[i] == '/' && code[i+1] == '*':
			end := strings.Index(code[i+2:], "*/")
			if end < 0 {
				return len(code)
			}
			i += end + 4
		default:
			return i
		}
	}
	return i
}

// los31eNameRune reports whether r can continue an identifier: an ASCII word byte
// or a non-ASCII letter, so "ésh(" is the other name and not the call. Non-ASCII
// whitespace is not part of a name; it only separates the name from its
// parenthesis (YM-342).
func los31eNameRune(r rune) bool {
	if r < utf8.RuneSelf {
		return isWordByte(byte(r))
	}
	return unicode.IsLetter(r)
}

// los31eShCallee reports whether the identifier at i names the shell: not the tail
// of a longer name (push, ésh) and not another object's member (obj.sh). The
// global object carries the same function, so the named globals count (YM-342). The
// byte right before the name decides; a member's dot cannot be separated from it.
func los31eShCallee(code string, i int) bool {
	if i == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(code[:i])
	if los31eNameRune(prev) {
		return false
	}
	if prev != '.' || i < 2 {
		return prev != '.'
	}
	end := i - 2
	if end >= 0 && code[end] == '?' {
		end--
	}
	start := end + 1
	for start > 0 && isWordByte(code[start-1]) {
		start--
	}
	switch code[start : end+1] {
	case "globalThis", "global", "self", "window", "this":
		return true
	}
	return false
}

// protectedSettingsPath is the installation's own Claude Code settings file,
// relative to the home directory. `yesmem setup` writes the hook registration, the
// mcpServers entry and the proxy environment block into it, so a rewrite through
// the Edit or Write tool drops whatever the caller does not reproduce, and with the
// hook registration it drops the guard itself (YM-269).
var protectedSettingsPath = filepath.Join(".claude", "settings.json")

// isProtectedFile checks if a file path names one of the auto-generated files in
// the directory they are generated into. The generator writes
// <projectDir>/.claude/<output_file> (claudemd.outputPathFor), so a file of the
// same name elsewhere is not the generated one and stays editable (YM-342).
//
// Two aliases count as the generated directory, because the earlier basename rule
// caught them and they write the same file: a directory that resolves to .claude
// through a symlink, and a case variant of the name, which is the same path on the
// case-insensitive filesystems macOS and Windows ship. On Linux the case rule errs
// towards protecting. A path whose directory cannot be resolved stays unprotected:
// a bare "yesmem-ops.md" says nothing about where it lies, and the tool payload
// carries an absolute path with a cwd in practice. That also loses a symlinked
// alias the call itself creates (`ln -s ~/.claude x && echo > x/yesmem-ops.md`): at
// check time the link does not exist yet, so the path does not resolve. The
// installation's settings file is unaffected, it is decided by isProtectedSettingsFile
// and does not hang on this predicate (YM-342).
//
// The installation's settings file is NOT part of this predicate: it is decided at
// the call site, because a whole-file Write is blocked while a surgical Edit only
// warns (see isProtectedSettingsFile).
func isProtectedFile(filePath string) bool {
	clean := filepath.Clean(filePath)
	if !protectedFileName(filepath.Base(clean)) {
		return false
	}
	dir := filepath.Dir(clean)
	if strings.EqualFold(filepath.Base(dir), protectedFileDir) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Base(resolved), protectedFileDir)
}

// protectedFileName reports whether base names one of the generated files.
func protectedFileName(base string) bool {
	for _, p := range protectedFiles {
		if strings.EqualFold(base, p) {
			return true
		}
	}
	return false
}

// resolveHookPath returns the absolute form of a tool input path. The hook payload
// carries absolute paths in practice, but a relative one is resolved against the
// session cwd so the protected-path comparison cannot be walked around, and the
// result is cleaned so a message never echoes the discarded segments of a path. The
// home shorthands are expanded here, the same three a shell command may use: a
// write to "$HOME/.claude/settings.json" targets the same file as "~/" does.
func resolveHookPath(path, cwd string) string {
	expanded := path
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		switch {
		case expanded == "~":
			// A bare "~" is the home directory itself, the target of "cd ~". Without
			// this case it stayed a relative directory named "~" under the cwd and a
			// write behind it compared against a path that does not exist (YM-387).
			expanded = home
		case strings.HasPrefix(expanded, "~/"):
			expanded = filepath.Join(home, expanded[len("~/"):])
		case strings.HasPrefix(expanded, "$HOME/"):
			expanded = filepath.Join(home, expanded[len("$HOME/"):])
		case strings.HasPrefix(expanded, "${HOME}/"):
			expanded = filepath.Join(home, expanded[len("${HOME}/"):])
		}
	}
	// Clean runs after the expansion: cleaning first would eat the "~" as a plain
	// segment and lose the home anchor.
	cleaned := filepath.Clean(expanded)
	if !filepath.IsAbs(cleaned) && cwd != "" {
		cleaned = filepath.Join(cwd, cleaned)
	}
	return cleaned
}

// isProtectedSettingsFile reports whether filePath is this installation's
// ~/.claude/settings.json. The path goes through resolveHookPath without a cwd, so
// the home shorthands are expanded and the result cleaned in one place. RunCheck
// resolves the tool input the same way before asking, so a path that only becomes
// absolute through the cwd matches too.
func isProtectedSettingsFile(filePath string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	return resolveHookPath(filePath, "") == filepath.Join(home, protectedSettingsPath)
}

// settingsEditWarning is the PreToolUse context for a surgical Edit of
// ~/.claude/settings.json. The file is warned about rather than blocked: an Edit
// touches only what it targets, while a whole-file Write is what loses keys.
func settingsEditWarning(filePath string) string {
	return fmt.Sprintf(
		"YesMem: %s carries the keys that 'yesmem setup' writes (hook registration, "+
			"MCP server entry, proxy environment). Change only the key you mean, and "+
			"run 'yesmem setup' afterwards if the guard hook or the MCP entry is gone.",
		filePath,
	)
}

// blockMinScore is the minimum match score for a gotcha to escalate to a hard block.
// Warning threshold is score >= 2; blocking requires stronger evidence to avoid
// cross-matching unrelated commands that share generic keywords.
const blockMinScore = 4

// isBashOriginAutoLearned reports whether g is a gotcha auto-learned from a Bash
// failure. Hook-learned rows do not record their origin reliably: the Go
// producer (learnFromAnyFailure) leaves OriginTool empty, and the daemon
// defaults a missing origin to "user" (handler_learnings.go). The origin is
// therefore taken from OriginTool when it carries a recognised label and
// otherwise from the transcript prefix the producers write.
func isBashOriginAutoLearned(g *models.Learning) bool {
	if g.Source != "hook_auto_learned" {
		return false
	}
	switch strings.ToLower(g.OriginTool) {
	case "bash", "bash_command_input":
		return true
	}
	return textutil.FailureOriginTool(g.Content) == "bash"
}

// isRelevantForTool checks whether a gotcha's actions make it relevant for the current tool.
// Gotchas with destructive/bash-only actions (rm, mv, git push, make, deploy) only fire on Bash/Write/Edit.
// Gotchas with no actions are always relevant (backward compatible).
// Gotchas with non-destructive actions fire on all tools.
func isRelevantForTool(g *models.Learning, toolName string) bool {
	if len(g.Actions) == 0 {
		return true
	}
	// Tools that can perform destructive/file operations. REPL runs JavaScript that
	// calls sh(), Write() and Edit(), so a gotcha about deleting or deploying is as
	// relevant there as it is on Bash (YM-269). MultiEdit and NotebookEdit write
	// files as well (YM-310).
	toolName = strings.ToLower(toolName)
	fileOpTools := map[string]bool{
		"bash": true, "write": true, "edit": true, "repl": true,
		"multiedit": true, "notebookedit": true,
	}
	if !fileOpTools[toolName] {
		for _, a := range g.Actions {
			if actionMentionsDangerousVerb(a) {
				return false
			}
		}
	}
	return true
}

// matchesV2Metadata reports whether one of the gotcha's entities or actions names
// one of the extracted keywords. Entities match in both directions — the entity
// may be the more specific side ("internal/hooks/check.go" vs "hooks") — while an
// action is only ever the container the keyword sits in.
func matchesV2Metadata(g *models.Learning, keywords []string) bool {
	if !g.IsV2() {
		return false
	}
	for _, entity := range g.Entities {
		for _, kw := range keywords {
			if entityKeywordMatch(entity, kw) {
				return true
			}
		}
	}
	for _, action := range g.Actions {
		actionLower := strings.ToLower(action)
		for _, kw := range keywords {
			if containsOnTokenBoundary(actionLower, kw) {
				return true
			}
		}
	}
	return false
}

// findBlockableGotcha returns the first match whose fail_count >= blockThreshold,
// match score >= blockMinScore, AND was auto-learned from an actual failure, or nil.
// Uses fail_count (real failures) not hit_count (view count from warnings).
// Rows imported from a file are excluded: their counters were observed in another
// installation, and a block exits the tool call with status 2, so a crafted
// export could otherwise make every Bash call unfixable (the remediation message
// itself suggests a tool call that would be blocked too).
func findBlockableGotcha(matches []matchedGotcha) *matchedGotcha {
	for i := range matches {
		if matches[i].score >= blockMinScore &&
			matches[i].learning.FailCount >= blockThreshold &&
			matches[i].learning.Source == "hook_auto_learned" &&
			matches[i].learning.OriginTool != "import" {
			return &matches[i]
		}
	}
	return nil
}

// blockGotchaCall outputs a block response for a gotcha that exceeded the fail threshold.
func blockGotchaCall(g models.Learning) {
	reason := fmt.Sprintf(
		"BLOCKED: This error occurred %dx — automatic block.\n"+
			"Gotcha: %s\n"+
			"Use hybrid_search() to find alternatives, "+
			"or resolve_by_text() if the issue is resolved.",
		g.FailCount, g.Content,
	)
	out := map[string]any{
		"decision": "block",
		"reason":   reason,
	}
	jsonOut, _ := json.Marshal(out)
	fmt.Print(string(jsonOut))
	os.Exit(2)
}

// blockEdit outputs a JSON response that blocks the tool call (exit 2).
func blockEdit(filePath string) {
	reason := fmt.Sprintf(
		"BLOCKED: %s is auto-generated from learnings. "+
			"Use remember() with supersedes parameter to update the source learning, "+
			"then regenerate via 'yesmem claudemd'.",
		filepath.Base(filePath),
	)
	if isProtectedSettingsFile(filePath) {
		reason = fmt.Sprintf(
			"BLOCKED: %s carries the keys that 'yesmem setup' writes: the hook "+
				"registration, the MCP server entry and the proxy environment block. "+
				"A whole-file write drops whichever of them you did not reproduce, and "+
				"with the hook registration the gotcha guard itself. Let the "+
				"installation write it ('yesmem setup'), or edit only the key you need.",
			filePath,
		)
	}
	out := map[string]any{
		"decision": "block",
		"reason":   reason,
	}
	jsonOut, _ := json.Marshal(out)
	fmt.Print(string(jsonOut))
	os.Exit(2)
}

// blockCodeNav outputs a JSON response that blocks a shell navigation command
// when yesmem MCP tools should be used instead.
func blockCodeNav(reason string) {
	out := map[string]any{
		"decision": "block",
		"reason":   reason,
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PreToolUse",
			"additionalContext": reason,
		},
	}
	jsonOut, _ := json.Marshal(out)
	fmt.Print(string(jsonOut))
	os.Exit(2)
}

// filterAlreadyBriefedGotchas removes gotchas that are already present in the
// session-start briefing. Keeps: failure-based gotchas (FailCount>0 — real errors),
// gotchas created since session start (new discoveries), and file-specific gotchas
// (entity matches the current input, e.g. file path for Edit/Write). Old info-level
// gotchas are redundant with the briefing and get skipped.
func filterAlreadyBriefedGotchas(matches []matchedGotcha, sessionStart time.Time, inputStr string) []matchedGotcha {
	var filtered []matchedGotcha
	for _, m := range matches {
		if m.learning.FailCount > 0 {
			filtered = append(filtered, m)
		} else if m.learning.CreatedAt.After(sessionStart) {
			filtered = append(filtered, m)
		} else if inputStr != "" && entitiesMatchInput(m.learning.Entities, inputStr) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// entitiesMatchInput reports whether any entity names part of the input. The
// input is a whole command or path, so the entity is the nested side and counts
// only on token boundaries — "sh" inside "push" is not a match (YM-159).
func entitiesMatchInput(entities []string, input string) bool {
	inputLower := strings.ToLower(input)
	for _, e := range entities {
		if entityKeywordMatch(e, inputLower) {
			return true
		}
	}
	return false
}

// projectFromCWD extracts the key the code index is stored under: the same key
// the scanner writes with (codescan.ScanCacheKey). A second heuristic here
// would drift — a linked worktree is its own checkout and resolves to its own
// root, the main checkout to its own, so both read the index of the tree their
// commands see (YM-453). A cwd below either ascends to that checkout.
func projectFromCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	return codescan.ScanCacheKey(cwd)
}
