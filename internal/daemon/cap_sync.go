package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// CapFileToParams flattens a parsed CAP.md into the param map expected by
// handleSaveCap. Scripts are passed as a JSON array string.
func CapFileToParams(cf capfile.CapFile) map[string]any {
	params := map[string]any{
		"name":        cf.Name,
		"description": cf.Description,
		"version":     float64(cf.Version),
	}

	if len(cf.Scripts) > 0 {
		scriptMetas := make([]ScriptMeta, len(cf.Scripts))
		for i, sc := range cf.Scripts {
			scriptMetas[i] = ScriptMeta{
				Name:         sc.Name,
				Kind:         sc.Kind,
				Runtime:      sc.Runtime,
				Lang:         sc.Lang,
				Body:         sc.Body,
				Schema:       sc.Schema,
				Sandbox:      sc.Sandbox,
				AllowedHosts: sc.AllowedHosts,
				Timeout:      sc.Timeout,
			}
		}
		scriptsJSON, _ := json.Marshal(scriptMetas)
		params["scripts"] = string(scriptsJSON)
	}

	if len(cf.Tags) > 0 {
		params["tags"] = strings.Join(cf.Tags, ",")
	}
	if len(cf.Requires) > 0 {
		params["requires"] = strings.Join(cf.Requires, ",")
	}
	if cf.Tested {
		params["tested"] = true
	}
	// auto_active is passed even when false: an absent field in CAP.md is the
	// opt-out, and save_cap's own default is the opposite.
	params["auto_active"] = cf.AutoActive
	if cf.Scope != "" {
		params["scope"] = cf.Scope
	}
	if len(cf.Actions) > 0 {
		actionsJSON, _ := json.Marshal(cf.Actions)
		params["actions"] = string(actionsJSON)
	}
	return params
}

// CapMetaToCapFile converts stored cap metadata into the disk format. The
// project string indicates project-scoped caps when non-empty. defaultProfile is
// the configured default a script inherits when it names none; a project-scoped
// cap may not run unsandboxed, so that value decides which scripts the file has
// to repair on the way out.
func CapMetaToCapFile(meta CapMeta, project string, defaultProfile SandboxProfile) capfile.CapFile {
	scope := "project"
	if project == "" {
		scope = "user"
	}

	scripts := make([]capfile.Script, len(meta.Scripts))
	for i, sm := range meta.Scripts {
		lang := sm.Lang
		if lang == "" {
			switch sm.Runtime {
			case "repl":
				lang = "javascript"
			case "bash":
				lang = "bash"
			}
		}
		sandbox := sm.Sandbox
		if scope == "project" && scriptRunsUnsandboxed(sandbox, defaultProfile) {
			// capfile.Parse and save_cap both refuse an unsandboxed script on a
			// project-scoped cap, so exporting one would produce a file that can
			// never come back (YM-230, E5). The row violates the rule; the file
			// states the profile the rule demands, and the line says so. The check
			// runs on the effective profile: a script without a sandbox line is
			// unsandboxed exactly when the configured default is none.
			log.Printf("[cap-export] %s: script %q: %q is not a sandboxed profile for a project-scoped cap, exporting standard", meta.Name, sm.Name, sandbox)
			sandbox = "standard"
		}
		scripts[i] = capfile.Script{
			Name:         sm.Name,
			Kind:         sm.Kind,
			Runtime:      sm.Runtime,
			Lang:         lang,
			Body:         sm.Body,
			Schema:       sm.Schema,
			Sandbox:      sandbox,
			AllowedHosts: sm.AllowedHosts,
			Timeout:      sm.Timeout,
		}
	}

	return capfile.CapFile{
		Name:        meta.Name,
		Description: meta.Description,
		Version:     meta.Version,
		Tags:        meta.Tags,
		Requires:    meta.Requires,
		Scope:       scope,
		Tested:      meta.Tested,
		AutoActive:  meta.AutoActive,
		Actions:     meta.Actions,
		Scripts:     scripts,
	}
}

// CapDDLLooker resolves the CREATE TABLE statements for a cap's data tables.
type CapDDLLooker interface {
	GetCapTableDDL(capName string) (string, error)
}

