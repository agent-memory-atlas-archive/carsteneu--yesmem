package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestLos31eProtectedFileNeedsTheGeneratedDirectory covers finding 2 of YM-342:
// isProtectedFile matched the basename alone, so a file of that name was treated
// as the generated one wherever it lay. The generator writes
// <project>/.claude/<output_file> (claudemd.outputPathFor), so the directory is
// part of the file's identity.
func TestLos31eProtectedFileNeedsTheGeneratedDirectory(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"the generated file", "/home/x/project/.claude/yesmem-ops.md", true},
		{"the generated file with a trailing separator", "/home/x/project/.claude/yesmem-ops.md/", true},
		{"the same name in the project root", "/home/x/project/yesmem-ops.md", false},
		{"the same name in a temp dir", "/tmp/yesmem-ops.md", false},
		{"the same name in another hidden dir", "/home/x/project/.config/yesmem-ops.md", false},
		{"a case variant of the generated file", "/home/x/project/.CLAUDE/YESMEM-OPS.MD", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isProtectedFile(tt.path); got != tt.want {
				t.Errorf("isProtectedFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestLos31eOpsFileAliasesStayProtected covers the spellings the earlier basename
// rule caught and a lexical directory comparison would lose: an escaped separator,
// a directory symlink, and a copy into the generated directory.
func TestLos31eOpsFileAliasesStayProtected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("create generated dir: %v", err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(filepath.Join(home, ".claude"), link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	blocked := []struct{ name, command string }{
		{"escaped separator", `echo '{}' > ` + home + `/.claude\/yesmem-ops.md`},
		{"through a directory symlink", `echo '{}' > ` + link + `/yesmem-ops.md`},
		{"copied into the generated directory", `cp /x/yesmem-ops.md ` + filepath.Join(home, ".claude") + `/`},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
				ToolName:  "Bash",
				CWD:       home,
				ToolInput: json.RawMessage(`{"command":` + quoteJSON(tt.command) + `}`),
			})
			if code != 2 {
				t.Errorf("%q must be blocked, exit = %d, out = %q", tt.command, code, out)
			}
			if !strings.Contains(out, `"decision":"block"`) {
				t.Errorf("expected a block decision for %q, got: %q", tt.command, out)
			}
		})
	}
	if !isProtectedFile(filepath.Join(link, "yesmem-ops.md")) {
		t.Error("a symlinked directory that resolves to .claude must protect the ops file")
	}
}

// TestLos31eWriteOfAStrayOpsFileIsNotBlocked is the behavioural half of finding
// 2: a Write to a file that merely shares the name must reach the normal path,
// while the generated one keeps blocking.
func TestLos31eWriteOfAStrayOpsFileIsNotBlocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stray := filepath.Join(home, "notes", "yesmem-ops.md")
	out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "Write",
		CWD:       home,
		ToolInput: json.RawMessage(`{"file_path":` + quoteJSON(stray) + `}`),
	})
	if code != 0 {
		t.Errorf("a stray file named yesmem-ops.md must not block, exit = %d, out = %q", code, out)
	}
	if strings.Contains(out, `"decision"`) {
		t.Errorf("a stray file named yesmem-ops.md must not produce a block decision, got: %q", out)
	}

	generated := filepath.Join(home, ".claude", "yesmem-ops.md")
	out, code = runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
		ToolName:  "Write",
		CWD:       home,
		ToolInput: json.RawMessage(`{"file_path":` + quoteJSON(generated) + `}`),
	})
	if code != 2 {
		t.Errorf("the generated ops file must stay blocked, exit = %d, out = %q", code, out)
	}
}

