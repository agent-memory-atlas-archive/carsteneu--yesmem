package setup

import (
	"fmt"
	"io"
	"log"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/carsteneu/yesmem/caps"
	"github.com/carsteneu/yesmem/internal/capfile"
)

const (
	// capManifestName is the provenance file in the caps dir.
	capManifestName = capfile.ManifestFileName
	// bundledCapManifestPath is the provenance the binary ships, one entry per
	// bundled cap (caps/bundled-caps/manifest.yaml).
	bundledCapManifestPath = "bundled-caps/manifest.yaml"
)

// The provenance file is part of the caps dir format, so its model and its parser
// live in internal/capfile: the daemon needs the same file to decide whether an
// installed body may be rolled back, and it must not import setup for that (setup
// imports the daemon). What stays here is the installer's side of the format: the
// embedded bundle, the state file of one installation, and the conflict report.
type capManifestEntry = capfile.ManifestEntry

type capManifest = capfile.Manifest

// loadBundledCapManifest reads the provenance this binary ships. The file is part
// of the build, so a missing or broken one is an error rather than a reason to
// install every cap over whatever an installation holds.
func loadBundledCapManifest() (capManifest, error) {
	data, err := caps.BundledCaps.ReadFile(bundledCapManifestPath)
	if err != nil {
		return capManifest{}, fmt.Errorf("read bundled cap manifest: %w", err)
	}
	return parseCapManifest(data)
}

// loadInstalledCapManifest reads the state of one installation. A missing file is an
// empty, usable manifest, which is what a first install has: the bundled claims then
// decide. A file that exists but cannot be read or parsed is reported and NOT marked
// usable, because the caller must not overwrite it: it may hold a reconciliation
// someone typed by hand, and that is the one file the conflict report asks them to
// edit. The run keeps working from the bundled claims either way.
func loadInstalledCapManifest(path string) (capManifest, bool) {
	data, err := capfile.ReadNoFollow(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("setup: %s: %v (using the bundled claims, file left as it is)", path, err)
		}
		return capManifest{}, true
	}
	m, err := parseCapManifest(data)
	if err != nil {
		log.Printf("setup: %s: %v (using the bundled claims, file left as it is)", path, err)
		return capManifest{}, false
	}
	return m, true
}

func parseCapManifest(data []byte) (capManifest, error) {
	return capfile.ParseManifest(data)
}

// saveInstalledCapManifest writes the state atomically and owner-only: the file
// names every cap the installation holds.
func saveInstalledCapManifest(path string, m capManifest) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("render cap manifest: %w", err)
	}
	header := "# Written by yesmem setup. Provenance of the installed caps: derived_from_hash\n" +
		"# is the body the bundled CAP.md was derived from, revision counts the installs\n" +
		"# that reached this installation (YM-450). Edit it only to reconcile a reported\n" +
		"# conflict.\n"
	return capfile.WriteOwnerOnly(path, append([]byte(header), data...))
}

// CapConflict is a bundled cap that was deliberately not installed: the bundle was
// not derived from the body the installation holds, so writing it would throw away
// whatever the installation did after the bundle was cut (YM-450).
type CapConflict struct {
	Name          string
	ManifestHash  string
	InstalledHash string
	// Untracked marks a cap nothing vouches for: no entry in the installation's
	// manifest and none in the bundle, while a body sits on disk.
	Untracked bool
	// Unparseable marks an installed body that does not parse at all. It is
	// reported, not replaced: no bundle was derived from a body nobody can read.
	Unparseable bool
}

// String renders the conflict as one line that names the way out. Reconciling the
// bundle alone does not end it: the next run compares against the same claim, so
// the line also names the hash to record. Without that, a conflict would be the
// silent inertness it replaced, only louder.
func (c CapConflict) String() string {
	var msg string
	if c.Untracked {
		msg = fmt.Sprintf("cap %s: no manifest entry vouches for this bundle, the installed body hashes to %s; reconcile the bundle with the installed body (caps/bundled-caps/%s/CAP.md), then record derived_from_hash: %s in the manifest.yaml next to the installed caps",
			c.Name, c.InstalledHash, c.Name, c.InstalledHash)
	} else {
		msg = fmt.Sprintf("cap %s: the bundle was derived from %s, the installed body hashes to %s; reconcile the bundle with the installed body (caps/bundled-caps/%s/CAP.md), then record derived_from_hash: %s in the manifest.yaml next to the installed caps",
			c.Name, c.ManifestHash, c.InstalledHash, c.Name, c.InstalledHash)
	}
	if c.Unparseable {
		msg += " (the installed body does not parse)"
	}
	return msg
}

// PrintCapConflicts writes one line per conflict. No output means every cap either
// matched the bundle or had nothing to compare against; a conflict is never silent.
func PrintCapConflicts(w io.Writer, conflicts []CapConflict) {
	if len(conflicts) == 0 {
		return
	}
	fmt.Fprintf(w, "  Cap conflicts: %d, the installed body was kept:\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(w, "    %s\n", c)
	}
}