// scriptRunsUnsandboxed reports whether a script's effective profile is none.
// Fail-closed: a value it cannot resolve counts as unsandboxed, because the
// export must not hand out a file that save_cap then refuses. ParseCapMeta
// validates the stored values, so this only guards a row written by something
// else.
func scriptRunsUnsandboxed(scriptSandbox string, defaultProfile SandboxProfile) bool {
	profile, err := resolveScriptSandboxWith(scriptSandbox, defaultProfile)
	return err != nil || profile == ProfileNone
}

// installedCapBody returns the body digest the installation's manifest records for
// name, and whether such an entry exists. The installer writes that file next to the
// cap directories it describes, so the caps dir of a cap is its own parent directory.
//
// An entry is evidence for exactly one thing: this installation was given that body.
// A missing entry, an unreadable manifest and the bundle's own self-hash say nothing
// about the file that sits on disk, so the callers fall back to the counters there.
func installedCapBody(capsDir, name string) (digest string, tracked bool) {
	if capsDir == "" {
		return "", false
	}
	path := filepath.Join(capsDir, capfile.ManifestFileName)
	manifest, err := capfile.ReadManifest(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[cap-provenance] %s: %s (nothing vouches for %s)", path, logSafe(err.Error()), name)
		}
		return "", false
	}
	if entry := manifest.Find(name); entry != nil {
		return entry.DerivedFromHash, true
	}
	return "", false
}

// logSafe keeps file-borne text out of the log as it is: a yaml error quotes the
// offending value, and a manifest.yaml someone edited by hand can put escape sequences
// or line breaks there. Control characters, format characters (the bidi marks among
// them) and the unicode line separators go, so the line stays one line.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
}

func WriteCapToDisk(meta CapMeta, project, baseDir string, ddl CapDDLLooker, defaultProfile SandboxProfile) error {
	// A row can arrive through remember() instead of save_cap, so the writer checks
	// the name itself: a traversal-shaped name would turn the join below into a path
	// outside the caps dir (YM-353).
	if err := storage.ValidateCapName(meta.Name); err != nil {
		return err
	}
	// filepath.Join("", name) is a bare relative name and a relative base dir resolves
	// against the daemon's cwd, the user's home (YM-353).
	if !filepath.IsAbs(baseDir) {
		return fmt.Errorf("write cap %q: caps dir %q is not absolute", meta.Name, baseDir)
	}
	cf := CapMetaToCapFile(meta, project, defaultProfile)
	if cf.Version == 0 {
		cf.Version = 1
	}
	if cf.Purpose == "" {
		cf.Purpose = cf.Description
	}
	if ddl != nil {
		if sql, err := ddl.GetCapTableDDL(meta.Name); err == nil && sql != "" {
			cf.DatabaseSQL = sql
		}
	}
	dir := filepath.Join(baseDir, cf.Name)

	capPath := filepath.Join(dir, "CAP.md")
	// The directory is judged before anything inside it is touched: a link at this
	// level would hand both the read and the mode repair to a directory outside the
	// caps tree, and it would also let foreign content steer the version gate below.
	// WriteFile repeats the check for the write path (YM-359).
	if err := capfile.EnsureDirOwnerOnly(dir); err != nil {
		return fmt.Errorf("write cap %q: %w", meta.Name, err)
	}
	// Neither the read nor the mode repair may follow a link: the version gate would
	// compare foreign content and the repair would change a foreign file. The read
	// refuses it, and the write below refuses it again with the export's error
	// (YM-359).
	if data, err := capfile.ReadNoFollow(capPath); err == nil {
		diskDigest := textutil.BodyDigest(data)
		installedDigest, tracked := installedCapBody(baseDir, cf.Name)
		vouched := tracked && installedDigest == diskDigest
		diskCF, parseErr := capfile.Parse(data)
		// Provenance decides before the counters. A vouched file is the body the installer
		// delivered, and a clean row may not replace it: either the row's source is a LATER
		// revision than the file (a bundle cut from the live body carries a lower number
		// than the repairs the row collected, so the counters alone always said "the row
		// wins", and every delivered body was rolled back at the next start), or the row
		// was imported from exactly this file and has nothing of its own to add. Both
		// cases must leave the bytes alone: a rewritten file no longer hashes to its
		// manifest entry, and the next release would only be reported as a conflict.
		//
		// A row that carries a repair is the other case: the repair is content the file
		// does not have, and the export is the only way it reaches the disk.
		if vouched && !capRowRepairedSinceImport(meta) && parseErr == nil && meta.ImportedVersion >= diskCF.Version {
			if meta.ImportedVersion > diskCF.Version {
				// The step back is reported, never taken silently.
				log.Printf("WriteCapToDisk: %s: keep the delivered body v%d, the row (v%d) was imported from v%d; the installer's body wins", cf.Name, diskCF.Version, cf.Version, meta.ImportedVersion)
			}
			if chmodErr := capfile.RepairFileOwnerOnly(capPath); chmodErr != nil {
				log.Printf("WriteCapToDisk: %s: repair mode: %v", cf.Name, chmodErr)
			}
			return nil
		}
		if parseErr == nil && diskCF.Version >= cf.Version {
			log.Printf("WriteCapToDisk: skip %s (disk v%d >= db v%d)", cf.Name, diskCF.Version, cf.Version)
			// The content is current, the mode may not be: a CAP.md exported before
			// YM-346 carries 0644 and never passes through WriteFile again.
			if chmodErr := capfile.RepairFileOwnerOnly(capPath); chmodErr != nil {
				log.Printf("WriteCapToDisk: %s: repair mode: %v", cf.Name, chmodErr)
			}
			return nil
		}
		// The row wins from here because its counter is higher, and the file is not the
		// body the installation recorded. That may be the right call, but it may not
		// pass without a word: a file nothing vouches for is either a hand-edited body
		// or a lost install, and the two are only distinguishable from the hashes.
		if !vouched {
			if tracked {
				log.Printf("WriteCapToDisk: %s: the file hashes to %s, the installed manifest names %s; the row (v%d) replaces it", cf.Name, diskDigest, installedDigest, cf.Version)
			} else {
				log.Printf("WriteCapToDisk: %s: the file hashes to %s and no manifest entry vouches for it; the row (v%d) replaces it", cf.Name, diskDigest, cf.Version)
			}
		}
	}

	return capfile.WriteFile(&cf, dir)
}

