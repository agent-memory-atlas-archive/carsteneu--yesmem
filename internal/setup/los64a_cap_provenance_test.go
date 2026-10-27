package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/caps"
	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// los64aBundledBody returns the CAP.md this binary ships for name.
func los64aBundledBody(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := caps.BundledCaps.ReadFile("bundled-caps/" + name + "/CAP.md")
	if err != nil {
		t.Fatalf("read bundled %s: %v", name, err)
	}
	return raw
}

// los64aOtherBody renders the bundled cap with a different purpose and version, the
// shape a locally revised body has: parseable, related, but not the bundled bytes.
func los64aOtherBody(t *testing.T, name, purpose string, version int) []byte {
	t.Helper()
	cf, err := capfile.Parse(los64aBundledBody(t, name))
	if err != nil {
		t.Fatalf("parse bundled %s: %v", name, err)
	}
	cf.Purpose = purpose
	cf.Version = version
	rendered := capfile.Render(cf)
	if _, err := capfile.Parse(rendered); err != nil {
		t.Fatalf("rendered fixture for %s does not parse: %v", name, err)
	}
	return rendered
}

// los64aInstallBody puts body at <capsDir>/<name>/CAP.md and returns the path.
func los64aInstallBody(t *testing.T, capsDir, name string, body []byte) string {
	t.Helper()
	dir := filepath.Join(capsDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "CAP.md")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// los64aWriteState seeds the installer's provenance file for the caps dir.
func los64aWriteState(t *testing.T, capsDir string, entries ...capManifestEntry) {
	t.Helper()
	if err := saveInstalledCapManifest(filepath.Join(capsDir, capManifestName), capManifest{Caps: entries}); err != nil {
		t.Fatalf("write state manifest: %v", err)
	}
}

func los64aReadState(t *testing.T, capsDir string) capManifest {
	t.Helper()
	m, usable := loadInstalledCapManifest(filepath.Join(capsDir, capManifestName))
	if !usable {
		t.Fatalf("state manifest is not usable")
	}
	return m
}

func los64aConflict(t *testing.T, conflicts []CapConflict, name string) CapConflict {
	t.Helper()
	for _, c := range conflicts {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no conflict reported for %s, got %d conflicts", name, len(conflicts))
	return CapConflict{}
}

// YM-450 (Los 64a), key (a): a cap that appears in the bundle without a manifest
// entry and without an installed body has nothing to compare against. It is
// installed — that is how a new cap reaches an installation — and it is tracked from
// that body on, so the next bundle is judged against it instead of against nothing.
// A body already sitting there is the other half of this rule and stays (see
// TestLos64aUntrackedCapDoesNotReplaceAnInstalledBody).
func TestLos64aUntrackedCapIsInstalledAndTracked(t *testing.T) {
	capsDir := t.TempDir()

	// An empty manifest is the state of a bundle that lists none of its caps.
	installed, conflicts, err := installBundledCaps(capsDir, capManifest{})
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("an untracked cap is not a conflict, got %v", conflicts)
	}
	if installed == 0 {
		t.Fatalf("installed = 0, want the bundled caps written")
	}

	want := los64aBundledBody(t, "telegram")
	got, err := os.ReadFile(filepath.Join(capsDir, "telegram", "CAP.md"))
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the bundled body was not installed")
	}

	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatalf("no provenance entry was recorded for telegram: the cap stays untracked")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(want) {
		t.Errorf("recorded hash = %s, want the hash of the installed body %s", entry.DerivedFromHash, textutil.BodyDigest(want))
	}
	if entry.Revision != 1 {
		t.Errorf("revision = %d, want 1 for a first install", entry.Revision)
	}
}

