package capfile

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// plainFlowScalar is the set of characters that need no quoting in a YAML flow
// sequence: every adapter name and every tag that exists today is in it.
var plainFlowScalar = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// quoteFlowList renders a YAML flow sequence. Tags and adapter names reach Render
// from save_cap, i.e. from caller text: an element carrying a newline or a "---"
// ended the frontmatter early unquoted, and the next read imported whatever block
// the element had smuggled in. Only elements outside the plain set are quoted
// (strconv.Quote emits exactly the escapes YAML's double-quoted style accepts),
// so a normal cap keeps its readable `requires: [store, web]`.
func quoteFlowList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		if plainFlowScalar.MatchString(item) {
			quoted[i] = item
			continue
		}
		quoted[i] = strconv.Quote(item)
	}
	return strings.Join(quoted, ", ")
}

func Render(cf *CapFile) []byte {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("name: %s\n", cf.Name))
	b.WriteString(fmt.Sprintf("description: %q\n", cf.Description))
	if cf.Version > 0 {
		b.WriteString(fmt.Sprintf("version: %d\n", cf.Version))
	}
	if len(cf.Tags) > 0 {
		b.WriteString(fmt.Sprintf("tags: [%s]\n", quoteFlowList(cf.Tags)))
	}
	if len(cf.Requires) > 0 {
		b.WriteString(fmt.Sprintf("requires: [%s]\n", quoteFlowList(cf.Requires)))
	}
	if cf.Scope != "" {
		b.WriteString(fmt.Sprintf("scope: %s\n", cf.Scope))
	}
	if cf.Tested {
		b.WriteString("tested: true\n")
	}
	if cf.AutoActive {
		b.WriteString("auto_active: true\n")
	}
	b.WriteString("---\n")

	b.WriteString("\n## Purpose\n\n")
	b.WriteString(cf.Purpose)
	b.WriteString("\n")

	b.WriteString("\n## Scripts\n")
	for _, sc := range cf.Scripts {
		renderScript(&b, sc)
	}

	if cf.DatabaseSQL != "" {
		b.WriteString("\n## Database\n\n")
		b.WriteString("```sql\n")
		b.WriteString(formatSQL(cf.DatabaseSQL))
		b.WriteString("\n```\n")
	}

	if len(cf.Actions) > 0 {
		b.WriteString("\n## Actions\n\n")
		names := make([]string, 0, len(cf.Actions))
		for k := range cf.Actions {
			names = append(names, k)
		}
		sort.Strings(names)
		for i, name := range names {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(fmt.Sprintf("### %s\n\n", name))
			body := strings.TrimRight(cf.Actions[name], "\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}

	return []byte(b.String())
}

func renderScript(b *strings.Builder, sc Script) {
	b.WriteString(fmt.Sprintf("\n### %s\n", sc.Name))

	// Inline metadata: only emit non-default values.
	if sc.Kind != "" && sc.Kind != "tool" {
		b.WriteString(fmt.Sprintf("kind: %s\n", sc.Kind))
	} else if sc.Kind == "tool" {
		// For clarity, always emit kind: tool when explicitly tool — readers know default but readability wins.
		b.WriteString("kind: tool\n")
	}

	// Only emit explicit runtime if it differs from what the code fence implies.
	derived := deriveRuntime(sc.Lang)
	if sc.Runtime != "" && sc.Runtime != derived {
		b.WriteString(fmt.Sprintf("runtime: %s\n", sc.Runtime))
	}

	// Schema: emit when present and non-default. For repl-tool scripts, the schema
	// is derived from the JS signature on parse — only emit if it's non-derivable.
	if sc.Kind == "tool" && sc.Schema != "" && sc.Schema != "{}" {
		derivedSchema := DeriveSchema(sc.Body)
		if sc.Schema != derivedSchema {
			b.WriteString(fmt.Sprintf("schema: %s\n", sc.Schema))
		}
	}

	if sc.Sandbox != "" {
		b.WriteString(fmt.Sprintf("sandbox: %s\n", sc.Sandbox))
	}
	if len(sc.AllowedHosts) > 0 {
		b.WriteString(fmt.Sprintf("allowed_hosts: %s\n", strings.Join(sc.AllowedHosts, ", ")))
	}
	if sc.Timeout > 0 {
		b.WriteString(fmt.Sprintf("timeout: %d\n", sc.Timeout))
	}

	b.WriteString("\n")

	lang := sc.Lang
	if lang == "" {
		switch sc.Runtime {
		case "repl":
			lang = "javascript"
		case "bash":
			lang = "bash"
		}
	}

	b.WriteString("```" + lang + "\n")
	body := sc.Body
	if lang == "javascript" || lang == "js" {
		body = formatJS(body)
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
}

// EnsureDirOwnerOnly creates dir with mode 0700 and repairs an existing
// directory. MkdirAll applies its mode only on creation, so the repair is
// explicit. The final component is Lstat'ed first: os.Chmod follows a symlink,
// so a symlink planted at a cap path would hand the mode change to its target
// (the same check runtime_dir.go makes). A refused symlink is an error, a plain
// chmod failure only a log line — the mode is hardening and must not cost the
// write (YM-353).
func EnsureDirOwnerOnly(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, refusing to change the mode of its target", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
			log.Printf("capfile: chmod %s: %v", dir, chmodErr)
		}
	}
	return nil
}

// WriteFile renders the cap and replaces <dir>/CAP.md atomically. The file is
// written owner-only: the daemon imports scripts from this directory, and a body
// names endpoints, paths and command lines. The directory gets the same mode: its
// listing names every installed cap and its mtime (YM-353).
func WriteFile(cf *CapFile, dir string) error {
	if err := EnsureDirOwnerOnly(dir); err != nil {
		return fmt.Errorf("create cap directory: %w", err)
	}
	return WriteOwnerOnly(filepath.Join(dir, "CAP.md"), Render(cf))
}

// WriteOwnerOnly replaces path with data atomically and refuses to follow a
// symlink on the way. os.WriteFile follows one, and a cap directory is reachable
// from a run: a link planted at the temp path received the rendered cap body, and
// the rename that followed moved the link into place as CAP.md (YM-359). The temp
// path is created with O_EXCL|O_NOFOLLOW, so it cannot be an existing link either.
// A hard link needs no check here, unlike in the read and repair paths: the rename
// replaces the directory entry, so it separates a shared inode instead of reaching
// it.
func WriteOwnerOnly(path string, data []byte) error {
	if err := refuseSymlinkTarget(path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// A leftover from a crashed write may sit here, and it is removed. A link at
	// this path is not a leftover: it is exactly the write primitive this function
	// closes, so it is refused rather than silently deleted.
	if info, err := os.Lstat(tmp); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, refusing to write through it", tmp)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", tmp)
		}
		if err := os.Remove(tmp); err != nil {
			return fmt.Errorf("clear temp file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat temp file: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

// refuseSymlinkTarget rejects a path that exists as anything but a regular file.
// The check runs before the write, because the write itself would follow the link.
func refuseSymlinkTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, refusing to write through it", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

// RepairFileOwnerOnly sets mode 0600 on an existing cap file without following a
// symlink. os.Chmod follows one, so the repair of a CAP.md exported before YM-346
// handed the mode change to whatever file the link named (YM-359). The mode is set
// on the file descriptor, so a swap between check and chmod cannot retarget it.
//
// O_WRONLY is enough for fchmod, so a write-only file is still repairable; a file
// that denies writing cannot be opened and therefore not repaired through a
// descriptor at all.
func RepairFileOwnerOnly(path string) error {
	f, err := openRegularNoFollow(path, os.O_WRONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode().Perm() == 0o600 {
		return nil
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// ReadNoFollow reads a cap file without following a symlink. The export path reads
// CAP.md before it decides whether the file is current, and a link there would feed
// the version gate foreign content (YM-359).
func ReadNoFollow(path string) ([]byte, error) {
	f, err := openRegularNoFollow(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCapFileSize+1))
	if err != nil {
		return nil, err
	}
	// A cap file is a document of a few kilobytes. The limit keeps a planted file
	// from turning the export or the install into an allocation nobody asked for.
	if len(data) > maxCapFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxCapFileSize)
	}
	return data, nil
}

// maxCapFileSize bounds the read of a cap file. The bundled bodies are a few
// kilobytes, and the largest one is far below this.
const maxCapFileSize = 4 << 20

// openRegularNoFollow opens an existing cap file for reading or writing and refuses
// anything that is not a plain, singly linked regular file.
//
// O_NOFOLLOW alone is not enough. It guards the last path component only, and
// open(2) blocks on a FIFO, so a Stat on the descriptor would never run. The Lstat
// answers that before the open, O_NONBLOCK answers it for the window in which the
// entry is swapped after the Lstat (a FIFO then fails or returns at once instead of
// blocking), and the Stat repeats the type check on the descriptor. The open still
// carries O_NOFOLLOW, because the entry can be swapped to a symlink between the two.
// A file with more than one link is refused for the same reason the sandbox narrows
// its own secret file that way: the mode change would reach an inode of somebody
// else's choosing.
func openRegularNoFollow(path string, flag int) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink", path)
		}
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !stat.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// A failed assertion would silently turn the link check off, so it is refused
	// instead: every platform this builds for carries the field.
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("%s: link count unavailable", path)
	}
	if sys.Nlink > 1 {
		f.Close()
		return nil, fmt.Errorf("%s has %d hard links, refusing to touch it", path, sys.Nlink)
	}
	return f, nil
}

