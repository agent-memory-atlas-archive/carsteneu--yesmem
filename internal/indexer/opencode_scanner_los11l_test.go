package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-381 (Los 11l): der Scanner oeffnete die fremde opencode.db mit
// sql.Open("sqlite", s.dbPath). Der Pfad macht ausschliesslich SELECTs
// (querySessions, queryMessages), braucht also kein Schreibrecht; ein
// schreibfaehiges Handle darf eine Datei, die einer anderen Anwendung gehoert,
// trotzdem nicht oeffnen. Der Test fragt genau das Handle, das der Scanner
// benutzt, ob es schreiben kann.
func TestOpencodeScannerOpensForeignDBReadOnly_Los11l(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode.db")
	fixture, err := createTestOpencodeDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-time.Hour)
	if err := writeOpencodeSession(fixture, "ses_ro", "/home/test/ro", "Read Only", created, created, ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}

	foreign, err := openOpencodeDB(dbPath)
	if err != nil {
		t.Fatalf("open foreign db: %v", err)
	}
	defer foreign.Close()

	var sessions int
	if err := foreign.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&sessions); err != nil {
		t.Fatalf("read from the foreign database must work: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("session count = %d, want 1", sessions)
	}

	if _, err := foreign.Exec(`DELETE FROM session`); err == nil {
		t.Error("the handle the scanner uses accepts writes to the foreign database")
	}
}

// Die Gegenprobe auf dem ganzen Pfad: eine opencode.db ohne Schreibrecht fuer den
// Prozess und ohne beschreibbares Verzeichnis muss trotzdem indiziert werden. Die
// Fixture laeuft im Rollback-Journal-Modus; fuer WAL braucht auch ein read-only
// Handle ein beschreibbares Verzeichnis fuer die -shm, dann waere der Scan dort
// nicht moeglich (heute liegt opencode.db im WAL-Modus, das Verzeichnis ist
// beschreibbar). Die Zusicherung unten gilt also fuer den Journal-Modus.
func TestOpencodeScannerScansForeignDBWithoutWriteAccess_Los11l(t *testing.T) {
	tmpDir := t.TempDir()
	ocDBPath := filepath.Join(tmpDir, "opencode.db")
	ocDB, err := createTestOpencodeDB(ocDBPath)
	if err != nil {
		t.Fatal(err)
	}
	sessID := "ses_readonly"
	created := time.Now().Add(-time.Hour)
	if err := writeOpencodeSession(ocDB, sessID, "/home/test/readonly", "Read Only", created, created.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := writeOpencodeMessage(ocDB, "msg_1", sessID, "user", created, map[string]any{"role": "user"}); err != nil {
		t.Fatal(err)
	}
	if err := writeOpencodePart(ocDB, "part_1", "msg_1", sessID, created, map[string]any{"type": "text", "text": "nur lesen"}); err != nil {
		t.Fatal(err)
	}
	if err := ocDB.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(ocDBPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tmpDir, 0o755) })

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	scanner := NewOpencodeScanner(ocDBPath, store, nil, nil, nil, nil)
	scanner.lastCheckAt = time.Now().Add(-2 * scanOpencodeInterval)
	scanner.MaybeScan()

	normID := models.NormalizeSessionID(models.SourceAgentOpencode, sessID)
	sess, err := store.GetSession(normID)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatalf("session %s was not indexed from a database the process may only read", normID)
	}

	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(ocDBPath + suffix); err == nil {
			t.Errorf("scan created %s beside the foreign database", filepath.Base(ocDBPath+suffix))
		}
	}
}
