package textutil

import "testing"

// YM-450 (Los 64b): the provenance digest of a CAP.md body keeps the case. A cap
// body is a bash or python program, and $URL -> $url or True -> true is a different
// program. ContentHash folds the case because it identifies a learning by meaning;
// that is the wrong question for a body a bundle descends from, and the installer
// answered it with ContentHash until now.
func TestLos64bBodyDigestKeepsCase(t *testing.T) {
	pairs := [][2]string{
		{"curl -H \"$URL\"", "curl -H \"$url\""},
		{"if True:\n    pass", "if true:\n    pass"},
	}
	for _, p := range pairs {
		if BodyDigest([]byte(p[0])) == BodyDigest([]byte(p[1])) {
			t.Errorf("BodyDigest folds the case of %q and %q: the change would be no difference", p[0], p[1])
		}
		// The learning dedupe rides on ContentHash. Switching the provenance to its
		// own digest must not touch that semantic.
		if ContentHash(p[0]) != ContentHash(p[1]) {
			t.Errorf("ContentHash changed with it: %q and %q no longer hash alike", p[0], p[1])
		}
	}
}

// The failures a body collects without becoming a different body: a CRLF checkout,
// a trailing newline the editor added, a missing one at the end.
func TestLos64bBodyDigestIgnoresLineEndingsAndTrailingNewlines(t *testing.T) {
	base := "version: 1\nname: telegram\n\nbody\n"
	want := BodyDigest([]byte(base))
	for _, other := range []string{
		"version: 1\r\nname: telegram\r\n\r\nbody\r\n",
		base + "\n",
		base + "\n\n\n",
		"version: 1\nname: telegram\n\nbody",
	} {
		if got := BodyDigest([]byte(other)); got != want {
			t.Errorf("BodyDigest treats %q as a different body", other)
		}
	}
}

// Everything else is content, indentation included: a re-indented python body is a
// different program.
func TestLos64bBodyDigestKeepsIndentation(t *testing.T) {
	flat := "scripts:\n  - body: |\n      echo one\n"
	deep := "scripts:\n  - body: |\n        echo one\n"
	if BodyDigest([]byte(flat)) == BodyDigest([]byte(deep)) {
		t.Error("BodyDigest folds indentation: a re-indented body is a different body")
	}
}
