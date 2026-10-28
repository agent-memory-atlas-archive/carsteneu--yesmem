package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Config holds the ingest pipeline configuration.
type Config struct {
	Project           string
	Domain            string // code, marketing, legal, finance, general
	DryRun            bool
	Destill           bool                 // run LLM destillation (Phase 6)
	LLMClient         extraction.LLMClient // required when Destill=true, can be nil otherwise
	Name              string               // doc source name
	Version           string               // doc source version
	TriggerExtensions string               // comma-separated, e.g. ".go,.mod" — auto-inject docs for these file types
	DocType           string               // "reference" (auto-inject) or "style" (explicit search only)
}

// Result holds the outcome of an ingest run.
type Result struct {
	FilesProcessed      int
	FilesSkipped        int // not indexed: unchanged hash, oversized, unreadable, or a named source that is not a document (a walk does not count the files it never considered)
	FilesRemoved        int // documents whose chunks were swept because the file is gone from a completely walked tree
	UnreadableEntries   int // walk entries dropped because they could not be read; a directory is not a file, so this is not FilesSkipped
	ChunksCreated       int
	ChunksUpdated       int
	LearningsSuperseded int
	LearningsCreated    int // from destillation
}

// maxDocFileBytes caps one source document. Documentation is prose; a file
// beyond this is a log, a dump or an accident, and chunking it would flood the
// index with thousands of chunks in one sync. Variable so a test can exercise
// the bound without a multi-megabyte fixture (same shape as
// internal/update/download.go).
var maxDocFileBytes int64 = 20 << 20

// pdfExtractTimeout bounds one pdftotext run. A converter that hangs on a
// malformed PDF must not stall the documents after it in the same run, and the
// daemon's doc-sync tick pays that stall too. Variable so a test can shorten it
// (same shape as maxDocFileBytes).
var pdfExtractTimeout = 60 * time.Second

