package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/repo"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

func runAddDocs(args []string) {
	fs := flag.NewFlagSet("add-docs", flag.ExitOnError)
	name := fs.String("name", "", "Documentation source name (required)")
	version := fs.String("version", "", "Version string")
	path := fs.String("path", "", "Directory path to index")
	file := fs.String("file", "", "Single file to index")
	url := fs.String("url", "", "Git URL to clone (e.g. https://github.com/user/repo)")
	project := fs.String("project", "", "Project name")
	domain := fs.String("domain", "code", "Domain (code, marketing, legal, finance, general)")
	dryRun := fs.Bool("dry-run", false, "Show what would happen without writing")
	destill := fs.Bool("destill", false, "Run LLM destillation to create low-trust learnings")
	fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required")
		os.Exit(1)
	}
	// Deleting a source requires the name to be a single path element; accept
	// nothing at registration that remove-docs would later refuse.
	if !ingest.ValidSourceName(*name) {
		fmt.Fprintf(os.Stderr, "Error: --name %q is not a valid source name\n", *name)
		os.Exit(1)
	}

	var paths []string
	if *url != "" {
		// Clone git repo to persistent docs directory
		clonePath := cloneOrPullRepo(*name, *url)
		if clonePath == "" {
			log.Fatalf("Failed to clone %q", *url)
		}
		paths = append(paths, clonePath)
	}
	if *path != "" {
		paths = append(paths, *path)
	}
	if *file != "" {
		paths = append(paths, *file)
	}
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "Error: --path, --file, or --url is required")
		os.Exit(1)
	}

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %s", textutil.ErrorLine(err))
	}
	defer store.Close()

	cfg := ingest.Config{
		Name:    *name,
		Version: *version,
		Project: *project,
		Domain:  *domain,
		DryRun:  *dryRun,
		Destill: *destill,
	}

	// Create LLM client for destillation if requested
	if cfg.Destill && !*dryRun {
		appCfg, cfgErr := config.Load(filepath.Join(dataDir, "config.yaml"))
		if cfgErr != nil {
			log.Fatalf("config (needed for --destill): %v", cfgErr)
		}
		model := appCfg.SummarizeModelID()
		client, clientErr := extraction.NewLLMClient(appCfg.LLM.Provider, appCfg.ResolvedAPIKey(), model, appCfg.LLM.ClaudeBinary, appCfg.ResolvedOpenAIBaseURL())
		if clientErr != nil {
			log.Fatalf("llm client for destillation: %v", clientErr)
		}
		if client == nil {
			log.Fatalf("no LLM client available for --destill: set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure claude_binary")
		}
		client = redacted(appCfg, client)
		cfg.LLMClient = client
	}

	result, err := ingest.Run(cfg, paths, store)
	if err != nil {
		log.Fatalf("ingest failed: %s", textutil.ErrorLine(err))
	}

	fmt.Printf("\nDone.\n")
	fmt.Printf("  Files processed: %d\n", result.FilesProcessed)
	fmt.Printf("  Files skipped:   %d (not indexed)\n", result.FilesSkipped)
	fmt.Printf("  Chunks created:  %d\n", result.ChunksCreated)
	if result.LearningsSuperseded > 0 {
		fmt.Printf("  Learnings superseded: %d\n", result.LearningsSuperseded)
	}
	if result.LearningsCreated > 0 {
		fmt.Printf("  Learnings created:    %d (from destillation)\n", result.LearningsCreated)
	}
}

