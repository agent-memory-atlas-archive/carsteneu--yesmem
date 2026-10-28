package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// protectedTargetAction is what the guard does with a write to one of the
// installation's own files.
type protectedTargetAction int

const (
	protectedNone protectedTargetAction = iota
	protectedWarn
	protectedBlock
)

// protectedWriteAction decides between block and warn for a write to path.
// wholeFile reports whether the operation replaces the file's entire content,
// which is what drops the keys the caller did not reproduce; a write that can
// only touch what it targets is warned about instead (YM-269). The generated ops
// file is blocked either way: it is rewritten from the source learnings on the
// next regeneration, so the fix belongs in the learning, not in the file. Its
// identity includes the directory it is generated into, matching the Edit and
// Write paths (YM-342).
func protectedWriteAction(path string, wholeFile bool) protectedTargetAction {
	if isProtectedFile(path) {
		return protectedBlock
	}
	if isProtectedSettingsFile(path) {
		if wholeFile {
			return protectedBlock
		}
		return protectedWarn
	}
	return protectedNone
}

// isProtectedWritePath reports whether path is one of the files protectedWriteAction
// has an opinion about.
func isProtectedWritePath(path string) bool {
	return isProtectedFile(path) || isProtectedSettingsFile(path)
}

// bashWriteVerbs are the commands whose last operand is a file they replace.
var bashWriteVerbs = map[string]bool{
	"cp": true, "mv": true, "install": true, "ln": true, "rsync": true,
	"truncate": true, "shred": true,
}

// bashInPlaceVerbs are the editors whose in-place flag rewrites the file from its
// own content, the shell counterpart of a surgical Edit.
var bashInPlaceVerbs = map[string]bool{
	"sed": true, "gsed": true, "perl": true,
}

// bashWrappers run another command; the verb that matters is the wrapped one.
var bashWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true, "exec": true,
	"nohup": true, "time": true, "nice": true, "ionice": true, "stdbuf": true,
	"xargs": true, "timeout": true, "watch": true,
}

// bashShells take a command of their own behind -c.
var bashShells = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true,
}

// bashWrapperValueFlags are the sudo flags whose value sits in the next word, so the
// wrapped command is not mistaken for that value: without them "sudo -u root cp file
// target" stops at "root" and never sees the copy. The other wrappers take attached
// or self-evident values the prefix walk skips on its own (a number, a duration, a
// number behind -n, an attached -I{}). A flag this list does not know, whose value
// is a separate word, can swallow the command name; that loses a detection instead
// of inventing one, which is the direction this heuristic errs in everywhere.
var bashWrapperValueFlags = map[string]bool{
	"-u": true, "-g": true, "-p": true, "-C": true, "-T": true,
	"-r": true, "-t": true, "-U": true, "-R": true, "-D": true,
}

// bashProtectedWrite reports a shell command that writes one of the protected
// files. It returns the resolved target and whether the write replaces the whole
// file. The scan works per simple command and per word, not as a shell parser: a
// quoted span never counts as an operator, a wrapper command is looked through, and
// a write is only reported when a redirect, a write verb or an in-place flag names
// the file, so a command that merely mentions it stays silent.
func bashProtectedWrite(command, cwd string) (target string, wholeFile bool, ok bool) {
	return bashProtectedWriteAt(command, cwd, 0)
}

// maxShellDepth bounds the "bash -c '…'" recursion: the inner command is shorter
// than the outer one on every round, but a deeply nested body must not reach a stack
// overflow, which would end the hook with a fatal exit and block the tool call.
const maxShellDepth = 4

func bashProtectedWriteAt(command, cwd string, depth int) (target string, wholeFile bool, ok bool) {
	if depth > maxShellDepth {
		return "", false, false
	}
	// A backslash at the end of a line joins the next line before the shell splits
	// words, so the command is joined once here and every later scan sees the words
	// the shell would see.
	command = joinLineContinuations(command)
	// The command as written is scanned first: its words are what the shell executes, and a
	// compound substitution ("echo $(cat a; cp x ~/.claude/settings.json)") exposes its
	// parts here.
	if t, whole, hit := scanBashSegments(command, cwd, depth); hit {
		return t, whole, true
	}
	// Then the same command with every substitution collapsed into one unknown run: a
	// substitution that spells a name fragment completes a protected path
	// ("> $HOME/.claude/$(printf settings.json)"), and its inner blanks would otherwise
	// split the target word. Two scans, because one view cannot answer both questions:
	// collapsing loses the write inside a compound body, and keeping the words loses the
	// fragment (cold review Los 56h).
	if collapsed := collapseSubstitutionSpans(command); collapsed != command {
		return scanBashSegments(collapsed, cwd, depth)
	}
	return "", false, false
}

// scanBashSegments is the segment loop of one scan pass.
func scanBashSegments(command, cwd string, depth int) (target string, wholeFile bool, ok bool) {
	dir := cwd
	vars := shellVars{}
	for _, segment := range bashSegments(command) {
		vars.record(segment)
		if next, changed := changedDirectory(segment, dir, vars); changed {
			dir = next
			continue
		}
		if t, whole, hit := protectedWriteInSegment(segment, dir, depth, vars); hit {
			return t, whole, true
		}
	}
	return "", false, false
}

// changedDirectory follows a "cd <dir>" segment, so a later relative path is
// resolved against the directory the command actually works in:
// "cd ~/.claude && echo '{}' > settings.json" writes the settings file.
func changedDirectory(segment, cwd string, vars shellVars) (string, bool) {
	raw := strings.Fields(segment)
	masked := strings.Fields(maskQuoted(segment))
	if len(raw) != 2 || len(masked) != 2 || masked[0] != "cd" {
		return "", false
	}
	return resolveCdTarget(raw[1], cwd, vars), true
}

// resolveCdTarget resolves the directory a "cd" changes into. A word whose expansion
// is unknown resolves to nothing instead of to cwd: "cd $TMPDIR && … > settings.json"
// must not read as "cd ." and block a write that lands in $TMPDIR (YM-383). A
// variable this command assigned is known and resolves like its value (YM-387).
func resolveCdTarget(word, cwd string, vars shellVars) string {
	if expanded := dequotedTargetWord(word, vars); expanded != "" {
		return resolveHookPath(expanded, cwd)
	}
	return ""
}

