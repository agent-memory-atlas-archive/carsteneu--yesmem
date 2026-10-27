package hooks

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractKeywords(t *testing.T) {
	tests := []struct {
		cmd     string
		wantMin int
		wantHas []string
	}{
		// The path segments in these rows are filtered by pathNoise since YM-269,
		// so "bin" and "yesmem" are no longer keywords; the dotted file name is.
		{"cp foo ~/.local/bin/bar", 2, []string{"foo", ".local", "bar"}},
		{"git push --force origin main", 2, []string{"git", "push", "origin", "main"}},
		{"sudo systemctl restart nginx", 1, []string{"systemctl", "restart", "nginx"}},
		{"echo hello", 0, nil},
		{"ls -la", 0, nil},
		{"sqlite3 ~/.claude/yesmem/yesmem.db", 2, []string{"sqlite3", "yesmem.db"}},
		{"go build -o ~/.local/bin/yesmem .", 2, []string{"build", ".local"}},
	}

	for _, tt := range tests {
		kw := extractKeywords(tt.cmd)
		if len(kw) < tt.wantMin {
			t.Errorf("extractKeywords(%q) = %v (%d), want at least %d keywords", tt.cmd, kw, len(kw), tt.wantMin)
		}
		for _, want := range tt.wantHas {
			found := false
			for _, k := range kw {
				if k == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("extractKeywords(%q) = %v, missing %q", tt.cmd, kw, want)
			}
		}
	}
}

// TestExtractKeywordsDropsInfrastructureSegments is the regression guard for the
// first half of YM-269. extractKeywords carried no noise list at all, so every
// path segment of a command became a keyword, and hasLongKeywordMatch accepts any
// single keyword of six characters or more. A command that merely runs inside a
// worktree of this checkout therefore lifted every gotcha naming ".worktrees",
// "yesmem" or "memory" to the warning threshold.
//
// The scope was measured against the 8948 active gotchas of this installation:
// "cd /home/tester/memory/yesmem/.worktrees/x && go build ./..." matched 481 of
// them with the old keyword set and 15 with pathNoise applied. A build that also
// names bin, cmd, src and lib still matched 295 with only the checkout names
// dropped, and 0 with the full list, so the generic path words belong in the
// filter as well: they add up to a coincidental score of two on their own.
func TestExtractKeywordsDropsInfrastructureSegments(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		noise   []string
		wantHas []string
	}{
		{
			name:    "worktree path of this checkout",
			cmd:     "cd /home/tester/memory/yesmem/.worktrees/yesloop-los-31c && go build ./...",
			noise:   []string{"home", "chief", "memory", "yesmem", ".worktrees"},
			wantHas: []string{"build", "yesloop-los-31c"},
		},
		{
			name:    "dotted infrastructure segments",
			cmd:     "sqlite3 ~/.claude/yesmem/yesmem.db",
			noise:   []string{".claude", "yesmem"},
			wantHas: []string{"sqlite3", "yesmem.db"},
		},
		{
			name:    "generic build directories",
			cmd:     "go build -o bin/yesmem ./cmd/yesmem && ls src/lib",
			noise:   []string{"bin", "cmd", "src", "lib", "yesmem"},
			wantHas: []string{"build"},
		},
		{
			// Nothing but installation paths: the filter would leave no keyword at
			// all, and RunCheck returns before its lookup on an empty set. Losing the
			// keywords here means losing the hard block on the one command class that
			// deletes the installation, so the path words come back as the signal.
			name:    "command that is nothing but a path into this installation",
			cmd:     "rm -rf /home/tester/.claude/yesmem",
			wantHas: []string{"home", "chief", ".claude", "yesmem"},
		},
		{
			// The fallback must not depend on this developer's user name being in
			// pathNoise: "/home/alice" has to be recognised as a home path, otherwise
			// exactly one short keyword ("alice") survives and the command drops below
			// the match threshold.
			name:    "path-only command under another user name",
			cmd:     "rm -rf /home/alice/.claude/yesmem",
			wantHas: []string{"home", "alice", ".claude", "yesmem"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kw := extractKeywords(tt.cmd)
			for _, noise := range tt.noise {
				for _, k := range kw {
					if k == noise {
						t.Errorf("extractKeywords(%q) = %v, %q is path noise and must not be a keyword", tt.cmd, kw, noise)
					}
				}
			}
			for _, want := range tt.wantHas {
				found := false
				for _, k := range kw {
					if k == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("extractKeywords(%q) = %v, missing specific keyword %q", tt.cmd, kw, want)
				}
			}
		})
	}
}

