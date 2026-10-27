package storage

import (
	"testing"
	"time"
)

// YM-317 Key 5: `source` is caller-written free text. A source spelled
// 'User_Stated' or padded with blanks is the same claim as 'user_stated', so both
// cleanups must spare it. The two cleanups share the predicate, so this test
// drives both against the same seed.
func TestLos19c_CleanupsSpareMixedCaseUserSources(t *testing.T) {
	s := mustOpen(t)
	seedShortSession(t, s, "sess-lc-short", 2, 72*time.Hour)
	seedShortSession(t, s, "sess-lc-open", 40, 72*time.Hour)

	// Short-session victims: long content, finished short session.
	shortMixedStated := insertSessionLearning(t, s, "sess-lc-short", "User_Stated", "A rule the user stated in mixed case")
	shortPadded := insertSessionLearning(t, s, "sess-lc-short", " agreed_upon ", "An agreement padded with blanks")
	shortMixedOverride := insertSessionLearning(t, s, "sess-lc-short", "user_OVERRIDE", "An override in mixed case")
	shortAuto := insertSessionLearning(t, s, "sess-lc-short", "llm_extracted", "Auto-extracted content from the session")

	// Junk victims: junk shape (shorter than 10 characters), live session.
	junkMixedStated := insertSessionLearning(t, s, "sess-lc-open", "User_Stated", "zu kurz")
	junkPadded := insertSessionLearning(t, s, "sess-lc-open", "Agreed_Upon", "noch kurz")
	junkAuto := insertSessionLearning(t, s, "sess-lc-open", "llm_extracted", "junk")

	if _, err := s.SupersedeShortSessionLearnings(); err != nil {
		t.Fatalf("supersede short session learnings: %v", err)
	}
	if _, err := s.CleanupJunkLearnings(); err != nil {
		t.Fatalf("cleanup junk learnings: %v", err)
	}

	requireActive(t, s, shortMixedStated, "short-session victim with source User_Stated")
	requireActive(t, s, shortPadded, "short-session victim with padded source")
	requireActive(t, s, shortMixedOverride, "short-session victim with source user_OVERRIDE")
	requireSuperseded(t, s, shortAuto, "short-session victim with source llm_extracted")

	requireActive(t, s, junkMixedStated, "junk victim with source User_Stated")
	requireActive(t, s, junkPadded, "junk victim with source Agreed_Upon")
	requireSuperseded(t, s, junkAuto, "junk victim with source llm_extracted")
}
