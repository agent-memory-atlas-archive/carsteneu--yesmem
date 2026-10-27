package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/carsteneu/yesmem/internal/capfile"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/repo"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

type capResult struct {
	ID      int64   `json:"id"`
	Project string  `json:"project,omitempty"`
	Source  string  `json:"source"`
	Meta    CapMeta `json:"meta"`
}

func (h *Handler) handleGetCaps(params map[string]any) Response {
	project := stringOr(params, "project", "")
	name := stringOr(params, "name", "")
	tag := stringOr(params, "tag", "")
	limit := intOr(params, "limit", 50)

	learnings, err := h.capRowsForProject(project)
	if err != nil {
		return errorResponse(fmt.Sprintf("get_caps: %v", err))
	}

	var results []capResult
	// One answer per name when the caller names a project: capRowsForProject puts
	// its own rows first, so the first row of a name is the one that answers. The
	// mark is set after the filters, because a shadowing row that does not match
	// the query must not hide the global row that does. Without a project the
	// question is the plain inventory and every row is listed, since there is no
	// caller row to prefer.
	seen := make(map[string]bool, len(learnings))
	collapse := project != ""
	for _, l := range learnings {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		// A retired cap is hidden even when its row is still active: an install
		// that only received a new binary never ran the retirement step, and the
		// catalog must not offer it in the meantime (YM-181, YM-197).
		if capfile.IsRetiredCap(meta.Name) {
			continue
		}
		if name != "" && meta.Name != name {
			continue
		}
		if tag != "" && !meta.HasTag(tag) {
			continue
		}
		if collapse && seen[meta.Name] {
			continue
		}
		seen[meta.Name] = true
		// Legacy fields duplicate script bodies; scripts carry the same
		// handlers via normalize(). Suppress them from all responses.
		meta.HandlerBash = ""
		meta.HandlerREPL = ""
		meta.Schema = ""
		if name == "" {
			// List mode: metadata only — script bodies dominate the payload.
			// Full definition available via name-filtered call.
			meta.Scripts = nil
		}
		results = append(results, capResult{
			ID:      l.ID,
			Project: l.Project,
			Source:  l.Source,
			Meta:    h.filterSetupActions(meta),
		})
		if len(results) >= limit {
			break
		}
	}
	if results == nil {
		results = []capResult{}
	}
	return jsonResponse(results)
}

func (h *Handler) filterSetupActions(meta CapMeta) CapMeta {
	if len(meta.Actions) == 0 || meta.Actions["setup"] == "" {
		return meta
	}
	rows, err := h.store.CapsQuery(meta.Name, "config", "key = ?", []any{"_setup_complete"}, 1)
	if err == nil && len(rows) > 0 {
		filtered := make(map[string]string, len(meta.Actions))
		for k, v := range meta.Actions {
			if k != "setup" {
				filtered[k] = v
			}
		}
		meta.Actions = filtered
		if len(meta.Actions) == 0 {
			meta.Actions = nil
		}
	}
	return meta
}

// capSupersededBy answers whether a save that lands in effectiveProject may
// retire the cap row bound to rowProject. The binding decides, not the name.
//
// A project save must not retire the global cap it merely shadows: the row would
// vanish for every other project, silently, because the supersede path reports
// success (YM-402). A plain global save must not retire another project's row
// either, which is the guard Los 56f already pinned.
//
// scope=user is the documented way OUT of a project binding (the sandbox lock's
// "use scope=user" answer) and stays the one exception: it clears the project and
// retires the row it converts, pinned by
// TestHandleSaveCap_MetadataOnlySaveLeavingProjectScopeIsAllowed. A caller that
// names no project leaves open WHICH row it converts; the caller of this function
// therefore drops that row when two projects carry the name.
func capSupersededBy(rowProject, effectiveProject, project, scope string) bool {
	if rowProject == "" {
		return effectiveProject == ""
	}
	if effectiveProject != "" {
		return capProjectMatches(rowProject, effectiveProject)
	}
	return scope == "user" && (project == "" || capProjectMatches(rowProject, project))
}