func SyncCapsFromDisk(h *Handler, userDir, projectDir string) {
	caps, errs := capfile.ScanAll(userDir, projectDir)
	for _, err := range errs {
		log.Printf("[cap-sync] scan error: %v", err)
	}
	// The stored profiles are read BEFORE the loop: after a save the row carries
	// the file's values, which would make every comparison below vacuous.
	stored := storedScriptSandboxes(h)
	for _, cf := range caps {
		// A cap yesmem no longer ships is not imported: its CAP.md survives on
		// existing installations, and importing it would put the retired cap back
		// into the catalog at every daemon start (YM-181, YM-197).
		if capfile.IsRetiredCap(cf.Name) {
			log.Printf("[cap-sync] %s skipped: retired cap, its file is ignored", cf.Name)
			continue
		}
		// A project-scoped CAP.md in the user directory has no project to bind to:
		// the save would land as a user-scoped cap, which makes the cap visible in
		// every checkout and leaves the project-bound row untouched. ExportAllCaps
		// writes every active row there, project-bound ones included, so this is
		// the normal case and not a malformed file — it is simply not importable
		// from this directory.
		if cf.Scope == "project" && projectDir == "" {
			log.Printf("[cap-sync] %s skipped: %s declares scope: project but the user caps directory binds no project", cf.Name, cf.SourcePath)
			continue
		}
		params := CapFileToParams(cf)
		if cf.Scope == "project" && projectDir != "" {
			params["project"] = filepath.Base(filepath.Dir(filepath.Dir(cf.SourcePath)))
		}
		resp := h.handleSaveCapFrom(params, cf.SourcePath)
		if resp.Error != "" {
			log.Printf("[cap-sync] save %s: %s", cf.Name, resp.Error)
			continue
		}
		if unchangedResponse(resp) {
			log.Printf("[cap-sync] %s unchanged (disk v%d: %s)", cf.Name, cf.Version, unchangedReason(resp))
			continue
		}
		logSandboxAdoption(cf, stored[cf.Name])
		log.Printf("[cap-sync] synced %s from %s", cf.Name, cf.SourcePath)
	}
}

