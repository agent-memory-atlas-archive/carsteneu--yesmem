package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ProjectDirName has to reproduce the encoding Claude Code uses for
// ~/.claude/projects, otherwise a project's exported sessions land beside, not
// inside, the folder holding its JSONL copies.
func TestProjectDirNameMatchesClaudeProjectEncoding(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/home/tester/memory/yesmem", "-home-tester-memory-yesmem"},
		{"/home/tester/memory/.claude", "-home-tester-memory--claude"},
		{"/home/tester/memory/yesmem/", "-home-tester-memory-yesmem"},
		// Claude Code drops the "+" as well (live: the worktree directory below
		// exists as -home-tester-memory-yesmem--claude-worktrees-feat-capability-memory).
		{"/home/tester/memory/yesmem/.claude/worktrees/feat+capability-memory", "-home-tester-memory-yesmem--claude-worktrees-feat-capability-memory"},
		{"/home/user/a+b", "-home-user-a-b"},
		// Claude's rule is "every character outside [A-Za-z0-9] becomes a dash"
		// (extracted from the shipped CLI and probed with an isolated
		// CLAUDE_CONFIG_DIR: dir_with_underscore, "a b@c+d" all lose the character).
		{"/home/tester/projects/dora_bank11", "-home-tester-projects-dora-bank11"},
		{"/tmp/a b@c", "-tmp-a-b-c"},
		{"", "unknown"},
		{"/", "unknown"},
		{"  ", "unknown"},
	}
	for _, tc := range cases {
		if got := ProjectDirName(tc.path); got != tc.want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// That "+" and "_" collide with "-" is Claude Code's behavior and therefore
// wanted here: two projects whose names differ only in such a character share
// one folder. The archive has no reverse mapping, so the fold must not surprise
// anyone later as a bug.
func TestProjectDirNameAdoptsClaudesCollisions(t *testing.T) {
	for _, tc := range [][2]string{
		{"/home/user/a-b", "/home/user/a+b"},
		{"/home/user/a-b", "/home/user/a_b"},
		{"/home/user/a-b", "/home/user/a b"},
	} {
		got := ProjectDirName(tc[0])
		// Pinned absolutely as well: a constant encoder would satisfy the
		// equality check alone.
		if got != "-home-user-a-b" {
			t.Fatalf("ProjectDirName(%q) = %q, want -home-user-a-b", tc[0], got)
		}
		if ProjectDirName(tc[1]) != got {
			t.Errorf("%q and %q were told apart (%q vs %q), Claude Code folds both",
				tc[0], tc[1], got, ProjectDirName(tc[1]))
		}
	}
}

func TestArchiveFile(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	// Create a fake JSONL
	srcFile := filepath.Join(srcDir, "test-session.jsonl")
	content := []byte(`{"type":"user","message":"hello"}` + "\n")
	if err := os.WriteFile(srcFile, content, 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	a := New(archiveDir)

	// Archive it under project "myproject"
	err := a.ArchiveFile(srcFile, "myproject")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Verify archived file exists
	archived := filepath.Join(archiveDir, "myproject", "test-session.jsonl")
	data, err := os.ReadFile(archived)
	if err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if string(data) != string(content) {
		t.Error("archived content doesn't match original")
	}
}

func TestArchiveFile_Dedup(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()

	srcFile := filepath.Join(srcDir, "session.jsonl")
	os.WriteFile(srcFile, []byte("v1"), 0644)

	a := New(archiveDir)
	a.ArchiveFile(srcFile, "proj")

	// Archive same file again with different content AND size — should overwrite
	os.WriteFile(srcFile, []byte("version-two-longer"), 0644)
	a.ArchiveFile(srcFile, "proj")

	archived := filepath.Join(archiveDir, "proj", "session.jsonl")
	data, _ := os.ReadFile(archived)
	if string(data) != "version-two-longer" {
		t.Errorf("expected 'version-two-longer', got %q", string(data))
	}
}

func TestArchiveFile_SrcNotFound(t *testing.T) {
	a := New(t.TempDir())
	err := a.ArchiveFile("/nonexistent.jsonl", "proj")
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

// readArchived reads the archived copy of a session file.
func readArchived(t *testing.T, archiveDir, project, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(archiveDir, project, name))
	if err != nil {
		t.Fatalf("read archived %s: %v", name, err)
	}
	return data
}

// A running session is archived after every write, so the copy must grow with the
// file instead of being rewritten: a full copy per append is O(file size) per
// turn, which is what the incremental index path exists to avoid.
func TestArchiveFileAppendsGrowthIncrementally(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "grow.jsonl")
	a := New(archiveDir)

	first := strings.Repeat("a", 4096) + "\n"
	if err := os.WriteFile(src, []byte(first), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if got := a.fullCopies.Load(); got != 1 {
		t.Fatalf("first archive: %d full copies, want 1", got)
	}

	second := first + strings.Repeat("b", 8192) + "\n"
	if err := os.WriteFile(src, []byte(second), 0644); err != nil {
		t.Fatalf("grow src: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if got := a.appendedCopies.Load(); got != 1 {
		t.Errorf("growth: %d appends, want 1 (the copy was rewritten)", got)
	}
	if got := a.fullCopies.Load(); got != 1 {
		t.Errorf("growth: %d full copies, want still 1", got)
	}
	if got := readArchived(t, archiveDir, "proj", "grow.jsonl"); string(got) != second {
		t.Errorf("archived copy does not match the source (%d bytes, want %d)", len(got), len(second))
	}

	// A third pass with nothing new must not touch the copy at all.
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("third archive: %v", err)
	}
	if got := a.fullCopies.Load() + a.appendedCopies.Load(); got != 2 {
		t.Errorf("unchanged file: %d copies in total, want still 2", got)
	}
}

// The copy may not be assumed to be a prefix of the source: when the file was
// rewritten (not appended to), the archived copy has to be replaced, not
// extended with a tail that no longer follows it.
func TestArchiveFileRewritesWhenCopyIsNotAPrefix(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "rewritten.jsonl")
	a := New(archiveDir)

	first := strings.Repeat("a", 512) + "\n"
	if err := os.WriteFile(src, []byte(first), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("first archive: %v", err)
	}

	// Same length, different bytes, then a longer tail: the offsets line up but
	// the content does not.
	rewritten := strings.Repeat("c", 512) + "\n" + strings.Repeat("d", 64)
	if err := os.WriteFile(src, []byte(rewritten), 0644); err != nil {
		t.Fatalf("rewrite src: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if got := a.fullCopies.Load(); got != 2 {
		t.Errorf("rewritten file: %d full copies, want 2 (the stale copy was appended to)", got)
	}
	if got := readArchived(t, archiveDir, "proj", "rewritten.jsonl"); string(got) != rewritten {
		t.Errorf("archived copy does not match the rewritten source")
	}
}

// The archived copy must survive the checksum being a window: a session that grew
// in several steps still ends up byte-identical to its source.
func TestArchiveFileGrowsInStepsWithoutLosingBytes(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "steps.jsonl")
	a := New(archiveDir)

	content := strings.Repeat("x", 700) + "\n"
	for i := 0; i < 5; i++ {
		content += strings.Repeat(string(rune('0'+i)), 300) + "\n"
		if err := os.WriteFile(src, []byte(content), 0644); err != nil {
			t.Fatalf("step %d: write src: %v", i, err)
		}
		if err := a.ArchiveFile(src, "proj"); err != nil {
			t.Fatalf("step %d: archive: %v", i, err)
		}
	}

	if got := a.fullCopies.Load(); got != 1 {
		t.Errorf("full copies: %d, want 1 (every step copied the whole file)", got)
	}
	if got := a.appendedCopies.Load(); got != 4 {
		t.Errorf("appends: %d, want 4 (one per step after the first archive)", got)
	}
	if got := readArchived(t, archiveDir, "proj", "steps.jsonl"); string(got) != content {
		t.Errorf("archived copy does not match the source after 5 steps")
	}
}

// The rewrite check reads windows at the start, the middle and the end of the
// copy, so a rewrite is caught wherever it landed in the copied prefix — not only
// when it happens to touch the last 256 bytes.
func TestArchiveFileDetectsRewriteAwayFromTheTail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(prefix string) string
	}{
		{"start", func(p string) string { return "START-REWRITTEN" + p[15:] }},
		{"middle", func(p string) string {
			mid := len(p) / 2
			return p[:mid] + "MIDDLE-REWRITTEN" + p[mid+16:]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()
			archiveDir := t.TempDir()
			src := filepath.Join(srcDir, "rewrite.jsonl")
			a := New(archiveDir)

			// Multiple of 256 so every window is fully populated.
			first := strings.Repeat("0123456789abcdef", 160) + "\n" // 2561 bytes
			if err := os.WriteFile(src, []byte(first), 0644); err != nil {
				t.Fatalf("write src: %v", err)
			}
			if err := a.ArchiveFile(src, "proj"); err != nil {
				t.Fatalf("first archive: %v", err)
			}

			// Same length, rewritten away from the tail, then longer: the offsets
			// line up everywhere.
			rewritten := tc.rewrite(first) + strings.Repeat("z", 512) + "\n"
			if len(rewritten) <= len(first) {
				t.Fatalf("test setup: rewrite did not grow the file (%d vs %d)", len(rewritten), len(first))
			}
			if err := os.WriteFile(src, []byte(rewritten), 0644); err != nil {
				t.Fatalf("rewrite src: %v", err)
			}
			if err := a.ArchiveFile(src, "proj"); err != nil {
				t.Fatalf("second archive: %v", err)
			}

			if got := a.fullCopies.Load(); got != 2 {
				t.Errorf("full copies: %d, want 2 (the %s rewrite was appended to instead of replaced)", got, tc.name)
			}
			if got := readArchived(t, archiveDir, "proj", "rewrite.jsonl"); string(got) != rewritten {
				t.Errorf("archived copy does not match the rewritten source")
			}
		})
	}
}

