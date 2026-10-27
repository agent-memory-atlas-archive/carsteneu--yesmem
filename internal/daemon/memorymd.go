package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/briefing"
	"github.com/carsteneu/yesmem/internal/storage"
)

const yesmemMarker = "# --- YesMem Auto-Generated (do not edit below) ---"

// wireMutationHook points the handler's post-mutation side effect at MEMORY.md
// regeneration. Only the production assembly calls this: NewHandler is also used
// by tests, and GenerateAllMemoryMDs writes into the configured Claude projects
// directory.
//
// Six call sites fire the hook without waiting, and each run walks every project
// directory. The wrapper keeps that to one sweep in flight plus at most one
// queued rerun, so a burst of resolves cannot stack concurrent full sweeps.
func wireMutationHook(h *Handler, store *storage.Store, claudeProjects string) {
	var mu sync.Mutex
	pending, running := false, false

	h.OnMutation = func() {
		mu.Lock()
		if running {
			pending = true
			mu.Unlock()
			return
		}
		running = true
		mu.Unlock()

		go func() {
			// A panic here would take the daemon down with it, and it would leave
			// the hook marked as running forever. Neither is acceptable for a
			// background sweep that only refreshes a generated file.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("MEMORY.md regeneration panicked: %v", r)
					mu.Lock()
					running, pending = false, false
					mu.Unlock()
				}
			}()
			for {
				GenerateAllMemoryMDs(store, claudeProjects)
				mu.Lock()
				if !pending {
					running = false
					mu.Unlock()
					return
				}
				pending = false
				mu.Unlock()
			}
		}()
	}
}

// GenerateAllMemoryMDs creates/updates MEMORY.md for every project.
// Writes only a narrative redirect to yesmem MCP tools — no learning content.
//
// claudeProjects is the directory Claude Code names its project folders in, as
// resolved by config.Config.ClaudeProjectsDir. The directory belongs to the
// configuration, and a hand-built $HOME path refreshes a folder nobody reads once
// the configuration names another one. Production callers pass the resolved
// value; an empty one falls back to the home default, which keeps direct callers
// on the historical location.
func GenerateAllMemoryMDs(store *storage.Store, claudeProjects string) {
	projects, err := store.ListProjects()
	if err != nil {
		log.Printf("warn: list projects for MEMORY.md: %v", err)
		return
	}

	if claudeProjects == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			// filepath.Join("", ".claude", "projects") is relative to the working
			// directory, which is not where MEMORY.md belongs.
			log.Printf("warn: MEMORY.md sweep skipped: no Claude projects directory configured and HOME is unset")
			return
		}
		claudeProjects = filepath.Join(home, ".claude", "projects")
	}

	s := briefing.ResolveStrings(briefing.DefaultStringsPath())
	narrative := s.MemoryMDNarrative

	var written, noDirectory, writeFailed int
	for _, p := range projects {
		projDir := findProjectDir(claudeProjects, p.Project)
		if projDir == "" {
			noDirectory++
			continue
		}

		memDir := filepath.Join(projDir, "memory")
		memFile := filepath.Join(memDir, "MEMORY.md")

		if err := writeMemoryMD(memDir, memFile, "\n"+narrative+"\n"); err != nil {
			log.Printf("warn: MEMORY.md for %s: %v", p.ProjectShort, err)
			writeFailed++
			continue
		}
		written++
	}

	// All three numbers: a project whose Claude directory does not exist gets no
	// file, and without the count a mapping regression looks like a clean run.
	log.Printf("MEMORY.md updated for %d of %d projects (%d without a Claude directory, %d write errors)",
		written, len(projects), noDirectory, writeFailed)
}

func writeMemoryMD(memDir, memFile, yesmemContent string) error {
	os.MkdirAll(memDir, 0755)

	// Read existing file
	existing, err := os.ReadFile(memFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var result string

	if len(existing) > 0 {
		content := string(existing)
		// Find marker
		idx := strings.Index(content, yesmemMarker)
		if idx >= 0 {
			// Replace everything from marker onwards
			result = content[:idx] + yesmemMarker + "\n" + yesmemContent
		} else {
			// Append marker + content
			result = content + "\n\n" + yesmemMarker + "\n" + yesmemContent
		}
	} else {
		// New file
		result = yesmemMarker + "\n" + yesmemContent
	}

	// The narrative is constant, so after the first write a sweep changes nothing.
	// Skipping the write keeps the hook from rewriting every project file on every
	// mutation (and from fighting a concurrent writer for no reason).
	if len(existing) > 0 && string(existing) == result {
		return nil
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(memFile); err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomic(memFile, []byte(result), mode)
}

// writeFileAtomic replaces path via a temp file in the same directory. A plain
// truncate-then-write exposes an empty MEMORY.md to a reader while the hook and
// the CLI can write the same file concurrently. A concurrent edit of the manual
// prefix above the marker is still last-writer-wins; the rename only removes the
// torn-read window.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".MEMORY.md-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// CreateTemp creates 0600. An existing destination keeps its mode; a new file
	// keeps 0600 as well, which is tighter than the umask would have allowed.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// findProjectDir maps a project path to its .claude/projects/ directory.
// The encoding is Claude Code's own (archive.ProjectDirName: every character
// outside [A-Za-z0-9] becomes a dash), so it is not reimplemented here. A
// mismatch means Claude Code never saw this project; a scan for a directory
// whose name merely ends like ours would attribute a foreign project instead
// (YM-279: /tmp/opencode matched -home-tester-projects-opencode) and the
// MEMORY.md sweep would write into it.
func findProjectDir(claudeProjects, projectPath string) string {
	full := filepath.Join(claudeProjects, archive.ProjectDirName(projectPath))
	if _, err := os.Stat(full); err == nil {
		return full
	}
	return ""
}
