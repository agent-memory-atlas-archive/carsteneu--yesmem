package daemon

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/storage"
)

const (
	// unfixablePrefix opens the reply that declines a repair.
	unfixablePrefix = "UNFIXABLE:"
	// redactionMarker is what the secret redactor leaves behind. A replacement
	// carrying it would write a placeholder into a live cap.
	redactionMarker = "[REDACTED:"
	// The reply protocol markers. Each must be a line of its own, so a shell body
	// containing one of them cannot split a block.
	oldMarker = "<<<<"
	newMarker = "===="
	endMarker = ">>>>"
)

// fixSystemPrompt asks for the repair as a patch. It is paired with the daemon's
// AutoCorrectClient, so the provider, the budget, the api gate and the per-call
// cap are the same ones every other daemon LLM path uses.
//
// The prompt carries the failing command and the failure, never the script: a
// whole-body reply to a prompt without the body would be a blind overwrite of a
// live cap. The model names the block it wants to change, and the daemon applies
// that to the body it holds (applyFixPatch), so a reply that does not fit the
// script changes nothing.
const fixSystemPrompt = `You repair one failing bash script. You never see the whole script: you see the failing command, the exit code, the error and an output excerpt, plus the cap the script belongs to.

Answer in exactly one of these two forms, without explanation and without a code fence.

A repair: the block of the script to replace, and what replaces it:
REPLACE
<<<<
old block, copied verbatim from the failing command
====
new block, shell text only
>>>>

A refusal, when the excerpt does not show enough to repair the script safely:
UNFIXABLE: one sentence why

Keep the repair as small as possible and reuse only commands the script already uses.`

// autoCorrectPrompt renders the fix request for the auto-correct pass.
//
// It carries no script body. What the model gets is the excerpt the failure
// produced (failing command, error, output) and the cap's own metadata, which is
// enough to name a repair block but not enough to rewrite the script. The reply
// is applied to the body the daemon holds, and only the block it names changes
// (applyFixPatch), so the live cap is never rebuilt from model text.
//
// Every untrusted part is redacted here, and the failing line is withheld when
// the redactor would have changed it: a redacted copy would come back as the
// patch's old block and match nothing. That is why this path no longer needs an
// unredacted client (see llmClientOpts).
func (h *Handler) autoCorrectPrompt(run storage.BashJobRun, sc ScriptMeta, meta *CapMeta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A scheduled bash cap %q failed. Repair the %s script %q.\n", h.redact(run.CapName), h.redact(sc.Kind), h.redact(sc.Name))
	if meta != nil {
		fmt.Fprintf(&b, "Cap: %s\n", h.redact(meta.Name))
		if meta.Description != "" {
			fmt.Fprintf(&b, "What the cap does: %s\n", h.redact(meta.Description))
		}
		fmt.Fprintf(&b, "Scripts in this cap: %s\n", h.redact(strings.Join(capScriptNames(meta.Scripts), ", ")))
	}
	fmt.Fprintf(&b, "\n%s\n", h.failingCommandExcerpt(sc.Body, run))
	fmt.Fprintf(&b, "Exit: %d\n", run.ExitCode)
	fmt.Fprintf(&b, "Error: %s\n", h.redact(run.ErrorMsg))
	fmt.Fprintf(&b, "Output:\n%s", h.redactThenTruncate(run.Output, 2000))
	return b.String()
}

// failingCommandExcerpt renders the one script line the failure points at. A line
// the redactor would rewrite is withheld instead of sent redacted: the redactor
// is one-way, so the model's copy of it could never match the live body.
func (h *Handler) failingCommandExcerpt(body string, run storage.BashJobRun) string {
	lineNo, text, ok := failingLineExcerpt(body, run.ErrorMsg, run.Output)
	// A one-line body is its own failing line, so quoting the line would send the
	// whole script. The statement that failed is all the model may see.
	wholeBody := ok && isWholeBody(text, body)
	if !ok || wholeBody {
		stmt, found := failingStatement(body, run.ErrorMsg, run.Output)
		lineNo, text, ok = 1, stmt, found
	}
	if !ok {
		if wholeBody {
			return "Failing command: withheld, naming it would send the whole script."
		}
		return "Failing command: could not be identified from the error text."
	}
	if h.redact(text) != text {
		return fmt.Sprintf("Failing command (line %d): withheld, the line carries a literal.", lineNo)
	}
	return fmt.Sprintf("Failing command (line %d): %s", lineNo, text)
}

func isWholeBody(text, body string) bool {
	return strings.TrimSpace(text) == strings.TrimSpace(body)
}

