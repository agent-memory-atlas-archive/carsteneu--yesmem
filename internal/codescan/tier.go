package codescan

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Tier represents the project size classification for adaptive index depth.
type Tier int

const (
	TierTiny   Tier = iota // <10 source files — full content in index
	TierSmall              // <50 source files — all signatures
	TierMedium             // <200 source files — packages with signatures
	TierLarge              // >=200 source files — top-20 packages prioritized
)

func (t Tier) String() string {
	switch t {
	case TierTiny:
		return "tiny"
	case TierSmall:
		return "small"
	case TierMedium:
		return "medium"
	case TierLarge:
		return "large"
	default:
		return "unknown"
	}
}

// ProjectStats holds file count and LOC metrics from a scan.
type ProjectStats struct {
	FileCount int
	TotalLOC  int
	RootDir   string
}

// DetectTier walks the project directory, counts source files and LOC,
// and returns the appropriate tier classification.
// NOTE: The Scanner implementations compute tiers during their own walk to avoid
// double-walking the filesystem. This function exists for standalone tier detection
// (e.g. CLI diagnostics) outside the scan path.
func DetectTier(rootDir string) (Tier, ProjectStats) {
	stats := ProjectStats{RootDir: rootDir}
	walkRoot := ResolveScanRoot(rootDir)
	var skippedLinks int
	var firstSkipped string

	filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(walkRoot, path)
		if shouldSkipDir(rel, info) {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if !isSourceFile(path) {
			return nil
		}
		readPath := path
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, ok := ResolveWithinRoot(walkRoot, rel)
			if !ok {
				skippedLinks++
				if firstSkipped == "" {
					firstSkipped = rel
				}
				return nil
			}
			readPath = resolved
		}
		stats.FileCount++
		stats.TotalLOC += countLOC(readPath)
		return nil
	})

	logSkippedLinks("tier", walkRoot, skippedLinks, firstSkipped)

	tier := classifyTier(stats)
	return tier, stats
}

func classifyTier(stats ProjectStats) Tier {
	switch {
	case stats.FileCount < 10:
		return TierTiny
	case stats.FileCount < 50:
		return TierSmall
	case stats.FileCount < 200:
		return TierMedium
	default:
		return TierLarge
	}
}

// ShouldSkipDir checks if a directory should be excluded from scanning.
// rootDir is used to compute relative paths for the skip decision.
func ShouldSkipDir(path string, info os.FileInfo, rootDir string) bool {
	rel, err := filepath.Rel(rootDir, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return shouldSkipDir(rel, info)
}

// IsSourceFile returns true for files with recognized source code extensions.
func IsSourceFile(path string) bool {
	return isSourceFile(path)
}
func shouldSkipDir(rel string, info os.FileInfo) bool {
	if !info.IsDir() {
		return false
	}
	// The walk root is never a skip candidate. filepath.Rel reports it as ".",
	// and info.Name() of a root given as "." or "proj/." is ".", so the dot
	// rule below would prune the whole tree at its root (Learning #102155).
	// This also means an explicitly requested directory (get_file_index dir=…)
	// is listed even when its name is in the skip list: the explicit request
	// wins over the default pruning.
	if rel == "." || rel == "" {
		return false
	}
	name := info.Name()
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "vendor", "node_modules", "dist", "build", "__pycache__", ".claude",
		"gopath", "go-path", "go-cache", "gocache",
		".worktrees", "testdata", "third_party":
		return true
	}
	return false
}

// isSourceFile returns true for files with recognized source code extensions.
var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".rs": true, ".java": true, ".kt": true, ".scala": true, ".rb": true,
	".php": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true,
	".cs": true, ".swift": true, ".m": true, ".lua": true, ".zig": true,
	".ex": true, ".exs": true, ".erl": true, ".hs": true, ".ml": true,
	".vue": true, ".svelte": true, ".dart": true, ".r": true,
	".sh": true, ".bash": true, ".zsh": true,
}

func isSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return sourceExts[ext]
}

// countLOC counts non-empty lines in a file.
func countLOC(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count
}