// capRowsForProject returns the active cap rows a caller in project may see: the
// rows bound to its own project first, then the global ones. The order is the
// answer to "which row of this name": a caller that collapses a name to one entry
// keeps the first it sees, so a bound row shadows the global row of the same name,
// which is the choice loadCapMeta makes when it picks the cap to run.
//
// This is the project question of the catalog-shaped answers (get_caps,
// register_caps), and it is answered by capProjectMatches — the same predicate
// execute_cap and the catalog use — not by a copy of it in SQL. The query filter
// on canonical_project hid every global cap, because save_cap stores "." in that
// column for them while the filter admits only the empty value (YM-403).
//
// Two limits are deliberate. Without a project the answer is the plain inventory
// in list order, because that is a different question. A caller that names only a
// basename reaches the global caps and no row bound to an absolute path: a short
// name is not tied to a repository, the same fail-closed rule execute_cap applies
// (YM-359). It still pairs with a row that is itself relative, because neither
// side names a repository there.
func (h *Handler) capRowsForProject(project string) ([]models.Learning, error) {
	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		return nil, err
	}
	if project == "" {
		return caps, nil
	}
	// A session in a subdirectory or in a worktree names the same project as its
	// checkout; capProjectMatches accepts both spellings once the path is folded.
	project = repo.RootOrSelf(project)
	var own, global []models.Learning
	for _, l := range caps {
		switch {
		case l.Project == "":
			global = append(global, l)
		case capProjectMatches(l.Project, project):
			own = append(own, l)
		}
	}
	return append(own, global...), nil
}

// handleSaveCap is the entry point every caller that is not a file import uses: an MCP
// call, the auto-correct pass, a scheduled repair. It carries no provenance, because a
// caller may not claim that its body is the one this installation was given.
func (h *Handler) handleSaveCap(params map[string]any) Response {
	return h.handleSaveCapFrom(params, "")
}

