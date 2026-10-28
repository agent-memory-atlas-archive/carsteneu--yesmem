package ingest

import (
	"fmt"
	"os"
	"path/filepath"
)

// PathMissing reports whether a registered source path is gone. Only absolute
// paths are judged: a relative path was recorded against some unknown working
// directory, so it can neither be verified nor safely treated as dead.
func PathMissing(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	_, err := os.Stat(path)
	return err != nil
}

// ValidSourceName reports whether name can serve as a single path element. The
// docs clone directory is derived from it, so a name with separators or dots
// would escape that directory, and a leading dash reads as an option wherever
// the derived path is passed to git.
func ValidSourceName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if name != filepath.Base(name) {
		return false
	}
	return name[0] != '-'
}

// CloneDir returns the clone directory of a URL-imported source under dataDir.
// Every caller derives it here: it is the target of a recursive delete, so a
// second, unguarded derivation would reintroduce the escape this guards against.
func CloneDir(dataDir, name string) (string, error) {
	if !ValidSourceName(name) {
		return "", fmt.Errorf("invalid doc source name %q", name)
	}
	if dataDir == "" {
		return "", fmt.Errorf("data dir is empty")
	}
	return filepath.Join(dataDir, "docs", name), nil
}
