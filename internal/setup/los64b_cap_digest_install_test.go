package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// YM-450 (Los 64b), Lücke 1: the provenance digest was textutil.ContentHash, which
// lowercases. A body that changed only in case therefore hashed like the body it
// was derived from, the installer took the "same content" branch, and the change
// never reached the installation. In a bash or python body a case change is a
// different program.
func TestLos64bCaseOnlyChangeIsInstalled(t *testing.T) {
	capsDir := t.TempDir()

	bundled := los64aBundledBody(t, "telegram")
	// An installed body that differs from the bundle only in case. Here the case sits
	// in a description line and is cosmetic, but the installer cannot know that:
	// inside a script the same edit is a different program. So it is a different body.
	caseVariant := bytes.Replace(bundled, []byte("Bidirectional Telegram messaging"),
		[]byte("bidirectional telegram messaging"), 1)
	if bytes.Equal(caseVariant, bundled) {
		t.Fatal("fixture: the case variant is byte-identical to the bundle")
	}
	if _, err := capfile.Parse(caseVariant); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}

	los64aInstallBody(t, capsDir, "telegram", caseVariant)
	los64aWriteState(t, capsDir, capManifestEntry{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(caseVariant),
		Revision:        1,
	})

	_, conflicts, err := installBundledCaps(capsDir, capManifest{Caps: []capManifestEntry{{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(caseVariant),
	}}})
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("the bundle was derived from the installed body, that is no conflict: %v", conflicts)
	}

	got, err := os.ReadFile(filepath.Join(capsDir, "telegram", "CAP.md"))
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, bundled) {
		t.Error("a body that changed only in case was not installed: the digest still folds the case")
	}

	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatal("telegram lost its provenance entry")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(bundled) {
		t.Errorf("recorded hash = %s, want the hash of the newly installed body", entry.DerivedFromHash)
	}
	if entry.Revision != 2 {
		t.Errorf("revision = %d, want 2 after the install", entry.Revision)
	}
}

// The other half of the same rule: what the parser does not see is no change. A
// CRLF checkout and a trailing newline an editor added are the same body, and the
// installer must neither rewrite the file nor call it a conflict.
func TestLos64bLineEndingsAndTrailingNewlinesAreNoChange(t *testing.T) {
	capsDir := t.TempDir()
	bundledBody := los64aBundledBody(t, "telegram")
	onDisk := bytes.ReplaceAll(bundledBody, []byte("\n"), []byte("\r\n"))
	onDisk = append(onDisk, '\n')

	los64aInstallBody(t, capsDir, "telegram", onDisk)
	bundled := capManifest{Caps: []capManifestEntry{{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(bundledBody),
	}}}

	_, conflicts, err := installBundledCaps(capsDir, bundled)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("line endings are no divergent body, that is no conflict: %v", conflicts)
	}

	got, err := os.ReadFile(filepath.Join(capsDir, "telegram", "CAP.md"))
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, onDisk) {
		t.Error("a body that differed only in line endings was rewritten")
	}

	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatal("a cap that matches the bundle must be tracked from that run on")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(bundledBody) {
		t.Errorf("recorded hash = %s, want the hash of the bundled body", entry.DerivedFromHash)
	}
	if entry.Revision != 1 {
		t.Errorf("revision = %d, want 1: nothing was installed, so nothing was counted", entry.Revision)
	}
}

// The shipped manifest is what protects a cap that already sits in an installation.
// Its hashes have to be built with the digest the installer compares, otherwise
// every cap in the bundle reads as a divergent body on the first run after the
// switch. A bundle cut from a live body is the one case where an entry is not the
// digest of its own CAP.md; such a cut changes this test on purpose.
func TestLos64bShippedManifestIsDerivedFromTheBundledBodies(t *testing.T) {
	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	if len(manifest.Caps) == 0 {
		t.Fatal("bundled manifest is empty")
	}
	for _, entry := range manifest.Caps {
		body := los64aBundledBody(t, entry.Name)
		if want := textutil.BodyDigest(body); entry.DerivedFromHash != want {
			t.Errorf("%s: derived_from_hash = %s, want the body digest %s (the manifest was not regenerated after the digest changed)",
				entry.Name, entry.DerivedFromHash, want)
		}
	}
}