// TestHomeUserNames pins the generic user-name detection: the path noise list
// carries this developer's name, so the detection cannot be the list.
func TestHomeUserNames(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{"linux home", "rm -rf /home/alice/.claude/yesmem", []string{"alice"}},
		{"macOS home", "cat /users/bob/.claude.json", []string{"bob"}},
		{"two homes", "/home/a/x /home/b/y", []string{"a", "b"}},
		{"no home path", "go build ./...", nil},
		{"empty name", "ls /home/", nil},
		{"name at end without separator", "du -sh /home/carol", []string{"carol"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := homeUserNames(strings.ToLower(tt.cmd))
			if len(got) != len(tt.want) {
				t.Fatalf("homeUserNames(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
			for _, want := range tt.want {
				if !got[want] {
					t.Errorf("homeUserNames(%q) = %v, missing %q", tt.cmd, got, want)
				}
			}
		})
	}
}

func TestMatchScore(t *testing.T) {
	keywords := []string{"sandbox", "local", "bin"}
	content := "When copying to ~/.local/bin/ sandbox must be disabled"

	score := matchScore(keywords, content)
	if score != 3 {
		t.Errorf("matchScore got %d, want 3", score)
	}
}

func TestMatchScoreNoMatch(t *testing.T) {
	keywords := []string{"docker", "compose"}
	content := "Deploy nach ~/.local/bin/ IMMER mit dangerouslyDisableSandbox"

	score := matchScore(keywords, content)
	if score != 0 {
		t.Errorf("matchScore got %d, want 0", score)
	}
}

func TestHasLongKeywordMatch(t *testing.T) {
	tests := []struct {
		keywords []string
		content  string
		want     bool
	}{
		{[]string{"systemctl", "nginx"}, "systemctl restart failed", true},
		{[]string{"cp", "bin"}, "copy to bin directory", false}, // both < 6 chars
		{[]string{"sqlite3"}, "sqlite3 CLI ist NICHT installiert", true},
	}

	for _, tt := range tests {
		got := hasLongKeywordMatch(tt.keywords, tt.content)
		if got != tt.want {
			t.Errorf("hasLongKeywordMatch(%v, %q) = %v, want %v", tt.keywords, tt.content, got, tt.want)
		}
	}
}

// TestHasFileKeywordMatchRejectsHiddenSegments is the regression guard for the
// second half of YM-269. Every keyword that carried a dot counted as file-like, so
// a gotcha that merely named a hidden directory (.git, .venv, .config, .cache,
// .worktrees, .claude) was specific enough to inject on its own below that
// directory. A hidden path segment carries its dot at position 0 and nothing after
// it; a file name carries the dot later ("main.go", ".claude.json").
//
// Accepted direction: an extension-less dotfile such as ".gitignore" is no longer
// file-like. That errs toward silence, which is the safe direction for this guard,
// and the command path still matches such a name on its length through
// hasLongKeywordMatch.
func TestHasFileKeywordMatchRejectsHiddenSegments(t *testing.T) {
	tests := []struct {
		name     string
		keywords []string
		content  string
		fileBase string
		want     bool
	}{
		{"hidden dir .git", []string{".git"}, "objects liegen unter .git/objects", "", false},
		{"hidden dir .venv", []string{".venv"}, "python kommt aus .venv", "", false},
		{"hidden dir .config", []string{".config"}, "liegt in .config/opencode", "", false},
		{"hidden dir .cache", []string{".cache"}, "cache liegt in .cache/go-build", "", false},
		{"hidden dir .worktrees", []string{".worktrees"}, "editiert in .worktrees/feature-x", "", false},
		{"hidden dir .claude", []string{".claude"}, "Hook aus .claude/hooks", "", false},
		{"real file name", []string{"settings.json"}, "settings.json editieren", "settings.json", true},
		{"hidden file with extension", []string{".claude.json"}, "lies .claude.json", ".claude.json", true},
		// On a file op the target tells a hidden FILE from a hidden directory: a
		// keyword equal to the edited name counts wherever its dot sits.
		{"extension-less dotfile as the edited file", []string{".env"}, "Die Datei .env ueberschreibt Credentials", ".env", true},
		{"extension-less dotfile as the edited file (.gitignore)", []string{".gitignore"}, "Eintrag in .gitignore", ".gitignore", true},
		{"extension-less dotfile on an unrelated edit", []string{".env"}, "in .env steht ein Key", "config.yaml", false},
		{"hidden dir segment while editing another file", []string{".git"}, "liegt under .git", "config", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasFileKeywordMatch(tt.keywords, tt.content, tt.fileBase); got != tt.want {
				t.Errorf("hasFileKeywordMatch(%v, %q, %q) = %v, want %v",
					tt.keywords, tt.content, tt.fileBase, got, tt.want)
			}
		})
	}

	// The command path has no target to compare against, so an extension-less
	// dotfile stays matchable only through the length rule there.
	if !hasLongKeywordMatch([]string{".gitignore"}, "Eintrag in .gitignore") {
		t.Error("hasLongKeywordMatch must still admit a long dotfile name like .gitignore")
	}
	if hasLongKeywordMatch([]string{".env"}, ".env fehlt") {
		t.Error("a four-rune dotfile cannot reach hasLongKeywordMatch; that is the accepted gap")
	}
}

