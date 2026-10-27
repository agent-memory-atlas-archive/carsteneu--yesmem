package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los31fBlocked asserts that a Bash command is blocked and that the block names the
// file the command actually writes, so a hit on the wrong guard cannot pass.
func los31fBlocked(t *testing.T, home, command, want string) {
	t.Helper()
	out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "Bash",
		CWD:       home,
		ToolInput: json.RawMessage(`{"command":` + quoteJSON(command) + `}`),
	})
	if code != 2 {
		t.Errorf("%q must be blocked, exit = %d, out = %q", command, code, out)
		return
	}
	if !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("expected a block decision for %q, got: %q", command, out)
		return
	}
	if !strings.Contains(out, want) {
		t.Errorf("the block for %q must name %q, got: %q", command, want, out)
	}
}

// los31fBlockedRepl is the same assertion for a REPL body whose sh() reaches the
// shell.
func los31fBlockedRepl(t *testing.T, home, code, want string) {
	t.Helper()
	out, exit := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "REPL",
		CWD:       home,
		ToolInput: json.RawMessage(`{"code":` + quoteJSON(code) + `}`),
	})
	if exit != 2 {
		t.Errorf("%q must be blocked, exit = %d, out = %q", code, exit, out)
		return
	}
	if !strings.Contains(out, want) {
		t.Errorf("the block for %q must name %q, got: %q", code, want, out)
	}
}

// los31fAllowed asserts that a command stays silent.
func los31fAllowed(t *testing.T, home, command string) {
	t.Helper()
	out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "Bash",
		CWD:       home,
		ToolInput: json.RawMessage(`{"command":` + quoteJSON(command) + `}`),
	})
	if code != 0 {
		t.Errorf("%q does not write a protected file and must not exit %d, out = %q", command, code, out)
	}
	if strings.Contains(out, `"decision":"block"`) {
		t.Errorf("%q does not write a protected file and must stay silent, got: %q", command, out)
	}
}

// TestLos31fClobberRedirectReachesTheGuard covers finding 1 of YM-375: ">|" is the
// clobber redirect and truncates the target like ">" does, but bashSegments split
// the command at the "|", so the operator arrived without its target and both write
// locks stayed silent. This is the path that can drop the hook registration itself.
func TestLos31fClobberRedirectReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := []struct {
		name, command, want string
	}{
		{"settings file", `echo '{}' >| $HOME/.claude/settings.json`, settings},
		{"settings file with an attached target", `echo '{}' >|$HOME/.claude/settings.json`, settings},
		{"settings file behind an fd prefix", `echo '{}' 1>| $HOME/.claude/settings.json`, settings},
		{"generated ops file", `echo x >| $HOME/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"generated ops file with an attached target", `echo x >|$HOME/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"relative target after cd", `cd $HOME/.claude && echo '{}' >| settings.json`, settings},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			los31fBlocked(t, home, tt.command, tt.want)
		})
	}

	allowed := []struct {
		name, command string
	}{
		{"a pipe to a reader", `echo '{}' | cat`},
		{"a pipe that only mentions the file", `cat $HOME/.claude/settings.json | grep hook`},
		{"a clobber redirect into an unrelated file", `echo '{}' >| ` + filepath.Join(home, "other.json")},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			los31fAllowed(t, home, tt.command)
		})
	}
}

// TestLos31fQuotedHomeReachesTheGuard covers finding 2 of YM-375: the shell joins
// adjacent quoted and unquoted parts of one word, so "$HOME"/.claude/settings.json
// names the settings file. The guard only trimmed the outer quote characters, which
// left an inner quote in the path and lost the match.
func TestLos31fQuotedHomeReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := []struct {
		name, command, want string
	}{
		{"quoted home", `echo '{}' > "$HOME"/.claude/settings.json`, settings},
		{"quoted brace home", `echo '{}' > "${HOME}"/.claude/settings.json`, settings},
		{"quoted ops file", `echo x > "$HOME"/.claude/yesmem-ops.md`, "yesmem-ops.md"},
		{"quoted directory part", `echo '{}' > "$HOME/.claude"/settings.json`, settings},
		{"quote opened after the prefix", `echo '{}' > $HOME"/.claude/settings.json"`, settings},
		{"cd into a quoted home", `cd "$HOME"/.claude && echo '{}' > settings.json`, settings},
		{"copy into a quoted home", `cp /x/yesmem-ops.md "$HOME"/.claude/`, "yesmem-ops.md"},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			los31fBlocked(t, home, tt.command, tt.want)
		})
	}

	allowed := []struct {
		name, command string
	}{
		{"another file below the quoted home", `echo '{}' > "$HOME"/notes/settings.json`},
		{"a quoted path that is only read", `cat "$HOME"/.claude/settings.json`},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			los31fAllowed(t, home, tt.command)
		})
	}
}

