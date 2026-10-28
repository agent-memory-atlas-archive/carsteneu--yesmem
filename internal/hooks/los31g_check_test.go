package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los31gCases asserts every command in blocked is blocked and names want, and every
// command in allowed stays silent. Both directions matter: the redirect lexer reads
// operator bytes, so a shape that only mentions the file must not turn into a block.
func los31gAssert(t *testing.T, home string, blocked [][3]string, allowed [][2]string) {
	t.Helper()
	for _, tt := range blocked {
		t.Run(tt[0], func(t *testing.T) {
			los31fBlocked(t, home, tt[1], tt[2])
		})
	}
	for _, tt := range allowed {
		t.Run(tt[0], func(t *testing.T) {
			los31fAllowed(t, home, tt[1])
		})
	}
}

// TestLos31gGluedRedirectOperatorReachesTheGuard covers finding 1 of YM-383: the
// operator may follow its left operand without a space ("x>"), and its target may be
// the next word. redirectTarget reported "no target in this word" for that shape,
// which the caller read as "not a redirect", so the operator word was scanned as an
// ordinary operand and the target word never reached the guard.
func TestLos31gGluedRedirectOperatorReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `printf PWNED x> $HOME/.claude/settings.json`, settings},
		{"a longer left operand", `printf PWNED echo> $HOME/.claude/settings.json`, settings},
		{"an absolute home", `printf PWNED x> ` + settings, settings},
		{"a tilde target", `printf PWNED x> ~/.claude/settings.json`, settings},
		{"a quoted target", `printf PWNED x> "$HOME/.claude/settings.json"`, settings},
		{"a clobber operator behind an operand", `printf PWNED x>| $HOME/.claude/settings.json`, settings},
		{"a double redirect behind an operand", `printf PWNED x>> $HOME/.claude/settings.json`, settings},
		{"the generated ops file", `printf PWNED x> $HOME/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"a relative target after cd", `cd $HOME/.claude && printf PWNED x> settings.json`, settings},
	}
	allowed := [][2]string{
		{"a target outside the protected files", `printf PWNED x> ` + filepath.Join(home, "notes.txt")},
		{"a plain command", `echo hello`},
		{"a redirect into an unrelated file", `printf PWNED > ` + filepath.Join(home, "notes.txt")},
		{"an fd duplication keeps its neighbour a plain operand", `echo hi 2>&1 $HOME/.claude/settings.json`},
		{"a comparison", `test 1 > 2`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos31gVariablePrefixTargetReachesTheGuard covers finding 2 of YM-383: the shell
// joins the value of an expansion onto the rest of the word, so
// "${x}$HOME/.claude/settings.json" names the settings file as soon as the variable is
// empty. The word never compared equal to the path, so the guard stayed silent.
func TestLos31gVariablePrefixTargetReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `printf PWNED > ${x}$HOME/.claude/settings.json`, settings},
		{"a bare variable prefix", `printf PWNED > $x$HOME/.claude/settings.json`, settings},
		{"a brace home behind the prefix", `printf PWNED > ${x}${HOME}/.claude/settings.json`, settings},
		{"an absolute home behind the prefix", `printf PWNED > ${x}` + settings, settings},
		{"a quoted home behind the prefix", `printf PWNED > ${x}"$HOME"/.claude/settings.json`, settings},
		{"an attached operator", `printf PWNED >${x}$HOME/.claude/settings.json`, settings},
		{"the generated ops file", `echo x > ${x}$HOME/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"a relative target after cd", `cd $HOME/.claude && printf PWNED > ${x}settings.json`, settings},
		{"a cd into a prefixed home", `cd ${x}$HOME/.claude && printf PWNED > settings.json`, settings},
		{"a copy into a prefixed home", `cp /x/yesmem-ops.md ${x}$HOME/.claude/`, "yesmem-ops.md"},
		{"a copy whose source carries the prefix", `cp ${x}yesmem-ops.md $HOME/.claude/`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a prefix in front of an unrelated file", `printf PWNED > ${x}$HOME/notes.txt`},
		{"a bare prefix with a relative path", `printf PWNED > $x/notes.txt`},
		{"a quoted mention without a redirect", `echo "${x}$HOME/.claude/settings.json"`},
		{"a read through a prefix", `cat ${x}$HOME/.claude/settings.json`},
		{"a prefix onto an absolute path outside home", `printf PWNED > ${x}/var/tmp/notes.txt`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos31gBothStreamsRedirectReachesTheGuard covers the sibling of the clobber
// redirect: ">&file" and "&> file" redirect both streams to the file and truncate it
// (measured on bash 5.2). bashSegments split at every "&", so the operator arrived
// without its target and both write locks stayed silent.
func TestLos31gBothStreamsRedirectReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"an attached target", `printf PWNED >&$HOME/.claude/settings.json`, settings},
		{"a separate target", `printf PWNED >& $HOME/.claude/settings.json`, settings},
		{"the other spelling", `printf PWNED &>$HOME/.claude/settings.json`, settings},
		{"a second redirect behind it", `printf PWNED >&$HOME/.claude/settings.json 2>&1`, settings},
		{"the generated ops file", `echo x >&$HOME/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"a relative target after cd", `cd $HOME/.claude && printf PWNED >& settings.json`, settings},
	}
	allowed := [][2]string{
		{"a descriptor duplication", `echo hi >&2`},
		{"a descriptor duplication keeps its neighbour a plain operand", `echo hi >&2 $HOME/.claude/settings.json`},
		{"a both-streams redirect into an unrelated file", `printf x >& ` + filepath.Join(home, "notes.txt")},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos31gLexerKeepsMentionsSilent pins the direction the lexer must not lose: a
// command that only reads the path, or that quotes or escapes the operator byte, stays
// silent. Without this half, reading operator bytes would turn every mention of the
// file into a block.
func TestLos31gLexerKeepsMentionsSilent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	allowed := [][2]string{
		{"an escaped operator", `grep \> $HOME/.claude/settings.json`},
		{"an escaped operator inside a word", `echo x\>y $HOME/.claude/settings.json`},
		{"a quoted operator beside a quoted argument with a newline", "grep '> $HOME/.claude/settings.json' x \"a\nb\""},
		{"a quoted operator beside a masked non-breaking space", "printf X > \"$HOME/.claude/settings.json>keep\" \"a\u00a0b\""},
		{"a plain mention", `echo $HOME/.claude/settings.json`},
		{"a read", `cat $HOME/.claude/settings.json`},
	}
	los31gAssert(t, home, nil, allowed)
}

// TestLos31gQuotedSpanKeepsItsStateAcrossWords covers the security finding that the
// per-word mask started every word unquoted: a quoted argument that holds a whitespace
// rune splits the raw text, and its closing quote then opened a new span that swallowed
// the redirect behind it. bash truncates the file in every one of these shapes
// (measured on bash 5.2).
func TestLos31gQuotedSpanKeepsItsStateAcrossWords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"a quoted newline", "printf PWNED \"x\ny\">$HOME/.claude/settings.json", settings},
		{"a quoted non-breaking space", "printf PWNED \"x\u00a0y\">$HOME/.claude/settings.json", settings},
		{"a single-quoted non-breaking space", "printf PWNED 'x\u00a0y'>$HOME/.claude/settings.json", settings},
		{"an append", "printf PWNED \"x\ny\">>$HOME/.claude/settings.json", settings},
		{"the generated ops file", "echo x \"x\u00a0y\">$HOME/.claude/yesmem-ops.md", "yesmem-ops.md"},
	}
	los31gAssert(t, home, blocked, nil)
}

// TestLos31gCraftedWordDoesNotPanicTheGuard pins that a mask longer than its word cannot
// slice past it. A panic ends the hook without a decision, which is worse than either
// answer; the four payloads crashed the lexer before the clamp.
func TestLos31gCraftedWordDoesNotPanicTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	allowed := [][2]string{
		{"a doubled quoting of a non-breaking space", "printf PWNED >\"\u00a0 b\""},
		{"a single-quoted doubled space", "printf PWNED >'\u00a0 b'"},
		{"an attached operator with a doubled space", "printf PWNED x>\"\u00a0 b\""},
		{"a doubled space next to a real argument", "echo \"a\u00a0 b\" >\"\u00a0 c\""},
	}
	los31gAssert(t, home, nil, allowed)
}

// TestLos31gExpansionContinuingThePathReachesTheGuard covers the third security finding:
// the empty-value model only closes a variable in front of the path. A variable that
// continues it ("$HOME/.claude$x" with x="/settings.json") names the file as well
// (measured on bash 5.2), and the resolved word stops at the directory.
func TestLos31gExpansionContinuingThePathReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"a bare variable behind the directory", `printf PWNED > $HOME/.claude$x`, settings},
		{"a brace variable behind the directory", `printf PWNED > $HOME/.claude${x}`, settings},
		{"a pre-set variable", `x=/settings.json; printf PWNED > $HOME/.claude$x`, settings},
		{"an attached operator", `printf PWNED >$HOME/.claude$x`, settings},
		{"a variable behind a partial file name", `printf PWNED > $HOME/.claude/settings$x`, settings},
		{"the generated ops file", `printf PWNED > $HOME/.claude/yesmem-ops.m$x`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a variable behind a longer name", `printf PWNED > $HOME/.claudex`},
		{"a variable behind an unrelated directory", `printf PWNED > $HOME/notes$x`},
		{"a read through the same shape", `cat $HOME/.claude$x`},
		{"a directory that carries no expansion", `ls $HOME/.claude`},
		{"a copy into the directory", `cp a $HOME/.claude/`},
		// A component that is nothing but an expansion names a file of its own; naming it
		// in the block would claim a write the command does not make.
		{"a variable as a whole component", `printf PWNED > $HOME/.claude/$f`},
		{"a variable as a whole component in the project", `f=notes.txt; printf PWNED > .claude/$f`},
		{"a copy of such a component", `cp a $HOME/.claude/$f`},
		// "install -d" creates directories and replaces no file.
		{"a directory install", `install -d $HOME/.claude$x`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos31gRedirectIsNoOperandReachesTheGuard covers the regression the both-streams
// exception introduced: merging "&>" into the segment put the redirect and its target
// into the operand list, so the copy analysis took /dev/null as the destination and lost
// the write. The same flaw hid "cp a <settings> > /dev/null" on origin/main.
func TestLos31gRedirectIsNoOperandReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"a copy silenced with a both-streams redirect", `cp a ` + settings + ` &>/dev/null`, settings},
		{"the spaced spelling", `cp a ` + settings + ` &> /dev/null`, settings},
		{"an append behind the copy", `cp a ` + settings + ` &>>/dev/null`, settings},
		{"a move", `mv a ` + settings + ` &>/dev/null`, settings},
		{"a link", `ln -sf a ` + settings + ` &>/dev/null`, settings},
		{"a truncate", `truncate -s0 ` + settings + ` &>/dev/null`, settings},
		{"a copy into the directory", `cp /x/yesmem-ops.md $HOME/.claude/ &>/dev/null`, "yesmem-ops.md"},
		{"a copy silenced with a plain redirect", `cp a ` + settings + ` > /dev/null`, settings},
		{"a copy silenced through stderr", `cp a ` + settings + ` 2>/dev/null`, settings},
		{"a copy silenced on both streams", `cp a ` + settings + ` >/dev/null 2>&1`, settings},
	}
	allowed := [][2]string{
		{"a copy into an unrelated file", `cp a ` + filepath.Join(home, "notes.txt") + ` &>/dev/null`},
		{"a redirect into /dev/null only", `echo hi &>/dev/null`},
		{"a descriptor duplication", `echo hi >&2`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// shared word-to-path resolver first introduced: a cd into a directory the guard cannot
// name must not resolve to cwd, or a write into a temporary directory blocks as soon as
// the shell sits in ~/.claude.
func TestLos31gUnresolvableCdKeepsRelativeTargetsRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")

	allowed := []struct{ name, command string }{
		{"an unresolvable cd target", `cd $TMPDIR && printf PWNED > settings.json`},
		{"a quoted unresolvable cd target", `cd "$TMPDIR" && printf PWNED > settings.json`},
		{"an unresolvable cd target before a copy", `cd $TMPDIR && cp a settings.json`},
		{"an empty cd target", `cd $B && printf PWNED > settings.json`},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
				ToolName:  "Bash",
				CWD:       claudeDir,
				ToolInput: json.RawMessage(`{"command":` + quoteJSON(tt.command) + `}`),
			})
			if code != 0 {
				t.Errorf("%q must stay silent from %s, exit = %d, out = %q", tt.command, claudeDir, code, out)
			}
		})
	}

	// The same directory reached through $HOME still blocks: the fix must not trade the
	// false positive for a miss.
	out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "Bash",
		CWD:       home,
		ToolInput: json.RawMessage(`{"command":` + quoteJSON(`cd $HOME && printf PWNED > .claude/settings.json`) + `}`),
	})
	if code != 2 || !strings.Contains(out, filepath.Join(home, ".claude", "settings.json")) {
		t.Errorf("a cd into $HOME must still block the settings write, exit = %d, out = %q", code, out)
	}
}

// 2: a shell character appended to the target word ("target>", "target<") starts
// another redirection, but the truncation of the word's own target still happens
// (measured on bash 5.2: "printf X >f> /dev/null" leaves f created and empty). The
// guard compared the whole word against the path and lost the match.
func TestLos31gOperatorGluedToTheTargetReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"a redirect behind the target", `printf PWNED >$HOME/.claude/settings.json> /dev/null`, settings},
		{"an input redirect behind the target", `printf PWNED >$HOME/.claude/settings.json</dev/null`, settings},
		{"an appended redirect behind a spaced operator", `printf PWNED > $HOME/.claude/settings.json> /dev/null`, settings},
		{"a clobber redirect behind the target", `printf PWNED >|$HOME/.claude/settings.json> /dev/null`, settings},
		{"an appended redirect on the ops file", `echo x >$HOME/.claude/yesmem-ops.md> /dev/null`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"an appended redirect on an unrelated file", `printf PWNED >` + filepath.Join(home, "notes.txt") + `> /dev/null`},
		{"a descriptor duplication", `echo hi 2>&1 | cat`},
		{"a descriptor duplication onto /dev/null", `echo hi > /dev/null 2>&1`},
	}
	los31gAssert(t, home, blocked, allowed)
}