// Appending is not idempotent on its own, and two archive passes can run at once:
// the pool indexes with more than one worker, and in a real corpus three archive
// destinations are fed by two source paths each (subagent files that share a
// name). Both callers can read the same copy size and append the same tail, which
// leaves a copy of twice the appended length — a mix that matches no version of
// the source. Each pass verifies the length it produced and replaces the copy if
// somebody else appended in between.
func TestArchiveFileSurvivesConcurrentAppends(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "concurrent.jsonl")
	a := New(archiveDir)

	base := strings.Repeat("a", 4096) + "\n"
	if err := os.WriteFile(src, []byte(base), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("first archive: %v", err)
	}

	for round := 0; round < 40; round++ {
		grown := base + strings.Repeat("z", 512+round) + "\n"
		if err := os.WriteFile(src, []byte(grown), 0644); err != nil {
			t.Fatalf("round %d: grow src: %v", round, err)
		}

		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = a.ArchiveFile(src, "proj")
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent archive %d: %v", round, i, err)
			}
		}

		got := readArchived(t, archiveDir, "proj", "concurrent.jsonl")
		if string(got) != grown {
			t.Fatalf("round %d: archived copy is %d bytes (source %d): an append was appended twice",
				round, len(got), len(grown))
		}
	}
}

// A session document is replaced, not extended, and it holds the conversation
// itself: the copy must stay owner-only and must not leave temp files behind.
func TestWriteSessionDocReplacesPrivately(t *testing.T) {
	archiveDir := t.TempDir()
	a := New(archiveDir)

	write := func(body string) func(io.Writer) error {
		return func(w io.Writer) error {
			_, err := io.WriteString(w, body)
			return err
		}
	}
	if err := a.WriteSessionDoc("proj", "session.json", write(`{"v":1}`)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := a.WriteSessionDoc("proj", "session.json", write(`{"v":2}`)); err != nil {
		t.Fatalf("second write: %v", err)
	}

	path := filepath.Join(archiveDir, "proj", "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != `{"v":2}` {
		t.Errorf("archived document = %q, want the replacement", string(data))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("archived document mode = %o, want 600 (it contains the conversation)", perm)
	}

	entries, err := os.ReadDir(filepath.Join(archiveDir, "proj"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("project folder holds %v, want only the document", names)
	}
}

// The name is formed in another package; the archiver refuses anything that
// would leave the project folder.
func TestWriteSessionDocRejectsEscapingNames(t *testing.T) {
	archiveDir := t.TempDir()
	a := New(archiveDir)

	for _, name := range []string{"", ".", "..", "../escape.json", "sub/escape.json", "/abs.json"} {
		err := a.WriteSessionDoc("proj", name, func(w io.Writer) error { return nil })
		if err == nil {
			t.Errorf("name %q was accepted, want an error", name)
		}
	}
	for _, project := range []string{"", ".", "..", "../escaped-project", "a/b"} {
		err := a.WriteSessionDoc(project, "s.json", func(w io.Writer) error { return nil })
		if err == nil {
			t.Errorf("project %q was accepted, want an error", project)
		}
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "escape.json")); err == nil {
		t.Error("a write escaped the project folder")
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "..", "escaped-project", "s.json")); err == nil {
		t.Error("a write escaped the archive base directory")
	}
}

// A failing writer must not replace the previous document with a truncated one.
func TestWriteSessionDocKeepsPreviousCopyOnWriteError(t *testing.T) {
	archiveDir := t.TempDir()
	a := New(archiveDir)

	if err := a.WriteSessionDoc("proj", "s.json", func(w io.Writer) error {
		_, err := io.WriteString(w, `{"v":1}`)
		return err
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	boom := errors.New("boom")
	err := a.WriteSessionDoc("proj", "s.json", func(w io.Writer) error {
		io.WriteString(w, `{"v":2`)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("second write: got %v, want the writer's error", err)
	}

	data, err := os.ReadFile(filepath.Join(archiveDir, "proj", "s.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != `{"v":1}` {
		t.Errorf("archived document = %q, want the previous complete copy", string(data))
	}
	entries, err := os.ReadDir(filepath.Join(archiveDir, "proj"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("project folder holds %d entries, want only the document", len(entries))
	}
}