// Run executes the ingest pipeline for given paths.
func Run(cfg Config, paths []string, store *storage.Store) (*Result, error) {
	result := &Result{}
	var changedChunks []storage.DocChunk

	// A doc source is re-read later by doc-sync, which runs in another process
	// with a different working directory. Registration is the only moment the
	// caller's working directory carries meaning, so the path is resolved here
	// and stored absolute.
	absPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			// filepath.Abs("") returns the working directory without an error.
			return nil, fmt.Errorf("empty source path")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		absPaths = append(absPaths, abs)
	}
	paths = absPaths

	// The sweep below may only touch rows that existed before this pass looked
	// at the tree. Doc-sync ticks and the ingest handler run in different
	// goroutines and can ingest the same source at once: a document the other
	// pass indexed is absent from this walk and would look deleted. Rows written
	// later carry higher ids than the watermark. 0 means "unknown" and disables
	// the sweep, which is the safe direction.
	watermark := int64(0)
	if !cfg.DryRun {
		w, wErr := store.MaxDocChunkID()
		if wErr != nil {
			log.Printf("warn: doc chunk watermark: %q; skipping the deletion sweep", wErr)
		} else {
			watermark = w
		}
	}

	// 1. Collect all files
	var files []sourceFile
	// walkComplete reports whether the collected set is the whole tree. The
	// deletion sweep below trusts that set, and last_sync is compared against a
	// document's mtime: after a partial walk a document in the unreadable part
	// would both look deleted and look out of date.
	walkComplete := true
	// completeWalks records, per source path, whether the look at that path is
	// the whole tree. The sweep anchors on the registered path, so it needs the
	// completeness of that one walk: with several paths a partial look at one of
	// them must not lend its authority to another.
	completeWalks := make(map[string]bool, len(paths))
	// basePath is the anchor for the stored relative chunk paths. It has to come
	// from a path that yielded files: taking paths[0] would anchor everything on
	// a source that was skipped.
	basePath := ""
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			// A path that no longer resolves is skipped like the other unusable
			// ones: one bad path in a multi-path run must not cost the caller the
			// paths that are indexable.
			log.Printf("Skipped source %q: %q", p, err)
			result.FilesSkipped++
			// "Does not exist" is a complete observation of absence: the path has
			// no documents, so the sweep may act on it. Any other error is a
			// partial look at a tree that may well hold documents.
			completeWalks[p] = os.IsNotExist(err)
			if !completeWalks[p] {
				walkComplete = false
			}
			continue
		}
		if info.IsDir() {
			// filepath.WalkDir Lstats its root, so a source that is a symlink to
			// a directory reached the callback as a link and yielded nothing,
			// although os.Stat above followed the link and called it a
			// directory. Walk the target but keep every entry named after the
			// registered path: the stored relative paths, the basePath anchor
			// and the os.OpenInRoot root stay what the caller registered.
			walkRoot := p
			pathComplete := true
			if linfo, lerr := os.Lstat(p); lerr != nil {
				// Stat followed the link a moment ago, so the link is gone now.
				// The walk would see nothing and the sweep would read that as
				// "every document is gone".
				pathComplete = false
			} else if linfo.Mode()&fs.ModeSymlink != 0 {
				resolved, rerr := filepath.EvalSymlinks(p)
				if rerr != nil {
					// Same reason: the link broke between the stat and this
					// resolution.
					pathComplete = false
				} else {
					walkRoot = resolved
				}
			}
			if !pathComplete {
				walkComplete = false
			}
			links, firstLink := 0, ""
			irregular := 0
			before := len(files)
			if err := filepath.WalkDir(walkRoot, func(walkPath string, d fs.DirEntry, err error) error {
				path := walkPath
				if walkRoot != p {
					if rel, relErr := filepath.Rel(walkRoot, walkPath); relErr == nil {
						path = filepath.Join(p, rel)
					}
				}
				if err != nil {
					// An unreadable subdirectory costs its own subtree only.
					// The daemon calls Run once per source, so returning the
					// error cost it that one source per tick; the CLI passes
					// several paths at once and lost all of them. The root
					// stays fatal: an unusable source is the caller's error.
					if walkPath == walkRoot {
						return err
					}
					log.Printf("Skipped unreadable entry %q: %q", path, err)
					result.UnreadableEntries++
					pathComplete = false
					walkComplete = false
					if d != nil && d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				// The type comes with the directory entry, so this is one
				// decision about one entry, not a check the entry can slip past
				// before the read (YM-277).
				if d.Type()&fs.ModeSymlink != 0 {
					links++
					if firstLink == "" {
						firstLink = path
					}
					return nil
				}
				// A FIFO named like a document blocks os.Open forever and never
				// yields content; a device file has no end. Neither is a document.
				if !d.Type().IsRegular() {
					irregular++
					return nil
				}
				if isDocFile(path) {
					files = append(files, sourceFile{root: p, path: path})
				}
				return nil
			}); err != nil {
				return nil, fmt.Errorf("walk %q: %w", p, err)
			}
			if links > 0 {
				log.Printf("Skipped %d symlink(s) in %q (first: %q)", links, p, firstLink)
			}
			if irregular > 0 {
				log.Printf("Skipped %d non-regular entry/entries in %q", irregular, p)
			}
			if len(files) > before && basePath == "" {
				basePath = p
			}
			completeWalks[p] = pathComplete
		} else {
			// A source named directly bypassed every gate the walk applies: it
			// was opened whatever its type and chunked as markdown whatever its
			// extension. It is skipped, not fatal: one bad path in a multi-path
			// run must not cost the caller the paths that are indexable.
			switch {
			case !info.Mode().IsRegular():
				log.Printf("Skipped source %q: not a regular file", p)
				result.FilesSkipped++
			case !isDocFile(p):
				log.Printf("Skipped source %q: no indexable format (want one of %s)", p, docFormatsList())
				result.FilesSkipped++
			default:
				files = append(files, sourceFile{root: filepath.Dir(p), path: p})
				if basePath == "" {
					basePath = p
				}
			}
			// A named path is one observation of one file: nothing below it
			// exists that a walk of it could have missed.
			completeWalks[p] = true
		}
	}

	if len(files) == 0 {
		log.Printf("No indexable files found in %q", paths)
		// An existing source keeps its old last_sync here, and doc-sync compares
		// that to the source path's mtime: it would re-run this unusable source
		// on every tick, forever. Refresh only when the row's own path is one of
		// the paths that just yielded nothing; stamping an unrelated source would
		// swallow a sync that its changed file is waiting for. An incomplete walk
		// is not a completed look at the tree either, so it does not stamp.
		if src, err := store.GetDocSource(cfg.Name, cfg.Project); err == nil && src != nil && pathListed(src.Path, paths) {
			// Nothing yielded a document. For a source whose own path is the one
			// that yielded nothing that means the whole tree is gone, which is
			// the same loss the sweep handles for a subdirectory: without it the
			// chunks of the last document stay in the index. Only an explicit
			// ingest reaches this point: doc-sync and sync-docs skip a source
			// whose path no longer exists before they call Run.
			if !cfg.DryRun && src.Path != "" && completeWalks[src.Path] && watermark > 0 {
				if err := sweepRemovedDocuments(store, src.ID, nil, src.Path, completeWalks, watermark, result); err != nil {
					log.Printf("warn: sweep removed documents: %q", err)
				}
			}
			switch {
			case cfg.DryRun:
				// A dry run writes nothing, here either: stamping would hide the
				// loss the caller only previewed and stop the sweep for good.
			case walkComplete:
				if err := store.UpdateDocSourceStats(src.ID); err != nil {
					log.Printf("warn: refresh stats of unusable source %q: %q", cfg.Name, err)
				}
			default:
				if err := store.UpdateDocSourceChunkCount(src.ID); err != nil {
					log.Printf("warn: refresh chunk count of %q: %q", cfg.Name, err)
				}
			}
		}
		return result, nil
	}

	// 2. Upsert doc source
	ds := &storage.DocSource{
		Name:              cfg.Name,
		Version:           cfg.Version,
		Path:              basePath,
		Project:           cfg.Project,
		TriggerExtensions: cfg.TriggerExtensions,
		DocType:           cfg.DocType,
	}

	// A dry run must not write. Registering here would move the source's row for
	// a pass that indexed nothing, and a row at the epoch stamp schedules a real
	// ingest on the next tick, i.e. exactly the work the caller only previewed.
	// The existing id is read so the per-file change check still compares
	// against real chunks.
	//
	// previousPath is the basePath the stored rows of this source are keyed on.
	previousPath := ""
	sourceID := int64(0)
	if src, err := store.GetDocSource(cfg.Name, cfg.Project); err == nil && src != nil {
		previousPath = src.Path
		sourceID = src.ID
	}
	if !cfg.DryRun {
		id, err := store.UpsertDocSource(ds)
		if err != nil {
			return nil, fmt.Errorf("upsert doc source: %w", err)
		}
		sourceID = id
	}

	// 3. Process each file
	for i, file := range files {
		log.Printf("[%d/%d] Processing %q", i+1, len(files), file.path)

		processed, deltaChunks, err := processFile(cfg, file, basePath, sourceID, store, result)
		if err != nil {
			log.Printf("  ERROR: %q", err)
			// Count it: an oversized or unreadable document used to vanish from
			// both counters, so the caller read "0 processed, 0 skipped" while
			// the reason sat in the daemon log only.
			result.FilesSkipped++
			continue
		}
		if !processed {
			result.FilesSkipped++
			continue
		}
		result.FilesProcessed++
		changedChunks = append(changedChunks, deltaChunks...)
	}

	// 3b. Sweep the chunks of documents that are gone. A document deleted inside
	// a walked tree never reaches processFile: the walk simply does not yield
	// it, so its chunks stayed in the index forever and docs_search kept serving
	// a file that no longer exists. The walked file set is the only witness,
	// which is why the sweep requires a complete walk of the tree it judges.
	//
	// That tree is the registered previousPath, not this run's basePath: with
	// several paths basePath is only "the first path that yielded files", so it
	// moves between runs, while the rows were keyed on whichever path wrote
	// them. Anchoring on basePath therefore either skipped the sweep entirely
	// (the two differed) or, had it judged the rows of another run's anchor,
	// deleted documents that are still on disk. previousPath is the path the
	// caller registered, and completeWalks carries the completeness of exactly
	// that walk. Rows keyed outside it are judged under the root they name,
	// and only when this run walked that root (see sweepRemovedDocuments). The
	// keys this run maps are counted under the registered anchor and, when it
	// differs, under the base path too (see presentDocuments): a document under
	// a moved base can share its relative path with an older row.
	//
	// The sweep mirrors the disk, so a tree that is only temporarily absent (an
	// unmounted volume, a directory moved aside) loses its chunks, and the
	// mtime-driven doc-sync gate does not bring them back on its own once the
	// files return with unchanged mtimes. That gap predates the sweep for
	// documents whose edit kept an old mtime; a manual re-ingest restores the
	// chunks. It does not restore the learnings distilled from them: those are
	// retired here for good, exactly as a changed chunk retires the learnings of
	// its old content. The same applies to a source re-registered under another
	// path: its rows are keyed on the old base and are swept as soon as the new
	// path is the registered one.
	if !cfg.DryRun && previousPath != "" && completeWalks[previousPath] && watermark > 0 {
		anchors := []string{previousPath}
		if basePath != previousPath {
			anchors = append(anchors, basePath)
		}
		if err := sweepRemovedDocuments(store, sourceID, presentDocuments(anchors, files), previousPath, completeWalks, watermark, result); err != nil {
			log.Printf("warn: sweep removed documents: %q", err)
		}
	}

	// 4. Update source stats. last_sync is the "this pass saw the whole tree"
	// stamp that doc-sync compares a document's mtime against, so a walk that
	// dropped an entry must not move it: the dropped subtree would go
	// unretried until some unrelated change. Not stamping keeps the source due
	// on the next tick, which is the retry this exists for.
	if !cfg.DryRun {
		if walkComplete {
			store.UpdateDocSourceStats(sourceID)
		} else if err := store.UpdateDocSourceChunkCount(sourceID); err != nil {
			log.Printf("warn: refresh chunk count of %q: %q", cfg.Name, err)
		}
	}

	// 5. Destillation: extract learnings from chunks via LLM
	if cfg.Destill && !cfg.DryRun {
		if cfg.LLMClient == nil {
			log.Printf("warn: --destill requested but no LLM client configured, skipping")
		} else {
			if len(changedChunks) > 0 {
				log.Printf("Destilling %d changed chunks via %s...", len(changedChunks), cfg.LLMClient.Model())
				created, err := DestillChunks(changedChunks, cfg, cfg.LLMClient, store)
				if err != nil {
					log.Printf("warn: destillation: %q", err)
				}
				result.LearningsCreated = created
				log.Printf("Destillation complete: %d learnings created", created)
			}
		}
	}

	return result, nil
}

