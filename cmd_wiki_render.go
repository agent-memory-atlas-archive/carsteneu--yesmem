package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/wikidir"
	"github.com/carsteneu/yesmem/internal/wikirender"
)

func runWikiRender(args []string) error {
	fs := flag.NewFlagSet("wiki-render", flag.ExitOnError)
	project := fs.String("project", "", "Project name (required)")
	out := fs.String("out", "", "Output dir (default ~/.claude/yesmem/wiki/<basename of --project>)")
	quiet := fs.Bool("quiet", false, "Suppress progress output")
	jsonLine := fs.Bool("json", true, "Emit final JSON status line on stdout")
	scan := fs.Bool("scan", false, "Use CBM scanner for live code graph (imports, calls)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		fs.Usage()
		return fmt.Errorf("--project is required")
	}
	if *out == "" {
		root := wikidir.Root(yesmemDataDir())
		name := wikidir.AssignNames(root, []string{*project})[*project]
		if name == "" {
			return fmt.Errorf("--project %q has no usable wiki directory name", *project)
		}
		*out = filepath.Join(root, name)
	}

	store, err := storage.Open(filepath.Join(yesmemDataDir(), "yesmem.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	var graph *codescan.CodeGraph
	var scanJSON string
	var saveErr error
	var scanErr error
	if *scan {
		dir := store.ResolveProjectPath(*project)
		if dir != "" && codescan.FindCBMBinary() != "" {
			graph, scanJSON, scanErr, saveErr = scanForWikiRender(store, dir, codescan.NewCBMScanner())
		}
	}

	r, err := wikirender.Render(context.Background(), wikirender.RenderConfig{
		Project:   *project,
		OutputDir: *out,
		Store:     store,
		CodeGraph: graph,
		ScanJSON:  []byte(scanJSON),
		Quiet:     *quiet,
	})
	if err != nil {
		return err
	}

	// Claim the directory so the wiki-tick does not hand it to another project
	// with the same basename. Only a canonical (absolute path) project
	// identifier claims ownership — the daemon keys projects by path, so a short
	// name would take the directory of the project that really owns it. An empty
	// render claims nothing, and a directory owned by another project is never
	// taken over.
	if r.Learnings > 0 && filepath.IsAbs(*project) {
		if owner := wikidir.MarkerProject(*out); owner != "" && owner != *project {
			return fmt.Errorf("wiki dir %q belongs to project %q", *out, owner)
		}
		if err := wikidir.WriteMarker(*out, *project); err != nil {
			return fmt.Errorf("mark wiki dir %q: %w", *out, err)
		}
	}

	if *jsonLine {
		j, _ := json.Marshal(map[string]any{
			"project": r.Project, "learnings": r.Learnings,
			"quarantined": r.Quarantined, "topics": r.Topics,
			"files": r.Files, "sessions": r.Sessions,
			"contradictions": r.Contradictions,
			"out":            *out, "built_at": r.BuiltAt,
		})
		fmt.Println(string(j))
	} else if !*quiet {
		fmt.Printf("rendered project=%q learnings=%d topics=%d files=%d sessions=%d in %dms\n",
			r.Project, r.Learnings, r.Topics, r.Files, r.Sessions, r.DurationMs)
	}
	return errors.Join(saveErr, scanErr)
}

// scanForWikiRender walks dir and stores the result under the checkout's scan
// key when the document is that checkout's index. The errors are returned
// instead of raised: the render runs from the returned graph and document, and
// throwing away a finished wiki over a failed cache write or a failed scan would
// be the worse outcome. The caller reports them after the render.
func scanForWikiRender(store *storage.Store, dir string, scanner codescan.Scanner) (graph *codescan.CodeGraph, scanJSON string, scanErr, saveErr error) {
	// Read the freshness markers before the scan, the way the cached scanner
	// does (codescan/cache.go): a commit landing during the scan must not be
	// recorded as covered by it (YM-277). Both are read at the key the document
	// is stored under, so a worktree scan records the worktree's head and not
	// the main checkout's (YM-453).
	key := codescan.ScanCacheKey(dir)
	head := codescan.ReadGitHead(key)
	cbmMtime := codescan.CBMIndexMtime(key).Unix()

	sr, err := scanner.Scan(dir)
	if err != nil {
		return nil, "", fmt.Errorf("scan %q: %w", dir, err), nil
	}
	graph = codescan.BuildCodeGraph(sr)
	raw, err := json.Marshal(sr)
	if err != nil {
		return graph, "", nil, nil
	}
	scanJSON = string(raw)
	// Only the checkout's own scan is its index (YM-277); the rule itself lives
	// in codescan instead of being spelled out here a second time. A failed
	// write used to be dropped, which left the wiki on the previous index
	// without a word (DEV-DT73).
	if codescan.ScannedRepositoryRoot(key, sr.RootDir) {
		if err := store.SaveProjectScan(&storage.ProjectScanRow{
			Project:  key,
			ScanJSON: scanJSON,
			GitHead:  head,
			CbmMtime: cbmMtime,
		}); err != nil {
			saveErr = fmt.Errorf("save project scan for %q: %w", key, err)
		}
	}
	return graph, scanJSON, nil, saveErr
}
