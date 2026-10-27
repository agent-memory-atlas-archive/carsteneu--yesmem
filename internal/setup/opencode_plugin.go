package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/carsteneu/yesmem/internal/buildinfo"
	yesmemPlugins "github.com/carsteneu/yesmem/plugins"
)

// isOpencodePluginRuntimeFile reports whether a bundled name belongs in the installed plugin
// directory. The bundle embeds the plugin's suite alongside its sources so the repo carries a
// runnable suite; neither the tests nor their shared harness reach the user. Two test naming
// conventions meet here: the plugin's own `_test.ts` and the `.test.ts` the suite inherited from
// the opencode fork, where the plugin was a built-in.
func isOpencodePluginRuntimeFile(name string) bool {
	if strings.HasSuffix(name, "_test.ts") || strings.HasSuffix(name, ".test.ts") {
		return false
	}
	return name != "opencode-yesmem-fixture.ts"
}

// installEmbeddedPluginDir mirrors an embedded plugin bundle into dstDir and
// returns the number of files written. A file whose content already matches is
// left alone. Stale plugin sources are deleted: the directory is generated
// output, so what the bundle no longer carries (a renamed module, a removed
// hook, a test file from an older version) would keep loading next to the
// current sources.
//
// The prune only considers extensions the bundle itself carries. Everything
// else in the directory (a file a user placed next to the sources, an
// extension the bundle does not ship) is not ours to delete.
func installEmbeddedPluginDir(fsys fs.FS, root, dstDir string) (int, error) {
	if fi, err := os.Lstat(dstDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// Writing through a symlinked plugin directory would put the bundle,
		// and the prune below, into whatever it points at.
		return 0, fmt.Errorf("plugin dir %s is a symlink, refusing to install", dstDir)
	}
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return 0, fmt.Errorf("create plugin dir: %w", err)
	}

	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return 0, fmt.Errorf("read embedded plugin: %w", err)
	}

	wanted := make(map[string]bool, len(entries))
	extensions := make(map[string]bool, len(entries))
	installed := 0
	for _, e := range entries {
		if e.IsDir() || !isOpencodePluginRuntimeFile(e.Name()) {
			continue
		}
		data, err := fs.ReadFile(fsys, root+"/"+e.Name())
		if err != nil {
			return installed, fmt.Errorf("read embedded %s: %w", e.Name(), err)
		}
		wanted[e.Name()] = true
		extensions[filepath.Ext(e.Name())] = true

		dstPath := filepath.Join(dstDir, e.Name())
		if existing, err := os.ReadFile(dstPath); err == nil {
			if sha256.Sum256(existing) == sha256.Sum256(data) {
				continue
			}
		}
		// Temp file plus rename: this replaces the directory entry, so an
		// existing symlink or hardlink under a bundle name is replaced rather
		// than written through.
		if err := writeFileAtomic(dstPath, data, 0644); err != nil {
			return installed, fmt.Errorf("write plugin %s: %w", e.Name(), err)
		}
		installed++
	}

	present, err := os.ReadDir(dstDir)
	if err != nil {
		return installed, fmt.Errorf("read plugin dir: %w", err)
	}
	for _, e := range present {
		if e.IsDir() || wanted[e.Name()] || !extensions[filepath.Ext(e.Name())] {
			continue
		}
		if err := os.Remove(filepath.Join(dstDir, e.Name())); err != nil {
			return installed, fmt.Errorf("remove stale plugin file %s: %w", e.Name(), err)
		}
	}
	return installed, nil
}

// opencodeV2Major is the first opencode major with the v2 plugin API. From it
// on, the server plugin is a directory that registers hooks through
// Plugin.define. Earlier majors are no longer supported: the v1 bundle is gone.
const opencodeV2Major = 2

// opencodeVersionUnknown is what the version probe reports when it cannot read
// a version. It is not a major, so it must stay distinguishable from a real
// 0.x: an unknown host gets the v2 install, a known pre-v2 host gets the
// requirement message instead.
const opencodeVersionUnknown = -1

// opencodeVersionFn reports the opencode major version to install for, or
// opencodeVersionUnknown. Tests pin it: setup must not depend on the host's
// installation.
var opencodeVersionFn = detectOpencodeMajorVersion

// detectOpencodeMajorVersion asks the installed opencode binary. Any failure
// (not installed, probe timeout, unparseable output) yields
// opencodeVersionUnknown: v2 is the only plugin there is, so an unknown host
// gets the v2 install anyway. The probe only decides whether the host gets the
// requirement reported.
func detectOpencodeMajorVersion() int {
	bin := detectOpencodeBinary()
	if !isTrustedExecutable(bin) {
		return opencodeVersionUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Bounded: a broken binary must not be able to stream its way into memory.
	stdout := &cappedWriter{limit: 64}
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout = stdout
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return opencodeVersionUnknown
	}
	return parseOpencodeMajorVersion(stdout.String())
}