// shellDequote reduces a shell word to the bytes it names. The shell joins every
// quoted and unquoted part of a word into one string and drops the quote characters,
// so "$HOME"/.claude/settings.json names the settings file just like the unquoted
// spelling; trimming only the outer quotes left an inner one in the path and lost the
// match (YM-375). Outside single quotes a backslash removes itself and protects the
// next byte, which also turns "settings\.json" into "settings.json" (YM-342).
//
// A quoted span keeps its backslashes in the shell except behind a double quote,
// where a backslash escapes the next byte; treating it as an escape everywhere inside
// double quotes keeps the comparison on the protecting side.
//
// An escaped space does not reach this function: the word was already split on it, so
// "\ " becomes two words before the dequote runs. It is applied to shell words only: a
// tool payload carries a plain path, and dequoting it would turn a Windows path into
// something else.
func shellDequote(word string) string {
	if !strings.ContainsAny(word, "\\\"'`") {
		return word
	}
	var b strings.Builder
	b.Grow(len(word))
	var quote byte
	for i := 0; i < len(word); i++ {
		c := word[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			if quote != '\'' && c == '\\' && i+1 < len(word) {
				i++
				b.WriteByte(word[i])
				continue
			}
			b.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '\\' && i+1 < len(word) {
			i++
			b.WriteByte(word[i])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// shellVars carries the assignments of the command being scanned. A Bash tool call
// runs in one shell, so a value assigned in an earlier segment is what a later
// "$name" names: "p=$HOME/.claude/settings.json; printf … > $p" writes the settings
// file (YM-387). Only the assignments of this command are known; an inherited
// environment variable stays unknown, which is what keeps "cd $TMPDIR && … >
// settings.json" silent.
type shellVars map[string]string

// maxVarExpansions bounds the substitution chain: a value may name another
// assignment ("a=$b"), and a self-naming one ("a=$a") must not spin.
const maxVarExpansions = 4

// record collects the "NAME=value" words of one segment. A word whose name is quoted
// was never an assignment; the value may be quoted, and the shell dequotes it when the
// variable is used later.
func (v shellVars) record(segment string) {
	if v == nil {
		return
	}
	raw := strings.Fields(segment)
	masked := strings.Fields(maskQuoted(segment))
	if len(raw) != len(masked) {
		return
	}
	for i := range raw {
		name, value, found := strings.Cut(raw[i], "=")
		if !found || !isAssignableName(name) {
			continue
		}
		// The name must be unquoted, so quoted text ("echo 'p=/x'") cannot define a
		// variable. The value may be quoted, and the shell dequotes it when the variable
		// is used later.
		if len(masked[i]) < len(name) || masked[i][:len(name)] != name {
			continue
		}
		v[name] = value
	}
}

// isAssignableName reports a shell variable name, so a dd operand ("of=path") and a
// flag ("--x=y") stay out of the table.
func isAssignableName(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isWordByte(name[i]) {
			return false
		}
	}
	return true
}

// substitute replaces the names this command assigned and leaves every other
// expansion as written, so an unknown word stays unrecognised instead of collapsing
// into an empty command name.
func (v shellVars) substitute(word string) string {
	if len(v) == 0 || !strings.Contains(word, "$") {
		return word
	}
	var b strings.Builder
	b.Grow(len(word))
	for i := 0; i < len(word); i++ {
		c := word[i]
		if c != '$' || i+1 >= len(word) {
			b.WriteByte(c)
			continue
		}
		end, name := i+1, ""
		if word[i+1] == '{' {
			close := strings.IndexByte(word[i+2:], '}')
			if close < 0 {
				b.WriteByte(c)
				continue
			}
			name, end = word[i+2:i+2+close], i+2+close
		} else {
			for end < len(word) && isWordByte(word[end]) {
				end++
			}
			name, end = word[i+1:end], end-1
		}
		if value, ok := v[name]; ok {
			b.WriteString(value)
		} else {
			b.WriteString(word[i : end+1])
		}
		i = end
	}
	return b.String()
}

// protectedWriteInSegment scans one simple command. raw holds the words as written,
// masked the same words with quoted spans blanked; both index the same way because
// maskQuoted keeps every byte in place.
func protectedWriteInSegment(segment, cwd string, depth int, vars shellVars) (string, bool, bool) {
	// maskWords slices the segment-wide mask at the same offsets as the raw text, so the
	// two views always line up and a quoted span keeps its quote state across a word
	// boundary (YM-383).
	raw := strings.Fields(segment)
	masked := maskWords(segment)
	if len(raw) == 0 {
		return "", false, false
	}
	if len(masked) != len(raw) {
		// maskWords walks the same separators as strings.Fields, so this cannot happen.
		// Scan unmasked rather than drop the segment: a quoting detail must not lose a
		// write. The clamps below keep a longer mask from slicing past the raw word.
		masked = raw
	}

	verbIndex := 0
	for verbIndex < len(masked) && isWrapperPrefixWord(masked[verbIndex], verbIndex) {
		if bashWrapperValueFlags[masked[verbIndex]] {
			verbIndex++ // the flag's value is not the command either
		}
		verbIndex++
	}
	if verbIndex >= len(masked) {
		return "", false, false
	}
	verb := commandName(vars.substitute(masked[verbIndex]))

	// A backtick span is a command substitution, not a quote: the shell runs its body, so
	// a redirect inside it writes like the "$(…)" spelling. maskQuoted blanks the whole
	// span, which hides that body from every later scan of this segment, so the bodies are
	// scanned here (YM-407). This runs before the shell branch below: the outer shell
	// resolves the span before it starts the nested program ("bash -l run.sh `…`").
	for _, body := range backtickBodies(segment) {
		if t, whole, hit := bashProtectedWriteAt(body, cwd, depth+1); hit {
			return t, whole, true
		}
	}

	// A shell's -c argument is a command of its own, and the innermost command is
	// what writes the file. Its variables are its own: a value this command assigned
	// is not exported to it, so the inner scan starts with an empty table.
	if bashShells[verb] {
		if body := shellCommandBodyIndex(raw, vars, verbIndex+1); body > verbIndex {
			return bashProtectedWriteAt(strings.Trim(nestedShellBody(segment, body), "\"'"), cwd, depth+1)
		}
		return "", false, false
	}

	// A redirect word and its target are not operands: without this, "cp a b > /dev/null"
	// hands /dev/null to the copy analysis as the destination (YM-383).
	redirectWord := make([]bool, len(raw))
	for i := range raw {
		if _, nextWord, redirect := redirectTargets(masked[i], raw[i]); redirect {
			redirectWord[i] = true
			if nextWord && i+1 < len(raw) {
				redirectWord[i+1] = true
			}
		}
	}

	if dst, ok := bashCopyIntoProtectedDir(verb, masked[verbIndex+1:], raw[verbIndex+1:], redirectWord[verbIndex+1:], cwd, vars); ok {
		return dst, true, true
	}

	inPlace := bashInPlaceVerbs[verb] && hasInPlaceFlag(masked[verbIndex+1:])
	wholeTargets := bashWholeWriteTargets(verb, masked[verbIndex+1:], redirectWord[verbIndex+1:])

	for i := range raw {
		targets, nextWord, redirect := redirectTargets(masked[i], raw[i])
		if redirect {
			for _, path := range targets {
				if t := protectedTargetWord(path, cwd, vars); t != "" {
					return t, true, true
				}
			}
			if nextWord && i+1 < len(raw) {
				// The target word ends where its own operator starts: "> path> out"
				// truncates path and only then redirects to out.
				head := redirectTargetHead(masked[i+1])
				if head > len(raw[i+1]) {
					// A mask longer than the word must not slice past it.
					head = len(raw[i+1])
				}
				if head > 0 {
					if t := protectedTargetWord(raw[i+1][:head], cwd, vars); t != "" {
						return t, true, true
					}
				}
			}
			continue
		}
		t := protectedTargetWord(trimKeyPrefix(raw[i]), cwd, vars)
		if t == "" {
			continue
		}
		switch {
		case wholeTargets[i-(verbIndex+1)]:
			return t, true, true
		case inPlace:
			return t, false, true
		}
	}
	return "", false, false
}

// shellCommandBodyIndex returns the index of the word that carries the command a shell
// runs behind -c, or 0 when the invocation has none. The shell reads its options as
// short flags that may be clustered ("-lc", "-ec"), and it keeps reading them behind -c
// ("bash -c -e '…'", "bash -c -- '…'"), so the body is the first word behind the flag
// that is not an option itself (measured on bash 5.2). The flag word is read as the shell
// reads it, with its quotes and its assigned variables resolved ("bash \"-c\" …",
// "f=-c; bash $f …"), and -o/-O take their value in the next word ("bash -c -o pipefail
// '…'"): without that, the value was taken for the body. A shell that runs a script file
// has no body this scan can read, which is the silence it had before; the flag search
// still walks past the script operand, as the plain "-c" comparison always did.
func shellCommandBodyIndex(raw []string, vars shellVars, start int) int {
	flag := -1
	for i := start; i < len(raw); i++ {
		w := unquotedWord(vars.substitute(raw[i]))
		if len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.ContainsRune(w[1:], 'c') {
			flag = i
			break
		}
	}
	if flag < 0 {
		return 0
	}
	for i := flag + 1; i < len(raw); i++ {
		w := unquotedWord(vars.substitute(raw[i]))
		if w == "-o" || w == "-O" {
			i++ // the option's value is not the command either
			continue
		}
		if strings.HasPrefix(w, "-") {
			continue
		}
		return i
	}
	return 0
}

// backtickBodies returns the bodies of the backtick spans of one segment. Such a span is
// a command substitution, so a redirect inside it writes like the "$(…)" spelling.
// maskQuoted blanks the whole span as if it were a quote, which hides the body from
// every later scan. A span inside single quotes is literal text, and a backslash escapes
// the backtick behind a double quote and outside quotes alike, as the shell does.
func backtickBodies(segment string) []string {
	if !strings.Contains(segment, "`") {
		return nil
	}
	var bodies []string
	var quote byte
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			continue
		case c == '\\' && i+1 < len(segment):
			i++
			continue
		case quote == '"':
			if c == '"' {
				quote = 0
				continue
			}
		case c == '\'' || c == '"':
			quote = c
			continue
		}
		if c != '`' {
			continue
		}
		end := -1
		for j := i + 1; j < len(segment); j++ {
			if segment[j] == '\\' {
				j++
				continue
			}
			if segment[j] == '`' {
				end = j
				break
			}
		}
		if end < 0 {
			break
		}
		bodies = append(bodies, segment[i+1:end])
		i = end
	}
	return bodies
}

// bashCopyIntoProtectedDir catches the restore shape "cp backup/settings.json
// ~/.claude/": the destination is the directory, and the copy writes the protected
// file because the source carries its name.
func bashCopyIntoProtectedDir(verb string, maskedArgs, rawArgs []string, redirectWord []bool, cwd string, vars shellVars) (string, bool) {
	if !bashWriteVerbs[verb] || len(rawArgs) < 2 || len(rawArgs) != len(maskedArgs) {
		return "", false
	}
	if verb == "install" && hasDirectoryMode(maskedArgs) {
		return "", false // "install -d" creates directories, not the source's file
	}
	operands := make([]int, 0, len(rawArgs))
	for i := range rawArgs {
		if maskedArgs[i] == "" || strings.HasPrefix(maskedArgs[i], "-") || redirectWord[i] {
			continue
		}
		operands = append(operands, i)
	}
	if len(operands) < 2 {
		return "", false
	}
	dstWord := dequotedTargetWord(rawArgs[operands[len(operands)-1]], vars)
	dst := resolveHookPath(dstWord, cwd)
	if isProtectedWritePath(dst) {
		return "", false // the plain word scan already covers a direct destination
	}
	// Only a destination that is a directory receives the source's name; otherwise the
	// join would invent a path ("mv a.md b.md" must not become b.md/yesmem-ops.md).
	if !strings.HasSuffix(dstWord, "/") {
		if info, err := os.Stat(dst); err != nil || !info.IsDir() {
			return "", false
		}
	}
	source := filepath.Base(dequotedTargetWord(rawArgs[operands[len(operands)-2]], vars))
	joined := filepath.Join(dst, filepath.Base(source))
	if isProtectedWritePath(joined) {
		return joined, true
	}
	return "", false
}

// bashSegments splits the command at its unquoted separators. maskQuoted keeps the
// byte positions, so a "|" or ";" inside a quoted argument no longer starts a new
// segment and the raw text can be sliced at the masked offsets.
func bashSegments(command string) []string {
	masked := maskQuoted(command)
	var segments []string
	start := 0
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case ';', '|', '&', '\n', '\r':
			// ">|" is the clobber redirect and ">&file" a redirect of both streams,
			// not separators: splitting there would leave the operator without its
			// target and lose the write (YM-375, YM-383).
			if i > start && masked[i-1] == '>' && (masked[i] == '|' || masked[i] == '&') {
				continue
			}
			if masked[i] == '&' && i+1 < len(masked) && masked[i+1] == '>' {
				continue
			}
			if i > start {
				segments = append(segments, command[start:i])
			}
			start = i + 1
		}
	}
	if start < len(command) {
		segments = append(segments, command[start:])
	}
	return segments
}

