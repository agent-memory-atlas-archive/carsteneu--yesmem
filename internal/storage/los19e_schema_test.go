package storage

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// YM-378: createSchema runs the ALTER statements before the CREATE TABLE
// statements, so an ALTER-only column is absent during the whole first lifetime
// of a fresh store and heals only on the second open. These five columns are
// read in exactly that window: the proxy calls update_fixation_ratio (sessions)
// and track_proxy_usage with source=fork (token_usage). The CREATE TABLE
// constants must therefore carry them.
func TestLos19e_FreshStoreCarriesAlterOnlyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: store file already exists (%v), this is not a first lifetime", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	defer s.Close()

	// The declared type and default are part of the contract: presence alone
	// would accept a column the read paths cannot use as expected.
	for _, c := range []struct {
		table, column, typ, def string
	}{
		{"sessions", "fixation_ratio", "REAL", "0"},
		{"sessions", "agent_role", "TEXT", "''"},
		{"token_usage", "fork_input_tokens", "INTEGER", "0"},
		{"token_usage", "fork_output_tokens", "INTEGER", "0"},
		{"token_usage", "fork_request_count", "INTEGER", "0"},
	} {
		typ, def, ok := columnDefLos19e(t, s, c.table, c.column)
		if !ok {
			t.Errorf("%s.%s missing on a first-open store: the CREATE TABLE constant does not carry an ALTER-only column", c.table, c.column)
			continue
		}
		if typ != c.typ || def != c.def {
			t.Errorf("%s.%s declared as %s DEFAULT %s, want %s DEFAULT %s (the ALTER statement is the reference)", c.table, c.column, typ, def, c.typ, c.def)
		}
	}
}

// The two symptoms the missing columns produced, on a first-open store: the
// proxy path update_fixation_ratio must work, and a fork usage report must land.
func TestLos19e_FirstOpenServesFixationAndForkUsage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	defer s.Close()

	if _, err := s.DB().Exec(`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at)
		VALUES ('ses_los19e', 'probe', 'probe', '2026-01-01', '/probe', '2026-01-01')`); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if ratio, err := s.GetSessionFixationRatio("ses_los19e"); err != nil || ratio != 0 {
		t.Errorf("GetSessionFixationRatio on a first-open store = %v, %v; want default 0", ratio, err)
	}
	if err := s.UpdateSessionFixationRatio("ses_los19e", 0.5); err != nil {
		t.Errorf("UpdateSessionFixationRatio on a first-open store: %v", err)
	}
	if ratio, err := s.GetSessionFixationRatio("ses_los19e"); err != nil || ratio != 0.5 {
		t.Errorf("GetSessionFixationRatio after the write = %v, %v; want 0.5", ratio, err)
	}

	if err := s.TrackForkTokenUsage("thread_los19e", 11, 7); err != nil {
		t.Errorf("TrackForkTokenUsage on a first-open store: %v", err)
	}
	_, _, forkIn, forkOut, forkReqs, err := s.GetFullTokenUsage("thread_los19e")
	if err != nil || forkIn != 11 || forkOut != 7 || forkReqs != 1 {
		t.Errorf("GetFullTokenUsage = in %d, out %d, reqs %d, %v; want 11/7/1", forkIn, forkOut, forkReqs, err)
	}
}

// columnDefLos19e returns the declared type and default of a column. PRAGMA
// table_info quotes a text default, so agent_role reports "''" rather than "".
func columnDefLos19e(t *testing.T, s *Store, table, column string) (typ, def string, ok bool) {
	t.Helper()
	rows, err := s.DB().Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan PRAGMA table_info(%s): %v", table, err)
		}
		if name == column {
			return colType, defaultValue.String, true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	return "", "", false
}