// YM-450, key (b): the installed body is the one the bundle was derived from — the
// live cap sits at revision 603 while the bundle carries 195. Provenance decides,
// not the counter: the bundle is installed and the revision is counted up.
func TestLos64aProvenanceBeatsTheInstalledRevision(t *testing.T) {
	live := los64aOtherBody(t, "telegram", "live revision 603", 603)
	capsDir := t.TempDir()
	los64aInstallBody(t, capsDir, "telegram", live)
	los64aWriteState(t, capsDir, capManifestEntry{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(live),
		Revision:        41,
	})

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("a bundle derived from the installed body is no conflict, got %v", conflicts)
	}

	want := los64aBundledBody(t, "telegram")
	got, err := os.ReadFile(filepath.Join(capsDir, "telegram", "CAP.md"))
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the bundle was not installed over the body it was derived from")
	}
	// The written body carries the bundle's revision, not the higher live one: the
	// counter no longer gates anything.
	cf, err := capfile.Parse(got)
	if err != nil {
		t.Fatalf("parse installed telegram: %v", err)
	}
	bundledCF, err := capfile.Parse(want)
	if err != nil {
		t.Fatalf("parse bundled telegram: %v", err)
	}
	if cf.Version != bundledCF.Version {
		t.Errorf("installed version = %d, want the bundled %d", cf.Version, bundledCF.Version)
	}

	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatalf("no provenance entry for telegram after the install")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(want) {
		t.Errorf("recorded hash = %s, want the hash of the newly installed body", entry.DerivedFromHash)
	}
	if entry.Revision != 42 {
		t.Errorf("revision = %d, want 42 (41 counted up)", entry.Revision)
	}
}

// YM-450, key (c): a body the bundle was NOT derived from is left alone and
// reported. Neither hash is guessable from the message alone, so both are named
// and the report says what to do. This is the case that was a silent skip before.
func TestLos64aDivergentBodyIsKeptAndReported(t *testing.T) {
	live := los64aOtherBody(t, "telegram", "a body the bundle knows nothing about", 602)
	capsDir := t.TempDir()
	path := los64aInstallBody(t, capsDir, "telegram", live)
	// A CAP.md installed before the owner-only hardening still gets its mode fixed,
	// even on the branch that keeps it.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod installed cap: %v", err)
	}

	// The bundle claims to descend from a body this installation no longer holds.
	older := los64aOtherBody(t, "telegram", "the body the bundle was cut from", 400)
	manifest := capManifest{Caps: []capManifestEntry{{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(older),
		Revision:        0,
	}}}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}

	conflict := los64aConflict(t, conflicts, "telegram")
	if conflict.ManifestHash != textutil.BodyDigest(older) {
		t.Errorf("manifest hash = %s, want %s", conflict.ManifestHash, textutil.BodyDigest(older))
	}
	if conflict.InstalledHash != textutil.BodyDigest(live) {
		t.Errorf("installed hash = %s, want %s", conflict.InstalledHash, textutil.BodyDigest(live))
	}
	msg := conflict.String()
	if !strings.Contains(msg, conflict.ManifestHash) || !strings.Contains(msg, conflict.InstalledHash) {
		t.Errorf("conflict message names no hash pair: %q", msg)
	}
	if !strings.Contains(msg, "reconcile the bundle with the installed body") {
		t.Errorf("conflict message gives no way out: %q", msg)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, live) {
		t.Fatalf("a body the bundle was not derived from was replaced")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed cap: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after the conflict = %04o, want 0600", perm)
	}
}

// YM-450, key (d): the comparison runs through the content hash, which normalizes
// whitespace. A body that differs only in line endings and indentation is the same
// body — it must neither be reported nor rewritten.
func TestLos64aFormattingDifferenceIsNoConflict(t *testing.T) {
	reformatted := bytes.ReplaceAll(los64aBundledBody(t, "telegram"), []byte("\n"), []byte("\r\n"))
	capsDir := t.TempDir()
	path := los64aInstallBody(t, capsDir, "telegram", reformatted)

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("a line-ending difference is no conflict, got %v", conflicts)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, reformatted) {
		t.Fatalf("the installer rewrote a body that only differs in line endings")
	}
	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatalf("no provenance entry for telegram")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(reformatted) {
		t.Errorf("recorded hash = %s, want %s", entry.DerivedFromHash, textutil.BodyDigest(reformatted))
	}
}

