package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-354 (Los 11j): the encoder reproduced only the character rule. Claude Code
// also truncates an encoded name at 200 characters and appends "-" plus a
// base36 hash of the full path, so two long paths that share a prefix keep
// separate folders.
//
// The expected values come from the shipped CLI (version 2.1.301:
// XM truncates, Te prints abs(Kle(path)) in base 36, Kle is a 31-style hash over
// the path's UTF-16 code units with int32 wrap). The third case was verified end
// to end as well: a live Claude run with an isolated CLAUDE_CONFIG_DIR in that
// working directory created exactly that folder name. It also settles which
// string is hashed, the raw path (5aq5fp) and not the encoded name (tcdjh7).
func TestProjectDirNameCapsTheEncodedName_Los11j(t *testing.T) {
	probedName := strings.Repeat("a", 100) + "_" + strings.Repeat("b", 100)
	probedPath := "/home/chief/memory/yesmem/.worktrees/yesloop-los-11j/.yesmem/tmp/probe100/" + probedName

	cases := []struct{ name, path, want string }{
		{
			// Exactly at the limit: nothing is truncated, no hash is appended.
			// Live-confirmed with an isolated CLAUDE_CONFIG_DIR at a cwd of exactly
			// 200 encoded characters: the folder Claude created carried no hash.
			name: "encoded name of 200 characters stays untouched",
			path: "/home/chief/" + strings.Repeat("c", 188),
			want: "-home-chief-" + strings.Repeat("c", 188),
		},
		{
			// One over the limit: the first 200 characters survive, the rest is
			// replaced by "-<base36 hash of the path>". Live-confirmed at a cwd of
			// exactly 201 encoded characters: the folder was 207 characters long.
			name: "encoded name of 201 characters is cut and hashed",
			path: "/home/chief/" + strings.Repeat("c", 189),
			want: "-home-chief-" + strings.Repeat("c", 188) + "-gsrro8",
		},
		{
			// The live-probed path: 275 characters. The cut keeps the 74 characters
			// in front of the run, 100 a's, the dash the "_" becomes, and 25 b's.
			name: "long path keeps its prefix and a hash of the raw path",
			path: probedPath,
			want: "-home-chief-memory-yesmem--worktrees-yesloop-los-11j--yesmem-tmp-probe100-" +
				strings.Repeat("a", 100) + "-" + strings.Repeat("b", 25) + "-5aq5fp",
		},
	}
	for _, tc := range cases {
		got := ProjectDirName(tc.path)
		if got != tc.want {
			t.Errorf("%s: ProjectDirName(%d chars) = %q (len %d), want %q (len %d)",
				tc.name, len(tc.path), got, len(got), tc.want, len(tc.want))
		}
	}
}

// A long path must not collide with another long path sharing its prefix: the
// hash is what separates them. Without it both would truncate to the same 200
// characters and one project's archive would land in the other's folder.
func TestProjectDirNameSeparatesLongPathsSharingAPrefix_Los11j(t *testing.T) {
	prefix := "/home/chief/" + strings.Repeat("d", 250)
	a := ProjectDirName(prefix + "-alpha")
	b := ProjectDirName(prefix + "-beta")

	if len(a) <= 200 || len(b) <= 200 {
		t.Fatalf("expected a truncation plus a hash, got %d and %d characters", len(a), len(b))
	}
	if a == b {
		t.Fatalf("two long paths collapsed into %q: the hash is missing", a)
	}
	if a[:200] != b[:200] {
		t.Fatalf("the truncation point differs: %q vs %q", a[:200], b[:200])
	}
}

// ArchiveFile is exported and takes a directory name from its caller, while
// SessionDocPath has rejected non-plain components for a while. The asymmetry
// matters because a value like "../escaped" leaves the archive directory
// entirely: joining and MkdirAll would create a folder beside the base dir.
func TestArchiveFileRejectsAProjectOutsideItsFolder_Los11j(t *testing.T) {
	root := t.TempDir()
	a := New(filepath.Join(root, "archive"))
	src := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	for _, project := range []string{"", ".", "..", "../escaped", "../", "sub/dir", "../sub/dir", "/abs", "a/../b"} {
		if err := a.ArchiveFile(src, project); err == nil {
			t.Errorf("ArchiveFile accepted project %q, want an error", project)
		}
	}

	// Nothing may have appeared beside the archive directory, and the source
	// must still be the only other entry in the root.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "archive" && e.Name() != "session.jsonl" {
			t.Errorf("ArchiveFile created %q outside the archive", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Errorf("../escaped was created (stat error %v)", err)
	}
}

// The real-world value is Claude's encoded project name, dashes included: the
// new check must not reject it.
func TestArchiveFileAcceptsAClaudeEncodedProjectName_Los11j(t *testing.T) {
	root := t.TempDir()
	a := New(filepath.Join(root, "archive"))
	src := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := a.ArchiveFile(src, "-home-chief-memory-yesmem"); err != nil {
		t.Fatalf("ArchiveFile rejected an encoded project name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", "-home-chief-memory-yesmem", "session.jsonl")); err != nil {
		t.Fatalf("archived copy missing: %v", err)
	}
}
