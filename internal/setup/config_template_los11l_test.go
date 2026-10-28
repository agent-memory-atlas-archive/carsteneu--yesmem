package setup

import (
	"strings"
	"testing"
)

// YM-381 (Los 11l): die Vorlage lieferte paths.bleve_index mit, aber kein
// Produktionspfad liest den Wert. Die Suche laeuft seit der FTS5-Umstellung in
// SQLite (storage/messages.go:238), "yesmem migrate" raeumt das alte
// bleve-index-Verzeichnis sogar weg (cmd_migrate_messages.go). Ein Knopf ohne
// Leser ist ein Versprechen ohne Deckung, deshalb faellt der Schluessel aus der
// Vorlage. paths.db und paths.archive bleiben: beide haben Leser.
func TestConfigTemplateDropsBleveIndex_Los11l(t *testing.T) {
	cfgStr := generateConfig("deepseek-v4-flash", true, "sk-ant", "", "", "openai_compatible", "ghostty", "narrative", "quality", "summarize")

	if strings.Contains(cfgStr, "bleve_index") {
		t.Errorf("setup template still offers paths.bleve_index, but no production path reads it")
	}
	for _, key := range []string{"  db:", "  archive:", "  claude_projects:", "  opencode_db:"} {
		if !strings.Contains(cfgStr, key) {
			t.Errorf("setup template lost the paths entry %q, which does have a reader", strings.TrimSpace(key))
		}
	}
}