// YM-450, key (e): a body that does not parse cannot have a bundle derived from it,
// so the install is refused and the case is reported. It must not fail the run and
// must not be replaced behind the operator's back.
func TestLos64aUnparseableInstalledBodyIsReported(t *testing.T) {
	broken := []byte("stale copy without frontmatter\n")
	capsDir := t.TempDir()
	path := los64aInstallBody(t, capsDir, "telegram", broken)

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("an unparseable installed body must not fail the install: %v", err)
	}

	conflict := los64aConflict(t, conflicts, "telegram")
	if !conflict.Unparseable {
		t.Errorf("conflict does not mark the installed body as unparseable")
	}
	if !strings.Contains(conflict.String(), "does not parse") {
		t.Errorf("conflict message does not say the body does not parse: %q", conflict.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, broken) {
		t.Fatalf("an installed body no bundle was derived from was silently replaced")
	}
}

// The shipped manifest is the whole protection for caps that already sit in an
// installation. A cap dir it does not name falls back to the fresh-install rule:
// installed where nothing sits, reported where a body does.
func TestLos64aShippedManifestCoversEveryBundledCap(t *testing.T) {
	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	if len(manifest.Caps) == 0 {
		t.Fatalf("bundled manifest is empty")
	}

	entries, err := caps.BundledCaps.ReadDir("bundled-caps")
	if err != nil {
		t.Fatalf("read bundled caps: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if manifest.Find(entry.Name()) == nil {
			t.Errorf("%s ships without a manifest entry: an installation carries no evidence for it", entry.Name())
		}
	}
	for _, entry := range manifest.Caps {
		if len(entry.DerivedFromHash) != 64 || strings.Trim(entry.DerivedFromHash, "0123456789abcdef") != "" {
			t.Errorf("%s: derived_from_hash %q is not a sha256 hex digest", entry.Name, entry.DerivedFromHash)
		}
		if _, err := caps.BundledCaps.ReadFile("bundled-caps/" + entry.Name + "/CAP.md"); err != nil {
			t.Errorf("%s: manifest names a cap the bundle does not ship: %v", entry.Name, err)
		}
	}
}

// The report is the whole point of the conflict branch: a run that installs
// nothing must say so, once per cap and with both hashes in the line.
func TestLos64aConflictReportIsOneLinePerCap(t *testing.T) {
	var quiet strings.Builder
	PrintCapConflicts(&quiet, nil)
	if quiet.Len() != 0 {
		t.Fatalf("no conflicts but output: %q", quiet.String())
	}

	conflicts := []CapConflict{
		{Name: "telegram", ManifestHash: "aa11", InstalledHash: "bb22"},
		{Name: "cap_search", ManifestHash: "cc33", InstalledHash: "dd44", Unparseable: true},
	}
	var report strings.Builder
	PrintCapConflicts(&report, conflicts)

	lines := strings.Split(strings.TrimSuffix(report.String(), "\n"), "\n")
	if len(lines) != 1+len(conflicts) {
		t.Fatalf("report = %d lines, want a header plus one per conflict:\n%s", len(lines), report.String())
	}
	if !strings.Contains(lines[0], "Cap conflicts: 2") {
		t.Errorf("header does not name the conflict count: %q", lines[0])
	}
	for i, c := range conflicts {
		line := lines[i+1]
		if !strings.Contains(line, c.Name) || !strings.Contains(line, c.ManifestHash) || !strings.Contains(line, c.InstalledHash) {
			t.Errorf("line %d does not name cap and both hashes: %q", i+1, line)
		}
	}
}

