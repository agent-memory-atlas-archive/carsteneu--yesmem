package storage

import (
	"database/sql"
	"testing"
)

// TestLearningsSchema_HasOriginToolColumn verifies that the learnings table
// exposes origin_tool as a TEXT column with empty-string default — both on
// fresh test DBs (built from the CREATE TABLE constant) and on production
// DBs that arrive via the ALTER TABLE migration list.
func TestLearningsSchema_HasOriginToolColumn(t *testing.T) {
	s := mustOpen(t)

	rows, err := s.db.Query(`PRAGMA table_info(learnings)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()

	type colInfo struct {
		cid          int
		name         string
		typ          string
		notNull      int
		defaultValue sql.NullString
		pk           int
	}

	var found *colInfo
	for rows.Next() {
		var c colInfo
		if err := rows.Scan(&c.cid, &c.name, &c.typ, &c.notNull, &c.defaultValue, &c.pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		if c.name == "origin_tool" {
			found = &c
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	if found == nil {
		t.Fatal("column missing: learnings.origin_tool not found — add to CREATE TABLE and ALTER TABLE migration")
	}
	if found.typ != "TEXT" {
		t.Errorf("origin_tool type = %q, want TEXT", found.typ)
	}
	if !found.defaultValue.Valid || found.defaultValue.String != "''" {
		t.Errorf("origin_tool default = %v (%q), want ''", found.defaultValue.Valid, found.defaultValue.String)
	}
}