// handleSaveCapFrom imports one CAP.md the daemon read itself. sourcePath names where
// that body sits; its digest is compared against the installation's manifest, which is
// the only thing that can vouch for a file numbering below the revision the row was
// imported from (YM-450).
func (h *Handler) handleSaveCapFrom(params map[string]any, sourcePath string) Response {
	name := stringOr(params, "name", "")
	if name == "" {
		return errorResponse("save_cap: 'name' is required")
	}
	if err := storage.ValidateCapName(name); err != nil {
		return errorResponse(fmt.Sprintf("save_cap: %v", err))
	}
	// Retirement is a rule, not a one-off migration: this is the single point
	// every import path goes through (SyncCapsFromDisk, the CAP.md watcher, the
	// job socket, the tool itself), so a name yesmem no longer ships is refused
	// here. Without it the CAP.md that stays on disk, or an agent following an old
	// recipe, puts the cap back into the catalog (YM-181, YM-197).
	if capfile.IsRetiredCap(name) {
		return errorResponse(fmt.Sprintf("save_cap: %q is a retired cap name: yesmem no longer ships it and a CAP.md of that name is ignored. Pick another name, or use \"yesmem wiki-render\" for the wiki.", name))
	}
	description := stringOr(params, "description", "")
	project := stringOr(params, "project", "")
	testDate := stringOr(params, "test_date", "")
	source := stringOr(params, "source", "user_stated")

	tags := parseTagsParam(params)
	tested, _ := params["tested"].(bool)
	autoActive := true
	if v, ok := params["auto_active"].(bool); ok {
		autoActive = v
	}
	scope := stringOr(params, "scope", "")

	// Auto-supersede: find existing cap with same name.
	incomingVersion := 0
	if v, ok := params["version"].(float64); ok {
		incomingVersion = int(v)
	}
	// User-scoped caps are project-agnostic: an explicit scope=user clears the
	// project, so such a cap is never project-bound. This effective scope — not
	// the raw scope param — decides both the sandbox lock and the stored project.
	// It is needed before the supersede lookup, because the binding of the
	// incoming save decides which row it may retire.
	effectiveProject := project
	if scope == "user" {
		effectiveProject = ""
	}

	// Whether the save brings a body decides part of the binding question below, so
	// it is read before the supersede lookup.
	hasScriptsParam := stringOr(params, "scripts", "") != ""
	hasLegacyHandlers := stringOr(params, "handler_bash", "") != "" || stringOr(params, "handler_repl", "") != ""

	var supersededID *int64
	var supersededIDs []int64
	var oldMeta *CapMeta
	var oldProject string
	var oldID int64
	version := 1
	existing, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		return errorResponse(fmt.Sprintf("save_cap supersede lookup: %v", err))
	}
	// A save retires the rows of that name it may replace: a project-bound row and
	// a global one can both be live, because a project copy shadows the global cap
	// of its name instead of ending it (YM-402). Both are retired, so one name
	// keeps one row per binding instead of piling up a second global row on a
	// conversion. The bound row is the one the new row inherits from: scope=user
	// exists to convert exactly that row.
	var bound, global *models.Learning
	var boundMeta CapMeta
	boundOthers := 0
	for i := range existing {
		if !capSupersededBy(existing[i].Project, effectiveProject, project, scope) {
			continue
		}
		meta, err := ParseCapMeta(existing[i].Context)
		if err != nil || meta.Name != name {
			continue
		}
		if existing[i].Project == "" {
			if global == nil {
				global = &existing[i]
			}
			continue
		}
		if bound == nil {
			bound, boundMeta = &existing[i], meta
			continue
		}
		boundOthers++
	}
	// A scope=user save that names no project is the documented way out of a
	// binding, but it names no row either. It is the repair move: convert one
	// existing row without bringing a new body. A save that DOES carry scripts is
	// an import or a rewrite instead — every bundled CAP.md declares scope: user,
	// and retiring a project's copy of the name on a routine re-import would be a
	// silent loss. Two project copies of the name are ambiguous either way. In
	// both cases the save touches only the global row.
	if scope == "user" && project == "" &&
		(hasScriptsParam || hasLegacyHandlers || boundOthers > 0) {
		bound = nil
	}
	var predecessor *models.Learning
	var predecessorMeta CapMeta
	if bound != nil {
		predecessor, predecessorMeta = bound, boundMeta
	} else if global != nil {
		if meta, err := ParseCapMeta(global.Context); err == nil {
			predecessor, predecessorMeta = global, meta
		}
	}
	if predecessor != nil {
		id := predecessor.ID
		supersededID = &id
		oldID = predecessor.ID
		oldProject = predecessor.Project
		version = predecessorMeta.Version + 1
		metaCopy := predecessorMeta
		oldMeta = &metaCopy
		for _, l := range []*models.Learning{bound, global} {
			if l != nil {
				supersededIDs = append(supersededIDs, l.ID)
			}
		}
	}
	// A cap that arrives with a source revision starts at that revision. Starting
	// at 1 left the stored counter behind the file, and the file's own number then
	// had to climb after it on every start.
	if oldMeta == nil && incomingVersion > 0 {
		version = incomingVersion
	}
	// An import lands at or above the revision it came from. Otherwise the row
	// trails its own source, and WriteCapToDisk only rewrites a file whose version
	// is below the row's — a repair then never reached the disk, and the next start
	// read the old body back over it.
	if oldMeta != nil && incomingVersion > version {
		version = incomingVersion
	}

	content := name + " — " + description
	hash := textutil.ContentHash(content)

	var scripts []ScriptMeta
	if hasScriptsParam || hasLegacyHandlers {
		incoming, err := scriptsFromSaveCapParams(name, params)
		if err != nil {
			return errorResponse(fmt.Sprintf("save_cap: %v", err))
		}
		scripts = incoming
		// Merge is only needed for the scripts-array path — legacy
		// handler params are deprecated and replace all scripts.
		if hasScriptsParam && oldMeta != nil {
			scripts = mergeScripts(oldMeta.Scripts, scripts)
		}
	} else if oldMeta != nil {
		scripts = oldMeta.Scripts
	} else {
		return errorResponse("save_cap: 'scripts' is required for new caps")
	}

	// The lock runs on the set the save leaves behind, not on the incoming one:
	// a save that keeps an inherited sandbox=none script must not persist it as
	// project-bound. It applies when the cap lands in a project, when the caller
	// declares project scope (as the parser reads an explicit scope: project),
	// and to a cap that IS project-bound — that one stays under the lock unless
	// the caller leaves project scope with scope=user, the sanctioned way out.
	lockApplies := effectiveProject != "" || scope == "project" ||
		(oldProject != "" && scope != "user")

	// A metadata-only save is exempt while it leaves the binding where it was.
	// That keeps the repair path open for a cap which already is project-bound
	// with an inherited sandbox=none script, without being the way into — or out
	// of — that state. The comparison is canonical, like the supersede lookup:
	// another spelling of the same project (a worktree path) moves no binding.
	metadataOnlySameBinding := !hasScriptsParam && !hasLegacyHandlers &&
		canonicalProjectFor(oldProject) == canonicalProjectFor(effectiveProject)

	if err := h.validateScriptSandboxForScope(lockApplies, metadataOnlySameBinding, scripts); err != nil {
		return errorResponse(fmt.Sprintf("save_cap: %v", err))
	}

	// A project-scoped cap needs a project to bind to. Storing one without a project
	// would create a row that claims project scope while being visible in every
	// checkout, and it would shadow the project-bound row it was exported from.
	// ExportAllCaps writes every active row into the user caps directory, so this
	// arrives through the sync and the file watcher — no state change, and a log
	// line, because such a file is not importable from that directory.
	if scope == "project" && project == "" {
		log.Printf("save_cap: %s declares scope=project but no project was given — no binding to store", name)
		return unchangedCap(oldID, "project scope without a project")
	}

	// The source revision this row will be imported from. Both values are the row's
	// memory of its last import, so they only move when a file arrives: a save without a
	// version (the auto-correct pass) inherits them, and a save that merely carries a
	// number is an edit of the row, not an import. That is what makes an edit visible as
	// a repair on top of the body the row holds (capRowRepairedSinceImport).
	importedVersion := 0
	importedDigest := ""
	if oldMeta != nil {
		importedVersion = oldMeta.ImportedVersion
		importedDigest = oldMeta.ImportedDigest
	}
	if incomingVersion > 0 && sourcePath != "" {
		// A file is the source of the scripts it brought.
		importedVersion = incomingVersion
		importedDigest = scriptDigest(scripts)
	}

	meta := CapMeta{
		Name:            name,
		Description:     description,
		Scripts:         scripts,
		Requires:        capRequires(params, scripts),
		Tags:            tags,
		Version:         version,
		ImportedVersion: importedVersion,
		ImportedDigest:  importedDigest,
		Tested:          tested,
		TestDate:        testDate,
		AutoActive:      autoActive,
		Scope:           scope,
	}
	if actionsStr := stringOr(params, "actions", ""); actionsStr != "" {
		if err := json.Unmarshal([]byte(actionsStr), &meta.Actions); err != nil {
			return errorResponse(fmt.Sprintf("save_cap: actions: %v", err))
		}
	}
	// An update needs something to change. The comparison is on the content — the
	// scripts and the metadata a CAP.md round-trip carries — and not on the version
	// number: Version counts local revisions (the auto-correct pass raises it on
	// every repair), so a file's number says nothing about whether it is news.
	//
	// The source revision can only hold the gate closed once a row HAS one. A row
	// written before ImportedVersion existed carries none, and for it the content is
	// the only signal there is: it takes the first differing file, which is the
	// point (its own counter was burned by repairs and means nothing), and that
	// import sets the watermark. From then on two directions are closed: a revision
	// older than the one the row came from is not an update (a leftover copy must
	// not roll a cap back), and the revision the row WAS imported from cannot be
	// newer than the row — after an import with a repair on top, the row is the
	// later state, and re-reading the same file would silently undo the repair.
	if oldMeta != nil && incomingVersion > 0 {
		imported := oldMeta.ImportedVersion
		// What this file's bytes hash to. Only a body the installation's manifest names
		// may overrule the row's watermark below. The value comes from the file the
		// daemon read itself and never from the params: a caller must not be able to
		// claim provenance it does not have. It is a second read of the scanned path, so
		// a writer inside the caps dir could move the two apart between them; that is the
		// same writer who could edit manifest.yaml, so the gate claims nothing against it.
		sourceProvenance := ""
		if sourcePath != "" {
			if data, readErr := capfile.ReadNoFollow(sourcePath); readErr == nil {
				sourceProvenance = textutil.BodyDigest(data)
			}
		}
		switch {
		case capUnchanged(*oldMeta, oldProject, meta, effectiveProject):
			return unchangedCap(oldID, "content matches")
		case imported > 0 && incomingVersion < imported:
			// A leftover copy from an older release must not roll the cap back. But a
			// body the installation's own manifest names is not a leftover: the
			// installer delivered it because the bundle descends from the body this row
			// came from, so the row's counter is what is behind (YM-450). Provenance
			// decides, not the counter.
			capsDir := ""
			if sourcePath != "" {
				capsDir = filepath.Dir(filepath.Dir(sourcePath))
			}
			installedDigest, tracked := installedCapBody(capsDir, name)
			vouched := sourceProvenance != "" && tracked && installedDigest == sourceProvenance
			switch {
			case !vouched:
				if sourceProvenance != "" {
					if tracked {
						log.Printf("[cap-provenance] %s: the file (v%d) hashes to %s, the installed manifest names %s; the row keeps its state (imported v%d)", name, incomingVersion, sourceProvenance, installedDigest, imported)
					} else {
						log.Printf("[cap-provenance] %s: the file (v%d) hashes to %s and no manifest entry vouches for it; the row keeps its state (imported v%d)", name, incomingVersion, sourceProvenance, imported)
					}
				}
				return unchangedCap(oldID, "older source revision")
			case capRowRepairedSinceImport(*oldMeta):
				// The row holds a repair on top of the body it imported, and that is the
				// later state. A delivery below the row's own watermark is a step back, so
				// it must not overwrite the repair: the row stays and the line says why.
				// (A delivery ABOVE the watermark is the other case and takes the file: an
				// installation that was auto-corrected still has to receive a newer bundle,
				// los32b.)
				log.Printf("[cap-provenance] %s: the delivered body (v%d, %s) is not written over the row: the row carries a repair on top of the body it imported (imported v%d)", name, incomingVersion, sourceProvenance, imported)
				return unchangedCap(oldID, "row repaired since its import")
			}
			// The row takes the delivered body wholesale, so it takes its revision too.
			// The counter of the replaced body describes a body that is gone, and keeping
			// it would make the export rewrite the file the installer just delivered.
			// meta is already built above, so both values move.
			version = incomingVersion
			meta.Version = version
		case imported > 0 && incomingVersion == imported &&
			oldMeta.ImportedDigest != "" && oldMeta.ImportedDigest != scriptDigest(oldMeta.Scripts):
			// The file still carries the revision this row was imported from, and the
			// row no longer holds what that import brought: someone repaired it. The
			// row is the later state. An untouched row takes the edit — bundled caps
			// are edited without a version bump.
			return unchangedCap(oldID, "row changed since that import")
		}
	}

	ctxJSON, err := meta.ToJSON()
	if err != nil {
		return errorResponse(fmt.Sprintf("save_cap: %v", err))
	}

	l := &models.Learning{
		Content:            content,
		Category:           "cap",
		Source:             source,
		Project:            effectiveProject,
		CanonicalProject:   canonicalProjectFor(effectiveProject),
		Confidence:         1.0,
		Context:            ctxJSON,
		Domain:             "code",
		TriggerRule:        fmt.Sprintf("cap:%s", name),
		Keywords:           tags,
		AnticipatedQueries: append([]string{name, description}, tags...),
		CreatedAt:          timeNow(),
		ContentHash:        hash,
	}

	// Capabilities get clean embedding text — name + description + tags, NOT raw JSON Context
	l.EmbeddingText = content
	if len(tags) > 0 {
		l.EmbeddingText += " " + strings.Join(tags, " ")
	}

	id, err := h.store.InsertLearning(l)
	if err != nil {
		return errorResponse(fmt.Sprintf("save_cap: %v", err))
	}

	if len(supersededIDs) > 0 {
		// One call for both rows. The store records the predecessor in a single
		// scalar (learnings.supersedes on the new row), so only the first ID of
		// this list gets a backward edge; both rows still carry superseded_by and
		// are gone from every active read.
		if err := h.store.SupersedeLearningBatch(supersededIDs, id, "cap version upgrade"); err != nil {
			log.Printf("save_cap: supersede %v → %d failed: %v", supersededIDs, id, err)
		}
	}

	// Post-mutation: embed + MEMORY.md regen
	embedText := content
	if l.EmbeddingText != "" {
		embedText = l.EmbeddingText
	}
	h.EmbedLearning(id, embedText, "cap", project)
	go h.onMutation()

	// Best-effort: write CAP.md to disk so it's visible as a file
	// Skip when import came from disk (_from_disk flag) to avoid overwriting user edits
	if _, fromDisk := params["_from_disk"]; !fromDisk {
		// The configured caps_dir is the directory the daemon imports from and the
		// watcher scans. Deriving the path from the data dir instead let the two
		// diverge whenever caps_dir is set (YM-346).
		capsDir := config.CapsDirPath(h.capsDir, h.dataDir)
		// effectiveProject, not the raw param: a save with scope=user is stored
		// project-agnostic, and the file must not claim a binding the row does not
		// have (it would also take the project-side sandbox repair on the way out).
		if err := WriteCapToDisk(meta, effectiveProject, capsDir, h.store, h.configuredDefaultSandbox()); err != nil {
			log.Printf("save_cap: write CAP.md for %s: %v", name, err)
		}
	}

	result := map[string]any{"id": id, "version": version, "name": name}
	if supersededID != nil {
		result["superseded"] = *supersededID
	}
	return jsonResponse(result)
}