// sourceFile is one collected document plus the directory it was collected
// under. Reads resolve against that directory with os.OpenInRoot, so an entry
// that is or becomes a symlink cannot lead out of the document tree, and the
// containment decision and the open are one operation (YM-277).
type sourceFile struct {
	root string
	path string
}

// docFormats lists the extensions the chunkers handle. The directory walk and
// a directly named source file both apply this list, so one path cannot index a
// format the other refuses.
var docFormats = []string{".md", ".txt", ".rst", ".pdf"}

// docFormatsList renders the supported extensions for an error message.
func docFormatsList() string { return strings.Join(docFormats, ", ") }

// pathListed reports whether p is one of the given source paths.
func pathListed(p string, paths []string) bool {
	for _, candidate := range paths {
		if candidate == p {
			return true
		}
	}
	return false
}

// isDocFile reports whether the path carries an indexed document format.
func isDocFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range docFormats {
		if ext == allowed {
			return true
		}
	}
	return false
}

func processFile(cfg Config, file sourceFile, basePath string, sourceID int64, store *storage.Store, result *Result) (bool, []storage.DocChunk, error) {
	path := file.path
	// Compute relative path for storage (portable across machines)
	relPath, err := filepath.Rel(basePath, path)
	if err != nil {
		relPath = path // fallback to absolute if Rel fails
	}

	// Read file content (handle .pdf via pdftotext)
	content, err := readFile(file.root, path)
	if err != nil {
		return false, nil, err
	}

	// SHA256 hash
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	// Check existing chunks for this file
	existing, _ := store.GetDocChunksByFile(relPath, sourceID)
	if len(existing) > 0 && existing[0].SourceHash == hash {
		log.Printf("  Unchanged (hash match), skipping")
		return false, nil, nil
	}

	if cfg.DryRun {
		chunks := chunkByFormat(path, content)
		log.Printf("  DRY RUN: would create %d chunks", len(chunks))
		return true, nil, nil
	}

	newChunks := chunkByFormat(path, content)
	existingByHash := make(map[string][]storage.DocChunk, len(existing))
	for _, chunk := range existing {
		existingByHash[chunk.ContentHash] = append(existingByHash[chunk.ContentHash], chunk)
	}

	var changedChunks []storage.DocChunk
	var removedChunkIDs []int64
	var retainedExistingIDs = make(map[int64]bool, len(existing))

	for _, c := range newChunks {
		if matches := existingByHash[c.ContentHash]; len(matches) > 0 {
			match := matches[0]
			existingByHash[c.ContentHash] = matches[1:]
			retainedExistingIDs[match.ID] = true
			continue
		}
		dc := &storage.DocChunk{
			SourceID:     sourceID,
			SourceFile:   relPath,
			SourceHash:   hash,
			HeadingPath:  c.HeadingPath,
			SectionLevel: c.SectionLevel,
			Content:      c.Content,
			ContentHash:  c.ContentHash,
			TokensApprox: c.TokensApprox,
			Metadata:     c.Metadata,
		}

		id, err := store.InsertDocChunk(dc)
		if err != nil {
			log.Printf("  ERROR inserting chunk: %q", err)
			continue
		}
		dc.ID = id
		result.ChunksCreated++
		result.ChunksUpdated++
		changedChunks = append(changedChunks, *dc)
	}

	for _, chunk := range existing {
		if retainedExistingIDs[chunk.ID] {
			continue
		}
		removedChunkIDs = append(removedChunkIDs, chunk.ID)
	}

	if len(removedChunkIDs) > 0 {
		supersededIDs, supersedeErr := store.SupersedeByDocChunkRefs(sourceID, removedChunkIDs, "re-ingest: chunk changed")
		if supersedeErr != nil {
			log.Printf("  ERROR superseding learnings of the replaced chunks: %q", supersedeErr)
		}
		result.LearningsSuperseded += len(supersededIDs)
		if err := store.DeleteDocChunksByIDs(removedChunkIDs); err != nil {
			log.Printf("  ERROR deleting old chunks: %q", err)
		}
	}

	if err := store.UpdateDocChunksSourceHashByFile(relPath, sourceID, hash); err != nil {
		log.Printf("  ERROR updating source hash on retained chunks: %q", err)
	}

	log.Printf("  Incremental sync: %d new/changed chunks, %d unchanged, %d removed",
		len(changedChunks), len(retainedExistingIDs), len(removedChunkIDs))
	return true, changedChunks, nil
}