// unchangedReason names why save_cap left the cap alone; save_cap puts it in the
// response so a log line can tell "nothing new in the file" from "the row is
// newer than the file".
func unchangedReason(resp Response) string {
	var r struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(resp.Result, &r) != nil || r.Reason == "" {
		return "no change"
	}
	return r.Reason
}

// unchangedResponse reports whether save_cap left the cap alone because the
// incoming version was not newer.
func unchangedResponse(resp Response) bool {
	var r struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(resp.Result, &r) == nil && r.Status == "unchanged"
}

// logSandboxAdoption records how a landed sync changed the sandbox profiles of a
// cap's scripts, in both directions. Carrying a profile over is what makes a
// script's allowed_hosts effective; dropping the `sandbox:` line from a CAP.md
// strips the stored profile, because the merge replaces the whole script. YM-223
// asks for both to be visible rather than applied silently.
//
// stored is the cap's script-name -> profile map as it was before the save; nil
// for a cap the store did not have.
func logSandboxAdoption(cf capfile.CapFile, stored map[string]string) {
	for _, sc := range cf.Scripts {
		prev, known := stored[sc.Name]
		switch {
		case known && prev == sc.Sandbox:
			// unchanged
		case !known && sc.Sandbox == "":
			// a script the store did not have, without a profile
		case sc.Sandbox == "":
			log.Printf("[cap-sync] %s: script %s loses sandbox profile %q — the file carries none", cf.Name, sc.Name, prev)
		case !known:
			log.Printf("[cap-sync] %s: script %s takes sandbox profile %q from %s (new script)", cf.Name, sc.Name, sc.Sandbox, cf.SourcePath)
		case prev == "":
			log.Printf("[cap-sync] %s: script %s takes sandbox profile %q from %s (had none)", cf.Name, sc.Name, sc.Sandbox, cf.SourcePath)
		default:
			log.Printf("[cap-sync] %s: script %s profile %q -> %q from %s", cf.Name, sc.Name, prev, sc.Sandbox, cf.SourcePath)
		}
	}
}

// storedScriptSandboxes maps cap name -> script name -> stored sandbox profile
// for every active cap.
func storedScriptSandboxes(h *Handler) map[string]map[string]string {
	learnings, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		log.Printf("[cap-sync] read stored sandbox profiles: %v", err)
		return nil
	}
	out := make(map[string]map[string]string, len(learnings))
	for _, l := range learnings {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		scripts := make(map[string]string, len(meta.Scripts))
		for _, sc := range meta.Scripts {
			scripts[sc.Name] = sc.Sandbox
		}
		out[meta.Name] = scripts
	}
	return out
}

// logSandboxProfilesOnLoad records, once per daemon start, what the stored caps
// inherit. YM-230 changed what an absent sandbox line means (it used to be none,
// now it is the configured default), and these lines are the only trace of how
// far that change reaches: one line per cap, plus a summary. A project-bound cap
// whose effective profile is none is named as what it is: save_cap and the CAP.md
// parser refuse that state, and the run paths refuse it too (YM-346), so the line
// is the only signal that a stored row sits in it.
func logSandboxProfilesOnLoad(h *Handler) (inherited, unsandboxed int) {
	learnings, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		log.Printf("[cap-sandbox] read caps: %v", err)
		return 0, 0
	}
	inheritedDefault := h.configuredDefaultSandbox()
	for _, l := range learnings {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		withoutLine := 0
		for _, sc := range meta.Scripts {
			if sc.Sandbox == "" {
				withoutLine++
			}
		}
		if withoutLine == 0 {
			continue
		}
		if inheritedDefault == ProfileNone {
			// Nothing moved for these scripts, but a project-bound cap in this
			// state cannot be saved and is refused on the run paths.
			if capIsProjectBound(l.Project, meta) {
				unsandboxed++
				log.Printf("[cap-sandbox] cap %s: %d script(s) without a sandbox line resolve to none (default_sandbox_profile=none) while the cap is project-bound; save_cap and the run paths refuse it", meta.Name, withoutLine)
			}
			continue
		}
		inherited++
		log.Printf("[cap-sandbox] cap %s: %d script(s) without a sandbox line inherit %s (was none) (YM-230)", meta.Name, withoutLine, inheritedDefault)
	}
	if inherited > 0 {
		log.Printf("[cap-sandbox] %d cap(s) inherit %s where they ran unsandboxed before (YM-230)", inherited, inheritedDefault)
	}
	return inherited, unsandboxed
}