// handleRegisterCaps generates JavaScript registerTool() code for saved caps.
// Claude executes this in the REPL to make caps available as native tools.
func (h *Handler) handleRegisterCaps(params map[string]any) Response {
	project, _ := params["project"].(string)
	tag, _ := params["tag"].(string)

	caps, err := h.capRowsForProject(project)
	if err != nil {
		return errorResponse(fmt.Sprintf("register_caps: %v", err))
	}

	var lines []string
	// One registration per name when the caller names a project:
	// capRowsForProject returns the caller's own rows before the global ones, so a
	// bound row shadows the global row of its name. Registering both would emit
	// two registerTool calls for one name, the later one replacing the caller's
	// own body. Without a project this is the inventory question, as in get_caps.
	seen := make(map[string]bool, len(caps))
	collapse := project != ""
	for _, l := range caps {
		meta, err := ParseCapMeta(l.Context)
		if err != nil {
			continue
		}
		if seen[meta.Name] && collapse {
			continue
		}
		// The code below is executable: a retired cap must not be handed out even
		// while its row is still active (YM-181, YM-197).
		if capfile.IsRetiredCap(meta.Name) {
			continue
		}
		if tag != "" && !meta.HasTag(tag) {
			continue
		}
		// Marked after the filters: a shadowing row that does not match the query
		// must not hide a global row that does.
		seen[meta.Name] = true

		for _, sc := range meta.ToolScripts() {
			fn := scriptToJSFunc(sc)
			fn = capfile.ProviderToGeneric(fn, capfile.DefaultAdapters())
			if sc.Runtime == "repl" && capfile.UsesStoreAdapter(fn) {
				fn = capfile.WrapToolWithStore(fn, meta.Name)
			}

			schemaJS := "{}"
			if sc.Schema != "" && json.Valid([]byte(sc.Schema)) {
				schemaJS = sc.Schema
			}
			line := fmt.Sprintf("registerTool(%q, %q, %s, %s);", sc.Name, meta.Description, schemaJS, fn)
			lines = append(lines, line)
		}
	}

	code := strings.Join(lines, "\n\n")
	if code != "" {
		sharedJS := capfile.GenerateAdapterJS(capfile.DefaultAdapters(), true)
		if sharedJS != "" {
			code = sharedJS + "\n\n" + code
		}
	}
	return jsonResponse(map[string]any{"code": code, "count": len(lines)})
}