// joinLineContinuations removes the backslash that ends a line, joining the next
// line into the same word: the shell drops that pair before it splits, so a path cut
// in the middle of a name by a trailing backslash names the file it spells out
// (measured on bash 5.2). The pair stays where the shell keeps it literal: inside
// single quotes, and inside a comment, whose next line is comment text and therefore
// never a word of the command. A backslash at the end of the command without a
// newline is literal too and stays. Backslashes are walked as pairs, so an escaped
// backslash at the end of a line is not a continuation.
//
// A "#" starts a comment only where the shell allows one: at the start of a word and
// outside a "${...}" expansion, where it belongs to the word. Both conditions were
// measured: "#" behind ";-&|()" starts a comment that ends at the newline (the next
// line runs as a command of its own), while "${zz:- #}" keeps it in the word.
func joinLineContinuations(command string) string {
	if !strings.Contains(command, "\\\n") {
		return command
	}
	var b strings.Builder
	b.Grow(len(command))
	var quote byte
	comment, wordStart := false, true
	brace := 0
	for i := 0; i < len(command); i++ {
		c := command[i]
		if comment {
			if c == '\n' {
				comment, wordStart = false, true
			}
			b.WriteByte(c)
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && i+1 < len(command) {
				// Inside double quotes and backticks the shell drops a backslash before a
				// newline as well; everywhere else the pair is written whole, which is what
				// keeps an escaped backslash out of the continuation branch.
				if quote != '\'' && command[i+1] == '\n' {
					i++
					continue
				}
				b.WriteByte(c)
				i++
				b.WriteByte(command[i])
				continue
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case c == '#' && wordStart && brace == 0:
			comment = true
			b.WriteByte(c)
			continue
		case c == '\'' || c == '"' || c == '`':
			quote = c
			wordStart = false
			b.WriteByte(c)
			continue
		case c == '\\' && i+1 < len(command) && command[i+1] == '\n':
			i++
			continue
		case c == '\\' && i+1 < len(command):
			b.WriteByte(c)
			i++
			b.WriteByte(command[i])
			wordStart = false
			continue
		}
		if c == '$' {
			switch {
			case i+1 < len(command) && command[i+1] == '{':
				brace++
			case i+3 < len(command) && command[i+1] == '\\' && command[i+2] == '\n' && command[i+3] == '{':
				// The join removes the pair, so "$" and "{" end up adjacent and the
				// expansion opens just the same.
				brace++
			}
		} else if c == '}' && brace > 0 {
			brace--
		} else if c == '\n' {
			// Bash aborts a "${" that reaches the line break with a bad substitution
			// (measured on 5.2), so the counter never has to survive one. Resetting keeps
			// it from leaking out of a heredoc body, whose text is not executed and may
			// carry an unbalanced "${".
			brace = 0
		}
		b.WriteByte(c)
		wordStart = strings.IndexByte(" \t\n;&|()", c) >= 0
	}
	return b.String()
}

// substitutionSentinel stands for a whole command substitution in the collapsed copy
// of a command (see collapseSubstitutionSpans). It is a byte no word of a command
// carries, so a literal "*" the user typed stays a literal.
const substitutionSentinel = '\x01'

// unknownRun stands for a run of bytes the command does not fix: a command substitution,
// a backtick span, a variable it never assigned or a parameter fallback. It is a byte no
// word carries either, which keeps it apart from a literal "*" the caller typed: only
// the unknown run may vanish from a path, a glob stays a glob (YM-407).
const unknownRun = '\x02'

// collapseSubstitutionSpans returns the command with every command substitution and every
// backtick span replaced by one substitutionSentinel byte. The scan of the command as
// written keeps its words, so a compound body ("echo $(cat a; cp x settings.json)") is
// still cut apart and judged; this copy answers the other question: does a substitution
// that spells a name fragment complete a protected path
// ("> $HOME/.claude/$(printf settings.json)")? Single quotes keep their content literal,
// so nothing inside them is a substitution.
func collapseSubstitutionSpans(command string) string {
	if !strings.Contains(command, "$(") && !strings.Contains(command, "`") {
		return command
	}
	var b strings.Builder
	b.Grow(len(command))
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(command):
			b.WriteByte(c)
			i++
			b.WriteByte(command[i])
			continue
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		}
		if c == '$' && i+1 < len(command) && command[i+1] == '(' {
			if end := matchingParen(command, i+1); end > i {
				b.WriteByte(substitutionSentinel)
				i = end
				continue
			}
		}
		if c == '`' {
			if end := strings.IndexByte(command[i+1:], '`'); end >= 0 {
				b.WriteByte(substitutionSentinel)
				i = i + 1 + end
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// nestedShellBody returns the segment from its field number start on, so a nested
// shell body keeps its own quoting and line continuations. Rebuilding it from the
// split words with a space would turn a backslash-newline pair into a backslash and a
// space, which the inner shell does not join (YM-375).
func nestedShellBody(segment string, start int) string {
	i, field := 0, 0
	for i < len(segment) {
		if r, size := utf8.DecodeRuneInString(segment[i:]); unicode.IsSpace(r) {
			i += size
			continue
		}
		if field == start {
			return segment[i:]
		}
		for i < len(segment) {
			r, size := utf8.DecodeRuneInString(segment[i:])
			if unicode.IsSpace(r) {
				break
			}
			i += size
		}
		field++
	}
	return ""
}

// maskWords masks the quoted spans of one segment and returns, per raw word, the mask
// of the bytes that word covers. The mask is built for the whole segment and then sliced
// at the word offsets: masking each word on its own would start every word unquoted, and
// a closing quote of a span that holds a whitespace rune would then open a new span and
// swallow the operator behind it (YM-383).
func maskWords(segment string) []string {
	masked := maskQuoted(segment)
	var words []string
	for i := 0; i < len(segment); {
		r, size := utf8.DecodeRuneInString(segment[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		start := i
		for i < len(segment) {
			r, size = utf8.DecodeRuneInString(segment[i:])
			if unicode.IsSpace(r) {
				break
			}
			i += size
		}
		words = append(words, masked[start:i])
	}
	return words
}

// maskQuoted blanks quoted spans, so an operator or a verb inside a quoted string
// is not read as shell syntax: a commit message that quotes a command must not be
// blocked. The replacement keeps every byte in place, so the raw text can be sliced at
// the masked offsets and bashSegments does not split at a separator inside quotes.
//
// It keeps only " " and "\t": a newline or a non-breaking space inside a quoted span
// becomes word bytes, so the masked text can split into fewer words than the raw text
// (which splits on every unicode space). The caller compares the two word counts and
// withholds the operator lexer when they differ (YM-383).
func maskQuoted(s string) string {
	out := []byte(s)
	var quote byte
	quoted := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if !quoted {
			if c == '\'' || c == '"' || c == '`' {
				quoted = true
				quote = c
				out[i] = 'x'
			}
			continue
		}
		if c == quote {
			quoted = false
			out[i] = 'x'
			continue
		}
		if c != ' ' && c != '\t' {
			out[i] = 'x'
		}
	}
	return string(out)
}

// redirectTargetEnds are the bytes that end a redirect target: each of them starts a
// shell operator of its own, so the target is the run in front of the first one.
const redirectTargetEnds = "<>|;&()"

// redirectTargets reads one word as shell redirects. masked is the word with its
// quoted spans blanked, raw the word as written; both index the same way. It returns
// every file target the word carries, whether a further operator still expects the
// next word as its target, and whether the word is a redirect at all.
//
// The operator is read, not the word compared: ">path", "2>>path" and "x>path" all
// truncate path, and a second operator glued to the target (">path> /dev/null") still
// truncates it (measured on bash 5.2). A word that only duplicates a descriptor
// ("2>&1", ">&-") names no file, so its neighbour stays an ordinary operand.
func redirectTargets(masked, raw string) (targets []string, nextWord, redirect bool) {
	for i := 0; i < len(masked); i++ {
		if masked[i] != '>' || escapedAt(masked, i) {
			continue
		}
		redirect = true
		op := i
		for op < len(masked) && (masked[op] == '>' || masked[op] == '|') {
			op++
		}
		if op < len(masked) && masked[op] == '&' {
			if op+1 < len(masked) && isDescriptorName(masked[op+1]) {
				i = op + 1
				continue
			}
			op++ // ">&file" and "&>file" redirect to a file like ">" does
		}
		start := op
		end := op + redirectTargetHead(masked[start:])
		if end > len(raw) {
			// A mask that is longer than the word (a quoted span holding a rune the mask
			// does not preserve) must not slice past it.
			end = len(raw)
		}
		switch {
		case end > start:
			targets = append(targets, raw[start:end])
		case end >= len(masked):
			nextWord = true
		}
		// The cursor must move on: a mask longer than its word can clamp end behind the
		// current position.
		if end > i+1 {
			i = end - 1
		}
	}
	return targets, nextWord, redirect
}

// redirectTargetHead returns the length of the target in front of the first byte that
// starts another operator. An escaped byte ("a\>b") is a literal and ends nothing, and
// a command substitution is part of the name it is glued into: its own parentheses are
// not operators of this command.
func redirectTargetHead(masked string) int {
	for i := 0; i < len(masked); i++ {
		if masked[i] == '$' && i+1 < len(masked) && masked[i+1] == '(' {
			if end := matchingParen(masked, i+1); end > i {
				i = end
				continue
			}
		}
		if endsRedirectTarget(masked, i) {
			return i
		}
	}
	return len(masked)
}

// endsRedirectTarget reports a byte that starts a shell operator of its own.
func endsRedirectTarget(masked string, i int) bool {
	return strings.ContainsRune(redirectTargetEnds, rune(masked[i])) && !escapedAt(masked, i)
}

// escapedAt reports whether the byte at i is preceded by an odd number of backslashes,
// which makes it a literal character rather than an operator.
func escapedAt(masked string, i int) bool {
	backslashes := 0
	for j := i - 1; j >= 0 && masked[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// isDescriptorName reports the byte behind ">&": a number or "-" names a descriptor,
// anything else a file.
func isDescriptorName(c byte) bool {
	return c == '-' || (c >= '0' && c <= '9')
}

// bashWholeWriteTargets returns the indexes of the words the command replaces as a
// whole file: every operand of tee, the destination of a copy or move, the "of="
// operand of dd.
func bashWholeWriteTargets(verb string, words []string, redirectWord []bool) map[int]bool {
	args := make([]int, 0, len(words))
	for i, w := range words {
		if w == "" || strings.HasPrefix(w, "-") || redirectWord[i] {
			continue
		}
		args = append(args, i)
	}
	switch {
	case verb == "tee":
		return indexSet(args)
	case verb == "dd":
		for _, i := range args {
			if strings.HasPrefix(words[i], "of=") {
				return indexSet([]int{i})
			}
		}
	case bashWriteVerbs[verb] && len(args) > 0:
		if verb == "install" && hasDirectoryMode(words) {
			return nil // "install -d" creates directories, not the source's file
		}
		return indexSet(args[len(args)-1:])
	}
	return nil
}

// hasDirectoryMode reports install's -d/--directory switch.
func hasDirectoryMode(words []string) bool {
	for _, w := range words {
		if w == "--directory" || w == "-d" {
			return true
		}
	}
	return false
}

// hasInPlaceFlag reports the in-place switch of an editor: -i, -i.bak, -pi,
// --in-place. The "i" counts only at the end of a flag group or before a suffix,
// so perl's -MList::Util is not one.
func hasInPlaceFlag(args []string) bool {
	for _, a := range args {
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		for i := 1; i < len(a); i++ {
			if a[i] == 'i' && (i == len(a)-1 || a[i+1] == '.') {
				return true
			}
		}
	}
	return false
}

// protectedTargetWord returns the resolved path when a command word names one of
// the protected files.
func protectedTargetWord(word, cwd string, vars shellVars) string {
	word = strings.Trim(word, "()")
	if word == "" || strings.HasPrefix(word, "-") {
		return ""
	}
	// The ambiguity check reads the word as written: dequoting drops the backtick that
	// marks a substitution, and the pattern needs it to see an unknown run.
	written := word
	word = shellDequote(word)
	if word == "" {
		return ""
	}
	continues := gluedExpansion(written)
	resolved := resolveHookPath(expandTargetWord(word, vars), cwd)
	if isProtectedWritePath(resolved) {
		return resolved
	}
	if continues {
		// The expansion continues the path instead of preceding it: "$HOME/.claude$x"
		// names the settings file as soon as x is "/settings.json", and the resolved word
		// stops at the directory. Any other value moves the write away, so the block
		// names the file the word can still become.
		if completed, ok := completedProtectedPath(resolved); ok {
			return completed
		}
	}
	// What is left is a name the word can only partly spell ("$HOME/.claude/$f.json",
	// "$HOME/.claude/$(printf settings.json)"): the resolved prefix is no prefix of a
	// protected file, but the unknown run can still complete it. The pattern is built from
	// the word without its quote characters, so a quoted spelling builds the same pattern
	// as the bare one; the backtick stays, because it is the marker of an unknown run.
	if ambiguous, ok := ambiguousProtectedTarget(unquotedWord(written), cwd, vars); ok {
		return ambiguous
	}
	// A word that is nothing but a variable the command assigned takes the shape of its
	// value: "p=$HOME/.claude/$f.json; … > $p" is the same write as the direct spelling,
	// and the value is what carries the unknown run (cold review Los 56h).
	if value := vars.singleVariable(written); value != "" {
		if ambiguous, ok := ambiguousProtectedTarget(unquotedWord(shellDequote(value)), cwd, vars); ok {
			return ambiguous
		}
	}
	return ""
}

// unquotedWord removes the shell's quote characters from a word and keeps everything
// else, including the backtick that marks a command substitution. A quoted path names
// the same file as the bare spelling, so the ambiguity check has to see the same bytes:
// "> \"$HOME/.claude/$(true)/settings.json\"" writes the settings file like the unquoted
// word does (YM-407). A single-quoted word names no expansion in the shell, but this scan
// cannot tell the two apart and treats it like the double-quoted one, which errs towards
// a block, the same direction expandTargetWord takes.
func unquotedWord(word string) string {
	if !strings.ContainsAny(word, "\"'") {
		return word
	}
	var b strings.Builder
	b.Grow(len(word))
	var quote byte
	for i := 0; i < len(word); i++ {
		c := word[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			b.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// singleVariable returns the recorded value when word is exactly one "$name" or "${name}"
// and nothing else.
func (v shellVars) singleVariable(word string) string {
	name, _, end, ok := parameterAt(word, 0)
	if !ok || end != len(word)-1 {
		return ""
	}
	return v[name]
}

// gluedExpansion reports a word whose last path component mixes an expansion with at
// least one literal byte: "$HOME/.claude$x" and "$HOME/.claude/$f.json" can both still
// name a protected file, and neither resolves to one. A component that is nothing but
// an expansion ("$HOME/.claude/$f") names a file of its own and stays out of reach
// (YM-387).
func gluedExpansion(word string) bool {
	base := word
	if i := strings.LastIndexByte(word, '/'); i >= 0 {
		base = word[i+1:]
	}
	if !strings.ContainsAny(base, "$`") {
		return false
	}
	return literalRemainsAfterExpansions(base)
}

// literalRemainsAfterExpansions reports whether s carries a byte that is not part of
// an expansion.
func literalRemainsAfterExpansions(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			return true
		}
		if s[i+1] == '{' {
			if close := strings.IndexByte(s[i+2:], '}'); close >= 0 {
				i += 2 + close
				continue
			}
			return true
		}
		end := i + 1
		for end < len(s) && isWordByte(s[end]) {
			end++
		}
		if end == i+1 {
			return true // a lone "$" is a literal byte
		}
		i = end - 1
	}
	return false
}

// ambiguousNamePart reports whether the last path component carries a part the command
// does not fix in a way that can still complete a protected name: a command substitution
// (a program's output the guard cannot see, which the collapsed copy marks with
// substitutionSentinel), or a mixture of literal bytes and an unknown run. A component
// that is nothing but one variable stays out of reach: its value is the command's own
// choice, and blocking every "> ~/.claude/$name" would block the directory's ordinary
// writes (YM-383).
func ambiguousNamePart(word string) bool {
	base := word
	if i := strings.LastIndexByte(word, '/'); i >= 0 {
		base = word[i+1:]
	}
	if strings.Contains(base, "$(") || strings.Contains(base, "`") || strings.IndexByte(base, substitutionSentinel) >= 0 {
		return true
	}
	return literalRemainsAfterExpansions(base)
}

// ambiguousProtectedTarget returns the protected file a word with an unknown run in
// its name can still name. The directory part must be known (an unknown directory
// names a tree the guard cannot place), and the name pattern has to match one of the
// files that directory protects. "$HOME/.claude/$f.json" can be settings.json,
// "$HOME/.claude/$f.log" cannot, and the settings file counts only in the home
// directory, the same scope isProtectedWritePath uses.
func ambiguousProtectedTarget(word, cwd string, vars shellVars) (string, bool) {
	if !ambiguousNamePart(word) {
		return "", false
	}
	pattern := targetWordPattern(word, vars)
	// The unknown run may expand to nothing, and the path it sits in then collapses onto
	// the file its literal bytes already spell: "$HOME/.claude/$(true)/settings.json" and
	// "$HOME/.claude$(true)/settings.json" both name the settings file once the empty run
	// and the separator it leaves behind resolve away. filepath.Clean folds that leftover
	// separator, so the comparison stays exact, and a directory the word does not spell
	// out at all ("> $(mktemp -d)/settings.json") stays silent like every unknown
	// directory (YM-407, YM-383). Only the unknown run vanishes here: a literal "*" the
	// caller typed is a glob, and removing it would invent a path the command never
	// spells.
	collapsed := filepath.Clean(strings.ReplaceAll(pattern, string(unknownRun), ""))
	if collapsed != pattern {
		if resolved := resolveHookPath(collapsed, cwd); isProtectedWritePath(resolved) {
			return resolved, true
		}
	}
	// The same question for a name the empty run leaves behind partly unknown: the pattern
	// as written keeps the directory it spells out, the collapsed one lets an unknown run
	// in the directory vanish, so "$HOME/.claude/$(true)/*settings.json" matches too.
	for _, view := range []string{pattern, collapsed} {
		if hit, ok := protectedNameInPattern(view, cwd); ok {
			return hit, true
		}
	}
	return "", false
}

// protectedNameInPattern matches one pattern view against the protected names of its
// directory. The directory has to be known in this view: a literal glob stands for the
// files it matches, and an unknown run for a tree the guard cannot place, so neither can
// be compared with a directory at all.
func protectedNameInPattern(pattern, cwd string) (string, bool) {
	sep := strings.LastIndexByte(pattern, '/')
	if sep <= 0 {
		return "", false
	}
	dirPattern, nameRun := pattern[:sep], pattern[sep+1:]
	if strings.Contains(dirPattern, "*") || strings.ContainsRune(dirPattern, unknownRun) {
		return "", false
	}
	namePattern := strings.ReplaceAll(nameRun, string(unknownRun), "*")
	if !strings.Contains(namePattern, "*") {
		return "", false
	}
	dir := resolveHookPath(dirPattern, cwd)
	var dirs []struct {
		dir   string
		names []string
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		settings := filepath.Join(home, protectedSettingsPath)
		dirs = append(dirs, struct {
			dir   string
			names []string
		}{filepath.Dir(settings), []string{filepath.Base(settings)}})
	}
	if strings.EqualFold(filepath.Base(dir), protectedFileDir) {
		dirs = append(dirs, struct {
			dir   string
			names []string
		}{dir, protectedFiles})
	}
	for _, candidate := range dirs {
		if dir != candidate.dir {
			continue
		}
		for _, name := range candidate.names {
			if wildcardMatch(namePattern, name) {
				return filepath.Join(candidate.dir, name), true
			}
		}
	}
	return "", false
}

// targetWordPattern renders a word as a pattern in which every part the command does
// not fix stands for an unknown run of bytes: a variable it never assigned, a
// parameter with a fallback, and a command substitution. $HOME and the variables this
// command assigned are expanded for real, so a known path stays an exact word. A "*" the
// word itself carries is copied through: it is a glob, not an unknown run.
func targetWordPattern(word string, vars shellVars) string {
	var b strings.Builder
	b.Grow(len(word))
	for i := 0; i < len(word); i++ {
		c := word[i]
		if c == substitutionSentinel {
			// The whole substitution, in the collapsed copy of the command.
			b.WriteByte(unknownRun)
			continue
		}
		if c == '`' {
			if close := strings.IndexByte(word[i+1:], '`'); close >= 0 {
				b.WriteByte(unknownRun)
				i += 1 + close
				continue
			}
			// An unterminated backtick (a span the word splitter already cut) leaves the
			// rest of the word unknown.
			b.WriteByte(unknownRun)
			return b.String()
		}
		if c != '$' || i+1 >= len(word) {
			b.WriteByte(c)
			continue
		}
		if word[i+1] == '(' {
			// A command substitution runs a program the guard cannot see; its output is
			// an unknown run. The closing paren is found by depth, so a nested "$(…)"
			// inside the substitution does not end it early.
			depth, end := 0, -1
			for j := i + 1; j < len(word); j++ {
				if word[j] == '(' {
					depth++
					continue
				}
				if word[j] == ')' {
					depth--
					if depth == 0 {
						end = j
						break
					}
				}
			}
			if end < 0 {
				// An unterminated substitution (a word the splitter already cut) leaves
				// the rest of the word unknown.
				b.WriteByte(unknownRun)
				return b.String()
			}
			b.WriteByte(unknownRun)
			i = end
			continue
		}
		name, value, end, ok := parameterAt(word, i)
		if !ok {
			b.WriteByte(c)
			continue
		}
		switch {
		case name == "HOME":
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				b.WriteString(home)
			}
		case vars[name] != "":
			b.WriteString(targetWordPattern(vars[name], nil))
		case value != "":
			b.WriteString(targetWordPattern(value, vars))
		default:
			b.WriteByte(unknownRun)
		}
		i = end
	}
	return b.String()
}

// parameterAt reads one "$NAME" or "${NAME}" (with an optional fallback) starting at
// the dollar. It returns the parameter's name, the fallback the shell would use, the
// index of the last byte the expansion covers, and whether the shape is an expansion
// at all.
func parameterAt(word string, i int) (name, fallback string, end int, ok bool) {
	if i+1 >= len(word) || word[i] != '$' {
		return "", "", i, false
	}
	if word[i+1] == '{' {
		close := matchingBrace(word, i+2)
		if close < 0 {
			return "", "", i, false
		}
		param := word[i+2 : close]
		name, fallback = paramNameAndFallback(param)
		return name, fallback, close, true
	}
	end = i + 1
	for end < len(word) && isWordByte(word[end]) {
		end++
	}
	if end == i+1 {
		return "", "", i, false
	}
	return word[i+1 : end], "", end - 1, true
}

// paramNameAndFallback splits a "${…}" body into the parameter name and the fallback
// the shell substitutes when the parameter is unset or empty ("${x:-/elsewhere}").
func paramNameAndFallback(param string) (name, fallback string) {
	for _, sep := range []string{":-", ":=", "-", "="} {
		if i := strings.Index(param, sep); i >= 0 {
			return param[:i], param[i+len(sep):]
		}
	}
	return param, ""
}

// matchingBrace returns the index of the "}" that closes the expansion opened before
// start, counting nested "${" so a fallback that spells another expansion
// ("${x:-${HOME}/…}") does not end at the inner brace.
func matchingBrace(word string, start int) int {
	depth := 1
	for i := start; i < len(word); i++ {
		switch {
		case word[i] == '$' && i+1 < len(word) && word[i+1] == '{':
			depth++
			i++
		case word[i] == '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchingParen returns the index of the ")" that closes the "(" at start.
func matchingParen(s string, start int) int {
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// wildcardMatch reports whether name matches pattern, where "*" stands for any run of
// bytes, including an empty one. Only "*" is special: a protected file name carries
// no other pattern byte.
func wildcardMatch(pattern, name string) bool {
	p, n := 0, 0
	star, mark := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			p = star + 1
			mark++
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// completedProtectedPath returns the protected file a word can still be extended into.
// The word must be a proper prefix of the file's path; the settings file counts only in
// the home directory, the generated file in any ".claude" directory, the same scope
// isProtectedFile uses.
func completedProtectedPath(resolved string) (string, bool) {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, protectedSettingsPath))
	}
	// The generated file sits in a ".claude" directory, and the word may be a prefix of
	// that directory's name or of the file name inside it.
	dir := resolved
	if !strings.EqualFold(filepath.Base(dir), protectedFileDir) {
		dir = filepath.Dir(resolved)
	}
	if strings.EqualFold(filepath.Base(dir), protectedFileDir) {
		for _, name := range protectedFiles {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, candidate := range candidates {
		if candidate != resolved && strings.HasPrefix(candidate, resolved) {
			return candidate, true
		}
	}
	return "", false
}

// dequotedTargetWord returns the bytes a shell word names, with the expansions a
// command controls read as empty (see expandTargetWord). The three sinks that turn a
// word into a path (the redirect target, the copy destination, the cd operand) share
// it, so a variable prefix cannot slip through one of them while another expands it
// (YM-383).
func dequotedTargetWord(word string, vars shellVars) string {
	return expandTargetWord(shellDequote(word), vars)
}

// expandTargetWord expands the home shorthands anywhere in a shell word, the variables
// this command assigned to their values, and every other "$NAME" and "${NAME}" to the
// empty string. The shell joins an expansion onto the rest of the word, so
// "${x}$HOME/.claude/settings.json" truncates the settings file as soon as x is empty
// (YM-383). Empty is the one value a command can always arrange ("x= printf …"), and
// every other value moves the write to a path the protected-file comparison then
// rejects.
//
// A quoted "$HOME" was dequoted before this runs, so it expands like the bare
// spelling. A single-quoted word names no expansion in the shell, but this scan
// cannot tell the two apart and expands it too, which errs towards a block.
// "${x:-/elsewhere}" names its fallback whenever x is unset, and a command can leave a
// variable unset, so the fallback is expanded as well (YM-387). A parameter that is not
// a name ("$(") keeps its bytes: erring towards silence.
func expandTargetWord(word string, vars shellVars) string {
	if !strings.Contains(word, "$") {
		return word
	}
	return expandTargetWordDepth(word, vars, 0)
}

// expandTargetWordDepth bounds the substitution chain: a recorded value may name
// another assignment, and a self-naming one ("a=$a") must not spin.
func expandTargetWordDepth(word string, vars shellVars, depth int) string {
	if !strings.Contains(word, "$") || depth > maxVarExpansions {
		return word
	}
	home, _ := os.UserHomeDir()
	var b strings.Builder
	b.Grow(len(word))
	for i := 0; i < len(word); i++ {
		c := word[i]
		if c != '$' || i+1 >= len(word) {
			b.WriteByte(c)
			continue
		}
		name, fallback, end, ok := parameterAt(word, i)
		if !ok {
			// "${" without a close, or a lone "$": the shell has no expansion here, so
			// the bytes stay. "$@" and "$*" are empty without positional parameters,
			// which is how a non-interactive shell runs them (YM-383).
			if c == '$' && word[i+1] == '{' {
				b.WriteByte(c)
				continue
			}
			if c == '$' && (word[i+1] == '@' || word[i+1] == '*') {
				i++
				continue
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case name == "HOME" && home != "":
			b.WriteString(home)
		case vars[name] != "":
			b.WriteString(expandTargetWordDepth(shellDequote(vars[name]), vars, depth+1))
		case fallback != "":
			b.WriteString(expandTargetWordDepth(fallback, vars, depth+1))
		}
		i = end
	}
	return b.String()
}

// trimKeyPrefix strips a "key=" prefix so the operand of dd of=path is resolved as
// a path.
func trimKeyPrefix(word string) string {
	if rest, ok := strings.CutPrefix(word, "of="); ok {
		return rest
	}
	return word
}

// commandName reduces a word to the command it names, tolerating a leading path and
// quotes.
func commandName(word string) string {
	return strings.ToLower(filepath.Base(strings.Trim(word, "\"'`(")))
}

// isWrapperPrefixWord reports a word that still belongs to the wrapper in front of
// the command: the wrapper itself, one of its flags, a flag value, a duration or an
// assignment. Only the first word has to be named as a wrapper; behind one, flags
// and their operands are part of it.
func isWrapperPrefixWord(word string, index int) bool {
	if index == 0 {
		return bashWrappers[commandName(word)] || isAssignmentWord(word)
	}
	if bashWrappers[commandName(word)] {
		return true
	}
	return strings.HasPrefix(word, "-") || isNumber(word) || isDuration(word) || isAssignmentWord(word)
}

// isAssignmentWord reports "NAME=value", the shape env and a shell accept in front
// of a command.
func isAssignmentWord(word string) bool {
	name, _, found := strings.Cut(word, "=")
	if !found || name == "" || strings.HasPrefix(word, "-") {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isWordByte(name[i]) {
			return false
		}
	}
	return true
}

// isDuration reports timeout's "30s", "5m", "1h".
func isDuration(word string) bool {
	if len(word) < 2 {
		return false
	}
	switch word[len(word)-1] {
	case 's', 'm', 'h', 'd':
		return isNumber(word[:len(word)-1])
	}
	return false
}

// isNumber reports a word that is only digits, the duration of timeout or nice.
func isNumber(word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i < len(word); i++ {
		if word[i] < '0' || word[i] > '9' {
			return false
		}
	}
	return true
}

func indexSet(indexes []int) map[int]bool {
	if len(indexes) == 0 {
		return nil
	}
	set := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		set[i] = true
	}
	return set
}