// TestLos31eReplShellWriteReachesTheGuard covers finding 3: a REPL call runs
// JavaScript whose sh() reaches the shell, so a shell write to one of the
// installation's own files is the same write a Bash call performs. Before the
// fix the REPL branch only extracted keywords and never asked the write
// detector.
func TestLos31eReplShellWriteReachesTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")

	blocked := []struct{ name, code string }{
		{"redirect", fmt.Sprintf(`o.r = await sh("echo '{}' > %s")`, settings)},
		{"append with a tilde path", `o.r = await sh("echo '{}' >> ~/.claude/settings.json")`},
		{"inside a nested shell", fmt.Sprintf(`o.r = await sh("bash -c 'echo x > %s'")`, settings)},
		{"single quoted body", fmt.Sprintf(`o.r = await sh('echo x > %s')`, settings)},
		{"backtick body", fmt.Sprintf("o.r = await sh(`echo x > %s`)", settings)},
		{"escaped quotes around the path", fmt.Sprintf(`o.r = await sh("echo x > \"%s\"")`, settings)},
		{"an apostrophe in a comment does not switch the scan off", "// don't clobber the settings\n" +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"an optional call", fmt.Sprintf(`o.r = await sh?.("echo x > %s")`, settings)},
		{"a non-breaking space before the parenthesis", "o.r = await sh\u00a0(\"echo x > " + settings + "\")"},
		{"a regex carrying quotes", "const re = /['\"]/;\n" + fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"an apostrophe in a regex", "const re = /don't/;\n" +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings) + "\nconst s = 'end'"},
		{"a comment marker in a regex class", "const re = /[/*]/;\n" +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"an escaped slash in a regex", "const q = s.replace(/\\//g, \"x\"); " +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a block comment inside the call", fmt.Sprintf(`o.r = await sh(/* note */ "echo x > %s")`, settings)},
		{"a line comment inside the call", "o.r = await sh(\n// note\n" +
			fmt.Sprintf(`"echo x > %s")`, settings)},
		{"a regex after a keyword", "function f(s) { return /['\"]/.test(s) }\n" +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a property named do divides", "const t = obj.do / 2; " +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a postfix increment divides", "let t = 0; const u = t++ / 2; " +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a redundant parenthesis around the body", fmt.Sprintf(`o.r = await sh(("echo x > %s"))`, settings)},
		{"a property name behind a dot and a space", "const t = []. return / 2; " +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a property name behind a dot and a comment", "const t = x./*c*/return / 2; " +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a comment between keyword and pattern", "function f(s) { return /*c*/ /[']/.test(s) }\n" +
			fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"a pattern on the line after a comment", "function f(s) { return // note\n" +
			"/[']/.test(s) }\n" + fmt.Sprintf(`o.r = await sh("echo x > %s")`, settings)},
		{"an unterminated quote in a pattern does not swallow the call", "if (1) /[']/.test('a')\n" +
			fmt.Sprintf(`o.r = await sh('echo x > %s')`, settings)},
		{"the shell reached through the global object", fmt.Sprintf(`o.r = await globalThis.sh("echo x > %s")`, settings)},
		{"the shell reached through the global object optionally", fmt.Sprintf(`o.r = await globalThis?.sh("echo x > %s")`, settings)},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
				ToolName:  "REPL",
				CWD:       home,
				ToolInput: json.RawMessage(`{"code":` + quoteJSON(tt.code) + `}`),
			})
			if code != 2 {
				t.Errorf("%q must be blocked, exit = %d, out = %q", tt.code, code, out)
			}
			if !strings.Contains(out, `"decision":"block"`) {
				t.Errorf("expected a block decision for %q, got: %q", tt.code, out)
			}
		})
	}

	t.Run("in-place editor warns", func(t *testing.T) {
		code := fmt.Sprintf(`o.r = await sh("sed -i 's/a/b/' %s")`, settings)
		out := runCheckWithHook(t, openTempStore(t, func(*storage.Store) {}), HookInput{
			ToolName:  "REPL",
			CWD:       home,
			ToolInput: json.RawMessage(`{"code":` + quoteJSON(code) + `}`),
		})
		if !strings.Contains(out, "yesmem setup") {
			t.Errorf("an in-place REPL write of the settings file must warn, got: %q", out)
		}
	})

	notAWrite := []struct{ name, code string }{
		{"reading the file", fmt.Sprintf(`o.r = await sh("cat %s")`, settings)},
		{"copying it away", fmt.Sprintf(`o.r = await sh("cp %s /x/backup.json")`, settings)},
		{"grepping it", fmt.Sprintf(`o.r = await sh("grep -n hook %s")`, settings)},
		{"documenting the command", fmt.Sprintf(`await Write({file_path:"/tmp/doc.md", content:'never run: sh("echo x > %s")'})`, settings)},
		{"a name that only ends in sh", fmt.Sprintf(`o.r = await push("echo x > %s")`, settings)},
		{"a method named sh", fmt.Sprintf(`o.r = await obj.sh("echo x > %s")`, settings)},
		{"a non-ASCII prefix is another name", fmt.Sprintf(`o.r = await ésh("echo x > %s")`, settings)},
		{"a commented example", "// the old fix was sh(\"echo x > " + settings + "\")\n" +
			fmt.Sprintf(`o.r = await sh("cat %s")`, settings)},
		{"a block comment", "/* sh(\"echo x > " + settings + "\") */\n" +
			fmt.Sprintf(`o.r = await sh("cat %s")`, settings)},
	}
	for _, tt := range notAWrite {
		t.Run(tt.name, func(t *testing.T) {
			out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
				ToolName:  "REPL",
				CWD:       home,
				ToolInput: json.RawMessage(`{"code":` + quoteJSON(tt.code) + `}`),
			})
			if code != 0 {
				t.Errorf("%q does not write the file and must not exit %d, out = %q", tt.code, code, out)
			}
			if strings.Contains(out, "yesmem setup") || strings.Contains(out, `"decision"`) {
				t.Errorf("%q does not write the file and must stay silent, got: %q", tt.code, out)
			}
		})
	}
}

