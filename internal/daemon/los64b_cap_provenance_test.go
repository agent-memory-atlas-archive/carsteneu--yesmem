package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// los64bWriteManifest seeds the provenance file of a caps dir, the one the installer
// writes: name -> the body digest this installation holds.
func los64bWriteManifest(t *testing.T, capsDir, name, digest string) {
	t.Helper()
	body := "caps:\n  - name: " + name + "\n    derived_from_hash: " + digest + "\n    revision: 1\n"
	if err := os.WriteFile(filepath.Join(capsDir, capfile.ManifestFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write caps manifest: %v", err)
	}
}

func los64bInstalledBody(t *testing.T, capsDir, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(capsDir, name, "CAP.md"))
	if err != nil {
		t.Fatalf("read installed %s: %v", name, err)
	}
	return body
}

// los64bCaptureLog collects what the daemon logs during fn, so a conflict can be
// asserted where it is reported instead of where it happens to be returned.
func los64bCaptureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// YM-450 (Los 64b), Lücke 2b: the installer delivers a bundle the row's counter is
// behind, and the next daemon start writes the row back over it. The row is the older
// state here: the installation's own manifest names the file that sits on disk, so the
// export has nothing to say about it (live: telegram bundled v195 against a row of
// v604, so every delivered improvement was undone at the following start).
func TestLos64bExportKeepsTheInstalledBundle(t *testing.T) {
	h, _ := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 602, "echo live\n")
	SyncCapsFromDisk(h, dir, "")

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	bundle := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(bundle))

	logged := los64bCaptureLog(t, func() { ExportAllCaps(h, dir) })

	if got := los64bInstalledBody(t, dir, "sturdy"); !bytes.Equal(got, bundle) {
		t.Fatalf("the export rolled the installed bundle back:\n%s", got)
	}
	// The row is the older state here, and the step back is never taken silently.
	if !strings.Contains(logged, "keep the delivered body") || !strings.Contains(logged, "sturdy") {
		t.Errorf("the kept delivery is not reported:\n%s", logged)
	}
}

// Lücke 2a and 2b together, in the order the daemon runs them at startup: the import
// refuses the file as an older source revision, so the row never follows the bundle,
// and the export then writes the row over it. Both steps have to take the provenance
// into account, or the install is undone twice over.
func TestLos64bStartupKeepsTheInstalledBundle(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 602, "echo live\n")
	SyncCapsFromDisk(h, dir, "")

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	bundle := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(bundle))

	SyncCapsFromDisk(h, dir, "")
	ExportAllCaps(h, dir)

	meta := storedCapMeta(t, s, "sturdy")
	if meta == nil {
		t.Fatal("cap disappeared from the store")
	}
	if body := strings.TrimRight(meta.Scripts[0].Body, "\n"); body != "echo bundle" {
		t.Errorf("the row did not follow the installed bundle: body = %q (stored Version %d)", body, meta.Version)
	}
	if meta.ImportedVersion != 195 {
		t.Errorf("ImportedVersion = %d, want 195: the row took the body it was given", meta.ImportedVersion)
	}
	if got := los64bInstalledBody(t, dir, "sturdy"); !bytes.Equal(got, bundle) {
		t.Fatalf("the export rolled the installed bundle back:\n%s", got)
	}
}

// The repair path stays open, and this is its guard: a row that carries a repair on
// top of the body it imported is the later state, so the export writes it even though
// the file still is the delivered bundle and the manifest names it.
func TestLos64bExportWritesARepairOnTopOfTheInstalledBundle(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	SyncCapsFromDisk(h, dir, "")
	bundle := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(bundle))

	// What the auto-correct pass sends: no version, one repaired body.
	imported := storedCapMeta(t, s, "sturdy")
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

	ExportAllCaps(h, dir)

	got := string(los64bInstalledBody(t, dir, "sturdy"))
	if !strings.Contains(got, "echo repaired") {
		t.Errorf("the repair did not reach the disk, the installed bundle shadowed it:\n%s", got)
	}
}

// A save that carries a revision is not a file import, so it must not reset the row's
// memory of what its last import brought. Otherwise the repair reads as a clean import
// and the next delivery overwrites it: the reviewer's repro, and the path
// recipes.md recommends ("make this body official").
func TestLos64bVersionedRepairOnADeliveredBundleSurvives(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	SyncCapsFromDisk(h, dir, "")
	bundle := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(bundle))

	imported := storedCapMeta(t, s, "sturdy")
	if imported == nil {
		t.Fatal("cap was not imported")
	}
	params, err := autoCorrectSaveParams(nil, *imported, "run", "echo repaired\n")
	if err != nil {
		t.Fatalf("autoCorrectSaveParams: %v", err)
	}
	params["version"] = float64(imported.Version + 1)
	if resp := h.handleSaveCap(params); resp.Error != "" {
		t.Fatalf("versioned save: %s", resp.Error)
	}

	SyncCapsFromDisk(h, dir, "")
	ExportAllCaps(h, dir)

	meta := storedCapMeta(t, s, "sturdy")
	if meta == nil {
		t.Fatal("cap disappeared from the store")
	}
	if body := strings.TrimRight(meta.Scripts[0].Body, "\n"); body != "echo repaired" {
		t.Errorf("the delivery overwrote a versioned repair: body = %q", body)
	}
	if got := string(los64bInstalledBody(t, dir, "sturdy")); !strings.Contains(got, "echo repaired") {
		t.Errorf("the repair did not reach the disk:\n%s", got)
	}
}