func TestExtractKeywordsFiltersGoTestNoise(t *testing.T) {
	// "go test ./internal/ivf/ -run TestIsStale -count=1 -v -timeout 30s"
	// should NOT produce "test" or "internal" — these are too generic
	kw := extractKeywords("go test ./internal/ivf/ -run TestIsStale -count=1 -v -timeout 30s")

	for _, noise := range []string{"test", "internal", "count", "timeout", "run"} {
		for _, k := range kw {
			if k == noise {
				t.Errorf("extractKeywords(go test ...) should NOT include %q — too generic, causes cross-matching between unrelated go test commands", noise)
			}
		}
	}

	// But specific tokens like "ivf" and "testisstale" should still be present
	wantHas := []string{"ivf", "testisstale"}
	for _, want := range wantHas {
		found := false
		for _, k := range kw {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("extractKeywords(go test ...) = %v, missing specific keyword %q", kw, want)
		}
	}
}

func TestExtractPathKeywords(t *testing.T) {
	tests := []struct {
		path    string
		wantMin int
		wantHas []string
		wantNot []string
	}{
		// The checkout directory names ("memory", "yesmem") appear in nearly every
		// gotcha text, so as keywords they lift every edit of a file below them to
		// score 2 — the warning threshold — on their own (YM-160).
		{"/home/user/memory/yesmem/main.go", 3, []string{"user", "main.go", "main"}, []string{"memory", "yesmem"}},
		{"/home/user/memory/yesmem/internal/hooks/check.go", 3, []string{"user", "hooks", "check.go", "check"}, []string{"memory", "yesmem"}},
		// The extension-less stem is derived from the segment, so a noise word must
		// not slip back in through it.
		{"/x/data/yesmem.db", 2, []string{"data", "yesmem.db"}, []string{"yesmem"}},
		{"/var/www/html/erecht/analyser/src/Service/MetricsService.php", 3, []string{"erecht", "analyser", "service", "metricsservice.php", "metricsservice"}, nil},
		// YM-226: ".worktrees" and ".claude" are infrastructure segments like
		// "internal" — one sits in every worktree path, the other in every path
		// of the config directory. Because a dotted keyword is treated as
		// filename-like, a gotcha naming either one was injected on every edit
		// below it. The loop name and the file name stay keywords: those are
		// what a gotcha can legitimately name.
		{"/home/user/.claude/settings.json", 2, []string{"user", "settings.json", "settings"}, []string{".claude"}},
		{"/home/user/proj/.worktrees/feature-x/internal/hooks/check.go", 3, []string{"proj", "feature-x", "hooks", "check.go", "check"}, []string{".worktrees"}},
		{"/tmp/foo", 0, nil, nil}, // too short / noise
	}

	for _, tt := range tests {
		kw := extractPathKeywords(tt.path)
		if len(kw) < tt.wantMin {
			t.Errorf("extractPathKeywords(%q) = %v (%d), want at least %d", tt.path, kw, len(kw), tt.wantMin)
		}
		for _, want := range tt.wantHas {
			found := false
			for _, k := range kw {
				if k == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("extractPathKeywords(%q) = %v, missing %q", tt.path, kw, want)
			}
		}
		for _, unwanted := range tt.wantNot {
			for _, k := range kw {
				if k == unwanted {
					t.Errorf("extractPathKeywords(%q) = %v, must not contain %q", tt.path, kw, unwanted)
				}
			}
		}
	}
}

