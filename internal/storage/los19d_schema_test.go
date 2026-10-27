package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// YM-362 Key 3: createSchema discarded every migration error. A failing ALTER
// left the column missing and the extraction attempt budget silently
// unenforced. An error that is neither "duplicate column name" (a database that
// already carries the column) nor "no such table" (a fresh database, where the
// migrations run before the CREATE TABLE statements) must fail the open.
func TestLos19d_CreateSchemaReportsFailedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	first.Close()

	old := migrations
	migrations = append([]string{`UPDATE sessions SET los19d_missing_column = 1`}, old...)
	defer func() { migrations = old }()

	if _, err := Open(path); err == nil {
		t.Fatal("a failed migration was swallowed: the store opened without the repair having run")
	} else if !strings.Contains(err.Error(), "los19d_missing_column") {
		t.Fatalf("error does not name the failed migration: %v", err)
	}
}

// The errors the migration order produces by design stay benign.
func TestLos19d_BenignMigrationErrorsStayBenign(t *testing.T) {
	cases := []struct {
		err    string
		benign bool
	}{
		{"SQL logic error: duplicate column name: source", true},
		{"SQL logic error: no such table: sessions", true},
		{"SQL logic error: no such column: los19d_missing_column", false},
		{"disk I/O error", false},
	}
	for _, c := range cases {
		if got := benignMigrationError(errors.New(c.err)); got != c.benign {
			t.Errorf("benignMigrationError(%q) = %v, want %v", c.err, got, c.benign)
		}
	}
}

// A failed RENAME COLUMN is only benign when the new column really exists.
func TestLos19d_RenameIsBenignOnlyWhenTargetColumnExists(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.DB().Exec(`CREATE TABLE los19d_rename (new_name TEXT)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	missingOld := errors.New("SQL logic error: no such column: old_name")
	quotedOld := errors.New(`SQL logic error: no such column: "old_name" (1)`)

	if !renameAlreadyApplied(s.DB(), missingOld, `ALTER TABLE los19d_rename RENAME COLUMN old_name TO new_name`) {
		t.Error("a rename whose target column exists must count as already applied")
	}
	if !renameAlreadyApplied(s.DB(), quotedOld, `ALTER TABLE los19d_rename RENAME COLUMN old_name TO new_name`) {
		t.Error("SQLite quotes the column in the message; the rename must still count as applied")
	}
	if renameAlreadyApplied(s.DB(), missingOld, `ALTER TABLE los19d_rename RENAME COLUMN old_name TO missing_name`) {
		t.Error("a rename whose target column is missing must surface the error")
	}
	if renameAlreadyApplied(s.DB(), errors.New("SQL logic error: no such column: ganz_anders"),
		`ALTER TABLE los19d_rename RENAME COLUMN old_name TO new_name`) {
		t.Error("an error naming a different column must not count as this rename")
	}
	if renameAlreadyApplied(s.DB(), missingOld, `UPDATE los19d_rename SET old_name = 1`) {
		t.Error("a non-rename statement must never count as an applied rename")
	}
}
