package config

import (
	"path/filepath"
	"testing"
)

// YM-359 (OPS-88, key 2): CapsDirPath handed a configured caps_dir back verbatim.
// A relative value, and the "~/.claude/caps" spelling the config reference
// documents, then resolved against whatever working directory each consumer had
// — the watcher walked one directory, the sandbox mask named another, and the
// importer read a third. One derivation has to yield one absolute directory, and
// it has to be the same one in the daemon (working directory home) and in
// yesmem update (working directory the caller's shell), so a relative value is
// anchored at home and not at the working directory.
func TestLos35fCapsDirPathAnchorsARelativeValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := CapsDirPath("caps-here", "/data/yesmem")

	if !filepath.IsAbs(got) {
		t.Fatalf("a relative caps_dir stayed relative: %q", got)
	}
	if want := filepath.Join(home, "caps-here"); got != want {
		t.Errorf("CapsDirPath(%q) = %q, want %q", "caps-here", got, want)
	}
}

// Without a home directory there is nothing stable to anchor against. The
// derivation then yields no directory at all, so the watcher imports nothing and the
// mask names nothing, instead of each consumer resolving the same relative value
// against its own working directory.
func TestLos35fCapsDirPathYieldsNothingWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")

	if got := CapsDirPath("caps-here", "/data/yesmem"); got != "" {
		t.Errorf("CapsDirPath(%q) = %q, want the empty answer", "caps-here", got)
	}
}

// The config reference names the default as "~/.claude/caps". Stored verbatim,
// that is a directory literally named "~", which nothing imports.
func TestLos35fCapsDirPathExpandsATildeValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := CapsDirPath("~/.claude/caps", "/data/yesmem")

	if want := filepath.Join(home, ".claude", "caps"); got != want {
		t.Errorf("CapsDirPath(%q) = %q, want %q", "~/.claude/caps", got, want)
	}
}

// A configured absolute path stays what it is; the anchoring must not move it.
func TestLos35fCapsDirPathKeepsAnAbsoluteValue(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "caps-elsewhere")

	if got := CapsDirPath(configured, "/data/yesmem"); got != configured {
		t.Errorf("CapsDirPath(%q) = %q, want it unchanged", configured, got)
	}
}

// The default derived from a relative data dir is anchored as well, otherwise the
// "always absolute" promise would hold only on one branch.
func TestLos35fCapsDirPathAnchorsARelativeDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := CapsDirPath("", filepath.Join("rel", "yesmem"))

	if !filepath.IsAbs(got) {
		t.Fatalf("a relative data dir yielded a relative caps dir: %q", got)
	}
	if want := filepath.Join(home, "rel", "caps"); got != want {
		t.Errorf("CapsDirPath(empty, rel/yesmem) = %q, want %q", got, want)
	}
}
