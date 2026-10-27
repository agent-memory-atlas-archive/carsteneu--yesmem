package storage

import (
	"database/sql"
	"testing"
	"time"
)

// los19dActive reports whether a learning still counts as active.
func los19dActive(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	var supersededBy sql.NullInt64
	if err := s.DB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, id).Scan(&supersededBy); err != nil {
		t.Fatalf("read superseded_by of learning %d: %v", id, err)
	}
	return !supersededBy.Valid
}

// YM-362 Key 1: SupersedeSessionLearnings is the way back out of the extraction
// attempt budget (reextract). It compared source case-sensitively and without
// agreed_upon, so the reextract path could supersede exactly the rows the user
// owns. The narrower source set was a deliberate decision once; it is wrong
// here, because fresh extraction does not rebuild an agreement.
func TestLos19d_SupersedeSessionLearningsSparesUserOwnedSources(t *testing.T) {
	s := mustOpen(t)
	const sid = "sess-los19d-user-owned"

	owned := []struct {
		source string
		id     int64
	}{
		{"agreed_upon", insertSessionLearning(t, s, sid, "agreed_upon",
			"Abgestimmte Entscheidung: Kanten werden manuell gepflegt")},
		{" AGREED_UPON ", insertSessionLearning(t, s, sid, " AGREED_UPON ",
			"Zugestimmt, mit Leerzeichen und Grossbuchstaben geschrieben")},
		{"User_Stated", insertSessionLearning(t, s, sid, "User_Stated",
			"Nutzeraussage in gemischter Schreibweise")},
		{"user_override", insertSessionLearning(t, s, sid, "user_override",
			"Nutzeranweisung, die eine automatische Zeile ersetzt")},
	}
	autoID := insertSessionLearning(t, s, sid, "llm_extracted",
		"Automatisch extrahierter Befund ohne Nutzerbeteiligung")

	// A literal NULL source is machine-owned as well. The old query spelled the
	// NULL case out explicitly, so the regression test keeps it.
	res, err := s.DB().Exec(`INSERT INTO learnings (content, category, session_id, source, created_at, model_used)
		VALUES (?, 'gotcha', ?, NULL, ?, 'haiku')`,
		"Zeile ohne Quellenangabe, maschinell geschrieben", sid, time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert NULL-source learning: %v", err)
	}
	nullSourceID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("read NULL-source id: %v", err)
	}

	supersededIDs, err := s.SupersedeSessionLearnings(sid)
	if err != nil {
		t.Fatalf("supersede session learnings: %v", err)
	}
	superseded := make(map[int64]bool, len(supersededIDs))
	for _, id := range supersededIDs {
		superseded[id] = true
	}
	if !superseded[autoID] || !superseded[nullSourceID] {
		t.Fatalf("superseded %v, want the auto-extracted row %d and the NULL-source row %d", supersededIDs, autoID, nullSourceID)
	}
	for _, row := range owned {
		if superseded[row.id] {
			t.Errorf("source %q was superseded although the user owns the row", row.source)
		}
		if !los19dActive(t, s, row.id) {
			t.Errorf("source %q is no longer active in the database", row.source)
		}
	}
	if los19dActive(t, s, nullSourceID) {
		t.Error("the NULL-source row stayed active, so the reextract path replaces nothing machine-written")
	}
}
