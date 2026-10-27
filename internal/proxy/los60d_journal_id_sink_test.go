package proxy

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// YM-438: journalToken() has to cover the whole journal, not one line at a time.
// YM-400 fixed the thread tokens, YM-432 fixed two tool_use ids in
// validate_pairs.go, and both times the next line of the same file still wrote a
// client-supplied id raw. A per-line test cannot see that, so this one sweeps
// the package: every Printf-family call whose format prints an id-keyed value
// ("id=%s", "tid=%s", "tool_call_id=%s", ...) must route that value through
// journalToken. Anything else lets the payload end the "[req N tid=...]" prefix
// early and plant a journal entry of its own.
//
// What the sweep cannot see, by construction: the provenance of the wrapped
// argument (journalToken(otherValue) passes), writers other than the Printf
// family (fmt.Fprintf to an arbitrary writer is not the journal), and payload
// values printed in an unkeyed slot or under a key outside the vocabulary below.
// YM-440 closed the second kind for the proxy handlers: "model" and "session" are
// on the list now, so openai_handler.go:47/61, responses_handler.go:50/61 and
// proxy.go:1275 are covered here as well. YM-447 added the two shapes the project
// name is printed in: the "[req N <project> tid=...]" prefix slot (found by
// position, los62aPrefixProject) and "project=%s". The unkeyed slots that remain
// are found by the behavior tests, not by this scan (the thinking line in proxy.go
// and the "oc:%s" slot in openai_parity.go came from there); so does the commit
// subject, which cannot be a token because a subject has legitimate spaces.
func TestJournalIDSinksAreSanitized(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sinks := 0
	var findings []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				body = fn.Body
			case *ast.FuncLit:
				body = fn.Body
			default:
				return true
			}
			if body == nil {
				return true
			}
			// Do not descend: nested literals are reached through this body, and
			// descending would report their calls twice.
			bindings := los60dBindings(body)
			ast.Inspect(body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok || !los60dIsPrintf(call) || len(call.Args) < 2 {
					return true
				}
				args, keys, err := los60dSinkArgs(call)
				if err != nil {
					// Not skipped silently: an unreadable journal line is the
					// hole this test exists to close.
					findings = append(findings, fset.Position(call.Pos()).String()+": "+err.Error())
					return true
				}
				for i, key := range keys {
					if key == "" || i >= len(args) {
						continue
					}
					sinks++
					if los60dSanitizedAt(args[i], bindings) {
						continue
					}
					findings = append(findings, fset.Position(args[i].Pos()).String()+
						": "+key+" prints a value without journalToken")
				}
				return true
			})
			return false
		})
	}
	if sinks == 0 {
		t.Fatal("no id-keyed journal sink found; the scan no longer reaches the lines")
	}
	if len(findings) > 0 {
		sort.Strings(findings)
		t.Fatalf("client-supplied id written to the journal unsanitized:\n%s",
			strings.Join(findings, "\n"))
	}
}

// los60dIsPrintf matches the Printf family across the shapes the package uses:
// log.Printf, logger.Printf, s.logger.Printf.
func los60dIsPrintf(call *ast.CallExpr) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name == "Printf"
	case *ast.SelectorExpr:
		return f.Sel.Name == "Printf"
	}
	return false
}

// los60dSinkArgs maps the format string of call onto its arguments and returns
// the journal key each argument is printed under ("" for unkeyed slots).
func los60dSinkArgs(call *ast.CallExpr) (args []ast.Expr, keys []string, err error) {
	format, ok := los60dFormatLiteral(call.Args[0])
	if !ok {
		return nil, nil, los60dScanErr("format is not a string literal")
	}
	if strings.Contains(format, "%[") {
		return nil, nil, los60dScanErr("format uses an indexed verb")
	}
	args = call.Args[1:]
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			i++
			continue
		}
		key := los60dKeyBefore(format, i)
		if key == "" && los62aPrefixProject(format, i) {
			key = "project"
		}
		j := i + 1
		stars := 0
		for j < len(format) && strings.ContainsRune("+-# 0123456789.*", rune(format[j])) {
			if format[j] == '*' {
				stars++
			}
			j++
		}
		if j >= len(format) {
			return nil, nil, los60dScanErr("format ends inside a verb")
		}
		verb := format[j]
		i = j
		// A "*" width is an argument of its own and comes before the value.
		for ; stars > 0; stars-- {
			keys = append(keys, "")
		}
		if verb != 's' && verb != 'q' && verb != 'v' {
			keys = append(keys, "")
			continue
		}
		keys = append(keys, key)
	}
	return args, keys, nil
}

