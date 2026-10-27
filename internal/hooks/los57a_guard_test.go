package hooks

import (
	"path/filepath"
	"testing"
)

// TestLos57aClusteredShellFlagsReachTheGuard covers YM-407 case 1: the nest scan looked
// for the exact word "-c", so the clustered spelling that every login shell uses
// ("bash -lc '…'") skipped it. Flags may also stand between -c and the body ("bash -c -e
// '…'", "bash -c -- '…'"); both are measured shapes on bash 5.2.
func TestLos57aClusteredShellFlagsReachTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `bash -lc 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a clustered option pair", `sh -xc 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a flag behind -c", `bash -c -e 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"an option terminator behind -c", `bash -c -- 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a clustered flag behind a wrapper", `env bash -lc 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a login shell behind a timeout", `timeout 30 bash -lc 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a long option in front of the flag", `bash --norc -c 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a quoted flag word", `bash "-c" 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a flag word built from a variable", `f=-c; bash $f 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"an option value behind the flag", `bash -c -o pipefail 'printf PWNED > $HOME/.claude/settings.json'`, settings},
		{"a cluster nested two shells deep", `bash -c 'bash -lc "printf PWNED > $HOME/.claude/settings.json"'`, settings},
		{"the generated ops file", `bash -lc 'echo x > $HOME/.claude/yesmem-ops.md'`, "yesmem-ops.md"},
	}
	allowed := [][2]string{
		{"a login shell without -c", `bash -l script.sh`},
		{"a shell that only traces its script", `bash -x script.sh`},
		{"a clustered body without a write", `bash -lc 'echo hello'`},
		{"an unrelated command", `echo hello`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos57aCollapsingSubstitutionInDirectoryReachesTheGuard covers YM-407 case 2: an
// unknown run in the DIRECTORY part was read as "the tree cannot be placed", so the word
// stayed silent. Its expansion may be empty, and the path then collapses onto the file
// the literal bytes already spell: "$HOME/.claude/$(true)/settings.json" is the settings
// file for the kernel as well.
func TestLos57aCollapsingSubstitutionInDirectoryReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", `printf PWNED > $HOME/.claude/$(true)/settings.json`, settings},
		{"an empty printf", `printf PWNED > $HOME/.claude/$(printf '')/settings.json`, settings},
		{"a substitution that yields a dot", `printf PWNED > $HOME/.claude/$(echo .)/settings.json`, settings},
		{"a tilde directory", `printf PWNED > ~/.claude/$(true)/settings.json`, settings},
		{"a braced home", `printf PWNED > ${HOME}/.claude/$(true)/settings.json`, settings},
		{"a quoted word", `printf PWNED > "$HOME/.claude/$(true)/settings.json"`, settings},
		{"the generated ops file", `echo x > $HOME/.claude/$(true)/yesmem-ops.md`, "yesmem-ops.md"},
		{"a name fragment in the directory", `printf PWNED > $HOME/.cla$(true)ude/settings.json`, settings},
		{"an unknown run beside a glob", `printf PWNED > $HOME/.claude/$(true)/*settings.json`, settings},
	}
	allowed := [][2]string{
		{"a substitution in the directory of an unrelated name", `printf PWNED > $HOME/.claude/$(true)/notes.txt`},
		{"a substitution in a deeper subdirectory", `printf PWNED > $HOME/.claude/x/$(true)/settings.json`},
		// A whole unknown directory names a tree the guard cannot place, the same
		// decision that keeps "> $D/settings.json" silent (YM-383).
		{"a directory the command builds itself", `printf PWNED > $(mktemp -d)/settings.json`},
		{"a glob written by hand", `printf PWNED > $HOME/.claude/*/settings.json`},
		{"a glob above the protected directory", `printf PWNED > $HOME/*/.claude/settings.json`},
		{"a plain subdirectory", `printf PWNED > $HOME/.claude/sub/settings.json`},
	}
	los31gAssert(t, home, blocked, allowed)
}

// TestLos57aBacktickBodyReachesTheGuard covers YM-407 case 3: maskQuoted blanks a
// backtick span as if it were a quote, so a redirect inside the span was invisible. The
// shell runs that body, so it writes exactly like the "$(…)" spelling.
func TestLos57aBacktickBodyReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := [][3]string{
		{"the reported shape", "X=`printf PWNED > $HOME/.claude/settings.json`", settings},
		{"an absolute target inside the span", "X=`printf PWNED > " + settings + "`", settings},
		{"a span inside a double quoted word", `X="prefix$(true)` + "`printf PWNED > $HOME/.claude/settings.json`" + `"`, settings},
		{"the generated ops file", "X=`echo x > $HOME/.claude/yesmem-ops.md`", "yesmem-ops.md"},
		{"a span as a command of its own", "`printf PWNED > $HOME/.claude/settings.json`", settings},
		{"a span beside a shell that runs a script", "bash -l run.sh `printf PWNED > $HOME/.claude/settings.json`", settings},
	}
	allowed := [][2]string{
		{"a span without a write", "X=`date`"},
		{"a span in a single quoted word is literal", `echo '` + "`printf PWNED > $HOME/.claude/settings.json`" + `'`},
		{"an escaped backtick is literal", "echo \\`printf PWNED > $HOME/.claude/settings.json\\`"},
		{"a span writing an unrelated file", "X=`printf PWNED > " + filepath.Join(home, "notes.txt") + "`"},
		{"a backtick in a double quoted message", "echo \"run `date +%s` now\""},
		{"a backtick in a single quoted commit message", "git commit -m 'fix: use `cat > $HOME/.claude/settings.json`'"},
	}
	los31gAssert(t, home, blocked, allowed)
}
