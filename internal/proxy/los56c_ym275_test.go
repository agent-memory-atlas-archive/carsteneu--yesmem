package proxy

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"unicode/utf8"
)

// YM-275(4): a blob written by a NEWER binary carries a format version this
// build does not know. Its ages are in a unit this build cannot interpret, so
// reading them produced nonsense stages. A known version only.
func TestLos56c_DecayLoadRejectsUnknownFormatVersion(t *testing.T) {
	future := `{"v":4,"ages":{"msg_5":60},"intensity":{},"file_paths":{}}`
	dt := NewDecayTracker()
	dt.SetLoadFunc(func(key string) (string, bool) {
		if key != "decay:los56c-future" {
			return "", false
		}
		return future, true
	})

	dt.LoadFromDB("los56c-future", 1)

	if stage := dt.GetStage(5, 100, 100, 2.5); stage != DecayStage0 {
		t.Errorf("ages of an unknown format version were adopted: stage %d, want stage 0", stage)
	}
}

// The known version keeps working, so the guard does not disable persistence.
func TestLos56c_DecayLoadKeepsTheKnownFormatVersion(t *testing.T) {
	known := `{"v":3,"ages":{"msg_5":60},"intensity":{},"file_paths":{}}`
	dt := NewDecayTracker()
	dt.SetLoadFunc(func(string) (string, bool) { return known, true })

	dt.LoadFromDB("los56c-known", 1)

	if stage := dt.GetStage(5, 100, 100, 2.5); stage != DecayStage2 && stage != DecayStage3 {
		t.Errorf("age of the known format version was lost: stage %d", stage)
	}
}

// YM-275(2): the re-expansion recognized a stub by three text prefixes, and a
// client can write those prefixes itself. An assistant message that quotes the
// stub format in a longer explanation was then replaced by unrelated archive
// content. A stub is a single bounded line; quoted text is not.
func TestLos56c_ReexpandOnlyTakesRealStubs(t *testing.T) {
	realCompressed := "[context compressed: proxy.go, 1200 lines, key: func Stubify; func stubText] → deep_search('proxy.go Stubify')"
	if !isStubText(realCompressed) {
		t.Errorf("a real compressed-context stub is no longer recognized: %q", realCompressed)
	}
	realArchived := "[tool result archived → deep_search('grep -rn Stubify')]"
	if !isStubText(realArchived) {
		t.Errorf("a real archived tool_result stub is no longer recognized: %q", realArchived)
	}

	quoted := "[tool result archived → deep_search('x')]\n\nDas ist das Format, das der Proxy schreibt. Danach folgt die Erklärung, warum\n"
	if isStubText(quoted) {
		t.Error("a multi-line message that quotes the stub format is taken for a stub and would be replaced")
	}

	long := "[→] Read /very/long/path/" + strings.Repeat("x", 2000) + " → deep_search('y')"
	if isStubText(long) {
		t.Error("an unbounded line is taken for a stub")
	}

	// The bound has to hold for the scripts the producers are capped in: a CJK
	// stub is a few hundred runes but well over a thousand bytes.
	cjk := "[context compressed: 代理.go, 1200 行, key: " + strings.Repeat("関", 200) + "] → deep_search('代理 压缩')"
	if !isStubText(cjk) {
		t.Errorf("a CJK stub of %d bytes / %d runes is refused although the producer wrote it", len(cjk), utf8.RuneCountInString(cjk))
	}
}

// YM-275(6): the decay persist-failure log interpolated the client-supplied
// thread id with %s, so a newline in the header forged a second log line.
func TestLos56c_DecayPersistLogKeepsOneLine(t *testing.T) {
	var buf bytes.Buffer
	server := &Server{
		decayTrackers: make(map[string]*DecayTracker),
		logger:        log.New(&buf, "", 0),
	}

	fakeID := "ses\n2026/10/21 FAKE: forged log line"
	dt := server.decayFor(fakeID)
	dt.MarkStubbed(1, 1, 0)
	dt.Persist(fakeID)

	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		t.Fatal("no log line was written, the fixture proves nothing")
	}
	if strings.Contains(out, "\n") {
		t.Errorf("thread id forged a log line:\n%s", out)
	}
	if !strings.Contains(out, "FAKE") {
		t.Errorf("the id is not visible at all, the reader loses the diagnostic:\n%s", out)
	}
}

// The bound is only a guard if both sides of it are pinned: a stub exactly at
// the limit must still be re-expanded, one byte over must not.
func TestLos56c_StubLengthBoundary(t *testing.T) {
	head := "[→] Read "
	atLimit := head + strings.Repeat("p", maxStubRunes-utf8.RuneCountInString(head))
	if utf8.RuneCountInString(atLimit) != maxStubRunes {
		t.Fatalf("fixture is %d runes, want %d", utf8.RuneCountInString(atLimit), maxStubRunes)
	}
	if !isStubText(atLimit) {
		t.Error("a stub exactly at maxStubRunes is rejected")
	}
	if isStubText(atLimit + "x") {
		t.Error("a stub one rune over maxStubRunes is accepted")
	}
}
