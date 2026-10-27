package setup

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// legacyDBFiles are database files in the data directory that no code opens any
// more. capabilities.db was the first cap store and cap_store.db its successor;
// the daemon uses caps.db today. The entry with the asterisk is not a pattern:
// an old shell glob was never expanded and created a file of exactly that name.
// They hold nothing reachable, are never cleaned up and reappear in every audit
// (YM-180).
var legacyDBFiles = []string{"capabilities.db", "cap_store.db", "capabilities*.db"}

// RemoveLegacyDBFiles moves those files, together with their -wal and -shm
// siblings, out of dataDir into a timestamped directory under dataDir/backup and
// reports the names it moved.
//
// Moved, not deleted: a cap store from a v1 installation can hold hand-written
// cap scripts that exist nowhere else. The audit finding is that the files sit in
// the data directory where every reader falls over them, not that they must be
// gone. A missing file is not an error: the migration runs on every install.
func RemoveLegacyDBFiles(dataDir string) []string {
	if dataDir == "" {
		return nil
	}
	var found []string
	for _, name := range legacyDBFiles {
		for _, candidate := range []string{name, name + "-wal", name + "-shm"} {
			if _, err := os.Lstat(filepath.Join(dataDir, candidate)); err == nil {
				found = append(found, candidate)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}

	destDir := filepath.Join(dataDir, "backup", "legacy-"+time.Now().Format("20060102T150405"))
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		log.Printf("setup: legacy database files: %v", err)
		return nil
	}
	var moved []string
	for _, candidate := range found {
		src := filepath.Join(dataDir, candidate)
		// Rename only: a file that is still open elsewhere keeps its inode, so an
		// old process writing to it loses nothing that a read would have seen.
		if err := os.Rename(src, filepath.Join(destDir, candidate)); err != nil {
			log.Printf("setup: move legacy database file %s: %v", src, err)
			continue
		}
		moved = append(moved, candidate)
	}
	if len(moved) > 0 {
		log.Printf("setup: moved %d legacy database file(s) to %s", len(moved), destDir)
	}
	return moved
}

// DescribeLegacyDBRemoval is the one-line summary the migration prints.
func DescribeLegacyDBRemoval(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	return fmt.Sprintf("moved %d legacy database file(s) to backup/", len(removed))
}
