package setup

import (
	"path/filepath"
	"testing"
)

// YM-381 (Los 11l): mit paths.archive kann das Archiv ausserhalb des
// Datenverzeichnisses liegen. uninstall --delete-data entfernt nur dataDir und
// muss sagen, was liegen bleibt. Der Test haelt fest, dass der Vergleich ein
// Pfadvergleich ist und kein Praefixvergleich: die alte Fassung haette
// "/x/yesmem/../keep" als "inside" gemeldet, obwohl es ausserhalb liegt.
func TestArchiveOutsideDataDir_Los11l(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "yesmem")

	cases := []struct {
		name    string
		archive string
		want    bool
	}{
		{"default below the data dir", filepath.Join(dataDir, "archive"), false},
		{"data dir itself", dataDir, false},
		{"traversal stays inside", filepath.Join(dataDir, "sub", "..", "archive"), false},
		{"sibling of the data dir", filepath.Join(base, "archive"), true},
		{"traversal out of the data dir", filepath.Join(dataDir, "..", "keep", "archive"), true},
		{"unrelated absolute path", filepath.Join(base, "elsewhere", "archive"), true},
	}
	for _, tc := range cases {
		if got := archiveOutsideDataDir(tc.archive, dataDir); got != tc.want {
			t.Errorf("%s: archiveOutsideDataDir(%q, %q) = %v, want %v", tc.name, tc.archive, dataDir, got, tc.want)
		}
	}
}