// TestLos31eEscapedSpellingsReachTheGuard pins the shell escape rule: unquoted, a
// backslash is removed before any character, so "\." and "\/" name the same file as
// "." and "/". The guard compares paths and has to see through both spellings
// (measured on bash 5.2: $HOME/\.claude/settings.json writes the settings file).
func TestLos31eEscapedSpellingsReachTheGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	blocked := []string{
		`echo '{}' > $HOME\/.claude\/settings.json`,
		`echo '{}' > ${HOME}\/.claude\/settings.json`,
		`echo '{}' > $HOME/\.claude/settings.json`,
		`echo '{}' > $HOME/.claude/settings\.json`,
		`echo '{}' > $HOME\/\.claude\/settings\.json`,
		`echo x > $HOME/.claude/yesmem-ops\.md`,
		`echo x > $HOME/\.claude/yesmem-ops.md`,
		`cd $HOME/\.claude && echo '{}' > settings\.json`,
		`cp /x/yesmem-ops\.md $HOME/.claude/`,
	}
	for _, command := range blocked {
		t.Run(command, func(t *testing.T) {
			out, code := runCheckBlocking(t, openTempStore(t, func(*storage.Store) {}), HookInput{
				ToolName:  "Bash",
				CWD:       home,
				ToolInput: json.RawMessage(`{"command":` + quoteJSON(command) + `}`),
			})
			if code != 2 {
				t.Errorf("%q must be blocked, exit = %d, out = %q", command, code, out)
			}
		})
	}
}

// TestLos31eSettingsEditWarningDoesNotSkipTheGotcha covers finding 4: the Edit
// and Write warn branch emitted the settings hint and returned, so the gotcha
// lookup never ran for a surgical edit of the settings file. The Bash branch
// already carried its hint into the same call and kept going.
func TestLos31eSettingsEditWarningDoesNotSkipTheGotcha(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := openTempStore(t, func(s *storage.Store) {
		insertGotcha(t, s,
			"Never rewrite .claude/settings.json wholesale (LOS31E-GOTCHA-MARKER)",
			"claude_suggested", "")
	})

	out := runCheckWithHook(t, dir, HookInput{
		ToolName: "Edit",
		CWD:      home,
		ToolInput: json.RawMessage(`{"file_path":` +
			quoteJSON(filepath.Join(home, ".claude", "settings.json")) +
			`,"old_string":"a","new_string":"b"}`),
	})
	if !strings.Contains(out, "yesmem setup") {
		t.Errorf("the settings warning must still be emitted, got: %q", out)
	}
	if !strings.Contains(out, "LOS31E-GOTCHA-MARKER") {
		t.Errorf("a surgical settings edit must still reach the gotcha lookup, got: %q", out)
	}
}

// TestLos31eInjectionCapDoesNotHideTheBlock covers finding 5: the per-session
// injection cap ran before findBlockableGotcha, so a gotcha that had been
// injected three times and then escalated stopped blocking. The cap limits
// repetition, not a hard block.
func TestLos31eInjectionCapDoesNotHideTheBlock(t *testing.T) {
	t.Setenv("HOME", "/home/los31e")
	home := "/home/los31e"
	const session = "los31e-cap"

	var id int64
	dir := openTempStore(t, func(s *storage.Store) {
		id = insertGotcha(t, s, "rm -rf "+home+"/.claude/yesmem loescht alle Learnings", "hook_auto_learned", "bash")
		for i := 0; i < blockThreshold; i++ {
			if err := s.IncrementFailCounts([]int64{id}); err != nil {
				t.Fatalf("escalate fail count: %v", err)
			}
		}
		// The gotcha was injected three times before it escalated, so the cap
		// counter already sits at the limit.
		if err := s.SetProxyState(fmt.Sprintf("gotcha_count:%s:%d", session, id), "3"); err != nil {
			t.Fatalf("seed injection counter: %v", err)
		}
	})

	out, code := runCheckBlocking(t, dir, HookInput{
		SessionID: session,
		CWD:       home,
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":` + quoteJSON("rm -rf "+home+"/.claude/yesmem") + `}`),
	})
	if code != 2 {
		t.Fatalf("an escalated gotcha must block even when its injection cap is reached, exit = %d, out = %q", code, out)
	}
	if !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("expected a block decision, got: %q", out)
	}
	if !strings.Contains(out, "loescht alle Learnings") {
		t.Errorf("the block must name the escalated gotcha, got: %q", out)
	}
}
