package setup

import (
	"path/filepath"
	"strings"
	"testing"
)

// YM-450 (OPS-113): the cap-delivery docs described gates that no longer exist. They
// first claimed the installer ships a bundled body on any byte difference and that
// `make deploy` performs the copy; los 63c pinned the revision gate that replaced
// that, and los 64b removed the gate itself. What decides a delivery now is the
// provenance in `manifest.yaml`, and the three cases an installation can be in are
// the only thing a maintainer has to know:
//
//   - no entry: nothing vouches for the bundle, an installed body is kept and reported
//   - the entry names the installed body: the bundle is its successor and is written,
//     whatever the frontmatter `version:` says
//   - the entry names a different body: kept, and one conflict line names both hashes
//
// Standing proof for the revision gate that is gone: the rocketchat cap stood at v15
// before and after a deploy on 2026-10-23, and telegram sat at v603 live against a
// bundle of v195 while every setup run reported success.
func TestLos63cCapDeliveryDocsMatchTheMechanism(t *testing.T) {
	docs := []struct {
		name string
		path string
	}{
		{"docs/features/operations.md", filepath.Join("..", "..", "docs", "features", "operations.md")},
		{"yesmem-cap-builder/recipes.md", filepath.Join("..", "..", "skills", "bundled-skills", "yesmem-cap-builder", "recipes.md")},
		{"yesmem-cap-builder/SKILL.md", filepath.Join("..", "..", "skills", "bundled-skills", "yesmem-cap-builder", "SKILL.md")},
	}
	// Claims the mechanism no longer makes. Each one named a gate that is gone, and a
	// doc that still carries it sends the maintainer after a counter that decides
	// nothing.
	stale := []string{
		"no version-compare",
		"embed wins over disk",
		"copies if different",
		"strictly higher",
	}
	// What a reader needs to follow a delivery without reading the installer.
	required := []string{
		"derived_from_hash", // the entry that decides
		"manifest.yaml",     // the file it lives in, shipped and per installation
		"No entry",          // the case that reports instead of installing
		"display only",      // the frontmatter version no longer gates
		"yesmem migrate",    // the command that runs InstallBundledCaps; `make deploy` does not install caps
	}
	for _, d := range docs {
		doc := readFile(t, d.path)
		for _, s := range stale {
			if strings.Contains(doc, s) {
				t.Errorf("%s still claims %q; the installer decides from derived_from_hash", d.name, s)
			}
		}
		for _, r := range required {
			if !strings.Contains(doc, r) {
				t.Errorf("%s does not name %q, part of the provenance rule that replaced the version gate", d.name, r)
			}
		}
	}

	// The conflict line is the whole point of the report: it must name both digests,
	// or the operator has no hash to record.
	ops := readFile(t, docs[0].path)
	if !strings.Contains(ops, "hashes to") {
		t.Error("operations.md does not describe the conflict line that names both digests")
	}
	if !strings.Contains(ops, "cap_imported_version") {
		t.Error("operations.md does not name cap_imported_version, the watermark the import gate still compares against")
	}

	recipes := readFile(t, docs[1].path)
	if !strings.Contains(recipes, "~/.claude/caps/manifest.yaml") {
		t.Error("recipes.md does not point at the installation's own manifest, the file a conflict is reconciled in")
	}
}