// setup runs on every install and every migration, so a second run must find
// nothing to do: no writes, no conflict, and above all no revision inflation that
// would make the counter meaningless.
func TestLos64aSecondRunFindsNothingToDo(t *testing.T) {
	capsDir := t.TempDir()
	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	if _, _, err := installBundledCaps(capsDir, manifest); err != nil {
		t.Fatalf("first installBundledCaps: %v", err)
	}
	statePath := filepath.Join(capsDir, capManifestName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state manifest: %v", err)
	}

	installed, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("second installBundledCaps: %v", err)
	}
	if installed != 0 {
		t.Errorf("second run installed %d caps, want 0", installed)
	}
	if len(conflicts) != 0 {
		t.Errorf("second run reported conflicts: %v", conflicts)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state manifest after the second run: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a run with nothing to do rewrote the provenance file:\n%s\n---\n%s", before, after)
	}
	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil || entry.Revision != 1 {
		t.Fatalf("telegram entry after two runs = %+v, want revision 1", entry)
	}
}

// The installation's own entry wins over the bundled one: after a repair in the
// installation, the state is the later truth, and the bundled claim is a seed for
// installations that have none (the YM-446 case: the row was repaired after the
// import, the file on disk is the repair).
func TestLos64aInstalledManifestWinsOverTheBundledClaim(t *testing.T) {
	live := los64aOtherBody(t, "telegram", "repaired in the installation", 604)
	older := los64aOtherBody(t, "telegram", "a body the installation no longer holds", 600)
	capsDir := t.TempDir()
	path := los64aInstallBody(t, capsDir, "telegram", live)
	los64aWriteState(t, capsDir, capManifestEntry{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(older),
		Revision:        7,
	})

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}

	conflict := los64aConflict(t, conflicts, "telegram")
	if conflict.ManifestHash != textutil.BodyDigest(older) {
		t.Errorf("manifest hash = %s, want the installation's own claim %s, not the bundled one",
			conflict.ManifestHash, textutil.BodyDigest(older))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, live) {
		t.Fatalf("the installed body was replaced although its own manifest does not vouch for the bundle")
	}
}

// A cap the bundle added under a name an installation already uses: nothing vouches
// for the bundle there, so the local body stays and the case is reported. Without
// this, a new bundled cap would silently overwrite a same-named local cap. The
// shipped manifest is used on purpose: its entry carries the bundle's own hash, and
// that is no claim either, so the report must not pretend the bundle descends from a
// body it was never cut from.
func TestLos64aUntrackedCapDoesNotReplaceAnInstalledBody(t *testing.T) {
	local := los64aOtherBody(t, "telegram", "a local cap carrying the same name", 12)
	capsDir := t.TempDir()
	path := los64aInstallBody(t, capsDir, "telegram", local)

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}

	conflict := los64aConflict(t, conflicts, "telegram")
	if !conflict.Untracked {
		t.Errorf("conflict does not mark the cap as untracked")
	}
	if conflict.ManifestHash != "" {
		t.Errorf("manifest hash = %q, want none: no entry vouches for this bundle", conflict.ManifestHash)
	}
	if conflict.InstalledHash != textutil.BodyDigest(local) {
		t.Errorf("installed hash = %s, want %s", conflict.InstalledHash, textutil.BodyDigest(local))
	}
	msg := conflict.String()
	if !strings.Contains(msg, "no manifest entry") || !strings.Contains(msg, conflict.InstalledHash) {
		t.Errorf("untracked conflict message names neither the gap nor the hash: %q", msg)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, local) {
		t.Fatalf("a local cap was overwritten by a bundle that carries no provenance for it")
	}
}

// The bundled claim is the evidence for an installation that has no manifest of its
// own: a machine that still holds the body the bundle descends from takes the
// update. The shipped file carries self-hashes today, so this branch only becomes
// reachable once an author records the live hash of the body they derived from,
// which is exactly what this test stands in for.
func TestLos64aBundledClaimInstallsTheSuccessor(t *testing.T) {
	live := los64aOtherBody(t, "telegram", "the body the bundle was cut from", 500)
	capsDir := t.TempDir()
	los64aInstallBody(t, capsDir, "telegram", live)

	manifest := capManifest{Caps: []capManifestEntry{{
		Name:            "telegram",
		DerivedFromHash: textutil.BodyDigest(live),
		Revision:        0,
	}}}
	_, conflicts, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("the bundle descends from the installed body, so there is no conflict, got %v", conflicts)
	}

	want := los64aBundledBody(t, "telegram")
	got, err := os.ReadFile(filepath.Join(capsDir, "telegram", "CAP.md"))
	if err != nil {
		t.Fatalf("read installed telegram: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("an installation that held the body the bundle descends from was not updated")
	}
	entry := los64aReadState(t, capsDir).Find("telegram")
	if entry == nil {
		t.Fatalf("no provenance entry for telegram after the install")
	}
	if entry.DerivedFromHash != textutil.BodyDigest(want) {
		t.Errorf("recorded hash = %s, want the hash of the installed body", entry.DerivedFromHash)
	}
}

