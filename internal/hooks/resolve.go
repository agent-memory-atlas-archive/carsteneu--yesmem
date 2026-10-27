package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// commitCheckTimeout bounds the git call: the hook runs synchronously in Claude
// Code's PostToolUse pipeline, so a git that never returns would block the turn.
const commitCheckTimeout = 2 * time.Second

// commitFreshSlack widens the command window at its start: git writes the commit
// object at the end of the command, so HEAD can only be older than the window by
// clock granularity.
const commitFreshSlack = 10 * time.Second

// headState is what the repository says about HEAD.
type headState struct {
	sha         string
	subject     string
	committedAt time.Time
	// operation is the reflog verb that produced HEAD ("commit", "cherry-pick",
	// …). A "[branch sha] subject" line printed by a cherry-pick or a revert
	// describes HEAD without committing for this command.
	operation string
}

// RunResolveCheck reads PostToolUse JSON from stdin.
// If the tool was a git commit, checks the commit message against
// active unfinished items and resolves matches.
// Always exits 0 (never blocks).
func RunResolveCheck(dataDir string) {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	if out := resolveCheckOutput(dataDir, input); out != "" {
		outputHookResult(out)
	}
}

// resolveCheckOutput is the testable core of the hook: it returns the line to
// hand back to Claude, or "" when the command closes nothing.
func resolveCheckOutput(dataDir string, input []byte) string {
	var hook HookInput
	if json.Unmarshal(input, &hook) != nil {
		return ""
	}
	if hook.ToolName != "Bash" {
		return ""
	}

	var bash BashInput
	if json.Unmarshal(hook.ToolInput, &bash) != nil {
		return ""
	}
	if !isGitCommit(bash.Command) {
		return ""
	}
	msg := extractCommitMessage(bash.Command)
	if msg == "" {
		return ""
	}

	cwd := hook.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	dir := commitDir(bash.Command, cwd)
	// The window the commit has to fall into is the command's own runtime: the
	// payload reports it, so a long pre-commit suite or a chained command cannot
	// push the commit object out of the window.
	since := time.Now()
	if hook.DurationMS > 0 {
		since = since.Add(-time.Duration(hook.DurationMS) * time.Millisecond)
	}
	if !commitLanded(hook.ToolResponse, dir, msg, since) {
		return ""
	}

	// The witness is the commit's directory, the scope is the repository it
	// belongs to: a commit from a subdirectory closes the project's task.
	scope := repoRoot(dir)
	if scope == "" {
		scope = dir
	}

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		return ""
	}
	defer store.Close()
	return resolveMatchingTask(store, msg, scope)
}

// commandSegments splits a shell command at its chain operators, so that a later
// command in the same line cannot shadow the git call.
func commandSegments(command string) []string {
	return strings.FieldsFunc(command, func(r rune) bool {
		return r == '&' || r == ';' || r == '|' || r == '\n'
	})
}

// commitDir is the directory the commit command operates on: a `git -C <dir>`
// global option wins, then a `cd <dir>` in an earlier segment, then the session
// cwd. Only the segment whose verb is commit decides, so an earlier
// `git add`/`git status` in the same line cannot shadow it, and only the region
// before the verb is read, so a -C inside the message stays a message.
func commitDir(command, cwd string) string {
	dir := cwd
	for _, segment := range commandSegments(command) {
		fields := strings.Fields(segment)
		gitIdx := -1
		for i, field := range fields {
			if isGitToken(field) {
				gitIdx = i
				break
			}
			// `cd <dir> && git …`: the cd sits in an earlier segment, so the
			// directory carries forward to the segment that runs git.
			if field == "cd" && i+1 < len(fields) {
				dir = resolveCommandPath(dir, fields[i+1])
			}
		}
		if gitIdx < 0 {
			continue
		}
		segmentDir := dir
		verb := ""
		for j := gitIdx + 1; j < len(fields); {
			field := fields[j]
			if !strings.HasPrefix(field, "-") {
				verb = field
				break
			}
			if field == "-C" {
				if j+1 < len(fields) {
					segmentDir = resolveCommandPath(segmentDir, fields[j+1])
				}
				j += 2
				continue
			}
			// These options take the next token as their value.
			if field == "-c" || field == "--git-dir" || field == "--work-tree" || field == "--namespace" {
				j += 2
				continue
			}
			j++
		}
		if verb == "commit" {
			return segmentDir
		}
	}
	return dir
}

