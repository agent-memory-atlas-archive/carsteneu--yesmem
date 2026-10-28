package storage

import "testing"

// indexExists reports whether the named index is present in the schema.
func indexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var got string
	err := s.readerDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&got)
	if err != nil {
		return false
	}
	return got == name
}

// The heartbeat reads agent_messages once per second per running agent, filtered
// by target. Without an index every one of those reads scans the whole table.
//
// The indexes belong to the `indices` slice, not to `migrations`: migrations run
// before CREATE TABLE IF NOT EXISTS (and their errors are ignored), so an index
// listed there never appears on a fresh database.
func TestAgentMessages_IndexedOnTargetAndSender(t *testing.T) {
	fresh := mustOpen(t)
	for _, idx := range []string{"idx_agent_messages_target", "idx_agent_messages_sender"} {
		if !indexExists(t, fresh, idx) {
			t.Errorf("fresh store is missing %s", idx)
		}
	}
}

func TestAgentMessages_IndexesAreRecreated(t *testing.T) {
	s := mustOpen(t)

	for _, idx := range []string{"idx_agent_messages_target", "idx_agent_messages_sender"} {
		if _, err := s.db.Exec("DROP INDEX IF EXISTS " + idx); err != nil {
			t.Fatalf("drop %s: %v", idx, err)
		}
		if indexExists(t, s, idx) {
			t.Fatalf("%s survived the drop — test setup is wrong", idx)
		}

		// createSchema is what Store.Open runs for an existing database, so this
		// is the migration path a deployed store takes.
		if err := s.createSchema(); err != nil {
			t.Fatalf("createSchema: %v", err)
		}
		if !indexExists(t, s, idx) {
			t.Errorf("createSchema did not recreate %s", idx)
		}
	}

	// Running the migrations again must not fail.
	if err := s.createSchema(); err != nil {
		t.Fatalf("second createSchema: %v", err)
	}
}