func TestHasNonFilenameKeywordMatch(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    bool
	}{
		{
			name:    "file name only",
			path:    "/x/data/ab.txt",
			content: "Bash error: `cat ab.txt` → No such file",
			want:    false,
		},
		{
			name:    "file name with stem",
			path:    "/x/data/notes.txt",
			content: "Bash error: `cat notes.txt` → No such file",
			want:    false,
		},
		{
			name:    "echoed absolute path",
			path:    "/x/data/ab.txt",
			content: "Bash error: `cat /x/data/ab.txt` → cat: /x/data/ab.txt: No such file",
			want:    false,
		},
		{
			name:    "echoed directory only",
			path:    "/x/data/ab.txt",
			content: "Bash error: `ls -la /x/data` → Permission denied",
			want:    false,
		},
		{
			name:    "directory segment as a standalone word",
			path:    "/x/data/ab.txt",
			content: "Bash error: `cp ab.txt data` → data is read-only",
			want:    true,
		},
		{
			name:    "unrelated transcript",
			path:    "/x/data/ab.txt",
			content: "Bash error: `systemctl restart nginx` → Unit not found",
			want:    false,
		},
		{
			// MED-1: the file name is the last path segment, whether or not it
			// carries an extension. "Makefile" must not count as overlap just
			// because the transcript echoes "[Makefile:12:".
			name:    "extension-less file name only",
			path:    "/home/u/yesmem/Makefile",
			content: "Bash error: `make deploy` in /home/u/yesmem → make: *** [Makefile:12: deploy] Error 1",
			want:    false,
		},
		{
			// Guard against over-suppression: a non-filename path segment that
			// the transcript mentions as a standalone word still counts. The
			// segment must not be one of the pathNoise names — those are no
			// keywords at all (YM-160).
			name:    "extension-less file name plus standalone segment",
			path:    "/home/u/loop/Makefile",
			content: "Bash error: `make loop` → make: *** [Makefile:12: loop] Error 1",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasNonFilenameKeywordMatch(extractPathKeywords(tt.path), tt.content, filepath.Base(tt.path))
			if got != tt.want {
				t.Errorf("hasNonFilenameKeywordMatch(%q, %q) = %v, want %v",
					tt.path, tt.content, got, tt.want)
			}
		})
	}
}

// TestEntityKeywordMatch pins the token-boundary rule of YM-159: a nested hit
// only counts as a match when it sits on token boundaries, so the short generic
// entities the audit named no longer fire inside unrelated words.
func TestEntityKeywordMatch(t *testing.T) {
	tests := []struct {
		name    string
		entity  string
		keyword string
		want    bool
	}{
		// The false positives from the audit.
		{"two-letter entity inside push", "sh", "push", false},
		{"two-letter entity inside category", "go", "category", false},
		{"two-letter entity inside check.go", "go", "check.go", false},
		{"three-letter entity inside a longer word", "run", "runtime", false},
		{"entity inside a longer word, reversed", "gitignore", "git", false},
		{"empty entity", "", "hooks", false},
		{"empty keyword", "hooks", "", false},
		// Real matches that must survive.
		{"identical names", "hooks", "hooks", true},
		{"entity more specific than keyword", "internal/hooks/check.go", "hooks", true},
		{"keyword more specific than entity", "src", "src/main.go", true},
		{"dotted entity and its stem", "check.go", "check", true},
		{"dashed entity and its word", "hook-check", "hook", true},
		{"domain entity inside host", "reddit.com", "www.reddit.com", true},
		{"case insensitive", "Check.Go", "check", true},
		// Accepted trade-off of the token rule: an inflection is a different token,
		// so only the V2 metadata path narrows here (the content score still uses
		// substring search).
		{"plural is a different token", "hook", "hooks", false},
		// The bound counts runes, so one non-ASCII letter is not a three-character
		// nested side even though it is three bytes.
		{"single non-ASCII letter", "中", "foo-中-bar", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entityKeywordMatch(tt.entity, tt.keyword); got != tt.want {
				t.Errorf("entityKeywordMatch(%q, %q) = %v, want %v", tt.entity, tt.keyword, got, tt.want)
			}
		})
	}
}