// resolveCommandPath turns a shell path argument into an absolute path.
func resolveCommandPath(base, path string) string {
	path = strings.Trim(path, `"'`)
	if path == "" || path == "." || strings.HasPrefix(path, "-") {
		return base
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// gitCommitSummaryRe finds the sha git prints on a successful commit:
// "[main 1a2b3c4] subject" and "[main (root-commit) 1a2b3c4] subject".
var gitCommitSummaryRe = regexp.MustCompile(`\[[^\]]*\s([0-9a-f]{7,40})\]`)

// commitFromToolResponse reads the commits git reported for this call. A
// compound command reports one summary line per commit, so there can be several.
func commitFromToolResponse(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	var shas []string
	for _, m := range gitCommitSummaryRe.FindAllStringSubmatch(resp.Stdout, -1) {
		shas = append(shas, m[1])
	}
	return shas
}

// commitLanded reports whether dir's HEAD is a commit this command created.
//
// Claude Code's Bash tool_response carries stdout, stderr and interrupted but no
// exit status, so the repository is the witness: a rejected commit leaves HEAD
// where it was. When the response names the commits git created, HEAD has to be
// one of them, which also survives a commit-msg hook that rewrote the subject.
// Without a reported commit (a quiet commit prints nothing) the folded subject
// has to match HEAD's.
//
// Every failure direction fails closed, so the task stays open instead of closing
// on a commit that never happened. The one case neither witness can tell apart is
// a rejected commit whose message repeats the subject already at HEAD: the task
// is then resolved, because HEAD's subject does name it. It needs the same
// subject twice in a row.
func commitLanded(response json.RawMessage, dir, message string, since time.Time) bool {
	if dir == "" {
		log.Printf("hook-resolve: no directory to witness the commit in")
		return false
	}
	head, err := headCommit(dir)
	if err != nil {
		// Fail closed, but leave a trace: a git that cannot read HEAD would
		// otherwise disable resolution for every commit without any signal.
		log.Printf("hook-resolve: cannot read HEAD in %s: %v", dir, err)
		return false
	}
	if head.operation != "commit" {
		// HEAD was last moved by something else (cherry-pick, revert, merge), so
		// this command did not create it.
		log.Printf("hook-resolve: HEAD %s was produced by %q, not by a commit", head.sha, head.operation)
		return false
	}
	// HEAD has to be younger than the command. Without that, a sha the command
	// merely printed (or an identical subject already at HEAD) would pass as
	// evidence for a commit that never happened.
	if head.committedAt.Before(since.Add(-commitFreshSlack)) {
		log.Printf("hook-resolve: HEAD %s was committed at %s, older than this command", head.sha, head.committedAt.Format(time.RFC3339))
		return false
	}
	shas := commitFromToolResponse(response)
	for _, sha := range shas {
		if strings.HasPrefix(head.sha, sha) {
			return true
		}
	}
	want := gitSubject(message)
	if want != "" && head.subject == want {
		return true
	}
	log.Printf("hook-resolve: %q is not HEAD (%q), reported commits: %v", want, head.subject, shas)
	return false
}

// headCommit reads HEAD through the reflog: the sha, the folded subject, when it
// was committed and which operation produced it. The reflog is what makes the
// operation visible, and a fresh clone has one.
func headCommit(dir string) (headState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commitCheckTimeout)
	defer cancel()
	// The repository's own config decides whether git runs a program here:
	// log.showSignature makes every log verify HEAD's signature and invoke
	// gpg.program, and that key may name a file the repository ships. The -c
	// below overrides the config before it can run anything.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "-c", "log.showSignature=false", "--no-pager", "log", "-g", "-1", "--format=%H%x00%s%x00%ct%x00%gs").Output()
	if err != nil {
		return headState{}, err
	}
	parts := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x00", 4)
	if len(parts) != 4 {
		return headState{}, fmt.Errorf("unexpected git log output %q", string(out))
	}
	seconds, convErr := strconv.ParseInt(parts[2], 10, 64)
	if convErr != nil {
		return headState{}, fmt.Errorf("unexpected commit time %q", parts[2])
	}
	// An update-ref reflog entry carries no message, so the operation can be empty.
	operation := ""
	if fields := strings.Fields(parts[3]); len(fields) > 0 {
		operation = strings.TrimSuffix(fields[0], ":")
	}
	return headState{
		sha:         parts[0],
		subject:     parts[1],
		committedAt: time.Unix(seconds, 0),
		operation:   operation,
	}, nil
}

// repoRoot is the working tree dir belongs to. A commit made from a subdirectory
// belongs to that repository, and the task it closes is stored there.
func repoRoot(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), commitCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitSubject is what git records as the subject: the first paragraph of the
// message with its line breaks folded into spaces.
func gitSubject(message string) string {
	var parts []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " ")
}