// containedInBase reports whether a stored source_file can belong to a walk of
// basePath: "." or a relative path that stays inside it. A multi-path
// registration writes the roots below the others as "../...", and such a row
// must never be read as a deleted document of this tree.
func containedInBase(rel string) bool {
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	cleaned := filepath.Clean(rel)
	return cleaned != ".." && !strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

// walkedCovers reports whether target lies inside a path this run walked
// completely. A key outside the anchor can only be judged by the run that
// walked the root it names: the daemon syncs one path per source, so a
// multi-path registration leaves "../" rows the daemon has no witness for.
func walkedCovers(target string, walked map[string]bool) bool {
	for root, complete := range walked {
		if !complete {
			continue
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		if containedInBase(rel) {
			return true
		}
	}
	return false
}

// sweepRemovedDocuments deletes the chunks of documents that are no longer part
// of a completely walked tree, and supersedes the learnings derived from them.
// present carries the relative paths this pass treats as present: the keys
// processFile writes, plus the same documents under the registered anchor (see
// presentDocuments). A nil map means "this pass saw nothing", so every contained
// row is a deletion candidate. watermark bounds the rows this sweep may touch;
// see the caller for why.
//
// anchor is the path the stored keys are relative to, walked carries every path
// this run walked with its completeness. A key outside the anchor is judged only
// when this run walked the root it names (walkedCovers); a key inside it is
// judged blind only when the index carries no foreign key at all. A "../" key is
// the proof that rows were written relative to another base, and after the base
// moves those rows' plain keys look exactly like the anchor's (YM-374).
//
// The sweep follows the walk's inclusion rules, not only "does the file exist":
// a path that turned into a symlink or another non-regular entry is no longer
// indexed, so its old chunks go as well. Run logs those entries separately.
func sweepRemovedDocuments(store *storage.Store, sourceID int64, present map[string]bool, anchor string, walked map[string]bool, watermark int64, result *Result) error {
	indexed, err := store.GetDocChunkFiles(sourceID)
	if err != nil {
		return err
	}
	foreign := false
	for _, rel := range indexed {
		if filepath.IsAbs(rel) || containedInBase(rel) {
			continue
		}
		foreign = true
		break
	}
	hintedRetention := false
	for _, rel := range indexed {
		if present[rel] {
			continue
		}
		if filepath.IsAbs(rel) {
			// A key that names an absolute path comes from a run whose Rel failed.
			// The relative keys of this walk cannot describe it, so it is never
			// judged (same rule as containedInBase, kept explicit here because the
			// foreign branch below would otherwise resolve it through a walk root).
			continue
		}
		if containedInBase(rel) {
			if present == nil && foreign {
				// An empty pass with foreign keys in the index: a plain key can
				// belong to another root of a multi-path registration, whose rows
				// were keyed relative to a base that has moved since, and its
				// document may be on disk elsewhere. There is no witness for it,
				// so it stays until a pass walks its root. The price is a row that
				// can outlive its file: an empty pass stamps last_sync, so only a
				// document that reappears or an explicit remove-docs resolves it.
				if !hintedRetention {
					log.Printf("  note: %q stays in the index although this pass saw no documents — an anchor beside this tree has rows keyed elsewhere; 'yesmem remove-docs --name <source>' removes it explicitly", rel)
					hintedRetention = true
				}
				continue
			}
		} else if !walkedCovers(filepath.Clean(filepath.Join(anchor, rel)), walked) {
			// A row keyed outside this tree (a multi-path registration stores the
			// other roots as "../...") is not this walk's business unless the walk
			// covered that root: never delete what this walk could not have seen.
			continue
		}
		if err := removeDocumentChunks(store, sourceID, rel, watermark, result); err != nil {
			// One document's removal is atomic, the sweep is not: a failure here leaves that
			// document fully in place and must not stop the rest of the run from mirroring
			// the tree (cold review Los 56h).
			log.Printf("  warn: remove document %q: %v", rel, err)
		}
	}
	return nil
}

// presentDocuments maps the stored relative paths of the documents this pass
// saw, under every anchor the index may have keyed them on: the registered path
// whose rows the sweep judges, and the base path this run writes with. The two
// can differ when basePath moves (a multi-path run whose first root yields
// nothing, an alternating root order). A document under the new base can then
// carry the same relative path as an older row under the registered anchor, and
// processFile keeps that older row on a content match instead of writing a new
// one. Judging only the registered anchor would read that live document as
// deleted and take its learnings with it.
func presentDocuments(anchors []string, files []sourceFile) map[string]bool {
	present := make(map[string]bool, len(files))
	for _, anchor := range anchors {
		for _, f := range files {
			rel, err := filepath.Rel(anchor, f.path)
			if err != nil {
				rel = f.path
			}
			present[rel] = true
		}
	}
	return present
}

// SweepUnreachableSource removes every chunk of a source that has no registered
// path, and supersedes the learnings derived from them. Run compares a complete
// walk against the index; a source without a path has no tree to walk, so the
// only consistent reading of its index is that none of its documents is
// observable any more. The writers in this tree always store a path, so this
// covers rows that predate or bypass them (an older binary, a manual edit).
//
// watermark is the caller's reading taken before it listed the sources, and 0
// disables the sweep: rows above it may belong to a concurrent pass that
// re-registered the source with a path, and this sweep has no tree to judge them
// by (same rule as Run). The path itself is read by the store inside the same
// write transaction as the delete (see Store.SweepPathlessSourceChunks), so a
// caller-side re-read cannot close the window but does not have to either.
func SweepUnreachableSource(store *storage.Store, sourceID, watermark int64) (*Result, error) {
	result := &Result{}
	chunks, files, learnings, err := store.SweepPathlessSourceChunks(sourceID, watermark)
	if err != nil {
		return result, err
	}
	result.FilesRemoved = files
	result.LearningsSuperseded = len(learnings)
	if chunks > 0 {
		log.Printf("  Removed %d chunk(s) of %d document(s) of a source without a path", chunks, files)
	}
	return result, nil
}

// removeDocumentChunks deletes the chunks of one indexed document and retires the
// learnings derived from them. watermark bounds the rows it may touch. Both steps run in
// one write transaction (Store.SweepDocChunksByFile): a removal that fails halfway used
// to leave the learnings retired while the document stayed in the index (YM-395).
func removeDocumentChunks(store *storage.Store, sourceID int64, rel string, watermark int64, result *Result) error {
	chunks, learnings, err := store.SweepDocChunksByFile(sourceID, rel, watermark, "re-ingest: document removed")
	if err != nil {
		return err
	}
	if chunks == 0 {
		// Only rows from a concurrent pass: this walk cannot judge them.
		return nil
	}
	result.LearningsSuperseded += len(learnings)
	result.FilesRemoved++
	log.Printf("  Removed %d chunk(s) of deleted document %q", chunks, rel)
	return nil
}

// boundedWriter buffers at most limit bytes and records that it dropped the
// rest. It always reports a full write so the producing process keeps running
// instead of blocking on a pipe nobody drains; only the buffer is bounded.
type boundedWriter struct {
	buf     bytes.Buffer
	limit   int64
	overrun bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.overrun {
		return len(p), nil
	}
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) > room {
		if room > 0 {
			w.buf.Write(p[:room])
		}
		w.overrun = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

// readFile reads a document, using pdftotext for PDFs. The open is anchored at
// sourceRoot, so a link that resolves outside the document tree cannot be read
// and no window opens between a check and the read (YM-277). os.OpenInRoot
// follows an in-root link and refuses any link leaving the root (and any
// absolute link); the walks already drop symlink entries, so the anchor is the
// second layer for a link that appears after the walk. The PDF converter reads
// the same descriptor, so it sees exactly the file that was opened.
func readFile(sourceRoot, path string) (string, error) {
	rel, err := filepath.Rel(sourceRoot, path)
	if err != nil {
		return "", err
	}
	f, err := os.OpenInRoot(sourceRoot, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// A second layer over the walk's entry-type check: a non-regular file whose
	// open succeeds (a device) is refused before it is read. It does not cover a
	// FIFO swapped in between the walk and this open, because os.OpenInRoot
	// blocks inside the open, before any check can run. That residual needs write
	// access inside the document tree, i.e. the same trust domain as the
	// documents themselves.
	if info, statErr := f.Stat(); statErr == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("document %q is not a regular file", filepath.Base(path))
		}
		if info.Size() > maxDocFileBytes {
			return "", fmt.Errorf("document %q is %d bytes, above the %d byte ingest limit", filepath.Base(path), info.Size(), maxDocFileBytes)
		}
	}

	if strings.HasSuffix(strings.ToLower(path), ".pdf") {
		ctx, cancel := context.WithTimeout(context.Background(), pdfExtractTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-", "-")
		cmd.Stdin = f
		out := &boundedWriter{limit: maxDocFileBytes}
		cmd.Stdout = out
		// The deadline kills the converter itself. A process that inherited
		// the stdout pipe keeps it open past the kill, so without WaitDelay
		// the wait would outlive the deadline.
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("pdftotext timed out after %s on document %q", pdfExtractTimeout, filepath.Base(path))
			}
			return "", fmt.Errorf("pdftotext failed (is poppler-utils installed?): %w", err)
		}
		// The input size is bounded above, but extracted text can expand far past
		// it. The writer stops buffering at the limit and keeps accepting bytes,
		// so the child never blocks on a full pipe and the daemon's memory holds
		// only the bounded prefix.
		if out.overrun {
			return "", fmt.Errorf("document %q extracts to more than the %d byte ingest limit", filepath.Base(path), maxDocFileBytes)
		}
		return out.buf.String(), nil
	}
	// The extra byte makes an overrun observable without copying the file; the
	// stat above is a fast path, this is the bound that holds if the file grows.
	content, err := io.ReadAll(io.LimitReader(f, maxDocFileBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > maxDocFileBytes {
		return "", fmt.Errorf("document %q exceeds the %d byte ingest limit", filepath.Base(path), maxDocFileBytes)
	}
	return string(content), nil
}

// chunkByFormat selects the appropriate chunker based on file extension.
func chunkByFormat(relPath, content string) []Chunk {
	if strings.HasSuffix(strings.ToLower(relPath), ".rst") {
		return ChunkRST(content)
	}
	return ChunkMarkdown(content)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// NewestDocMtime reports the newest modification time at or before cutoff among
// the documents a walk of path would collect, using the same entry gates as Run.
// ok is false when the tree holds no such document. A directory's own mtime only
// moves when an entry is added or removed directly inside it, so a caller that
// needs to know whether the documents changed cannot stat the directory. cutoff
// is the moment the caller compares against: a document dated after it cannot be
// ordered against a last-sync timestamp at all, and alone it would report work on
// every tick.
func NewestDocMtime(path string, cutoff time.Time) (newest time.Time, ok bool) {
	// Same root shape as Run: a symlink to a directory is a directory (os.Stat
	// below follows it), but WalkDir Lstats its root and would report the link
	// itself, i.e. no documents at all.
	if linfo, err := os.Lstat(path); err == nil && linfo.Mode()&fs.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
			path = resolved
		}
	}
	// The walk error is deliberately not returned: this is a change signal, and
	// an unreadable subtree holds no readable document, so it cannot schedule
	// work. Run logs the entries it cannot read.
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() || !isDocFile(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if !ok || info.ModTime().After(newest) {
			newest = info.ModTime()
			ok = true
		}
		return nil
	})
	return newest, ok
}
