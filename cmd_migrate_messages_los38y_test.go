package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-369 (Los 38y): the copy paged the source with LIMIT/OFFSET, which is
// quadratic on a growing table, and a run interrupted between the copy and the
// DELETE left the compaction unfinished while the next run reported
// "nothing to do" with exit 0.

// los38ySeedForeignTarget fills messages.db with rows that share the two databases'
// count but not their identity, so a count-only check would call them covered.
func los38ySeedForeignTarget(t *testing.T, path string) {
	t.Helper()
	los38wSeedMessagesN(t, path, 0)
	for i := 0; i < los38wMessageRows; i++ {
		los38wExec(t, path, fmt.Sprintf(
			`INSERT INTO messages (session_id, source_agent, role, message_type, content, timestamp, sequence, model)
			 VALUES ('foreign-%d', 'claude', 'user', 'text', 'other body %d', '2026-10-19T00:00:00Z', %d, 'gpt-x')`, i, i, i))
	}
}

// TestLos38yCompletesInterruptedMigration: the target already holds every source
// row, so the earlier run copied them and stopped before the DELETE. The next run
// has to finish the delete and the compaction; reporting "nothing to do" and
// exiting 0 leaves yesmem.db carrying a second copy of every message forever.
func TestLos38yCompletesInterruptedMigration(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")
	messagesDB := filepath.Join(dataDir, "messages.db")

	los38wSeedMessages(t, messagesDB)

	bleveDir := filepath.Join(dataDir, "bleve-index")
	if err := os.MkdirAll(bleveDir, 0o700); err != nil {
		t.Fatalf("seed bleve dir: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Fatalf("exit code %d for a fully copied target, want 0", code)
	}
	if got := los38wMessageCount(t, mainDB); got != 0 {
		t.Errorf("source messages = %d after the resumed run, want 0: the interrupted compaction must finish", got)
	}
	if got := los38wMessageCount(t, messagesDB); got != los38wMessageRows {
		t.Errorf("target messages = %d, want %d: the resume must not recopy or drop rows", got, los38wMessageRows)
	}
	if _, err := os.Stat(bleveDir); err == nil {
		t.Error("bleve index survived a completed migration")
	}
}

// TestLos38yRefusesUncoveredTarget: a target that does not hold the source's rows
// cannot be completed, and it must say so instead of exiting 0.
func TestLos38yRefusesUncoveredTarget(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")

	los38ySeedForeignTarget(t, filepath.Join(dataDir, "messages.db"))

	bleveDir := filepath.Join(dataDir, "bleve-index")
	if err := os.MkdirAll(bleveDir, 0o700); err != nil {
		t.Fatalf("seed bleve dir: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 although the target holds foreign rows and the source is complete; want a failure")
	}
	if got := los38wMessageCount(t, mainDB); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d: an uncovered target must not delete the source", got, los38wMessageRows)
	}
	if _, err := os.Stat(bleveDir); err != nil {
		t.Errorf("bleve index was removed although nothing was migrated: %v", err)
	}
}

// TestLos38yRefusesStaleTargetContent: identity alone is not coverage. A target
// that holds the same (session_id, sequence) pairs with a different body is not a
// copy of the source, and deleting the source would leave the only copy of that
// text nowhere.
func TestLos38yRefusesStaleTargetContent(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")

	los38wSeedMessages(t, filepath.Join(dataDir, "messages.db"))
	los38wExec(t, filepath.Join(dataDir, "messages.db"),
		`UPDATE messages SET content = 'stale body ' || sequence`)

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 although every target row carries a different body; want a failure")
	}
	if got := los38wMessageCount(t, mainDB); got != los38wMessageRows {
		t.Errorf("source messages = %d, want %d: a target with a different body must not authorise the delete", got, los38wMessageRows)
	}
}

// TestLos38yRefusesTargetWithDifferentMetadata: the coverage check authorises a
// table-wide DELETE, so it has to compare more than the body. A target row with the
// same pair and the same text but different attribution is a different record, and
// the source is the only copy of its version. Every compared column gets its own
// run, so dropping one from the check fails here.
func TestLos38yRefusesTargetWithDifferentMetadata(t *testing.T) {
	for _, stmt := range []string{
		`UPDATE messages SET role = 'assistant'`,
		`UPDATE messages SET message_type = 'thinking'`,
		`UPDATE messages SET source_agent = 'claude'`,
		`UPDATE messages SET tool_name = 'Bash'`,
		`UPDATE messages SET file_path = '/other/file.go'`,
		`UPDATE messages SET timestamp = '2025-06-06T00:00:00Z'`,
		`UPDATE messages SET model = 'other-model'`,
	} {
		t.Run(stmt, func(t *testing.T) {
			home, dataDir := los38wInstallation(t)
			mainDB := filepath.Join(dataDir, "yesmem.db")

			los38wSeedMessages(t, filepath.Join(dataDir, "messages.db"))
			los38wExec(t, filepath.Join(dataDir, "messages.db"), stmt)

			if code := los38wRunCommand(t, home, ""); code == 0 {
				t.Errorf("exit code 0 although the target rows differ in metadata (%s); want a failure", stmt)
			}
			if got := los38wMessageCount(t, mainDB); got != los38wMessageRows {
				t.Errorf("source messages = %d, want %d", got, los38wMessageRows)
			}
		})
	}
}

