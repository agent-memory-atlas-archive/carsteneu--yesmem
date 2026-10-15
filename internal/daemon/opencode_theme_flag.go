package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// helpProbeTimeout bounds the `--help` probe. The opencode port is a Bun
// binary that starts in tens of milliseconds; a hang here must not stall a
// spawn.
const helpProbeTimeout = 5 * time.Second

// runHelpOutput executes `<bin> --help` and returns its output (stdout and
// stderr combined). A package variable so the help parsing and the cache can
// be tested without spawning processes.
var runHelpOutput = func(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helpProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	// A CLI may exit non-zero after printing help; the output is what counts.
	return string(out), nil
}

// binaryIdentity returns the cache key for a binary: modification time and
// size. The port replaces ~/.opencode/bin/opencode underneath the daemon, so
// the path alone is not an identity — a cached capability must expire when the
// bytes change. Reports false when the path cannot be stat'ed.
var binaryIdentity = func(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(fi.Size(), 10), true
}

// flagLinePattern matches a flag-definition line in a help page: indentation,
// an optional short alias, then the long flag name.
var flagLinePattern = regexp.MustCompile(`^([ \t]+)(?:-[A-Za-z],?[ \t]+)?(-{1,2}[A-Za-z][A-Za-z0-9-]*)`)

// themeFlagPlaceholderPattern matches a `--theme` flag that takes a value: the
// placeholder follows the flag immediately (space- or equals-separated) and is
// then separated from the description by the alignment gap (two or more
// spaces). A boolean flag puts its description's first word there instead,
// followed by a single space — that shape is rejected, because a flag without
// a value placeholder does not tell us it accepts one.
var themeFlagPlaceholderPattern = regexp.MustCompile(`--theme(?:=[ \t]*|[ \t]+)\S+[ \t]{2,}\S`)

// parseThemeFlagSupport reports whether a help page documents `--theme` with a
// value. Two independent conditions must hold:
//
//   - the flag sits at the help's flag column (the shallowest indentation any
//     flag definition uses). Prose that merely mentions "--theme", and wrapped
//     description lines that happen to start with it, are indented deeper.
//   - the flag line carries a value placeholder.
func parseThemeFlagSupport(help string) bool {
	lines := strings.Split(help, "\n")
	column := -1
	for _, line := range lines {
		if m := flagLinePattern.FindStringSubmatch(line); m != nil {
			if indent := len(m[1]); column < 0 || indent < column {
				column = indent
			}
		}
	}
	if column < 0 {
		return false
	}
	for _, line := range lines {
		m := flagLinePattern.FindStringSubmatch(line)
		if m == nil || len(m[1]) != column || m[2] != "--theme" {
			continue
		}
		if themeFlagPlaceholderPattern.MatchString(line[len(m[1]):]) {
			return true
		}
	}
	return false
}

type themeFlagEntry struct {
	identity  string
	supported bool
}

var (
	themeFlagMu    sync.Mutex
	themeFlagCache = map[string]themeFlagEntry{}
)

// resetThemeFlagCache drops all cached capability verdicts (tests).
func resetThemeFlagCache() {
	themeFlagMu.Lock()
	defer themeFlagMu.Unlock()
	themeFlagCache = map[string]themeFlagEntry{}
}

// opencodeSupportsThemeFlag reports whether bin accepts `--theme <name>`.
//
// One probe per binary generation: the verdict is cached under the binary's
// mtime+size, so an unchanged binary costs a stat and a replaced binary is
// probed again. Unknown (no stat, help failed) means "no flag" — the caller
// then falls back to the environment route instead of passing an unknown flag
// to an older binary, which would abort its start.
func opencodeSupportsThemeFlag(bin string) bool {
	identity, ok := binaryIdentity(bin)
	if !ok {
		return false
	}

	themeFlagMu.Lock()
	entry, cached := themeFlagCache[bin]
	themeFlagMu.Unlock()
	if cached && entry.identity == identity {
		return entry.supported
	}

	help, err := runHelpOutput(bin)
	supported := err == nil && parseThemeFlagSupport(help)

	themeFlagMu.Lock()
	themeFlagCache[bin] = themeFlagEntry{identity: identity, supported: supported}
	themeFlagMu.Unlock()
	return supported
}