// TestLos31fLineContinuationReachesTheGuard covers finding 3 of YM-375: a backslash
// at the end of a line joins the next line before the shell splits words, so
// settings.j\<newline>son names the settings file (measured on bash 5.2). The guard
// split at the newline and compared the two halves as separate words.
func TestLos31fLineContinuationReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := []struct {
		name, command, want string
	}{
		{"continuation inside the file name", "echo '{}' > $HOME/.claude/settings.j\\\nson", settings},
		{"continuation inside the directory name", "echo '{}' > $HOME/\\\n.claude/settings.json", settings},
		{"continuation behind the operator", "echo '{}' >\\\n$HOME/.claude/settings.json", settings},
		{"continuation inside the ops file name", "echo x > $HOME/.claude/yesmem-op\\\ns.md", "yesmem-ops.md"},
		{"continuation after a clobber redirect", "echo '{}' >|\\\n$HOME/.claude/settings.json", settings},
		{"continuation after an apostrophe in double quotes", "echo \"it's\" > $HOME/.claude/settings.j\\\nson", settings},
		{"continuation inside a quoted path", "echo '{}' > \"$HOME/.claude/settings.j\\\nson\"", settings},
		{"continuation inside a nested shell body", "bash -c 'echo x > $HOME/.claude/settings.j\\\nson'", settings},
		{"continuation behind a comment after a semicolon", "echo x;# c \\\ncp y $HOME/.claude/settings.json", settings},
		{"continuation behind a comment after &&", "echo x&&# c \\\ncp y $HOME/.claude/settings.json", settings},
		{"continuation behind a comment after a parenthesis", "(echo x)# c \\\ncp y $HOME/.claude/settings.json", settings},
		{"continuation behind a comment after a semicolon into the ops file", "echo x;# c \\\ntee $HOME/.claude/yesmem-ops.md", "yesmem-ops.md"},
		{"continuation inside a brace expansion", "echo ${zz:- #} > $HOME/.claude/settings.j\\\nson", settings},
		{"continuation between the dollar and its brace", "echo $\\\n{zz:- #} > $HOME/.claude/settings.j\\\nson", settings},
		// A heredoc body is not executed: its unbalanced "${" must not swallow the
		// comment detection for the rest of the command.
		{"continuation behind a heredoc body with an unbalanced expansion", "cat > n.md <<'EOD'\n${a\nEOD\n# c \\\ncp y $HOME/.claude/settings.json", settings},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			los31fBlocked(t, home, tt.command, tt.want)
		})
	}

	t.Run("continuation inside a REPL template literal", func(t *testing.T) {
		los31fBlockedRepl(t, home, "o.r = await sh(`echo x > $HOME/.claude/settings.j\\\nson`)", settings)
	})

	t.Run("continuation with a line separator in a REPL template literal", func(t *testing.T) {
		los31fBlockedRepl(t, home, "o.r = await sh(`echo x > $HOME/.claude/settings.j\\\u2028son`)", settings)
	})

	allowed := []struct {
		name, command string
	}{
		{"continuation into an unrelated file", "echo x > " + filepath.Join(home, ".claude", "other.j") + "\\\nson"},
		{"continuation inside a single quoted body", "printf 'a\\\nb' > " + filepath.Join(home, "out.txt")},
		// The shell names a file "yesmem-ops.m\" here; a join that swallowed the escaped
		// backslash would invent the protected name and block.
		{"an escaped backslash does not continue the line", "echo A > " + filepath.Join(home, ".claude", "yesmem-ops.m") + "\\\\\nd"},
		{"a continued comment is not an operand", "cp a b # \\\n$HOME/.claude/yesmem-ops.md"},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			los31fAllowed(t, home, tt.command)
		})
	}
}
