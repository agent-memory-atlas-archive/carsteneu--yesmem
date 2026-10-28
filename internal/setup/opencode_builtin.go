package setup

import (
	"bytes"
	"io"
	"os"
	"time"
)

// opencodeBuiltinPluginMarkers are literals that only the yesmem plugin source
// carries: the plugin id it registers (index.ts, Plugin.define) and the daemon
// RPC it calls when a session starts (session.ts, shell.ts). A build that
// compiles the plugin into the server has both in its binary; upstream opencode
// has neither.
//
// All of them must be present. One literal on its own is weak evidence: any
// launcher that merely mentions the plugin id would appear to be a build with
// the plugin compiled in, and the effect of that false positive is a skipped
// install, which is the one direction that removes the guard instead of adding a
// redundant copy.
var opencodeBuiltinPluginMarkers = [][]byte{
	[]byte("opencode.yesmem"),
	[]byte("register_pid"),
}

// markerChunkSize is the read window of the binary search. The search keeps the
// last bytes of every window, so a marker across the boundary between two
// windows is still found.
const markerChunkSize = 512 << 10

// markerScanBudget bounds the scan loop. A single blocking read is not covered
// (a stalled network home blocks inside Read, not between two of them), but the
// gate ahead of it constrains the input to a trusted regular file. An expired
// budget reports "not built in", which installs the external copy: the safe
// direction.
var markerScanBudget = 15 * time.Second

// opencodeBuiltinPluginFn reports whether the opencode binary on this host has
// the yesmem plugin compiled in, and which binary it examined. Tests pin it:
// setup must not depend on the build the host happens to have.
var opencodeBuiltinPluginFn = detectBuiltinOpencodePlugin

// opencodePluginBuiltin reports whether the installed opencode carries the
// yesmem plugin itself, together with the binary that was asked. Where it does,
// an external plugin copy is redundant: opencode keeps the built-in one and
// reports the second copy as a duplicate plugin id.
//
// The path belongs to the answer. One home has one plugin directory, but the
// decision is taken per binary: a host can start a different build than the one
// this probe found first on PATH, and the plugin directory serves all of them.
// Without the path in the message, a wrong skip cannot be told from a right one.
func opencodePluginBuiltin() (string, bool) {
	return opencodeBuiltinPluginFn()
}

// detectBuiltinOpencodePlugin asks the binary, not the version string. A fork
// build may carry any suffix and a release build none, so the version says
// nothing about what is compiled in; the plugin's own literals do.
func detectBuiltinOpencodePlugin() (string, bool) {
	bin := detectOpencodeBinary()
	// Same gate as the version probe: this answer decides whether setup writes
	// the guard plugin, so a group or world writable launcher must not give it.
	// It also keeps the read bounded to a regular file; a FIFO on PATH would
	// otherwise stream until the process runs out of patience.
	if !isTrustedExecutable(bin) {
		return bin, false
	}
	f, err := os.Open(bin)
	if err != nil {
		return bin, false
	}
	defer f.Close()
	found, err := fileContainsMarkers(f, opencodeBuiltinPluginMarkers)
	return bin, err == nil && found
}

// fileContainsMarkers streams f once and reports whether every marker appears
// in it. Chunked on purpose: a Bun single-file executable is hundreds of
// megabytes, and reading it whole would hold that much in memory during setup.
func fileContainsMarkers(f *os.File, markers [][]byte) (bool, error) {
	if len(markers) == 0 {
		return true, nil
	}
	keep := 0
	for _, marker := range markers {
		if len(marker) == 0 {
			return true, nil
		}
		if len(marker)-1 > keep {
			keep = len(marker) - 1
		}
	}
	found := make([]bool, len(markers))
	remaining := len(markers)

	// One buffer for the whole scan. The tail of the previous read stays at the
	// front of the next one, so every boundary is covered.
	buf := make([]byte, keep+markerChunkSize)
	filled := 0
	start := time.Now()
	for {
		n, err := f.Read(buf[filled:])
		filled += n
		if n > 0 || err == io.EOF {
			for i, marker := range markers {
				if !found[i] && bytes.Contains(buf[:filled], marker) {
					found[i] = true
					remaining--
				}
			}
			if remaining == 0 {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// A reader that returns neither bytes nor an error would spin here.
		// Stop and let the caller install the copy.
		if n == 0 {
			return false, nil
		}
		if time.Since(start) > markerScanBudget {
			return false, nil
		}
		if filled > keep {
			copy(buf[:keep], buf[filled-keep:filled])
			filled = keep
		}
	}
}
