package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-381 (Los 11l): paths.archive macht den Ablageort konfigurierbar, und damit
// greift die 0700-Haertung aus setup.go nicht mehr, die nur dataDir/archive kennt.
// Der Archivbaum traegt Projektpfade und Session-IDs in den Ordnernamen, ist also
// selbst Teil des Schutzes (los24d: die Kopie ist 0600).
func TestEnsureBaseDirCreatesOwnerOnlyBase_Los11l(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nested", "archive")

	if err := EnsureBaseDir(base); err != nil {
		t.Fatalf("EnsureBaseDir: %v", err)
	}
	assertOwnerOnly(t, base)

	src := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := New(base).ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	assertOwnerOnly(t, filepath.Join(base, "proj", "session.jsonl"))
}

// Ein vorhandenes Basisverzeichnis wird gemeldet, nicht veraendert: ein
// konfigurierter Pfad kann ein Verzeichnis sein, das yesmem nicht angelegt hat, und
// ein stilles chmod auf ein geteiltes Ziel waere schlimmer als eine Warnung.
func TestEnsureBaseDirLeavesExistingModeAlone_Los11l(t *testing.T) {
	base := filepath.Join(t.TempDir(), "archive")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatalf("seed mode: %v", err)
	}

	if err := EnsureBaseDir(base); err != nil {
		t.Fatalf("EnsureBaseDir: %v", err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("base mode = %04o, want the existing 0755 left alone", mode)
	}
}
