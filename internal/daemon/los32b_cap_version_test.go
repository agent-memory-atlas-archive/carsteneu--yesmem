package daemon

import (
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
)

// los32bWriteDiskCapFile lays a CAP.md into dir/<name>/CAP.md, as the bundle install
// and a hand-kept cap directory do.
func los32bWriteDiskCapFile(t *testing.T, dir, name string, version int, body string) {
	t.Helper()
	cf := capfile.CapFile{
		Name: name, Description: "disk cap", Version: version, Scope: "user", Purpose: "P",
		Scripts: []capfile.Script{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: body, Schema: `{"type":"object"}`,
		}},
	}
	if err := capfile.WriteFile(&cf, filepath.Join(dir, name)); err != nil {
		t.Fatalf("write %s v%d: %v", name, version, err)
	}
}

// YM-264: the auto-correct pass saves a repaired script through save_cap with no
// version param, and save_cap answered that with meta.Version+1. Every repair
// therefore raised the stored version, while the bundled CAP.md keeps its own
// small number — and the sync only imports a strictly higher file version. A
// bundled fix could never reach an installation that had been auto-corrected
// once (live: proxy_health v529 against bundle v73).
//
// The fix separates the two numbers: Version stays the local revision counter,
// ImportedVersion remembers which bundled revision the row came from.
func TestHandleSaveCap_AutoCorrectKeepsTheBundleVersion(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "burning", 3, "echo old\n")
	SyncCapsFromDisk(h, dir, "")

	imported := storedCapMeta(t, s, "burning")
	if imported == nil {
		t.Fatal("cap was not imported")
	}
	if imported.Version != 3 {
		t.Fatalf("imported Version = %d, want 3", imported.Version)
	}

	// What the auto-correct pass sends: no version, one repaired body.
	params, err := autoCorrectSaveParams(nil, *imported, "run", "echo repaired\n")
	if err != nil {
		t.Fatalf("autoCorrectSaveParams: %v", err)
	}
	if resp := h.handleSaveCap(params); resp.Error != "" {
		t.Fatalf("auto-correct save: %s", resp.Error)
	}
	repaired := storedCapMeta(t, s, "burning")
	if repaired == nil {
		t.Fatal("cap disappeared after the auto-correct save")
	}
	if got := repaired.Scripts[0].Body; got != "echo repaired\n" {
		t.Fatalf("the repair did not land: body = %q", got)
	}

	// The bundled revision right after the repair. It is not newer than the
	// burned local counter, but it IS newer than what this row last imported.
	los32bWriteDiskCapFile(t, dir, "burning", 4, "echo bundled-fix\n")
	SyncCapsFromDisk(h, dir, "")

	after := storedCapMeta(t, s, "burning")
	if after == nil {
		t.Fatal("cap disappeared after the bundle sync")
	}
	if got := after.Scripts[0].Body; got != "echo bundled-fix" {
		t.Errorf("script body = %q, want the bundled fix: a repair must not burn the bundle version (stored Version %d)", got, after.Version)
	}
}

// The counterpart of the watermark: the daemon exports its own rows back to
// ~/.claude/caps, and that file carries the (bumped) local version. Without a
// content comparison the next daemon start would import its own export as a new
// bundled revision and raise the version again, one step per start.
func TestHandleSaveCap_OwnExportIsNotReimportedAsABundledRevision(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "roundtrip", 2, "echo ok\n")
	SyncCapsFromDisk(h, dir, "")

	imported := storedCapMeta(t, s, "roundtrip")
	if imported == nil {
		t.Fatal("cap was not imported")
	}
	params, err := autoCorrectSaveParams(nil, *imported, "run", "echo repaired\n")
	if err != nil {
		t.Fatalf("autoCorrectSaveParams: %v", err)
	}
	if resp := h.handleSaveCap(params); resp.Error != "" {
		t.Fatalf("auto-correct save: %s", resp.Error)
	}
	before := storedCapMeta(t, s, "roundtrip")

	// What ExportAllCaps/WriteCapToDisk puts on disk: the row's own meta,
	// carrying the locally bumped version.
	exported := CapMetaToCapFile(*before, "", ProfileStandard)
	if err := capfile.WriteFile(&exported, filepath.Join(dir, "roundtrip")); err != nil {
		t.Fatalf("export: %v", err)
	}

	SyncCapsFromDisk(h, dir, "")

	after := storedCapMeta(t, s, "roundtrip")
	if after == nil {
		t.Fatal("cap disappeared")
	}
	if after.Version != before.Version {
		t.Errorf("Version %d -> %d: the daemon's own export must not count as a newer bundled revision", before.Version, after.Version)
	}
	if got := after.Scripts[0].Body; got != "echo repaired\n" {
		t.Errorf("body = %q, want the repaired one: the export is not a new revision either", got)
	}
}