// The export guards a repaired row against its counter, and the import has to guard it
// too: a delivery must not overwrite an edit the row collected after its import. The
// refusal is reported, not silent.
func TestLos64bImportDoesNotOverwriteARepairedRow(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 602, "echo live\n")
	SyncCapsFromDisk(h, dir, "")
	imported := storedCapMeta(t, s, "sturdy")
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

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	bundle := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(bundle))

	logged := los64bCaptureLog(t, func() { SyncCapsFromDisk(h, dir, "") })

	meta := storedCapMeta(t, s, "sturdy")
	if meta == nil {
		t.Fatal("cap disappeared from the store")
	}
	if body := strings.TrimRight(meta.Scripts[0].Body, "\n"); body != "echo repaired" {
		t.Errorf("the delivery overwrote a repair that was made after the import: body = %q", body)
	}
	if !strings.Contains(logged, "repair") {
		t.Errorf("the refused delivery is not reported:\n%s", logged)
	}
}

// A delivery above the row's watermark takes the file, and the export must then leave
// those bytes alone. Rewriting them breaks the manifest's claim (the file no longer
// hashes to its entry), and the next release is only reported as a conflict. The
// reviewer measured exactly that on the first cut of this gate: a metadata save raised
// the row's counter, the delivery at that counter was taken, and the export rewrote the
// file one revision higher.
func TestLos64bForwardDeliveryKeepsTheFileBytes(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo bundle\n")
	SyncCapsFromDisk(h, dir, "")

	// A metadata save: the body stays, the counter moves.
	if resp := h.handleSaveCap(map[string]any{
		"name":        "sturdy",
		"description": "disk cap, edited",
		"tags":        "a,b",
	}); resp.Error != "" {
		t.Fatalf("metadata save: %s", resp.Error)
	}
	if meta := storedCapMeta(t, s, "sturdy"); meta == nil || meta.Version <= 195 {
		t.Fatalf("the metadata save did not raise the counter: %+v", meta)
	}

	// The next release, one revision above the row's watermark.
	los32bWriteDiskCapFile(t, dir, "sturdy", 196, "echo bundle-196\n")
	delivered := los64bInstalledBody(t, dir, "sturdy")
	los64bWriteManifest(t, dir, "sturdy", textutil.BodyDigest(delivered))

	SyncCapsFromDisk(h, dir, "")
	ExportAllCaps(h, dir)

	if got := los64bInstalledBody(t, dir, "sturdy"); !bytes.Equal(got, delivered) {
		t.Fatalf("the export rewrote the delivered body:\n%s", got)
	}
	meta := storedCapMeta(t, s, "sturdy")
	if meta == nil {
		t.Fatal("cap disappeared from the store")
	}
	if body := strings.TrimRight(meta.Scripts[0].Body, "\n"); body != "echo bundle-196" {
		t.Errorf("the row did not take the newer delivery: body = %q", body)
	}
}

// The other side of the same rule: a file the manifest does not name stays a leftover
// copy. The row keeps its state and the refusal names both digests, so an operator can
// tell a stale copy from a lost install.
func TestLos64bImportKeepsTheRowAndReportsAnUnvouchedBody(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 602, "echo live\n")
	SyncCapsFromDisk(h, dir, "")

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo leftover\n")
	leftover := textutil.BodyDigest(los64bInstalledBody(t, dir, "sturdy"))
	other := textutil.BodyDigest([]byte("a body nobody installed\n"))
	los64bWriteManifest(t, dir, "sturdy", other)

	logged := los64bCaptureLog(t, func() { SyncCapsFromDisk(h, dir, "") })

	meta := storedCapMeta(t, s, "sturdy")
	if meta == nil {
		t.Fatal("cap disappeared from the store")
	}
	if body := strings.TrimRight(meta.Scripts[0].Body, "\n"); body != "echo live" {
		t.Errorf("an unvouched body below the watermark was imported: body = %q", body)
	}
	for _, want := range []string{"sturdy", leftover, other} {
		if !strings.Contains(logged, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, logged)
		}
	}
}

// And the export side of the report: here the row does win on its counter, because the
// file is not the body the installation recorded. That decision is legitimate, but it
// may not pass without a word: the line names what the file hashes to.
func TestLos64bExportReportsAFileTheManifestDoesNotName(t *testing.T) {
	h, _ := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "sturdy", 602, "echo live\n")
	SyncCapsFromDisk(h, dir, "")

	los32bWriteDiskCapFile(t, dir, "sturdy", 195, "echo hand-edited\n")
	handEdited := textutil.BodyDigest(los64bInstalledBody(t, dir, "sturdy"))
	recorded := textutil.BodyDigest([]byte("the body the installer delivered\n"))
	los64bWriteManifest(t, dir, "sturdy", recorded)

	logged := los64bCaptureLog(t, func() { ExportAllCaps(h, dir) })

	if got := textutil.BodyDigest(los64bInstalledBody(t, dir, "sturdy")); got == handEdited {
		t.Error("the row is the later state here (v602 against a file of v195); the export was expected to write it")
	}
	for _, want := range []string{"sturdy", handEdited, recorded} {
		if !strings.Contains(logged, want) {
			t.Errorf("the overwrite does not name %q:\n%s", want, logged)
		}
	}
}