// TestLos38yRemovesLeftoverBleveOnRetry: a run that stops after the delete leaves a
// stale index behind. The retry finds no source rows, so it has to finish that step
// instead of exiting 0 while the index stays forever.
func TestLos38yRemovesLeftoverBleveOnRetry(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")

	los38wSeedMessages(t, filepath.Join(dataDir, "messages.db"))
	los38wExec(t, mainDB, `DELETE FROM messages`)

	bleveDir := filepath.Join(dataDir, "bleve-index")
	if err := os.MkdirAll(bleveDir, 0o700); err != nil {
		t.Fatalf("seed bleve dir: %v", err)
	}

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Fatalf("exit code %d for an installation whose migration is complete, want 0", code)
	}
	if _, err := os.Stat(bleveDir); err == nil {
		t.Error("leftover bleve index survived a retry that exited 0")
	}
	if got := los38wMessageCount(t, filepath.Join(dataDir, "messages.db")); got != los38wMessageRows {
		t.Errorf("target messages = %d, want %d: the retry must not touch them", got, los38wMessageRows)
	}
}

// TestLos38yRefusesDuplicateSourcePairs: the target's unique pair index rejects a
// source that still carries duplicates, and the message has to name the remedy
// instead of an opaque constraint failure.
func TestLos38yRefusesDuplicateSourcePairs(t *testing.T) {
	home, dataDir := los38wInstallation(t)
	mainDB := filepath.Join(dataDir, "yesmem.db")
	los38wExec(t, mainDB,
		`INSERT INTO messages (session_id, source_agent, role, message_type, content, timestamp, sequence, model)
		 VALUES ('session-0', 'codex', 'user', 'text', 'duplicate', '2026-10-19T00:00:00Z', 0, 'gpt-x')`)

	if code := los38wRunCommand(t, home, ""); code == 0 {
		t.Error("exit code 0 although the source holds a duplicate (session_id, sequence) pair; want a failure")
	}
	if got := los38wMessageCount(t, mainDB); got != los38wMessageRows+1 {
		t.Errorf("source messages = %d, want %d: a refused run must not delete anything", got, los38wMessageRows+1)
	}
}

// TestLos38yCopiesAcrossBatchBoundary: the copy works in batches of 1000, so a
// migration larger than one batch is the only case that exercises the paging loop.
func TestLos38yCopiesAcrossBatchBoundary(t *testing.T) {
	const rows = 2500
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	mainDB := filepath.Join(dataDir, "yesmem.db")
	messagesDB := filepath.Join(dataDir, "messages.db")
	los38wSeedBulk(t, mainDB, rows)

	if code := los38wRunCommand(t, home, ""); code != 0 {
		t.Fatalf("exit code %d for a %d-row migration, want 0", code, rows)
	}
	if got := los38wMessageCount(t, messagesDB); got != rows {
		t.Errorf("target messages = %d, want %d", got, rows)
	}
	if got := los38wMessageCount(t, mainDB); got != 0 {
		t.Errorf("source messages = %d after the migration, want 0", got)
	}
	if got := los38wQueryInt(t, messagesDB,
		`SELECT COUNT(*) FROM (SELECT session_id, sequence FROM messages GROUP BY session_id, sequence HAVING COUNT(*) > 1)`); got != 0 {
		t.Errorf("duplicate (session_id, sequence) groups in the target = %d, want 0: the paging loop must not recopy a batch", got)
	}
}

// TestLos38yPagesWithKeysetOrder pins the SQL shape: OFFSET paging re-reads the
// whole prefix for every batch, which turns a 300k-row migration into a quadratic
// scan.
func TestLos38yPagesWithKeysetOrder(t *testing.T) {
	source, err := os.ReadFile("cmd_migrate_messages.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if strings.Contains(string(source), "LIMIT ? OFFSET ?") {
		t.Error("cmd_migrate_messages.go still pages with LIMIT/OFFSET; use a keyset (id > lastID)")
	}
}