// failingStatement picks the statement of a one-line body that the error names.
func failingStatement(body, errMsg, output string) (string, bool) {
	quoted := errMsg + "\n" + output
	for _, raw := range splitShellStatements(body) {
		stmt := strings.TrimSpace(raw)
		if stmt == "" || isWholeBody(stmt, body) {
			continue
		}
		for _, word := range errorWords(quoted) {
			if namesWord(stmt, word) {
				return stmt, true
			}
		}
	}
	return "", false
}

func capScriptNames(scripts []ScriptMeta) []string {
	names := make([]string, 0, len(scripts))
	for _, sc := range scripts {
		names = append(names, sc.Name)
	}
	return names
}

// failingLineExcerpt returns the script line a failure points at as a 1-based line
// number and its text. Three signals, most precise first: the line number the
// shell reports, a line the error or output quotes verbatim, and the one word the
// error names. A reported number counts from the command that ran, so the adapter
// prelude prepareBashCommand puts above the script is subtracted from it.
func failingLineExcerpt(body, errMsg, output string) (int, string, bool) {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	quoted := errMsg + "\n" + output

	if n, ok := reportedLineNumber(errMsg); ok {
		if idx := n - 1 - adapterPreludeLines(body); idx >= 0 && idx < len(lines) && usableScriptLine(lines[idx]) {
			return idx + 1, lines[idx], true
		}
	}
	if n, ok := reportedLineNumber(output); ok {
		if idx := n - 1 - adapterPreludeLines(body); idx >= 0 && idx < len(lines) && usableScriptLine(lines[idx]) {
			return idx + 1, lines[idx], true
		}
	}

	// A line quoted verbatim is the statement itself, which beats a single word
	// from the error. The longest match wins: a short line is more likely to be a
	// fragment that also appears somewhere else.
	best, bestLen := -1, 0
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if !usableScriptLine(raw) || len(line) < 4 {
			continue
		}
		if strings.Contains(quoted, line) && len(line) > bestLen {
			best, bestLen = i, len(line)
		}
	}
	if best >= 0 {
		return best + 1, lines[best], true
	}

	// bash quotes the command, not the line: the one body line carrying the named
	// word is the statement that failed. Ambiguity is not resolved by guessing.
	for _, word := range errorWords(quoted) {
		at := -1
		for i, raw := range lines {
			if !usableScriptLine(raw) || !namesWord(raw, word) {
				continue
			}
			if at >= 0 {
				at = -1
				break
			}
			at = i
		}
		if at >= 0 {
			return at + 1, lines[at], true
		}
	}
	return 0, "", false
}