func formatSQL(sql string) string {
	stmts := strings.Split(sql, ";")
	var formatted []string
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		formatted = append(formatted, formatCreateStatement(stmt)+";")
	}
	return strings.Join(formatted, "\n\n")
}

func formatCreateStatement(stmt string) string {
	openParen := strings.Index(stmt, "(")
	if openParen < 0 {
		return stmt
	}
	closeParen := strings.LastIndex(stmt, ")")
	if closeParen < 0 || closeParen <= openParen {
		return stmt
	}
	prefix := stmt[:openParen]
	cols := stmt[openParen+1 : closeParen]
	suffix := stmt[closeParen+1:]

	parts := splitColumns(cols)
	if len(parts) <= 1 {
		return stmt
	}

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("(\n")
	for i, col := range parts {
		b.WriteString("  ")
		b.WriteString(strings.TrimSpace(col))
		if i < len(parts)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(")")
	b.WriteString(suffix)
	return b.String()
}

func splitColumns(cols string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, c := range cols {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, cols[start:i])
				start = i + 1
			}
		}
	}
	if start < len(cols) {
		parts = append(parts, cols[start:])
	}
	return parts
}

func formatJS(code string) string {
	if strings.Contains(code, "\n") {
		return code
	}

	var b strings.Builder
	indent := 0
	inString := byte(0)
	inTemplate := false

	for i := 0; i < len(code); i++ {
		c := code[i]

		if inString != 0 {
			b.WriteByte(c)
			if c == inString && (i == 0 || code[i-1] != '\\') {
				inString = 0
			}
			continue
		}
		if c == '`' {
			inTemplate = !inTemplate
			b.WriteByte(c)
			continue
		}
		if inTemplate {
			b.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			inString = c
			b.WriteByte(c)
			continue
		}

		switch c {
		case '{':
			b.WriteString(" {\n")
			indent++
			writeIndent(&b, indent)
		case '}':
			b.WriteByte('\n')
			indent--
			if indent < 0 {
				indent = 0
			}
			writeIndent(&b, indent)
			b.WriteByte('}')
		case ';':
			b.WriteByte(';')
			if i+1 < len(code) && code[i+1] != '\n' && code[i+1] != '}' {
				b.WriteByte('\n')
				writeIndent(&b, indent)
			}
		case '\n':
			b.WriteByte('\n')
			writeIndent(&b, indent)
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimRight(b.String(), " \t\n")
}

func writeIndent(b *strings.Builder, level int) {
	for i := 0; i < level; i++ {
		b.WriteString("  ")
	}
}