// isTrustedExecutable reports whether path may be run to answer which plugin to
// install. Its answer steers what setup writes and deletes, so a group or
// world-writable launcher must not decide it.
//
// This is a heuristic, not a boundary. It follows symlinks (package managers
// link the launcher) and inspects the file only; a window between check and
// exec remains, and the owner is not checked because the check has to build on
// every platform. It keeps a sloppy PATH from deciding an install; it does not
// stop a local attacker who already controls this home.
func isTrustedExecutable(path string) bool {
	if path == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Mode().Perm()&0111 == 0 {
		return false
	}
	return fi.Mode().Perm()&0022 == 0
}

// setupLogText keeps a path taken from the environment from breaking setup's
// line-based output, and from reordering it on screen: a PATH or HOME entry with
// a control character, a format character (bidi override, zero width) or a
// Unicode line separator must not be able to forge or reshuffle a line. Same
// set the persona and scanner sanitizers use. Presentation only, not a trust
// boundary.
func setupLogText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '\u2028' || r == '\u2029' {
			return '?'
		}
		return r
	}, s)
}

// cappedWriter keeps at most limit bytes and reports every write as consumed.
type cappedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func (w *cappedWriter) String() string { return w.buf.String() }

// parseOpencodeMajorVersion reads the leading major of an opencode version
// string ("2.0.29-patched.1" and "v2.0.29" both give 2). Anything else is not a
// version this code can act on and yields opencodeVersionUnknown.
func parseOpencodeMajorVersion(out string) int {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return opencodeVersionUnknown
	}
	major, err := strconv.Atoi(strings.SplitN(strings.TrimLeft(fields[0], "vV"), ".", 2)[0])
	if err != nil || major < 0 {
		return opencodeVersionUnknown
	}
	return major
}

// installOpencodePluginFor installs the v2 plugin bundle and clears what a v1
// install left behind. opencode v1 is no longer supported (D17): a known
// pre-v2 host gets the requirement reported instead of a plugin. An unknown
// version is not a pre-v2 host, so it gets the v2 install.
//
// The returned detail is what setup shows next to the step: it carries a
// decision the file count cannot express. Empty means "nothing to add".
func installOpencodePluginFor(home string, major int) (int, string, error) {
	if major != opencodeVersionUnknown && major < opencodeV2Major {
		versionErr := fmt.Errorf("opencode %d.x is no longer supported, YesMem requires opencode %d or newer", major, opencodeV2Major)
		return 0, "", errors.Join(versionErr, removeOpencodeV1Install(home))
	}
	// A build with the plugin compiled in registers the built-in copy first and
	// drops the external one as a duplicate id, so setup delivers nothing there.
	if bin, builtin := opencodePluginBuiltin(); builtin {
		probed := setupLogText(bin)
		detail := "built into " + probed + ", external copy skipped"
		if redundant := redundantOpencodePluginCopy(home); redundant != "" {
			detail = "built into " + probed + ", the copy in " + setupLogText(redundant) + " is redundant"
		}
		return 0, detail, removeOpencodeV1Install(home)
	}
	// The bundle first: a config that cannot be rewritten must not keep the
	// usable plugin off the host.
	n, installErr := installOpencodePluginV2(home)
	return n, "", errors.Join(installErr, removeOpencodeV1Install(home))
}

// opencodePluginV2Dir is where the v2 plugin bundle is mirrored to.
func opencodePluginV2Dir(home string) string {
	return filepath.Join(home, opencodePluginsRelDir, "opencode-yesmem")
}

// redundantOpencodePluginCopy reports an external copy that a build with the
// built-in plugin does not need. It is named, not deleted: opencode keeps the
// built-in copy and drops the external one regardless of version, so the
// leftovers are cosmetic, and deleting a directory an older yesmem wrote is not
// this step's call.
func redundantOpencodePluginCopy(home string) string {
	dir := opencodePluginV2Dir(home)
	if _, err := os.Stat(filepath.Join(dir, "index.ts")); err != nil {
		return ""
	}
	return dir
}

// installOpencodePluginV2 mirrors the v2 server plugin into
// ~/.config/opencode/plugins/opencode-yesmem/. opencode v2 discovers plugin
// directories there, so no configuration entry is needed (and a configured file
// target would be dropped).
//
// Builds that compile the v2 plugin into the server carry a duplicate of this
// directory's plugin. opencode drops the second plugin with the same id
// ("Duplicate plugin ID"), so the copy stays inert there; it is the delivery
// vehicle for builds that do not.
func installOpencodePluginV2(home string) (int, error) {
	dstDir := opencodePluginV2Dir(home)
	n, err := installEmbeddedPluginDir(yesmemPlugins.BundledOpencodePluginV2, "opencode-yesmem-v2", dstDir)
	if err != nil {
		return n, err
	}
	manifest, err := writeOpencodePluginManifest(dstDir)
	if err != nil {
		return n, err
	}
	return n + manifest, nil
}