// The provenance file names every installed cap, so it is owner-only like the CAP.md
// files beside it.
func TestLos64aProvenanceFileIsOwnerOnly(t *testing.T) {
	capsDir := t.TempDir()
	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	if _, _, err := installBundledCaps(capsDir, manifest); err != nil {
		t.Fatalf("installBundledCaps: %v", err)
	}
	info, err := os.Stat(filepath.Join(capsDir, capManifestName))
	if err != nil {
		t.Fatalf("stat state manifest: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("manifest mode = %04o, want 0600", perm)
	}
}

// A link planted at the provenance path must not receive the file: the read refuses
// it, the run carries on with the bundled claims, and the target stays untouched
// (the same rule the CAP.md path has followed since YM-359).
func TestLos64aProvenanceFileRefusesASymlink(t *testing.T) {
	home := t.TempDir()
	capsDir := filepath.Join(home, "caps")
	if err := os.MkdirAll(capsDir, 0o700); err != nil {
		t.Fatalf("mkdir caps dir: %v", err)
	}
	target := filepath.Join(home, "outside.yaml")
	original := []byte("keep me\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("write link target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(capsDir, capManifestName)); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	installed, _, err := installBundledCaps(capsDir, manifest)
	if err != nil {
		t.Fatalf("a planted link must not fail the install run: %v", err)
	}
	if installed == 0 {
		t.Fatalf("installed = 0, want the caps written although the provenance path is a link")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read link target: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("the bundled manifest was written through the link: %q", got)
	}
}

// The provenance file is hand-editable and its values are printed. Anything but a cap
// name and a sha256 digest is refused at the parse boundary, so a value cannot carry
// its own line break into the conflict report.
func TestLos64aManifestRefusesValuesThatAreNoNameOrHash(t *testing.T) {
	for _, body := range []string{
		"caps:\n  - name: telegram\n    derived_from_hash: \"deadbeef\\n  Cap conflicts: 0\"\n",
		"caps:\n  - name: \"telegram\\u001b[31m\"\n    derived_from_hash: " + textutil.BodyDigest([]byte("x")) + "\n",
		"caps:\n  - name: telegram\n    derived_from_hash: \"\"\n",
	} {
		if _, err := parseCapManifest([]byte(body)); err == nil {
			t.Errorf("manifest accepted as a value what is no name or no hash:\n%s", body)
		}
	}
}

// A provenance file that does not parse is left as it is: the report tells the
// operator to edit exactly this file, so overwriting it would destroy the
// reconciliation it may hold.
func TestLos64aBrokenProvenanceFileIsLeftAlone(t *testing.T) {
	capsDir := t.TempDir()
	manifest, err := loadBundledCapManifest()
	if err != nil {
		t.Fatalf("load bundled manifest: %v", err)
	}
	if _, _, err := installBundledCaps(capsDir, manifest); err != nil {
		t.Fatalf("first installBundledCaps: %v", err)
	}

	path := filepath.Join(capsDir, capManifestName)
	broken := []byte("caps:\n  - name: telegram\n    derived_from_hash: reconciled-by-hand\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatalf("write broken manifest: %v", err)
	}
	if _, _, err := installBundledCaps(capsDir, manifest); err != nil {
		t.Fatalf("a broken provenance file must not fail the run: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state manifest: %v", err)
	}
	if !bytes.Equal(got, broken) {
		t.Fatalf("the provenance file was rewritten although it did not parse:\n%s", got)
	}
}