// reportedLineNumber reads "line N" out of a shell message.
func reportedLineNumber(quoted string) (int, bool) {
	m := lineNumberPattern.FindStringSubmatch(quoted)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

var (
	// lineNumberPattern matches the form the shell itself uses. A bare "line N"
	// also appears in program output ("File x.py, line 42"), and that number
	// points into another file entirely, so the prefix and the trailing colon are
	// part of the pattern.
	lineNumberPattern = regexp.MustCompile(`(?m)^[^\s:]*: line (\d+): `)
)

// adapterPreludeLines is how many lines prepareBashCommand puts above the script
// when the body uses the store()/llm() adapter. It is the shift between a line
// number the shell reports and the line in the script.
func adapterPreludeLines(body string) int {
	if prepareBashCommand(body) == body {
		return 0
	}
	return strings.Count(capfile.GenerateAdapterBash(), "\n")
}

// usableScriptLine filters the lines a failure can point at: a blank line, a
// comment or the shebang names no failing command.
func usableScriptLine(raw string) bool {
	line := strings.TrimSpace(raw)
	return line != "" && !strings.HasPrefix(line, "#")
}

// shellWords are the words the shell itself uses in its messages. A scan for the
// failing line must not settle on one of them.
var shellWords = map[string]bool{
	"bash": true, "sh": true, "line": true, "command": true, "not": true, "found": true,
	"such": true, "file": true, "directory": true, "permission": true, "denied": true,
	"exit": true, "status": true, "error": true, "cannot": true, "failed": true,
	"argument": true, "arguments": true, "invalid": true, "missing": true, "unknown": true,
	"option": true, "syntax": true, "unexpected": true, "the": true,
}

// errorWords lists the distinct words of a shell message that could name a
// command, longest first.
func errorWords(quoted string) []string {
	seen := make(map[string]bool, 8)
	var words []string
	for _, raw := range strings.FieldsFunc(quoted, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		word := strings.Trim(raw, "-_./=")
		if len(word) < 2 || seen[word] || shellWords[strings.ToLower(word)] {
			continue
		}
		seen[word] = true
		words = append(words, word)
	}
	sort.SliceStable(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
	return words
}

// namesWord reports whether a script line invokes the named word. A long word may
// appear anywhere in the line; a two-letter one only as its first token, because
// short words turn up inside arguments far too often to trust a substring match.
func namesWord(line, word string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, word) {
		rest := trimmed[len(word):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return true
		}
	}
	if len(word) < 3 {
		return false
	}
	return strings.Contains(trimmed, word)
}

// fixPatch is the model's answer to a fix request: the block of the script to
// replace together with its replacement, or the reason it declines to propose one.
type fixPatch struct {
	oldBlock  string
	newBlock  string
	unfixable string
}

// parseFixPatch reads the reply the fix prompt asks for.
func parseFixPatch(reply string) (fixPatch, error) {
	answer := strings.TrimSpace(trimCodeFence(strings.TrimSpace(reply)))
	// A provider that normalises to CRLF must still produce a block that matches
	// the LF body, so the line endings are unified before the lines are split.
	answer = strings.ReplaceAll(answer, "\r\n", "\n")
	if answer == "" {
		return fixPatch{}, fmt.Errorf("the model answered nothing")
	}
	// The markers are their own lines. Matching them as bare text splits the block
	// at any "====" a shell body or a quoted output happens to contain.
	lines := strings.Split(answer, "\n")
	open, sep, close := -1, -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case oldMarker:
			if open >= 0 {
				return fixPatch{}, fmt.Errorf("the answer carries more than one replace block, the pass needs exactly one")
			}
			open = i
		case newMarker:
			if sep >= 0 {
				return fixPatch{}, fmt.Errorf("the replace block has more than one %q separator", newMarker)
			}
			sep = i
		case endMarker:
			if close >= 0 {
				return fixPatch{}, fmt.Errorf("the replace block is closed more than once")
			}
			close = i
		}
	}
	// A refusal may be prefixed ("Answer: UNFIXABLE: ..."); without a replace block
	// it is a refusal wherever the marker sits, so its reason reaches the operator.
	if open < 0 {
		if i := strings.Index(answer, unfixablePrefix); i >= 0 {
			reason := strings.TrimSpace(answer[i+len(unfixablePrefix):])
			if reason == "" {
				reason = "the model gave no reason"
			}
			return fixPatch{unfixable: reason}, nil
		}
		return fixPatch{}, fmt.Errorf("the answer carries no replace block")
	}
	if sep < open || close < sep {
		return fixPatch{}, fmt.Errorf("the replace block is incomplete: %s, %s and %s each need their own line, in that order", oldMarker, newMarker, endMarker)
	}

	patch := fixPatch{
		// Only the block's edge newlines go: the first line's indentation is part
		// of the text the model copied, and a trimmed copy matches no indented body.
		oldBlock: blockText(lines[open+1 : sep]),
		newBlock: blockText(lines[sep+1 : close]),
	}
	if strings.TrimSpace(patch.oldBlock) == "" {
		return fixPatch{}, fmt.Errorf("the replace block names nothing to replace")
	}
	if strings.TrimSpace(patch.newBlock) == "" {
		return fixPatch{}, fmt.Errorf("the replace block carries no replacement")
	}
	// The client redacts the completion, so a marker here means the model echoed a
	// placeholder. Writing that into the cap would corrupt a literal for good.
	if strings.Contains(patch.newBlock, redactionMarker) {
		return fixPatch{}, fmt.Errorf("the replacement carries a redaction marker")
	}
	return patch, nil
}

// blockText renders the lines between two markers as script text. Only the edge
// newlines are trimmed: the first line's indentation is part of the text the
// model copied, and a trimmed copy matches no indented body line.
func blockText(lines []string) string {
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// applyFixPatch swaps the model's block in the live body. Exactly one occurrence
// is required: with none there is nothing to repair, and with several the daemon
// cannot tell which statement the model meant. The match is literal text, so a
// fragment of a line splices mid-line; the sandbox run and the diff class have to
// accept that splice before anything is saved.
func applyFixPatch(body string, p fixPatch) (string, error) {
	if strings.TrimSpace(p.oldBlock) == "" {
		return "", fmt.Errorf("the patch names no block to replace")
	}
	if n := strings.Count(body, p.oldBlock); n != 1 {
		return "", fmt.Errorf("the block to replace matches %d times in the script, a fix needs exactly one", n)
	}
	if p.newBlock == p.oldBlock {
		return "", fmt.Errorf("the replacement is the block itself")
	}
	return strings.Replace(body, p.oldBlock, p.newBlock, 1), nil
}

// trimCodeFence removes a surrounding markdown fence. The instruction asks for a
// bare script, but a model that fences anyway must not make every fix look like a
// substantial rewrite: the fence lines are new tokens and new command tokens.
func trimCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "\n"), "```")
	return strings.TrimRight(s, "\n")
}