// opencodePluginManifestName is the id opencode reports for the installed
// plugin directory.
const opencodePluginManifestName = "opencode-yesmem"

// opencodePluginManifest is the package.json of the installed plugin directory. Two verified effects:
// the host includes it in the mtime set it watches for a directory plugin
// (packages/core/src/config/plugin/source.ts), so an upgrade that rewrites it makes opencode reload the
// plugin; and it names the directory as the package it is. It does NOT change what `opencode plugin
// list` prints: that command maps every directory plugin to "local" regardless of its manifest
// (packages/cli/src/commands/handlers/plugin/list.ts). Generated rather than embedded so the version is
// the one of the binary that installed it.
func opencodePluginManifest(version string) ([]byte, error) {
	data, err := json.MarshalIndent(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Type    string `json:"type"`
	}{opencodePluginManifestName, version, "module"}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeOpencodePluginManifest writes the generated package.json and reports
// whether it changed, so an unchanged install keeps reporting zero updated
// files.
func writeOpencodePluginManifest(dstDir string) (int, error) {
	data, err := opencodePluginManifest(buildinfo.Version)
	if err != nil {
		return 0, fmt.Errorf("build plugin manifest: %w", err)
	}
	path := filepath.Join(dstDir, "package.json")
	if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, data) {
		return 0, nil
	}
	if err := writeFileAtomic(path, data, 0644); err != nil {
		return 0, fmt.Errorf("write plugin manifest: %w", err)
	}
	return 1, nil
}

// removeOpencodeV1Install deletes everything an opencode v1 install of yesmem
// left behind: the yesmem.ts symlink, the plugin entry pointing at the v1
// index.ts, and the source tree. opencode v2 also discovers .ts files in the
// plugin directory, so a leftover v1 entry is not inert there.
//
// The sources stay when the config entry cannot be removed (a config yesmem
// cannot parse): deleting both would leave the config pointing at a path that
// no longer exists.
func removeOpencodeV1Install(home string) error {
	if err := removeOpencodeV1PluginArtifacts(home); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(home, filepath.Dir(v1PluginPersistentRelPath))); err != nil {
		return fmt.Errorf("remove v1 plugin sources: %w", err)
	}
	return nil
}

// removeOpencodeV1PluginArtifacts deletes what a v1 install left in opencode's
// plugin directory and config: the yesmem.ts symlink and the plugin entry that
// points at the v1 index.ts. A config that cannot be parsed is left alone; the
// caller reports the error instead of rewriting it.
func removeOpencodeV1PluginArtifacts(home string) error {
	symlinkPath := filepath.Join(home, opencodePluginsRelDir, "yesmem.ts")
	if _, err := os.Lstat(symlinkPath); err == nil {
		if err := os.Remove(symlinkPath); err != nil {
			return fmt.Errorf("remove v1 plugin symlink: %w", err)
		}
	}

	cfgPath := opencodeConfigPath(home)
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", cfgPath, err)
	}
	cfg, err := readOpencodeConfigObject(cfgPath)
	if err != nil {
		return err
	}
	if !dropYesmemPluginEntry(cfg, home) {
		return nil
	}
	return writeOpencodeConfig(cfgPath, cfg)
}

// v1PluginPersistentRelPath is the install path a v1 plugin entry names,
// relative to the home directory.
const v1PluginPersistentRelPath = ".local/share/yesmem/plugins/opencode-yesmem/index.ts"

// isV1PluginEntry reports whether a config plugin entry points at a yesmem
// plugin file. Accepted: our own persistent install path, the v2 install dir
// and the dev fallback the v1 installer wrote (<bin>/../plugins/opencode-yesmem/
// index.ts) inside our home, and a system-wide install under share/yesmem. The
// same shape under another home's persistent install dir belongs to someone
// else and stays, and neither does a bare directory named opencode-yesmem
// outside our home belong to us: claiming every such path deleted another
// project's registration on uninstall and reported it as our install in status.
func isV1PluginEntry(entry, home string) bool {
	return isV1PluginEntryFrom(entry, home, runningExecutable())
}

// runningExecutable is the binary the v1 dev fallback was written next to.
func runningExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

