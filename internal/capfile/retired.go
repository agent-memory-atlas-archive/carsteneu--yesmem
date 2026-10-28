package capfile

// RetiredBundledCaps names caps yesmem used to ship and no longer does. Their
// CAP.md stays on existing installations, and without this list the import path
// would read it back as an active cap learning at every daemon start, so the cap
// reappears in the catalog and stays executable.
//
// wiki_export is the case that forced the list (YM-181, YM-197): the wiki
// rendering moved into "yesmem wiki-render" (cmd_wiki_render.go, which writes
// through internal/wikidir), and the cap was a third writer that left out the
// .project marker and could overwrite another project's directory.
//
// A name belongs here only when the cap is gone from caps/bundled-caps for good.
// The file itself is not deleted: it may have been edited by its owner, and an
// ignored file does no harm once nothing imports it.
var RetiredBundledCaps = []string{"wiki_export"}

// IsRetiredCap reports whether name is one of the caps in RetiredBundledCaps.
func IsRetiredCap(name string) bool {
	for _, retired := range RetiredBundledCaps {
		if name == retired {
			return true
		}
	}
	return false
}