func runSyncDocs(args []string) {
	fs := flag.NewFlagSet("sync-docs", flag.ExitOnError)
	name := fs.String("name", "", "Sync specific source")
	project := fs.String("project", "", "Sync all sources for project")
	all := fs.Bool("all", false, "Sync all registered sources")
	prune := fs.Bool("prune", false, "Remove sources whose path is gone (keeps rules and skill sources)")
	fs.Parse(args)

	if *name == "" && *project == "" && !*all {
		fmt.Fprintln(os.Stderr, "Error: --name, --project, or --all is required")
		os.Exit(1)
	}

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %s", textutil.ErrorLine(err))
	}
	defer store.Close()

	// Get sources to sync
	filterProject := ""
	if !*all {
		filterProject = *project
	}
	// The sweep may only judge rows that existed when the list below was read:
	// a concurrent pass that re-registers and re-indexes a source writes rows
	// above this mark, and those are not this run's to delete.
	watermark, watermarkErr := store.MaxDocChunkID()
	if watermarkErr != nil {
		// A failed read is not an empty table: without a watermark the sweep
		// would judge rows it cannot place in time, so it stays off for this run.
		log.Printf("chunk watermark: %v; skipping the sweep of sources without a path", watermarkErr)
		watermark = 0
	}
	sources, err := store.ListDocSources(filterProject)
	if err != nil {
		log.Fatalf("list sources: %v", err)
	}

	// YM-29: sources registered from a path that is gone (ad-hoc /tmp ingests)
	// stay in the table and look healthy in list-docs. --prune removes them.
	if *prune {
		removed, failed, kept := 0, 0, 0
		for _, src := range missingPathSources(sources) {
			if *name != "" && src.Name != *name {
				continue
			}
			// Rules and skill sources keep their body in doc_sources.full_content:
			// a missing path says nothing about it, and nothing regenerates it. They
			// go through remove-docs, which is an explicit decision per source.
			if src.IsRules || src.IsSkill {
				fmt.Printf("Keeping %q (%q) — path %q is gone, but its content is stored in the database; remove it explicitly with remove-docs.\n", src.Name, src.Project, src.Path)
				kept++
				continue
			}
			if _, err := store.DeleteDocSource(src.Name, src.Project); err != nil {
				log.Printf("  prune %q: %s", src.Name, textutil.ErrorLine(err))
				failed++
				continue
			}
			fmt.Printf("Removed %q (%q) — path %q is gone.\n", src.Name, src.Project, src.Path)
			removed++
		}
		if removed == 0 && failed == 0 && kept == 0 {
			fmt.Println("Nothing to prune — every registered source path exists.")
		}
		if failed > 0 {
			fmt.Printf("%d source(s) could not be removed.\n", failed)
		}
		if sources, err = store.ListDocSources(filterProject); err != nil {
			log.Fatalf("list sources: %v", err)
		}
	}

	for _, src := range sources {
		if *name != "" && src.Name != *name {
			continue
		}
		// Rules sources carry the condensed CLAUDE.md block, not chunks, and are
		// refreshed by the daemon's rules worker — never by a doc sync.
		if src.IsRules || src.IsSkill {
			continue
		}
		if src.Path == "" {
			sweepSourceWithoutPath(store, src.Name, src.Project, watermark)
			continue
		}
		// A relative path was registered against a working directory nobody
		// recorded. Re-reading it here would index whatever happens to sit at
		// that name now, under the source's name.
		if !filepath.IsAbs(src.Path) {
			log.Printf("Skipping %q: path %q is relative, so it cannot be re-read from another working directory (re-register it with an absolute path)", src.Name, src.Path)
			continue
		}
		if ingest.PathMissing(src.Path) {
			log.Printf("Skipping %q: path %q is gone (use --prune to remove the source)", src.Name, src.Path)
			continue
		}

		// Git-aware: if source path is inside a git repo, pull first
		gitUpdated := tryGitPull(src.Path)
		if gitUpdated {
			log.Printf("  Git pull: updates found")
		}

		log.Printf("Syncing %q (%q)...", src.Name, src.Path)
		cfg := ingest.Config{
			Name:    src.Name,
			Version: src.Version,
			Project: src.Project,
			Destill: false, // no re-destillation on sync
		}

		// Wire LLM client for re-destillation if needed
		if cfg.Destill {
			appCfg, cfgErr := config.Load(filepath.Join(dataDir, "config.yaml"))
			if cfgErr == nil {
				model := appCfg.SummarizeModelID()
				client, clientErr := extraction.NewLLMClient(appCfg.LLM.Provider, appCfg.ResolvedAPIKey(), model, appCfg.LLM.ClaudeBinary, appCfg.ResolvedOpenAIBaseURL())
				if clientErr == nil && client != nil {
					client = redacted(appCfg, client)
					cfg.LLMClient = client
				}
			}
		}

		result, err := ingest.Run(cfg, []string{src.Path}, store)
		if err != nil {
			log.Printf("  ERROR: %s", textutil.ErrorLine(err))
			continue
		}
		log.Printf("  Files: %d processed, %d skipped. Chunks: %d created.",
			result.FilesProcessed, result.FilesSkipped, result.ChunksCreated)
		if result.LearningsCreated > 0 {
			log.Printf("  Learnings: %d created (from destillation)", result.LearningsCreated)
		}
	}
}

