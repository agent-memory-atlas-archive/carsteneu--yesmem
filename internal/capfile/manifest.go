package capfile

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ManifestFileName is the provenance file of a caps directory. It is not a cap dir,
// so the scanner ignores it: a cap is a directory carrying a CAP.md, and this file
// sits next to those directories, not inside one.
//
// The file exists in two places with the same shape: the bundle ships one at
// caps/bundled-caps/manifest.yaml, and every installation keeps its own next to the
// installed caps. The bundle's entries are the bodies its caps were cut from; an
// installation's entries are the bodies the installer delivered to it. Both are read
// with the parser below, so the installer and the daemon agree on what the file says
// (YM-450).
const ManifestFileName = "manifest.yaml"

// manifestHashPattern is the only shape a provenance hash has. The value reaches a
// terminal line, and the file is hand-editable, so anything else is refused at the
// parse boundary instead of being formatted into the report.
var manifestHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxCapNameLen is the length bound a cap name has everywhere else
// (storage.ValidateCapName). namePattern alone does not carry it.
const maxCapNameLen = 64

// ManifestEntry is the evidence for one cap: the body this stand was derived from,
// and how many installs reached the installation it describes.
type ManifestEntry struct {
	Name            string `yaml:"name"`
	DerivedFromHash string `yaml:"derived_from_hash"`
	Revision        int    `yaml:"revision"`
}

type Manifest struct {
	Caps []ManifestEntry `yaml:"caps"`
}

func (m Manifest) Find(name string) *ManifestEntry {
	for i := range m.Caps {
		if m.Caps[i].Name == name {
			return &m.Caps[i]
		}
	}
	return nil
}

// Record notes that the installation holds hash from now on and counts one install.
// A body that is already recorded keeps its revision: an install run that finds
// nothing to do must not inflate the counter. It reports whether the manifest
// changed.
func (m *Manifest) Record(name, hash string) bool {
	for i := range m.Caps {
		if m.Caps[i].Name != name {
			continue
		}
		if m.Caps[i].DerivedFromHash == hash {
			return false
		}
		m.Caps[i].DerivedFromHash = hash
		m.Caps[i].Revision++
		return true
	}
	m.Caps = append(m.Caps, ManifestEntry{Name: name, DerivedFromHash: hash, Revision: 1})
	return true
}

// ParseManifest reads a provenance file. Both values reach a terminal line and the
// file can be edited by hand: the name alphabet is the one a cap directory already
// has, the hash has exactly one shape, and everything else is refused here rather
// than printed.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse cap manifest: %w", err)
	}
	seen := make(map[string]bool, len(m.Caps))
	for _, e := range m.Caps {
		if e.Name == "" {
			return Manifest{}, fmt.Errorf("cap manifest: entry without a name")
		}
		// The alphabet is the parser's own, and the length is the one a cap name has
		// everywhere else (storage.ValidateCapName). Both keep a value out of the report
		// line that could not be a cap and could not be printed.
		if !namePattern.MatchString(e.Name) || len(e.Name) > maxCapNameLen {
			return Manifest{}, fmt.Errorf("cap manifest: %q is not a cap name", e.Name)
		}
		if !manifestHashPattern.MatchString(e.DerivedFromHash) {
			return Manifest{}, fmt.Errorf("cap manifest: %s: derived_from_hash %q is not a sha256 hex digest", e.Name, e.DerivedFromHash)
		}
		if seen[e.Name] {
			return Manifest{}, fmt.Errorf("cap manifest: %s listed twice", e.Name)
		}
		seen[e.Name] = true
	}
	return m, nil
}

// ReadManifest reads the provenance of a caps directory. A missing file comes back
// as an os.IsNotExist error, because the callers answer that differently: the
// installer treats it as an installation that has no evidence yet and writes the
// file, the daemon treats it as no evidence at all.
func ReadManifest(path string) (Manifest, error) {
	data, err := ReadNoFollow(path)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}
