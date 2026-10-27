package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLos56hTildeCdReachesTheGuard covers YM-387 case 1: "cd ~" changes into the
// home directory, but resolveHookPath only expanded the "~/" spelling, so the bare
// "~" stayed a relative directory named "~" under the session cwd and the settings
// write behind it compared against a path that does not exist.
func TestLos56hTildeCdReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `cd ~ && printf PWNED > .claude/settings.json`, settings},
		{"a quoted tilde", `cd "~" && printf PWNED > .claude/settings.json`, settings},
		{"the generated ops file behind a tilde cd", `cd ~ && printf PWNED > .claude/yesmem-ops.md`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a tilde cd to an unrelated tree", `cd ~ && printf PWNED > notes.txt`},
		{"a plain command", `echo hello`},
	}
	los31gAssert(t, home, blocked, allowed)

	// A copy whose destination is the directory itself needs that directory: the guard
	// asks the filesystem whether the last operand receives the source's name.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("create .claude: %v", err)
	}
	los31gAssert(t, home, [][3]string{
		{"a tilde cd before a copy", `cd ~/.claude && cp /x/yesmem-ops.md .`, "yesmem-ops.md"},
	}, nil)
}

// TestLos56hAssignedVariableTargetReachesTheGuard covers YM-387 case 3: the path is
// built in a variable and the write names only the variable. A Bash tool call is one
// shell, so the assignment is part of the same command and its value is knowable.
func TestLos56hAssignedVariableTargetReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `p=$HOME/.claude/settings.json; printf PWNED > $p`, settings},
		{"a braced variable", `p=$HOME/.claude/settings.json; printf PWNED > ${p}`, settings},
		{"a quoted value", `p="$HOME/.claude/settings.json"; printf PWNED > "$p"`, settings},
		{"a directory variable before cd", `d=$HOME/.claude; cd $d && printf PWNED > settings.json`, settings},
		{"a file name variable", `f=settings.json; printf PWNED > $HOME/.claude/$f`, settings},
		{"a tee behind a variable verb", `x=tee; printf PWNED | $x $HOME/.claude/settings.json`, settings},
		{"the generated ops file", `p=$HOME/.claude/yesmem-ops.md; echo x > $p`, "yesmem-ops.md"},
		{"an assigned partial name", `p=$HOME/.claude/$f.json; printf PWNED > $p`, settings},
		{"an assigned substitution", `p=$HOME/.claude/$(printf settings.json); printf PWNED > $p`, settings},
	}
	allowed := [][2]string{
		{"a variable naming an unrelated file", `p=` + filepath.Join(home, "notes.txt") + `; printf PWNED > $p`},
		{"an unassigned variable stays unresolvable", `cd $B && printf PWNED > settings.json`},
		{"an environment variable stays unresolvable", `cd $TMPDIR && printf PWNED > settings.json`},
		{"an assignment that is never a target", `p=$HOME/.claude/settings.json; echo ok`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos56hDefaultExpansionReachesTheGuard covers YM-387 case 4: a parameter with a
// fallback names the fallback whenever the parameter is unset, and the write then
// lands on the file the fallback spells.
func TestLos56hDefaultExpansionReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `printf PWNED > ${x:-$HOME/.claude/settings.json}`, settings},
		{"a braced home in the fallback", `printf PWNED > ${x:-${HOME}/.claude/settings.json}`, settings},
		{"an absolute fallback", `printf PWNED > ${x:-` + settings + `}`, settings},
		{"the generated ops file", `echo x > ${x:-$HOME/.claude/yesmem-ops.md}`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a fallback into an unrelated file", `printf PWNED > ${x:-` + filepath.Join(home, "notes.txt") + `}`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos56hPartialExpansionReachesTheGuard covers YM-387 case 6 and the command
// substitution of case 5: the last path component mixes an unknown run with literal
// bytes, so the word can still name a protected file. A component that can only ever
// end in another name stays silent.
func TestLos56hPartialExpansionReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"an unknown name in front of the suffix", `printf PWNED > $HOME/.claude/$f.json`, settings},
		{"an unknown name behind a literal prefix", `printf PWNED > $HOME/.claude/settings$x`, "settings.json"},
		{"a command substitution as the name", `printf PWNED > $HOME/.claude/$(printf settings.json)`, settings},
		{"a backtick substitution as the name", "printf PWNED > $HOME/.claude/`printf settings.json`", settings},
		{"a substitution on the ops file", `echo x > $HOME/.claude/$(echo yesmem-ops).md`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a suffix that cannot name a protected file", `printf PWNED > $HOME/.claude/$f.log`},
		{"a substitution that cannot name a protected file", `printf PWNED > $HOME/.claude/$(date +%s).log`},
		{"a plain write into the directory", `printf PWNED > $HOME/.claude/notes.txt`},
		{"a write outside the protected directory", `printf PWNED > $HOME/$f.json`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos56hSubstitutionSpansKeepTheirOwnSeparators is the regression the cold review
// found: masking the separators inside a command substitution made a compound body look
// like one word, so a write in its middle was never scanned. main sees it, the first
// version of this branch did not (YM-387).
func TestLos56hSubstitutionSpansKeepTheirOwnSeparators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"a copy behind a separator", `echo $(cat a; cp /tmp/p ` + settings + `)`, settings},
		{"a copy behind a pipe", `echo $(cat a | cp /tmp/p ` + settings + `)`, settings},
	}
	// Two shapes stay uncaught, on main as well, so they are not this branch's business:
	// a substitution whose first word glues the command name ("$(a)$(cp x path)"), and a
	// backtick span holding a redirect (maskQuoted blanks the whole span).
	los31gAssert(t, home, blocked, nil)
}