// pathReReadHook is a test seam in the window between the source-state read and
// the sweep. Production leaves it nil; a test uses it to land a competing
// re-registration deterministically instead of racing with a sleep.
var pathReReadHook atomic.Pointer[func()]

// sweepSourceWithoutPath removes the chunks of a source that has no path, and
// retires the learnings derived from them.
//
// The caller judges from a snapshot (ListDocSources) and then spends real time
// on git pulls and full walks, so this row is re-read before the sweep: a pass
// that re-registered the source with a path and re-indexed it must keep its
// chunks. The read is a cheap early exit, not the guarantee. The guarantee is
// that the path check and the delete share one write transaction inside the
// store (Store.SweepPathlessSourceChunks): a content match keeps the old chunk
// ids, so a delete based on any earlier read would remove live rows.
func sweepSourceWithoutPath(store *storage.Store, name, project string, watermark int64) {
	current, err := store.GetDocSource(name, project)
	if err != nil {
		log.Printf("Re-read %q before the sweep: %v", name, err)
		return
	}
	if current == nil || current.Path != "" {
		return
	}
	if hook := pathReReadHook.Load(); hook != nil {
		(*hook)()
	}
	// A zero watermark makes this a no-op: either no chunk was ever indexed, or
	// the read failed. Both mean there is nothing this run could judge.
	swept, err := ingest.SweepUnreachableSource(store, current.ID, watermark)
	if err != nil {
		log.Printf("Sweep %q (no path): %v", name, err)
		return
	}
	if swept.FilesRemoved == 0 {
		log.Printf("Source %q: nothing to sweep", name)
		return
	}
	if err := store.UpdateDocSourceChunkCount(current.ID); err != nil {
		log.Printf("Refresh chunk count of %q: %v", name, err)
	}
	log.Printf("Removed %d document(s) and retired %d learning(s) of %q, which has no path",
		swept.FilesRemoved, swept.LearningsSuperseded, name)
}

// missingPathSources returns the sources whose registered path is gone.
func missingPathSources(sources []storage.DocSource) []storage.DocSource {
	var out []storage.DocSource
	for _, src := range sources {
		if ingest.PathMissing(src.Path) {
			out = append(out, src)
		}
	}
	return out
}

func runListDocs(args []string) {
	fs := flag.NewFlagSet("list-docs", flag.ExitOnError)
	project := fs.String("project", "", "Filter by project")
	fs.Parse(args)

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %s", textutil.ErrorLine(err))
	}
	defer store.Close()

	sources, err := store.ListDocSources(*project)
	if err != nil {
		log.Fatalf("list sources: %v", err)
	}

	if len(sources) == 0 {
		fmt.Println("No documentation sources registered.")
		return
	}

	fmt.Printf("%-15s %-10s %8s %-20s %s\n", "NAME", "VERSION", "CHUNKS", "LAST SYNC", "PROJECT")
	for _, s := range sources {
		proj := s.Project
		if proj == "" {
			proj = "(global)"
		}
		note := ""
		if ingest.PathMissing(s.Path) {
			note = "  (path missing)"
		}
		// Name, version and project come from the registration, and ValidSourceName is a
		// path guard, not a log guard: a line break inside one of them would end the row
		// and pass the rest of it off as another source (YM-389).
		fmt.Printf("%-15s %-10s %8d %-20s %s%s\n",
			textutil.SingleLine(s.Name, 40), textutil.SingleLine(s.Version, 20), s.ChunkCount,
			s.LastSync.Format("2006-01-02 15:04"), textutil.SingleLine(proj, 60), note)
	}
}