// parseTagsParam handles both []any (JSON array) and comma-separated string.
func parseTagsParam(params map[string]any) []string {
	if arr, ok := params["tags"].([]any); ok {
		var tags []string
		for _, v := range arr {
			if s, ok := v.(string); ok && s != "" {
				tags = append(tags, s)
			}
		}
		return tags
	}
	if csv := stringOr(params, "tags", ""); csv != "" {
		var tags []string
		for _, t := range strings.Split(csv, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				tags = append(tags, trimmed)
			}
		}
		return tags
	}
	return nil
}

// unchangedCap is the "nothing to do" answer of save_cap. It carries the reason:
// "the file brings no change" and "the row is newer than the file" are opposite
// situations for an operator reading the sync log, and both look like "unchanged".
func unchangedCap(id int64, reason string) Response {
	return jsonResponse(map[string]any{"id": id, "status": "unchanged", "reason": reason})
}

// parseRequiresParam reads the adapter declarations of a cap. CAP.md carries them
// as a comma-separated frontmatter list (CapFileToParams joins them the same way),
// a caller may also pass an array.
func parseRequiresParam(params map[string]any) []string {
	if arr, ok := params["requires"].([]any); ok {
		var out []string
		for _, v := range arr {
			if s, ok := v.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	}
	csv := stringOr(params, "requires", "")
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// handleActivateCap marks a cap active for the current thread
// and returns registerTool() JS code that the REPL can execute to make the
// cap tool-callable. The activation row persists in
// session_active_caps so the proxy can re-inject the registration on
// subsequent turns without the caller re-running the code.
//
// IMPORTANT: Call this as a native MCP tool, not from the REPL. Claude Code's
// REPL VM binds only a subset of MCP tools as JS globals (exact selection is
// VM-init-time). Calling activate_cap from the REPL may fail with a
// confusing ReferenceError.
func (h *Handler) handleActivateCap(params map[string]any) Response {
	name := stringOr(params, "name", "")
	if name == "" {
		return errorResponse("activate_cap: 'name' is required")
	}
	threadID := h.resolveSessionID(params, "thread_id")
	if threadID == "" {
		return errorResponse("activate_cap: 'thread_id' is required")
	}
	project := repo.RootOrSelf(stringOr(params, "project", ""))

	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		return errorResponse(fmt.Sprintf("activate_cap: %v", err))
	}

	// Capability names are unique (enforced by validateCapName at save
	// time in handleSaveCap), so the caller's own row is the answer. A row of
	// another project is only remembered as a fact: picking it because it
	// happened to come first made a newer foreign row shadow the caller's own cap,
	// and naming it in the error text told an unauthorised caller where that
	// project lives (YM-427).
	var match, foreign *models.Learning
	var meta CapMeta
	for i := range caps {
		m, err := ParseCapMeta(caps[i].Context)
		if err != nil {
			continue
		}
		if capfile.IsRetiredCap(m.Name) {
			continue
		}
		if m.Name != name {
			continue
		}
		// Project scope: user-scoped caps (cap_scope=="user") are project-agnostic.
		// Legacy caps without explicit scope still enforce project matching for
		// backward compat. The question is the one the catalog and execute_cap ask,
		// so it is answered by the same rule — exact string equality refused a cap
		// saved from a worktree session (YM-373).
		if m.Scope != "user" && caps[i].Project != "" && !capProjectMatches(caps[i].Project, project) {
			if foreign == nil {
				foreign = &caps[i]
			}
			continue
		}
		// The caller's own row is the answer. A global row must not win by list
		// order (newest-first): the code handed out here is what the REPL
		// registers, and execute_cap resolves the same name own-first through
		// loadCapMeta, so registering the global body while running the caller's
		// own would split one cap into two behaviors.
		if match != nil && caps[i].Project == "" {
			continue
		}
		match = &caps[i]
		meta = m
		if caps[i].Project != "" {
			break
		}
	}
	if match == nil {
		if foreign != nil {
			// The row's own project path stays out of the text: it tells a caller
			// without access where another project lives. The refusal still says
			// why it refused and which project the call came from, and the log keeps
			// the diagnosis the caller no longer gets (the ordinary case is a cap
			// bound to a worktree while the call runs in main, or the other way
			// round).
			log.Printf("[cap] activate_cap refused %q: bound to project %q, caller in %q", name, foreign.Project, project)
			return errorResponse(fmt.Sprintf(
				"activate_cap: cap %q is scoped to another project (caller project=%q)",
				name, project,
			))
		}
		return errorResponse(fmt.Sprintf("activate_cap: no cap found with name %q", name))
	}

	tools := meta.ToolScripts()
	if len(tools) == 0 {
		return errorResponse(fmt.Sprintf("activate_cap: cap %q has no tool scripts (only handlers)", name))
	}

	var regLines []string
	usesAdapters := false
	for _, sc := range tools {
		fn := scriptToJSFunc(sc)
		fn = capfile.ProviderToGeneric(fn, capfile.DefaultAdapters())
		if capfile.UsesGenericAdapters(fn) {
			usesAdapters = true
		}
		if sc.Runtime == "repl" && capfile.UsesStoreAdapter(fn) {
			fn = capfile.WrapToolWithStore(fn, meta.Name)
		}
		schemaJS := "{}"
		if sc.Schema != "" && json.Valid([]byte(sc.Schema)) {
			schemaJS = sc.Schema
		}
		regLines = append(regLines, fmt.Sprintf("registerTool(%q, %q, %s, %s);", sc.Name, meta.Description, schemaJS, fn))
	}

	code := strings.Join(regLines, "\n")
	if usesAdapters {
		sharedJS := capfile.GenerateAdapterJS(capfile.DefaultAdapters(), true)
		if sharedJS != "" {
			code = sharedJS + "\n\n" + code
		}
	}

	if err := h.store.ActivateCap(threadID, name); err != nil {
		return errorResponse(fmt.Sprintf("activate_cap: %v", err))
	}

	return jsonResponse(map[string]any{
		"code":        code,
		"name":        name,
		"version":     meta.Version,
		"description": meta.Description,
	})
}

// handleDeactivateCap removes the activation row for (thread, name).
// After deactivation the proxy will not re-inject the cap's
// registerTool snippet on subsequent turns.
func (h *Handler) handleDeactivateCap(params map[string]any) Response {
	name := stringOr(params, "name", "")
	if name == "" {
		return errorResponse("deactivate_cap: 'name' is required")
	}
	threadID := h.resolveSessionID(params, "thread_id")
	if threadID == "" {
		return errorResponse("deactivate_cap: 'thread_id' is required")
	}

	deleted, err := h.store.DeactivateCap(threadID, name)
	if err != nil {
		return errorResponse(fmt.Sprintf("deactivate_cap: %v", err))
	}
	return jsonResponse(map[string]any{
		"name":        name,
		"deactivated": deleted,
	})
}

// capProjectVisible reports whether a cap scoped to capProject is visible to a
// caller in project. Global caps (project=="") are visible everywhere.
//
// Visibility is the same question execute_cap answers in loadCapMeta, so it is
// answered by the same rule (capProjectMatches): canonical basename plus
// repository root. Judging it from exact path equality instead hid a cap that a
// worktree session had saved, while execute_cap ran it (YM-373).
//
// The caller names one value only. An earlier version also asked with the bare
// basename of an absolute path, which turned the relative-row half of the rule
// into a back door: a row stored as "yesmem/.worktrees/w1" matched every checkout
// of that name in the catalog while execute_cap refused it (YM-404).
func capProjectVisible(capProject, project string) bool {
	if capProject == "" {
		return true
	}
	return capProjectMatches(capProject, project)
}

// capRowRank orders the rows that carry one name for a caller: its own row first,
// then a global row, then a row of another project that only a thread-scoped
// activation can still reach. It is the same preference loadCapMeta applies.
func capRowRank(l *models.Learning, project string) int {
	switch {
	case l.Project == "":
		return 1
	case capProjectVisible(l.Project, project):
		return 0
	default:
		return 2
	}
}

// handleGetActiveCaps returns the caps currently active for
// the given thread, each with its full meta. Intended for the proxy's
// schema-injection pipeline — not exposed as a user-facing MCP tool.
// Results are scoped to the caller's project: project-scoped caps of other
// projects are filtered out, so a session never sees a foreign project's caps.
func (h *Handler) handleGetActiveCaps(params map[string]any) Response {
	threadID := h.resolveSessionID(params, "thread_id")
	if threadID == "" {
		return errorResponse("get_active_caps: 'thread_id' is required")
	}

	// Project scope of the requesting session. The proxy sends the canonical
	// absolute path; a bare basename is accepted as a legacy fallback and then
	// reaches global caps only. A git worktree path resolves to its main repo so
	// repo-scoped caps still match.
	project := repo.RootOrSelf(stringOr(params, "project", ""))

	active, err := h.store.GetSessionCaps(threadID)
	if err != nil {
		return errorResponse(fmt.Sprintf("get_active_caps: %v", err))
	}

	if parentID, ok := params["parent_thread_id"].(string); ok && parentID != "" && parentID != threadID {
		parentActive, err := h.store.GetSessionCaps(parentID)
		if err != nil {
			log.Printf("[caps] parent_thread_id=%s lookup failed: %v", parentID, err)
		} else {
			active = append(active, parentActive...)
		}
	}

	caps, err := h.store.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		return errorResponse(fmt.Sprintf("get_active_caps: %v", err))
	}
	byName := make(map[string]*models.Learning)
	metaByName := make(map[string]CapMeta)
	autoActiveNames := []string{}
	for i := range caps {
		m, err := ParseCapMeta(caps[i].Context)
		if err != nil {
			continue
		}
		if capfile.IsRetiredCap(m.Name) {
			continue
		}
		// One row per name, and the best-ranked row answers: the caller's own row,
		// else a global one, else a foreign row that only a thread-scoped
		// activation reaches. Order alone must not decide — the list is
		// newest-first, so a global row would lose to a foreign row it cannot even
		// see, and a project copy that shadows a global cap would be described from
		// the foreign row while execute_cap runs the caller's own (YM-402 keeps
		// both rows alive).
		cur := &caps[i]
		if prev, ok := byName[m.Name]; ok && capRowRank(prev, project) <= capRowRank(cur, project) {
			// The row already claimed is at least as good as this one.
		} else {
			byName[m.Name] = cur
			metaByName[m.Name] = m
		}
		// Auto-active caps are injected without explicit activation, so they
		// must be project-scoped: another project's auto-active cap must not
		// appear. Explicitly activated (session) caps stay resolvable via
		// byName regardless of project — activation is thread-scoped, and
		// filtering them here would silently drop a session's own caps when a
		// caller cannot supply its project.
		if m.AutoActive && capProjectVisible(cur.Project, project) {
			autoActiveNames = append(autoActiveNames, m.Name)
		}
	}

	results := []capResult{}
	seen := make(map[string]bool)
	for _, a := range active {
		if seen[a.CapName] {
			continue
		}
		l, ok := byName[a.CapName]
		if !ok {
			continue
		}
		results = append(results, capResult{
			ID:      l.ID,
			Project: l.Project,
			Source:  l.Source,
			Meta:    metaByName[a.CapName],
		})
		seen[a.CapName] = true
	}
	for _, name := range autoActiveNames {
		if seen[name] {
			continue
		}
		l := byName[name]
		results = append(results, capResult{
			ID:      l.ID,
			Project: l.Project,
			Source:  l.Source,
			Meta:    metaByName[name],
		})
		seen[name] = true
	}
	return jsonResponse(results)
}
