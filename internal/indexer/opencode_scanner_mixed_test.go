package indexer

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// A database that carries both generations keeps working: the fork's migration
// left the legacy tables in place. The scanner must follow the data, not the
// presence of a table, so a host that still writes v1 is read from v1 and one
// that has switched is read from v2.
func TestOpencodeScannerFollowsTheTableThatIsWritten(t *testing.T) {
	cases := []struct {
		name      string
		v1Newer   bool
		wantSess  string
		otherSess string
	}{
		{name: "v1 still written", v1Newer: true, wantSess: "ses_v1_live", otherSess: "ses_v2_old"},
		{name: "v2 already written", v1Newer: false, wantSess: "ses_v2_live", otherSess: "ses_v1_old"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			ocDBPath := filepath.Join(tmpDir, "opencode.db")
			db, err := sql.Open("sqlite", ocDBPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []string{
				`CREATE TABLE session (
					id TEXT PRIMARY KEY, project_id TEXT, parent_id TEXT,
					slug TEXT, directory TEXT, title TEXT, version TEXT,
					time_created INTEGER, time_updated INTEGER, agent TEXT, model TEXT
				)`,
				`CREATE TABLE message (
					id TEXT PRIMARY KEY, session_id TEXT,
					time_created INTEGER, time_updated INTEGER, data TEXT
				)`,
				`CREATE TABLE part (
					id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
					time_created INTEGER, time_updated INTEGER, data TEXT
				)`,
				`CREATE TABLE session_v2 (
					id TEXT PRIMARY KEY, project_id TEXT, workspace_id TEXT, parent_id TEXT,
					fork_session_id TEXT, fork_boundary TEXT, slug TEXT, directory TEXT, path TEXT,
					title TEXT, version TEXT, metadata TEXT, agent TEXT, model TEXT,
					time_created INTEGER, time_updated INTEGER
				)`,
				`CREATE TABLE session_message (
					id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL,
					seq INTEGER NOT NULL, time_created INTEGER, time_updated INTEGER, data TEXT NOT NULL
				)`,
			} {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}

			old := time.Now().Add(-48 * time.Hour).UnixMilli()
			fresh := time.Now().Add(-1 * time.Hour).UnixMilli()
			v1At, v2At := fresh, old
			if !tc.v1Newer {
				v1At, v2At = old, fresh
			}

			// One session in the legacy tables.
			if _, err := db.Exec(`INSERT INTO session
				(id, directory, title, agent, model, time_created, time_updated)
				VALUES ('ses_v1_live', '/home/test/mixed', 'legacy', '', '', ?, ?)`, v1At, v1At); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
				VALUES ('m1', 'ses_v1_live', ?, ?, '{"role":"user"}')`, v1At, v1At); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
				VALUES ('p1', 'm1', 'ses_v1_live', ?, ?, '{"type":"text","text":"from v1"}')`, v1At, v1At); err != nil {
				t.Fatal(err)
			}

			// One session in the v2 tables.
			if _, err := db.Exec(`INSERT INTO session_v2
				(id, project_id, directory, title, agent, model, time_created, time_updated)
				VALUES ('ses_v2_live', 'prj', '/home/test/mixed', 'v2', '', '', ?, ?)`, v2At, v2At); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO session_message
				(id, session_id, type, seq, time_created, time_updated, data)
				VALUES ('sm1', 'ses_v2_live', 'user', 1, ?, ?, '{"text":"from v2"}')`, v2At, v2At); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			store, err := storage.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			scanner := NewOpencodeScanner(ocDBPath, store, nil, nil, nil, nil)
			scanner.lastCheckAt = time.Now().Add(-2 * scanOpencodeInterval)
			scanner.MaybeScan()

			// Both storages exist in this fixture, so the newer one must win. The
			// session name is misleading on purpose: it says which table it lives in.
			want, err := store.GetSession(models.NormalizeSessionID(models.SourceAgentOpencode, tc.wantSess))
			if err != nil {
				t.Fatal(err)
			}
			if want == nil {
				t.Fatalf("%s was not imported", tc.wantSess)
			}

			msgs, err := store.GetMessagesBySession(models.NormalizeSessionID(models.SourceAgentOpencode, tc.wantSess))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) == 0 {
				t.Fatalf("%s was imported without its messages", tc.wantSess)
			}
			if msgs[0].Content != "from "+tc.wantSess[4:6] {
				t.Fatalf("first message %q does not come from the expected table", msgs[0].Content)
			}
		})
	}
}