func isV1PluginEntryFrom(entry, home, executable string) bool {
	clean := filepath.Clean(entry)
	if !filepath.IsAbs(clean) {
		return false
	}
	// A home reached through a symlink is spelled both ways: the entry carries
	// the resolved path when the install resolved it, while uninstall sees the
	// link (or the reverse). Comparing the home prefix covers both spellings
	// without requiring the file to still exist.
	if rest, ok := strings.CutSuffix(clean, "/"+v1PluginPersistentRelPath); ok {
		return sameHome(rest, home)
	}
	if !strings.HasSuffix(clean, "/plugins/opencode-yesmem/index.ts") {
		return false
	}
	// A path inside a persistent install belongs to that install, not to the
	// tail shape it happens to end with.
	if strings.Contains(clean, "/"+filepath.Dir(v1PluginPersistentRelPath)+"/") {
		return false
	}
	if isDevTreeEntry(clean, executable) {
		return true
	}
	for _, path := range opencodePluginV2EntryPaths(home) {
		if clean == path {
			return true
		}
	}
	return isSystemInstallEntry(clean)
}

// opencodePluginV2EntryPaths are the config entries that name the v2 install
// directory, in both spellings a symlinked home produces.
func opencodePluginV2EntryPaths(home string) []string {
	dirs := []string{opencodePluginV2Dir(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		if dir := opencodePluginV2Dir(resolved); dir != dirs[0] {
			dirs = append(dirs, dir)
		}
	}
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(dir, "index.ts"))
	}
	return paths
}

// isDevTreeEntry reports the shape the v1 installer wrote next to the binary it
// ran from: <bin>/../plugins/opencode-yesmem/index.ts. Anchoring on the running
// binary keeps it a proof instead of a tail match.
func isDevTreeEntry(clean, executable string) bool {
	if executable == "" {
		return false
	}
	dirs := []string{filepath.Dir(executable)}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		if dir := filepath.Dir(resolved); dir != dirs[0] {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		if clean == filepath.Join(dir, "..", "plugins", "opencode-yesmem", "index.ts") {
			return true
		}
	}
	return false
}

// systemInstallShareRoots are the roots a system-wide install writes its
// share/yesmem tree under. The list is exact: a share/yesmem tree below any
// other directory (/home/other, /opt/vendor-x) is a copy, not an install of ours.
var systemInstallShareRoots = []string{"/usr", "/usr/local", "/opt", "/opt/local", "/opt/homebrew"}

func isSystemInstallEntry(clean string) bool {
	const tail = "/share/yesmem/plugins/opencode-yesmem/index.ts"
	if !strings.HasSuffix(clean, tail) {
		return false
	}
	root := strings.TrimSuffix(clean, tail)
	for _, candidate := range systemInstallShareRoots {
		if root == candidate {
			return true
		}
	}
	return false
}

// sameHome compares two home directories, resolving symlinks on both sides. A
// path that cannot be resolved is compared literally, which is what the caller
// already did.
func sameHome(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	resolvedB, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return resolvedA == resolvedB
}

// dropYesmemPluginEntry removes yesmem's v1 file target from cfg["plugin"] and
// reports whether it changed anything. Entries for other plugins stay.
func dropYesmemPluginEntry(cfg map[string]any, home string) bool {
	entries, ok := cfg["plugin"].([]any)
	if !ok || len(entries) == 0 {
		return false
	}
	kept := make([]any, 0, len(entries))
	for _, entry := range entries {
		if s, ok := entry.(string); ok && isV1PluginEntry(s, home) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == len(entries) {
		return false
	}
	if len(kept) == 0 {
		delete(cfg, "plugin")
	} else {
		cfg["plugin"] = kept
	}
	return true
}

// writeOpencodeConfig stores an opencode config atomically. A truncate-in-place
// write that is interrupted leaves a file the fail-closed read cannot repair,
// and the user's manual settings would be gone.
func writeOpencodeConfig(path string, cfg map[string]any) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// A dotfile-managed config is a symlink into the user's repository. A
	// rename would replace the link and leave that repository stale, so the
	// target is written instead; the mode comes from it as well, so a config
	// with permissions of its own keeps them. The write itself needs only write
	// permission on the directory, so a 0444 config is replaced, not refused.
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			// A link a rename would silently turn into a regular file: the
			// config stays a link, and the caller reports why.
			return fmt.Errorf("config %s is a symlink that cannot be resolved: %w", path, err)
		}
		if fi, err := os.Stat(resolved); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("config %s is a symlink to a target that is not a regular file", path)
		}
		target = resolved
	}
	mode := os.FileMode(0644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".opencode-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	// Without the flush the rename can land before the data blocks, which is
	// exactly the torn file this replaces.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// writeFileAtomic replaces path with data through a temp file in the same
// directory. The rename swaps the directory entry, so an existing symlink or
// hardlink at path is replaced rather than written through.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plugin-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