func ExportAllCaps(h *Handler, baseDir string) {
	// The root listing names every installed cap, so the export repairs its mode
	// too: the installer is the only other place that does, and a daemon started on
	// an install created before YM-353 never went through it (YM-353).
	if baseDir != "" {
		if dirErr := capfile.EnsureDirOwnerOnly(baseDir); dirErr != nil {
			log.Printf("[cap-export] caps dir %s: %v", baseDir, dirErr)
		}
	}
	learnings, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		log.Printf("[cap-export] query: %v", err)
		return
	}
	var exported int
	for _, l := range learnings {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		// A retired cap must not be written back: the row of an older version can
		// still be active, and its CAP.md is what the next start would have read
		// (YM-181, YM-197).
		if capfile.IsRetiredCap(meta.Name) {
			continue
		}
		if err := WriteCapToDisk(meta, l.Project, baseDir, h.store, h.configuredDefaultSandbox()); err != nil {
			log.Printf("[cap-export] %s: %v", meta.Name, err)
		} else {
			exported++
		}
	}
	log.Printf("[cap-export] exported %d caps to %s", exported, baseDir)
}

type CapsDirWatcher struct {
	mtimes map[string]time.Time
	// rejected holds the last error text per path, so a refused entry is logged
	// once per distinct message instead of on every tick. It is only about the log
	// line: the read itself is retried on every tick.
	rejected map[string]string
}

func NewCapsDirWatcher() *CapsDirWatcher {
	return &CapsDirWatcher{mtimes: make(map[string]time.Time), rejected: make(map[string]string)}
}

func (w *CapsDirWatcher) ScanChanged(dir string) []capfile.CapFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var changed []capfile.CapFile
	seen := make(map[string]bool)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		capPath := filepath.Join(dir, entry.Name(), "CAP.md")
		// Lstat, not Stat: a link at this path is not a caps file, and following
		// it would both judge a foreign mtime and import foreign content (YM-373).
		info, err := os.Lstat(capPath)
		if err != nil {
			continue
		}
		seen[capPath] = true
		mtime := info.ModTime()

		if prev, ok := w.mtimes[capPath]; ok && !mtime.After(prev) {
			continue
		}

		data, err := capfile.ReadNoFollow(capPath)
		if err != nil {
			// Logged once per message, not remembered as an mtime: a repaired file
			// keeps the timestamp of the backup it was restored from, and skipping
			// it on that comparison would leave the cap unimported for the rest of
			// the daemon's life (cold review of YM-373). The rejection is forgotten
			// on the next successful read.
			if msg := err.Error(); w.rejected[capPath] != msg {
				log.Printf("CapsDirWatcher: skip %s: %v", capPath, err)
				w.rejected[capPath] = msg
			}
			continue
		}
		delete(w.rejected, capPath)
		cf, err := capfile.Parse(data)
		if err != nil {
			log.Printf("CapsDirWatcher: parse %s: %v", capPath, err)
			continue
		}
		if err := capfile.CheckDirName(entry.Name(), cf.Name); err != nil {
			log.Printf("CapsDirWatcher: skip %s: %v", capPath, err)
			w.mtimes[capPath] = mtime
			continue
		}
		// A retired cap is ignored for good. The mtime is recorded anyway, so the
		// file is not parsed again on every tick; the import below is not reached
		// for it at all (YM-181, YM-197).
		if capfile.IsRetiredCap(cf.Name) {
			w.mtimes[capPath] = mtime
			continue
		}
		cf.SourcePath = capPath
		w.mtimes[capPath] = mtime
		changed = append(changed, *cf)
	}

	for path := range w.mtimes {
		if !seen[path] {
			delete(w.mtimes, path)
		}
	}
	for path := range w.rejected {
		if !seen[path] {
			delete(w.rejected, path)
		}
	}

	return changed
}

func (w *CapsDirWatcher) RefreshMtime(capPath string) {
	info, err := os.Stat(capPath)
	if err != nil {
		return
	}
	w.mtimes[capPath] = info.ModTime()
}
