package codescan

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cbmGCMinAge: only CBM DBs whose last modification is older than this are
// GC candidates, so in-flight worktree indexes are never touched.
const cbmGCMinAge = 24 * time.Hour

// cbmWorktreeKinds maps the worktree-style markers inside CBM cache DB names
// to the worktree subdirectory they represent. A DB for /repo/.worktrees/wt
// is named "repo-.worktrees-wt.db" (leading "/" stripped, "/"→"-"); Claude
// Code worktrees live at /repo/.claude/worktrees/<name>. The more specific
// claude marker is checked first — it cannot nest inside the plain one.
var cbmWorktreeKinds = []struct {
	marker string
	sub    string
}{
	{marker: "-.claude-worktrees-", sub: ".claude/worktrees"},
	{marker: "-.worktrees-", sub: ".worktrees"},
}

// CBMGCFinding is one deletable item under the CBM cache directory.
type CBMGCFinding struct {
	Path      string
	SizeBytes int64
	Reason    string
}

func cbmOrphanedDBs(cacheDir string, now time.Time, bin string) ([]CBMGCFinding, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("cbm-gc: read cache dir: %w", err)
	}

	var findings []CBMGCFinding
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".db") || name == "_config.db" {
			continue
		}
		slug := strings.TrimSuffix(name, ".db")
		var kind struct {
			marker string
			sub    string
		}
		idx := -1
		for _, k := range cbmWorktreeKinds {
			if i := strings.LastIndex(slug, k.marker); i >= 0 {
				kind, idx = k, i
				break
			}
		}
		if idx < 0 {
			continue // main-repo or non-repo project DB — never touched
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < cbmGCMinAge {
			continue // fresh — the index may still be in use
		}

		// Cheap pre-filter: resolve the repo root and only keep candidates
		// whose recorded worktree path is gone. Ambiguous resolutions are
		// rejected — a lookalike directory must not rule the decision.
		baseSlug := slug[:idx]
		base, ok := resolveExistingSlugPath(baseSlug)
		if !ok || cbmProjectName(base) != baseSlug {
			continue // unresolvable or ambiguous repo root — never deleted
		}
		wtSlug := slug[idx+len(kind.marker):]
		wtPath := filepath.Join(base, kind.sub, wtSlug)
		if _, err := os.Stat(wtPath); err == nil || !os.IsNotExist(err) {
			continue // exists, or unreadable — never delete on a guess
		}

		// Authoritative gate: CBM's own registration decides. A DB whose
		// registered root path still exists (e.g. a main-repo DB whose path
		// merely contains a ".worktrees" component) stays untouched, and so
		// does anything we cannot verify.
		rootPath := cbmIndexRootPath(bin, slug)
		if rootPath == "" {
			continue
		}
		if _, err := os.Stat(rootPath); err == nil || !os.IsNotExist(err) {
			continue
		}

		findings = append(findings, CBMGCFinding{
			Path:      filepath.Join(cacheDir, name),
			SizeBytes: info.Size(),
			Reason:    "orphaned worktree index (registered root " + rootPath + " no longer exists)",
		})
		findings = append(findings, suffixFindings(cacheDir, name)...)
		// stage leftovers: <slug>.db.stage.<rand>
		stagePrefix := name + ".stage"
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), stagePrefix) {
				continue
			}
			if fi, err := e.Info(); err == nil {
				findings = append(findings, CBMGCFinding{
					Path:      filepath.Join(cacheDir, e.Name()),
					SizeBytes: fi.Size(),
					Reason:    "stage leftover from interrupted index",
				})
			}
		}
	}
	return findings, nil
}

// cbmIndexRootPath asks CBM for a project's registered root path. Empty on
// any error or unknown registration — the GC is conservative about
// everything it cannot verify.
func cbmIndexRootPath(bin, project string) string {
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "cli", "index_status", "--project", project, "--json").Output()
	if err != nil {
		log.Printf("cbm-gc: index_status %s: %v", project, err)
		return ""
	}
	text, err := parseCBMResponse(out)
	if err != nil {
		log.Printf("cbm-gc: index_status %s: %v", project, err)
		return ""
	}
	var st struct {
		RootPath string `json:"root_path"`
	}
	if err := json.Unmarshal([]byte(text), &st); err != nil {
		return ""
	}
	return st.RootPath
}

// suffixFindings flags -wal/-shm siblings of a to-be-deleted DB.
func suffixFindings(cacheDir, dbName string) []CBMGCFinding {
	var out []CBMGCFinding
	for _, suffix := range []string{"-wal", "-shm"} {
		p := filepath.Join(cacheDir, dbName+suffix)
		if info, err := os.Stat(p); err == nil {
			out = append(out, CBMGCFinding{Path: p, SizeBytes: info.Size(), Reason: "orphaned " + suffix + " sidecar"})
		}
	}
	return out
}

// resolveExistingSlugPathBudget bounds total explored path combinations per
// call, so a crafted or pathological slug cannot hang the daily GC task.
const resolveExistingSlugPathBudget = 512

// resolveExistingSlugPath reconstructs an absolute path from a CBM project
// slug (leading "/" stripped, "/"→"-"). Slashes vs dashes are ambiguous, so
// split combinations are tried with a hard exploration budget. Returns false
// when no existing directory matches or the budget runs out — callers must
// then skip the item.
func resolveExistingSlugPath(slug string) (string, bool) {
	budget := resolveExistingSlugPathBudget
	var rec func(pos int, path string) (string, bool)
	n := len(slug)
	rec = func(pos int, path string) (string, bool) {
		if budget <= 0 {
			return "", false
		}
		budget--
		if pos == n {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				return path, true
			}
			return "", false
		}
		c := slug[pos]
		if c == '-' {
			if p, ok := rec(pos+1, path+"/"); ok {
				return p, true
			}
			return rec(pos+1, path+"-")
		}
		return rec(pos+1, path+string(c))
	}
	return rec(0, "/")
}

// runCBMGCGated deletes (or only reports, with dryRun) the listed findings.
func runCBMGCGated(findings []CBMGCFinding, dryRun bool) {
	for _, f := range findings {
		if dryRun {
			log.Printf("cbm-gc: dry-run candidate: %s (%s: %s)", f.Path, fmtByteSize(f.SizeBytes), f.Reason)
			continue
		}
		if err := os.Remove(f.Path); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("cbm-gc: remove %s: %v", f.Path, err)
				continue
			}
		}
		log.Printf("cbm-gc: removed %s (%s: %s)", f.Path, fmtByteSize(f.SizeBytes), f.Reason)
	}
}

// RunCBMGC scans the default CBM cache directory and reports (dryRun) or
// deletes orphaned worktree index files. Called by `yesmem cbm-gc` and the
// daemon's daily GC task.
func RunCBMGC(dryRun bool) ([]CBMGCFinding, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cbm-gc: home dir: %w", err)
	}
	cacheDir := filepath.Join(home, ".cache", "codebase-memory-mcp")
	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), FindCBMBinary())
	if err != nil {
		return nil, err
	}
	runCBMGCGated(findings, dryRun)
	return findings, nil
}

// FormatCBMByteSize renders a byte count for CLI output.
func FormatCBMByteSize(b int64) string {
	return fmtByteSize(b)
}

func fmtByteSize(b int64) string {
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	switch {
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/gb)
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/mb)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
