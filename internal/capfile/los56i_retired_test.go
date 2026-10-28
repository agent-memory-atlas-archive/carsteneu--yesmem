package capfile

import "testing"

// YM-181 [LIVE-8] und YM-197 [DEV-DT33] (Los 56i): das Cap wiki_export ist aus
// dem Bundle verschwunden (die Pipeline liegt in "yesmem wiki-render"), aber es
// wird weiter aus einer CAP.md auf der Platte importiert und steht als aktives
// Cap-Learning im Katalog. Es schreibt als dritter Wiki-Schreiber ohne
// internal/wikidir und kann das Verzeichnis eines fremden Projekts
// ueberschreiben. Die Ausmusterung braucht eine benannte Liste.
func TestLos56i_RetiredBundledCapsNamesWikiExport(t *testing.T) {
	if !IsRetiredCap("wiki_export") {
		t.Error("wiki_export is not retired: it would be imported from disk and offered in the catalog again")
	}
	for _, name := range []string{"telegram", "cap_delete", "proxy_health", ""} {
		if IsRetiredCap(name) {
			t.Errorf("%q was reported as retired", name)
		}
	}
}