type los60dScanErr string

func (e los60dScanErr) Error() string { return "journal scan cannot read the line: " + string(e) }

// los60dFormatLiteral returns the format string, folding the literal
// concatenation ("id=" + "%s") that would otherwise hide the line.
func los60dFormatLiteral(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := los60dFormatLiteral(e.X)
		if !ok {
			return "", false
		}
		right, ok := los60dFormatLiteral(e.Y)
		return left + right, ok
	}
	return "", false
}

// los60dKeyBefore returns the identifier that stands in front of the verb as
// "<name>=", "<name>:" or "<name> ", or "" when the slot is not keyed. Only the
// keys the journal uses for payload identifiers count. "model" and "session" are
// on the list since YM-440: both carry client input (a JSON body field and a
// request header), and net/http guarding header values against CR, LF and ESC
// only rules out a line of its own. A value may still contain spaces and ']',
// so it can close the "[req N tid=...]" prefix early and pass the rest off as
// journal decoration.
func los60dKeyBefore(format string, verbStart int) string {
	end := verbStart
	if end > 0 && format[end-1] == ' ' {
		// "id %s" and "id = %s" keep the scan below working.
		end--
	}
	if end > 0 && (format[end-1] == '=' || format[end-1] == ':') {
		end--
	}
	start := end
	for start > 0 && los60dIsIdentChar(format[start-1]) {
		start--
	}
	key := format[start:end]
	switch key {
	case "id", "tid", "name", "tool_use_id", "tool_call_id", "session_id", "sid", "model", "session",
		// YM-447: the project name is client text as well. It is printed as
		// "project=%s" in the briefing and associative lines, and the prefix
		// slot has no key at all (los62aPrefixProject).
		"project", "proj":
		return key
	}
	return ""
}

// los62aPrefixProject reports whether the verb at verbStart prints the project
// name of the "[req N <project> tid=...]" journal prefix. The slot has no key of
// its own: it stands between the request number and the tid, which is exactly
// where a bracket or a space ends the prefix early (YM-447). The project name is
// client text (the working directory the request claims), so it follows the same
// token rule as an id even though the format gives it no "name=" marker.
func los62aPrefixProject(format string, verbStart int) bool {
	return strings.HasSuffix(format[:verbStart], "[req %d ") ||
		strings.HasSuffix(format[:verbStart], "[req %d  ")
}

func los60dIsIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

type los60dBinding struct {
	pos       token.Pos
	block     *ast.BlockStmt
	sanitized bool
}

// los60dBindings records every binding of a name in body together with the block
// that holds it. A use counts as sanitized only when the nearest binding visible
// at that position is journalToken: a shadowing "tid := journalToken(x)" inside an
// if-block does not cover a later raw use outside it, and a plain "tid = clientID"
// revokes an earlier sanitized binding instead of inheriting it.
func los60dBindings(body *ast.BlockStmt) map[string][]los60dBinding {
	out := map[string][]los60dBinding{}
	var blocks []*ast.BlockStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BlockStmt); ok {
			blocks = append(blocks, b)
		}
		return true
	})
	record := func(lhs ast.Expr, sanitized bool) {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" {
			return
		}
		out[id.Name] = append(out[id.Name], los60dBinding{
			pos:       id.Pos(),
			block:     los60dInnermostBlock(blocks, id.Pos()),
			sanitized: sanitized,
		})
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) != len(s.Rhs) {
				return true
			}
			for i := range s.Lhs {
				call, ok := s.Rhs[i].(*ast.CallExpr)
				record(s.Lhs[i], ok && los60dIsJournalToken(call))
			}
		case *ast.ValueSpec:
			if len(s.Names) != len(s.Values) {
				return true
			}
			for i := range s.Names {
				call, ok := s.Values[i].(*ast.CallExpr)
				record(s.Names[i], ok && los60dIsJournalToken(call))
			}
		case *ast.RangeStmt:
			// "for _, tid = range xs" replaces a sanitized binding with a raw value.
			if s.Key != nil {
				record(s.Key, false)
			}
			if s.Value != nil {
				record(s.Value, false)
			}
		}
		return true
	})
	return out
}