// resolveMatchingTask asks the commit resolver which open task the commit
// closes and resolves it. Returns the output message or "".
func resolveMatchingTask(store *storage.Store, msg, project string) string {
	decision, err := store.ResolveCommit(msg, project)
	if err != nil || decision == nil {
		return ""
	}
	if decision.Target == nil {
		// Empty Reason means the subject referenced nothing this hook can act on;
		// that is the normal case and must not inject a line into every commit.
		if decision.Reason == "" {
			return ""
		}
		return fmt.Sprintf("hook-resolve: commit detected, no open task resolved: %s", decision.Reason)
	}
	best := decision.Target
	return fmt.Sprintf("hook-resolve: auto-resolved #%d (%s) via commit", best.ID, truncate(best.Content, 80))
}

// outputHookResult writes a JSON response that Claude Code displays as hook output.
func outputHookResult(text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": text,
		},
	}
	jsonOut, _ := json.Marshal(out)
	fmt.Print(string(jsonOut))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// isGitToken reports whether a shell field invokes the git binary, including an
// absolute path (/usr/bin/git commit).
func isGitToken(field string) bool {
	return filepath.Base(strings.Trim(field, `"'`)) == "git"
}

// isGitCommit reports whether a shell command runs `git commit`, including a git
// call whose global options (-C <dir>, -c key=value) sit between git and the
// verb. Matching the literal "git commit" missed `git -C <dir> commit`, so the
// hook never saw those commits at all.
func isGitCommit(cmd string) bool {
	for _, segment := range commandSegments(cmd) {
		fields := strings.Fields(segment)
		for i := range fields {
			if !isGitToken(fields[i]) {
				continue
			}
			for j := i + 1; j < len(fields); {
				field := fields[j]
				if !strings.HasPrefix(field, "-") {
					if field == "commit" {
						return true
					}
					break
				}
				// These options take the next token as their value.
				if field == "-c" || field == "-C" || field == "--git-dir" || field == "--work-tree" || field == "--namespace" {
					j += 2
					continue
				}
				j++
			}
		}
	}
	return false
}

// extractCommitMessage tries to extract the -m message from a git commit command.
func extractCommitMessage(cmd string) string {
	// Handle heredoc pattern: git commit -m "$(cat <<'EOF'\n...\nEOF\n)"
	if idx := strings.Index(cmd, "<<'EOF'"); idx != -1 {
		start := idx + len("<<'EOF'")
		// Find content between <<'EOF' and EOF
		rest := cmd[start:]
		if end := strings.Index(rest, "EOF"); end != -1 {
			msg := strings.TrimSpace(rest[:end])
			return msg
		}
	}

	// -m "message", -qm "message" (combined short flags), --message "…",
	// --message="…".
	loc := commitMsgFlagRe.FindStringSubmatchIndex(cmd)
	if loc == nil {
		return ""
	}
	rest := strings.TrimSpace(cmd[loc[1]:])
	if len(rest) == 0 {
		return ""
	}

	// Quoted message
	if rest[0] == '"' || rest[0] == '\'' {
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end != -1 {
			return rest[1 : end+1]
		}
	}

	// Unquoted — take until next space or end
	if end := strings.IndexByte(rest, ' '); end != -1 {
		return rest[:end]
	}
	return rest
}

// commitMsgFlagRe finds the flag that carries the commit message. The short form
// may sit in a combined flag cluster (-qm, -am), so the group before the "m" is
// matched loosely.
var commitMsgFlagRe = regexp.MustCompile(`(?:^|\s)(?:--message|-([A-Za-z]*?)m)(?:\s+|=)`)