func runRemoveDocs(args []string) {
	fs := flag.NewFlagSet("remove-docs", flag.ExitOnError)
	name := fs.String("name", "", "Source name to remove (required)")
	project := fs.String("project", "", "Project filter")
	fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required")
		os.Exit(1)
	}
	if !ingest.ValidSourceName(*name) {
		fmt.Fprintf(os.Stderr, "Error: --name %q is not a valid source name\n", *name)
		os.Exit(1)
	}

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %s", textutil.ErrorLine(err))
	}
	defer store.Close()

	delResult, err := store.DeleteDocSource(*name, *project)
	if err != nil {
		log.Fatalf("delete source: %v", err)
	}

	deleted := len(delResult.DeletedLearningIDs)
	fmt.Printf("Removed %q — chunks + %d learnings deleted.\n", *name, deleted)

	// Clean up clone directory if it was a URL import
	if delResult != nil && delResult.SourcePath != "" {
		if cloneDir, err := docsCloneDir(*name); err != nil {
			log.Printf("warn: skip clone dir cleanup: %s", textutil.ErrorLine(err))
		} else if _, statErr := os.Stat(cloneDir); statErr == nil {
			if removeErr := os.RemoveAll(cloneDir); removeErr != nil {
				log.Printf("warn: remove clone dir: %s", textutil.ErrorLine(removeErr))
			} else {
				log.Printf("Clone directory removed: %q", cloneDir)
			}
		}
	}
}

// docsCloneDir returns the clone directory of a URL-imported source. It refuses
// names that are not a single path element, so a caller-supplied name cannot
// steer the recursive cleanup outside the docs directory.
func docsCloneDir(name string) (string, error) {
	return ingest.CloneDir(yesmemDataDir(), name)
}

// tryGitPull updates a doc source from its upstream, but only when the source
// path is itself a repository root (a docs clone) — see repo.CloneRoot. Walking
// up instead would run 'git pull' in the repository the user is working in.
// Returns true if there were updates.
func tryGitPull(path string) bool {
	gitRoot := repo.CloneRoot(path)
	if gitRoot == "" {
		return false
	}

	// Check for updates: fetch first, then compare
	fetchCmd := exec.Command("git", "-C", gitRoot, "fetch", "--quiet")
	if err := fetchCmd.Run(); err != nil {
		log.Printf("  Git fetch failed: %v", err)
		return false
	}

	// Check if there are updates
	diffCmd := exec.Command("git", "-C", gitRoot, "diff", "HEAD", "FETCH_HEAD", "--stat")
	output, err := diffCmd.Output()
	if err != nil || len(output) == 0 {
		return false // no updates or error
	}

	// Pull the updates
	pullCmd := exec.Command("git", "-C", gitRoot, "pull", "--quiet")
	if err := pullCmd.Run(); err != nil {
		log.Printf("  Git pull failed: %v", err)
		return false
	}

	return true
}

// cloneOrPullRepo clones a git URL to ~/.claude/yesmem/docs/<name>/ or pulls if exists.
func cloneOrPullRepo(name, url string) string {
	dataDir := yesmemDataDir()
	docsDir, err := ingest.CloneDir(dataDir, name)
	if err != nil {
		log.Printf("  %v", err)
		return ""
	}

	if _, err := os.Stat(filepath.Join(docsDir, ".git")); err == nil {
		// Already cloned — pull
		log.Printf("Pulling updates for %s...", name)
		cmd := exec.Command("git", "-C", docsDir, "pull", "--quiet")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("  Git pull failed: %s", textutil.ErrorLine(err))
			logOutputLines("  git: ", output)
		}
		return docsDir
	}

	// Clone fresh
	log.Printf("Cloning %q → %q", url, docsDir)
	if err := os.MkdirAll(filepath.Dir(docsDir), 0755); err != nil {
		log.Printf("  mkdir: %v", err)
		return ""
	}
	cmd := exec.Command("git", gitCloneArgs(url, docsDir)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("  Git clone failed: %s", textutil.ErrorLine(err))
		logOutputLines("  git: ", output)
		return ""
	}

	return docsDir
}

// gitCloneArgs builds the clone invocation. The option terminator keeps a URL
// that starts with '-' from being read as a git option.
func gitCloneArgs(url, dir string) []string {
	return []string{"clone", "--depth", "1", "--", url, dir}
}

// logOutputLines writes a child process's output as one log entry per line. The line
// structure stays, because a multi-line git diagnostic is what the operator needs, but
// every line is flattened: a control byte or an escape sequence from a remote's message
// would otherwise reach the terminal, and a line break inside the text would pass the rest
// off as a separate log entry (YM-389).
func logOutputLines(prefix string, output []byte) {
	for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
		if line == "" {
			continue
		}
		log.Printf("%s%s", prefix, textutil.SingleLine(line, 500))
	}
}