func los60dInnermostBlock(blocks []*ast.BlockStmt, pos token.Pos) *ast.BlockStmt {
	var best *ast.BlockStmt
	for _, b := range blocks {
		if pos < b.Pos() || pos >= b.End() {
			continue
		}
		if best == nil || b.End()-b.Pos() < best.End()-best.Pos() {
			best = b
		}
	}
	return best
}

func los60dIsJournalToken(call *ast.CallExpr) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "journalToken"
}

func los60dSanitizedAt(arg ast.Expr, bindings map[string][]los60dBinding) bool {
	if call, ok := arg.(*ast.CallExpr); ok {
		return los60dIsJournalToken(call)
	}
	id, ok := arg.(*ast.Ident)
	if !ok {
		return false
	}
	var best *los60dBinding
	for i := range bindings[id.Name] {
		b := &bindings[id.Name][i]
		if b.pos >= arg.Pos() || b.block == nil {
			continue
		}
		if arg.Pos() < b.block.Pos() || arg.Pos() >= b.block.End() {
			continue // binding sits in a block that does not enclose this use
		}
		switch {
		case best == nil,
			b.block.Pos()-b.block.End() > best.block.Pos()-best.block.End(),
			b.block == best.block && b.pos > best.pos:
			best = b
		}
	}
	return best != nil && best.sanitized
}

// The structure scan proves journalToken is called; these two prove it is called
// on the id that reaches the line. A payload with a line break and an escape
// sequence must stay inside its own journal line on both rewritten paths.
func TestOpenAITranslateLogsSanitizedIDs(t *testing.T) {
	hostileID := "toolu_1\n2026/10/23 12:00:00 [req 9 tid=x] upstream 200: forged\nOPENAI-FWD: forged line\x1b[31m"
	hostileName := "bash\x1b[0m\n[req 9 tid=y] forged"
	msgs := []OpenAIMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": hostileID, "name": hostileName, "input": map[string]any{}},
		}},
		{Role: "tool", ToolCallID: hostileID, Content: "out"},
	}

	out := los60dCaptureLog(t, func() {
		if _, err := translateMessages(msgs); err != nil {
			t.Fatalf("translateMessages: %v", err)
		}
	})
	los60dAssertJournalLines(t, out, 2, "toolu_1")
}

func TestOpenAIReverseLogsSanitizedID(t *testing.T) {
	hostileID := "toolu_2\n2026/10/23 12:00:00 [req 9 tid=x] upstream 200: forged\nOPENAI-REV: forged line\x1b[31m"
	out := los60dCaptureLog(t, func() {
		translateAnthropicUserMsg(map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": hostileID, "content": "orphan"},
			},
		})
	})
	los60dAssertJournalLines(t, out, 1, "toolu_2")
}

// los60dCaptureLog runs fn with the package journal redirected into a buffer.
// Sequential on purpose: the rewritten lines log through the global logger, so a
// parallel test in the same package would race on the writer.
func los60dCaptureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

var los60dLogLinePrefix = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `)

// los60dAssertJournalLines pins the shape of the whole capture: one dated line
// per log call and nothing else. Counting lines catches a forged entry even when
// the payload does not imitate the timestamp decoration.
func los60dAssertJournalLines(t *testing.T, out string, wantLines int, sanitizedID string) {
	t.Helper()
	if out == "" {
		t.Fatal("expected a journal line")
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != wantLines {
		t.Fatalf("payload opened %d journal line(s), want %d: %q", len(lines), wantLines, out)
	}
	for _, line := range lines {
		if !los60dLogLinePrefix.MatchString(line) {
			t.Fatalf("line is not a single dated journal entry: %q", line)
		}
		if strings.Contains(line, "\x1b") {
			t.Fatalf("payload kept its escape sequence: %q", line)
		}
	}
	if !strings.Contains(out, sanitizedID) {
		t.Fatalf("journal line lost the id entirely: %q", out)
	}
}
