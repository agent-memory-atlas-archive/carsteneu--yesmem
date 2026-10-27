package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maskEntry is one host path the sandbox hides, plus whether it is a directory
// on a real host. The type decides the placeholder when the path does not exist
// yet: ai-jail masks a file and a directory alike, but a directory path
// materialised as a file would make the daemon's own MkdirAll fail later.
type maskEntry struct {
	path string
	dir  bool
	// soft marks an entry whose placeholder may fail without refusing the run. The
	// path is protected when it can be materialised; when it cannot, the run goes
	// ahead with one open side channel. Used for the SQLite log siblings of the
	// opencode session database, which live in a directory the daemon may not own:
	// refusing every standard run because of an unwritable foreign directory would
	// cost more than the channel it closes.
	soft bool
}

// claudeGlobalConfig is the Claude Code config file in the home root. It is mapped
// read-only, not masked (newSandbox): masking it makes the CLI abort with
// "configuration file ... is corrupted: JSON Parse error: Unexpected EOF"
// (measured), while a read-only map blocks the write that this file's mcpServers
// entry would otherwise turn into a command at the next start — and the CLI runs
// (measured: `claude -p` returns, a write is refused with "read-only file system").
// The mask is the fallback for the shapes in which a map cannot hold at all (no
// work dir of its own, or a work dir whose cwd bind covers the file), and it costs
// a sandboxed `claude -p` in exactly those shapes. The residual of the map is
// confidentiality: a job can still read the file.
func claudeGlobalConfig(home string) string {
	return filepath.Join(home, ".claude.json")
}

// defaultSecretMasks lists host paths the sandbox replaces with an empty
// placeholder. Every returned path exists when it is returned: a path that does
// not exist yet is materialised first, because ai-jail treats a missing --mask
// path as a silent no-op. Measured on 0.9.2: the job created the masked file on
// the host and the run still exited 0.
func defaultSecretMasks() []string {
	return maskDefaults()
}

// maskDefaults builds the default mask list. It drops the error on purpose: it is
// the convenience form for callers that only read the list (tests, log output).
// Anything that starts a run has to use maskDefaultsChecked, or a mask that cannot
// be applied stays a silent no-op.
func maskDefaults() []string {
	paths, _ := maskDefaultsChecked()
	return paths
}

// maskDefaultsChecked is the constructing variant: it reports the first mask
// placeholder it could not create, so a caller that runs commands can refuse the
// run instead of hoping the mask holds (see SandboxConfig.PrepareErr).
func maskDefaultsChecked() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// Every default mask below is derived from the home path, so without one the
		// list comes back empty and a standard-profile run would start with provider
		// keys and agent state reachable. That is the same class of failure as a
		// placeholder that cannot be created, so it is reported the same way and the
		// run is refused (SandboxConfig.PrepareErr).
		return nil, fmt.Errorf("sandbox masks need a home directory: %w", err)
	}
	// Files in the home root that are only masked when they exist. They are never
	// materialised: a zero-byte ~/.ai-jail would replace a real ai-jail config with
	// an empty one on the very next run, and an empty ~/.mcp.json means nothing.
	// ~/.ai-jail is the global ai-jail config, which the next run reads
	// unconditionally; ~/.mcp.json is a user-level MCP carrier that setup writes
	// itself. A missing ~/.ssh holds nothing to hide, and creating one with the
	// wrong permissions would be its own problem.
	masks := existingPaths([]string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".ai-jail"),
		filepath.Join(home, ".mcp.json"),
	})
	entries := append(agentStateMasks(home), opencodeCredentialMasks(home)...)
	// The opencode data directory as a whole. The file masks inside it name what the
	// daemon reads today; a run that can create one of the absent names would win the
	// race any existence filter leaves open — opencode.db itself, its -wal/-shm/
	// -journal (YM-314). With the directory masked a run sees an empty, ephemeral
	// directory, so nothing it writes there reaches the host. Measured on 0.9.2: a
	// directory mask and a file mask inside it coexist, and a write below lands in
	// the tmpfs (the host directory stayed unchanged).
	entries = append(entries, opencodeDataMasksAt(filepath.Join(home, ".local", "share"))...)
	// An inherited XDG_CONFIG_HOME/XDG_DATA_HOME moves the agent tools' config and
	// key files to another root entirely. Both roots are masked: the home defaults
	// and, when the environment points elsewhere, the effective ones — otherwise a
	// host with a custom XDG root would keep its config (and the write channel that
	// file is) readable from a run.
	configRoot, dataRoot := xdgDirs()
	if configRoot != "" && configRoot != filepath.Join(home, ".config") {
		entries = append(entries, opencodeConfigMasksAt(configRoot)...)
	}
	if dataRoot != "" && dataRoot != filepath.Join(home, ".local", "share") {
		entries = append(entries, opencodeCredentialMasksAt(dataRoot)...)
		entries = append(entries, opencodeDataMasksAt(dataRoot)...)
	}
	// The opencode session database is read by the daemon unsandboxed — session
	// discovery, child-session gating, resumability and the DONE-guard evidence
	// layer all query it — while ai-jail 0.9.x leaves it writable from a run. The
	// daemon resolves it below $HOME regardless of XDG_DATA_HOME (handler.go:982,
	// daemon.go:295), so both roots are covered.
	entries = append(entries, opencodeSessionDBMaskEntries(filepath.Join(home, ".local", "share"))...)
	if dataRoot != "" && dataRoot != filepath.Join(home, ".local", "share") {
		entries = append(entries, opencodeSessionDBMaskEntries(dataRoot)...)
	}
	// The backups beside the key files carry the same key (measured on this host:
	// ~/.claude/settings.json.bak*, ~/.local/share/opencode/auth.json.bak-*).
	// Only existing ones can be listed: the name is a suffix, so a backup written
	// after this call is readable for the rest of that run — the list is rebuilt
	// per run, which closes it from the next one on.
	entries = append(entries, backupEntries(entries)...)
	paths, maskErr := materialiseMaskPaths(entries)
	masks = appendMissingPaths(masks, paths...)
	// ~/.claude/config.json is masked only when it is there: a missing key file
	// holds nothing to read, and materialising one would invent an install —
	// detect.go hasClaudeCodeConfig probes exactly this path for existence.
	conditional := append(agentCredentialMasks(home), backupsOf(agentCredentialMasks(home))...)
	masks = appendMissingPaths(masks, existingPaths(conditional)...)
	// The claude global config is mapped read-only rather than masked (a mask
	// aborts the CLI, measured), and its backups carry the same mcpServers. Nothing
	// reads them, so they are masked — existence filtered, never materialised: a
	// placeholder named ".backup" would look like a restore point.
	masks = appendMissingPaths(masks, existingPaths(backupsOf([]string{claudeGlobalConfig(home)}))...)
	return masks, maskErr
}

// backupEntries turns every file entry into an entry per existing backup beside it.
func backupEntries(entries []maskEntry) []maskEntry {
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.dir {
			files = append(files, entry.path)
		}
	}
	out := make([]maskEntry, 0, len(files))
	for _, path := range backupsOf(files) {
		out = append(out, maskEntry{path: path})
	}
	return out
}

// maxBackupMasks bounds the backup enumeration. The list becomes ai-jail's own
// argument list, and a run can create files beside a masked path: unbounded, one
// run could grow the argv past the exec limit and every later run would fail
// before it starts. Beyond this count the state is no longer user state.
const maxBackupMasks = 64

// backupsOf returns the existing backups of the given files. Glob only reports
// paths that are there, so nothing is invented.
func backupsOf(paths []string) []string {
	var out []string
	warned := false
	for _, path := range paths {
		for _, pattern := range []string{path + ".bak*", path + ".backup*"} {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			for _, match := range matches {
				if len(out) >= maxBackupMasks {
					if !warned {
						log.Printf("[sandbox] WARNING: more than %d backup files beside the masked paths, the rest stays unmasked", maxBackupMasks)
						warned = true
					}
					return out
				}
				out = appendMissingPaths(out, match)
			}
		}
	}
	return out
}

// materialiseMaskPaths creates the placeholder for every entry that does not
// exist yet and returns all paths. A path whose placeholder cannot be created is
// still returned, and the failure is reported to the caller: a mask ai-jail cannot
// apply protects nothing, but dropping the path would silently reopen the channel
// this list exists to close — a job could then create the file itself and the next
// agent start would read it. Handing the error back lets the run be refused
// instead (SandboxConfig.PrepareErr). Entries marked soft are the exception: their
// failure is logged and the run goes ahead, for channels where refusing every run
// would cost more than the one path (see maskEntry.soft).
func materialiseMaskPaths(entries []maskEntry) ([]string, error) {
	paths := make([]string, 0, len(entries))
	var firstErr error
	for _, entry := range entries {
		// Lstat, not Stat: a symlink is a path in its own right here. Following it
		// would aim the placeholder at whatever the link points to, and the link is
		// exactly what an attacker-controlled path would be made of.
		if _, err := os.Lstat(entry.path); err != nil {
			if err := createMaskPlaceholder(entry); err != nil {
				if entry.soft {
					log.Printf("[sandbox] WARNING: mask placeholder %s cannot be created (%v), that path stays reachable for this run", entry.path, err)
				} else if firstErr == nil {
					firstErr = fmt.Errorf("mask placeholder %s: %w", entry.path, err)
				}
			}
		}
		paths = append(paths, entry.path)
	}
	return paths, firstErr
}

// createMaskPlaceholder lays down an empty entry of the right type. A JSON file
// gets "{}" rather than zero bytes: a zero-byte file is not a valid JSON object,
// and this file outlives the run — Claude Code and opencode parse it at their next
// start.
//
// A file is opened with O_EXCL and never truncated: anything already at this path
// is user data, and this runs on the construction path of every cap and job.
func createMaskPlaceholder(entry maskEntry) error {
	if entry.dir {
		return os.MkdirAll(entry.path, 0o700)
	}
	if err := os.MkdirAll(filepath.Dir(entry.path), 0o700); err != nil {
		return err
	}
	var body []byte
	if strings.HasSuffix(entry.path, ".json") {
		body = []byte("{}\n")
	}
	file, err := os.OpenFile(entry.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// agentStateMasks lists the agent host state a sandboxed run must not reach
// (YM-284, extended by YM-291). Every entry is a channel that executes or steers
// code outside this sandbox:
//
//   - ~/.claude.json carries mcpServers, whose commands Claude Code starts on its
//     next launch.
//   - settings.json, settings.local.json and hooks are executed at every Claude
//     Code start; caps is the CAP.md re-import channel the daemon runs
//     unsandboxed elsewhere.
//   - backups holds the copies Claude Code writes when it aborts, ~/.claude.json
//     among them — the same mcpServers with the same force.
//   - CLAUDE.md, AGENTS.md, commands, skills, agents, rules and output-styles are
//     read as instructions by the next session or run as code by it (YM-291).
//   - the opencode config directory as a whole: the config file (plugin paths and
//     mcpServers the next opencode start executes) and the subdirectories opencode
//     discovers and loads on its own — plugin, plugins, agent, commands, prompts
//     and themes.
//
// This is a list of known channels, not a closed boundary: with ai-jail 0.9.x the
// standard profile bind-mounts ~/.claude, ~/.config and parts of ~/.local
// read-write (measured), so anything not named here stays reachable. It is also
// not a confidentiality boundary — session histories and databases remain
// readable; agentCredentialMasks covers keys only.
//
// opencode.json used to stay readable because two bundled caps read it for their
// /models command, which left plugin paths and mcpServers writable for the next
// opencode start. The directory is masked now and the model list reaches a run
// through its environment (newSandbox), so the command works without the file.
//
// Masked, not mounted read-only: ai-jail binds its own home dotdirs after the
// caller's mounts, so --map on a path below ~/.claude is overwritten by that
// writable bind (measured on 0.9.2), while --mask holds for a file and a
// directory alike.

// xdgDirs returns the config and data roots an agent tool uses on this host: the
// XDG variables when they are set, the home defaults otherwise. A sandbox pins
// them to exactly these values and masks the same directories, so a run neither
// follows an inherited redirect nor reads a config from a place no mask covers.
func xdgDirs() (configDir, dataDir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	configDir = os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	dataDir = os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share")
	}
	return configDir, dataDir
}

// opencodeConfigMasksAt is the opencode config directory below a config root,
// masked as a whole since YM-303. Naming single files was wrong twice over:
// opencode resolves its global config as opencode.jsonc → opencode.json →
// config.json (internal/opencode/paths.go), so masking opencode.json left the two
// other names open, and opencode.jsonc even takes priority over the masked file
// (measured on 0.9.2: a run whose mask list names only opencode.json writes
// opencode.jsonc on the host, and opencode reads that one). The directory likewise
// holds plugin, plugins, agent, commands, prompts and themes, which opencode loads
// by itself. Nothing a run needs lives in there: the model list two bundled caps
// show reaches them through their environment (newSandbox). The cost is a
// sandboxed opencode start without the user's providers — the run has no route to
// them anyway on 2.x, whose standard profile carries --no-network.
func opencodeConfigMasksAt(configRoot string) []maskEntry {
	return []maskEntry{{path: filepath.Join(configRoot, "opencode"), dir: true}}
}

// opencodeCredentialMasksAt lists the opencode key files below a data root.
func opencodeCredentialMasksAt(dataRoot string) []maskEntry {
	dir := filepath.Join(dataRoot, "opencode")
	return []maskEntry{
		{path: filepath.Join(dir, "auth.json")},
		{path: filepath.Join(dir, "account.json")},
		{path: filepath.Join(dir, "mcp-auth.json")},
	}
}

// opencodeDataMasksAt is the opencode data directory below a data root, masked as
// a whole. It carries the session database the daemon reads unsandboxed (session
// discovery, child-session gating, resumability, DONE-guard evidence), its SQLite
// log siblings, the provider key files and their backups. The single-file masks
// name what the daemon reads today; a run that can create one of the absent names
// wins the race any existence filter leaves open — opencode.db itself, its -wal/
// -shm/-journal. With the directory masked the run sees an empty, ephemeral
// directory. Measured on 0.9.2: a directory mask and a file mask inside it coexist,
// and a write below lands in the tmpfs (the host directory stayed unchanged).
func opencodeDataMasksAt(dataRoot string) []maskEntry {
	return []maskEntry{{path: filepath.Join(dataRoot, "opencode"), dir: true}}
}

// opencodeSessionDBMaskEntries lists the opencode session database and its SQLite
// log siblings below a data root. The database is existence filtered: a zero-byte
// placeholder would make the daemon start its opencode scanner against a schema
// that is not there (daemon.go:299). The siblings are materialised while absent,
// because SQLite deletes them on the last close and a run could otherwise append a
// frame the next read applies. An empty WAL is harmless (measured 18.10.2026 with
// sqlite3 3.45.1: read and write on a checkpointed database with zero-byte
// -wal/-shm present, both correct, SQLite removes them itself). Only the database's
// own mask is hard: the siblings are soft, so a data directory the daemon cannot
// write costs the run one side channel instead of refusing every standard launch.
func opencodeSessionDBMaskEntries(dataRoot string) []maskEntry {
	return opencodeSessionDBMaskEntriesFor(filepath.Join(dataRoot, "opencode", "opencode.db"))
}

// opencodeSessionDBMaskEntriesFor is the same list for a database path the caller
// resolved itself (paths.opencode_db in the config can move it anywhere).
func opencodeSessionDBMaskEntriesFor(db string) []maskEntry {
	if db == "" {
		return nil
	}
	// A relative path would make both the placeholder and ai-jail's own --mask
	// resolution depend on a cwd (daemon.go:290 takes the configured value as it is).
	if !filepath.IsAbs(db) {
		log.Printf("[sandbox] dropping the session db mask %q: a mask needs an absolute path", db)
		return nil
	}
	// Lstat, like every other mask entry: a symlink is a path in its own right, and
	// following it would aim the placeholder at its target.
	if _, err := os.Lstat(db); err != nil {
		return nil
	}
	// -journal is the same channel for a database in rollback-journal mode, which
	// the next reader applies as well.
	siblings := []string{db, db + "-wal", db + "-shm", db + "-journal"}
	entries := make([]maskEntry, 0, len(siblings))
	for _, path := range siblings {
		// The database itself exists, so its own mask needs no placeholder. The
		// siblings are created here and may fail in a directory the daemon does not
		// own; that costs the run one open side channel, not the whole run (soft).
		entries = append(entries, maskEntry{path: path, soft: path != db})
	}
	return entries
}

func agentStateMasks(home string) []maskEntry {
	claude := filepath.Join(home, ".claude")
	entries := []maskEntry{
		{path: filepath.Join(claude, "settings.json")},
		{path: filepath.Join(claude, "settings.local.json")},
		{path: filepath.Join(claude, "CLAUDE.md")},
		{path: filepath.Join(claude, "AGENTS.md")},
		{path: filepath.Join(claude, "caps"), dir: true},
		{path: filepath.Join(claude, "hooks"), dir: true},
		{path: filepath.Join(claude, "commands"), dir: true},
		{path: filepath.Join(claude, "skills"), dir: true},
		{path: filepath.Join(claude, "agents"), dir: true},
		{path: filepath.Join(claude, "rules"), dir: true},
		{path: filepath.Join(claude, "output-styles"), dir: true},
		{path: filepath.Join(claude, "plugins"), dir: true},
		{path: filepath.Join(claude, "backups"), dir: true},
	}
	return append(append(entries, opencodeConfigMasksAt(filepath.Join(home, ".config"))...),
		opencodeCredentialMasks(home)...)
}

// opencodeCredentialMasks are the opencode key files. Unlike the claude one they
// are materialised: nothing probes them for existence, and a job could otherwise
// create an auth.json that the next opencode start reads as its own config.
func opencodeCredentialMasks(home string) []maskEntry {
	return opencodeCredentialMasksAt(filepath.Join(home, ".local", "share"))
}

// agentCredentialMasks lists the files that hold provider keys and are only worth
// masking when they exist. ~/.claude/config.json has to stay conditional: setup's
// agent detection probes it for existence, so materialising one would report an
// installed Claude Code on a host that never had it.
func agentCredentialMasks(home string) []string {
	return []string{filepath.Join(home, ".claude", "config.json")}
}

// existingPaths keeps the candidates that exist on this host. Two kinds of
// callers want that and not the materialising path: files in the home root that
// must never be invented (a zero-byte ~/.ai-jail would break the next ai-jail
// launch) and the key directories, where a missing ~/.ssh holds nothing to hide
// and creating one with the wrong permissions would be its own problem.
//
// Whether a home-root file is reachable from the sandbox depends on the run's cwd:
// ai-jail binds its cwd read-write, so a run that inherits the daemon's cwd — the
// production case on 0.9.x — sees these files on the host (measured: identical
// inodes inside the jail, writes land on the host), while a run with its own work
// dir sees an empty tmpfs home. Masking them is therefore cheap insurance where
// they exist, and pointless where they do not, which is why the list is existence
// filtered instead of materialised.
func existingPaths(candidates []string) []string {
	kept := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		kept = append(kept, path)
	}
	return kept
}

// appendMissingPaths adds the entries of extra that dst does not carry yet. It
// returns a fresh slice, so a caller's own list is never written through, and it
// drops duplicates already present in dst.
func appendMissingPaths(dst []string, extra ...string) []string {
	out := make([]string, 0, len(dst)+len(extra))
	seen := make(map[string]bool, len(dst)+len(extra))
	for _, path := range dst {
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, path := range extra {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

type SandboxConfig struct {
	Enabled             bool `yaml:"enabled"`
	FallbackUnsandboxed bool `yaml:"fallback_unsandboxed"`
	// AllowedPorts is the job's port list. It is not rendered into flags: ai-jail
	// 0.9.2 only enforces a port allowlist under --lockdown, and lockdown cannot
	// reach the daemon socket. Kept for the ai-jail 2.x migration (--allow-host).
	AllowedPorts []int `yaml:"allowed_ports"`
	// ReadOnlyMounts are host paths mapped read-only into the sandbox for the
	// standard and strict profiles. Caps need the yesmem CLI and the restricted
	// job socket (store()/llm()); they never need to write to them. Both ai-jail
	// versions take the bare PATH form and bind it at the same path inside — but
	// only when the destination's top-level directory exists in the sandbox: a
	// mount below a directory the sandbox does not have at all (e.g. /var) is
	// dropped without a word. Every path mounted here lives under /home, /usr or
	// /run.
	//
	// A mount below one of the dotdirs ai-jail binds itself (~/.claude, ~/.config,
	// parts of ~/.local) is ineffective: ai-jail lays its own read-write binds
	// after the caller's mounts and shadows them (measured on 0.9.2, YM-303). The
	// paths that matter here — the socket sits in the runtime dir beside the data
	// dir, the binary under ~/.local/bin — are outside those binds today. A path
	// that moved under ~/.claude would need a --mask, not a map.
	ReadOnlyMounts []string `yaml:"read_only_mounts"`
	// MaskedPaths are host paths the sandbox replaces with an empty placeholder.
	// Used by the standard profile, which does not lock the home down.
	MaskedPaths []string `yaml:"masked_paths"`
	// AllowedHosts is the egress allowlist for the standard profile under
	// ai-jail 2.x. There, --no-network leaves no route off the host except
	// ai-jail's own CONNECT proxy, which dials exactly these hosts; an empty list
	// means no egress at all. ai-jail 0.9.2 has no equivalent — its standard
	// profile runs with an open network.
	AllowedHosts []string `yaml:"allowed_hosts"`
	// WorkDir is the working directory for a run. Empty means "inherit the
	// daemon's". A scheduled job sets its project directory so the run does not
	// depend on the daemon's cwd (ai-jail reads a .ai-jail from there).
	//
	// A work dir that is the user's home or an ancestor of it is not supported and
	// refuses the run: ai-jail binds this directory read-write after the caller's
	// mounts, so the home would be bound over the sandbox and the maps of the
	// yesmem CLI and the job socket would be void (YM-428, see workDirCoversHome).
	// The exception is `sandbox: none`, which starts the command directly and binds
	// nothing.
	WorkDir string `yaml:"work_dir"`
	// ForwardPorts are host loopback ports relayed into the sandbox on ai-jail
	// 2.x (--forward-port). A sandboxed agent needs the proxy port here, because
	// 2.x puts the sandbox on its own loopback. Ignored on 0.9.x, which shares
	// the host loopback and has no such flag.
	ForwardPorts []int `yaml:"forward_ports"`
	// RWMapPaths are host paths mapped read-write into the sandbox. A scheduled
	// job maps its log directory here: /tmp inside the jail is a throwaway tmpfs,
	// so a cap log written there never reaches the host (YM-218). Lockdown drops
	// every mount, so the strict profile cannot use it.
	RWMapPaths []string `yaml:"rw_map_paths"`
	// Env is appended to the environment of the run.
	Env []string `yaml:"env"`
	// PrepareErr records a preparation problem that makes the run unsafe to start: a
	// mask placeholder that could not be materialised, or a work dir that covers the
	// user's home (the cwd bind would shadow every mask and the maps of the CLI and
	// the job socket). ai-jail treats a missing --mask path as a silent no-op, so a
	// run carrying a mask that cannot be applied would leave exactly the channel open
	// the list exists to close. A caller that runs commands refuses the run while this
	// is set: a work-dir cause for every profile but none, a mask cause only where
	// masks are rendered (Run, WrapExecArgs, maskFailure); runNeverStarted marks it as
	// a run that never started.
	PrepareErr error `yaml:"-"`
	// ClaudeConfig is the Claude global config (~/.claude.json) to hide the write
	// channel of. Set by newSandbox. Whether it is masked or mapped read-only
	// depends on the run's cwd, which only the Sandbox knows; see
	// claudeConfigFlags.
	ClaudeConfig string `yaml:"-"`
}

type Sandbox struct {
	cfg          SandboxConfig
	binaryPath   string
	binaryOnce   sync.Once
	versionOnce  sync.Once
	versionMajor int
}

var (
	sandboxLookPath   = exec.LookPath
	sandboxDownloader = downloadAiJail
	// sandboxVersionProbe reads the ai-jail major version from the binary; a test
	// seam so flag rendering can be exercised without a real ai-jail.
	sandboxVersionProbe = probeAiJailMajor
)

// aiJailUnknownVersion marks a binary whose major version could not be read.
const aiJailUnknownVersion = -1

// probeAiJailMajor runs `ai-jail --version` and returns the major version, or
// aiJailUnknownVersion when the output cannot be parsed. The probe is bounded, so
// a hanging binary cannot block the job goroutine.
func probeAiJailMajor(binary string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return aiJailUnknownVersion
	}
	return parseAiJailMajor(string(out))
}

// parseAiJailMajor reads the major version out of an `ai-jail --version` line
// such as "ai-jail 2.10.0", or aiJailUnknownVersion when there is none.
func parseAiJailMajor(out string) int {
	for _, field := range strings.Fields(out) {
		field = strings.TrimPrefix(field, "v")
		dot := strings.IndexByte(field, '.')
		if dot <= 0 {
			continue
		}
		if major, err := strconv.Atoi(field[:dot]); err == nil {
			return major
		}
	}
	return aiJailUnknownVersion
}

// aiJailMajor reports the major version of the resolved binary. It is probed
// once per Sandbox: the value decides which flag set the run uses.
func (s *Sandbox) aiJailMajor() int {
	s.versionOnce.Do(func() {
		if s.binaryPath == "" {
			return
		}
		s.versionMajor = sandboxVersionProbe(s.binaryPath)
	})
	return s.versionMajor
}

// usesLegacyFlags reports whether the resolved binary needs the 0.9.x flag set.
// Only a version string that reads as 0.x or 1.x selects it: an unreadable
// version counts as 2.x, because those flags are the stricter set — a binary we
// cannot identify loses egress instead of gaining it. A real 0.9.2 reports
// itself, so the fallback never applies to the binary this host runs today.
func (s *Sandbox) usesLegacyFlags() bool {
	major := s.aiJailMajor()
	return major == 0 || major == 1
}

func NewSandbox(cfg SandboxConfig) *Sandbox {
	return &Sandbox{cfg: cfg}
}

func (s *Sandbox) ensureBinary() {
	s.binaryOnce.Do(func() {
		if !s.cfg.Enabled {
			return
		}
		path, err := sandboxLookPath("ai-jail")
		if err == nil {
			s.binaryPath = path
			return
		}
		downloaded, dlErr := sandboxDownloader()
		if dlErr != nil {
			log.Printf("[sandbox] ai-jail not available: %v (fallback=%v)", dlErr, s.cfg.FallbackUnsandboxed)
			return
		}
		s.binaryPath = downloaded
	})
}

func (s *Sandbox) Available() bool {
	s.ensureBinary()
	return s.binaryPath != ""
}

// Env is the environment the run has to carry itself. ai-jail 2.x only forwards
// the NAME of a variable (withEnvForwards) and takes the value from the process
// environment, while 0.9.x inherits it — so a value that sits only in cfg.Env
// never reaches a run that builds its own exec.Cmd. A caller assembling the
// command has to merge this in.
func (s *Sandbox) Env() []string {
	return s.cfg.Env
}

// dedupeEnv keeps the last entry per key. cfg.Env is assembled from caller values
// and defaults (a cap caller already sets YESMEM_SOCK, newSandbox appends it), and
// a duplicate name reaches ai-jail as a second --env flag on 2.x and as two entries
// in the child environment on 0.9.x — which of them a consumer takes is undefined.
func dedupeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	seen := make(map[string]bool, len(env))
	for i := len(env) - 1; i >= 0; i-- {
		key, _, _ := strings.Cut(env[i], "=")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, env[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// sandboxFlags renders the ai-jail flags for a profile, without the command.
//
// Two flag sets exist, chosen by the binary's major version (ai-jailMajor):
//
// ai-jail 0.9.x — verified against the 0.9.2 on this host:
//   - strict runs with --lockdown: the home becomes a tmpfs and everything
//     outside /tmp is read-only. ai-jail 0.9.2 ignores --map mounts in lockdown,
//     so a cap that needs the daemon socket (store()/llm()) cannot run under
//     strict. Whether strict also closes the network depends on the host config:
//     measured on 25.10.2026 with an empty ~/.ai-jail, 0.9.2 refuses a TCP
//     connect to 1.1.1.1:443; with the old allow_tcp_ports = [80, 443] in
//     ~/.ai-jail the same run reached it. So strict's egress on 0.9.x is a
//     property of the host file, not a promise of the profile.
//   - standard runs without --lockdown, because that is the only mode in which
//     the restricted job socket and the yesmem CLI stay reachable for caps.
//     Without lockdown ai-jail enforces no filesystem or network policy beyond the
//     masks, so the home stays writable and the network is open. What remains:
//     Landlock and seccomp (named explicitly so a project .ai-jail cannot switch
//     them off) and the masked paths — the data dir as a whole and the secret
//     paths (YM-276).
//
// ai-jail 2.x — verified against 2.10.0 with real runs:
//   - strict runs with --lockdown, which now really means no network (DNS and
//     22/80/443 are all closed, measured).
//   - standard also runs without --lockdown, but for a different reason: 2.x still
//     drops --map inside lockdown, so lockdown would cut caps off from the daemon
//     socket. Unlike 0.9.x, 2.x gives standard a private tmpfs home, no mounted
//     agent state, no toolchain caches and --no-network with an explicit
//     --allow-host list, which is stricter than the 0.9.x standard on every axis.
//
// Both profiles pass --no-save-config in both versions: ai-jail otherwise
// persists its effective flag set (including the command) into the .ai-jail of
// its working directory — the user's home for cap runs, a repository for job
// runs — and that file then hijacks later flag-less ai-jail sessions started in
// the same place.
//
// Both profiles also pass --exec, which drops ai-jail's PTY proxy and status
// banner. Without it every run writes "Jail Active"/"Landlock: fully enforced"
// to stderr, and runProcess merges stderr into the job output, so the banner
// ended up in every stored job result (YM-209). --exec keeps the exit code and
// stdin (verified against 0.9.2 and 2.10.0).
func (s *Sandbox) sandboxFlags(profile SandboxProfile) []string {
	if !s.usesLegacyFlags() {
		return s.sandboxFlagsV2(profile)
	}
	return s.sandboxFlagsLegacy(profile)
}

func (s *Sandbox) sandboxFlagsLegacy(profile SandboxProfile) []string {
	// --clean ignores a .ai-jail in the working directory, on 0.9.2 as well
	// (measured: the repo mask applied without it and did not apply with it). Since
	// YM-291 a run starts in its work dir, and a job's work dir is the project — so
	// the project's own .ai-jail would otherwise be read and merged over these
	// flags: the yesmem checkout carries a tracked one whose command is a telegram
	// poll, and its mask/rw_maps/hide_dotdirs keys would widen the sandbox. The
	// global ~/.ai-jail is still read; that is host state, not repository state.
	switch profile {
	case ProfileStrict:
		return s.withReadOnlyMounts([]string{"--exec", "--lockdown", "--no-save-config", "--clean"})
	case ProfileStandard:
		args := []string{"--exec", "--no-save-config", "--clean", "--landlock", "--seccomp"}
		for _, mask := range s.cfg.MaskedPaths {
			if mask == "" {
				continue
			}
			args = append(args, "--mask", mask)
		}
		return s.claudeConfigFlags(s.withRWMapMounts(s.withReadOnlyMounts(args)))
	default:
		return nil
	}
}

func (s *Sandbox) sandboxFlagsV2(profile SandboxProfile) []string {
	// --clean ignores a .ai-jail in the working directory. Those files are host
	// state that our runs must not depend on: one carrying the 2.x-removed
	// allow_tcp_ports fails the launch before the command runs. It does not cover
	// the global ~/.ai-jail, which ai-jail reads unconditionally — that file has
	// to be migrated before the 2.x binary goes live.
	switch profile {
	case ProfileStrict:
		// Lockdown already disables the network in 2.x; --no-network spells it out
		// so a future default change cannot quietly open strict up. A forwarded
		// loopback port (below) is the one deliberate exception, because an agent
		// run under strict still has to reach the proxy.
		return s.withEnvForwards(s.withForwardPorts([]string{"--exec", "--no-save-config", "--clean", "--lockdown", "--no-network"}))
	case ProfileStandard:
		args := []string{"--exec", "--no-save-config", "--clean", "--landlock", "--seccomp", "--no-agent-state", "--no-toolchains", "--no-network"}
		for _, host := range s.cfg.AllowedHosts {
			if host == "" {
				continue
			}
			args = append(args, "--allow-host", host)
		}
		for _, mask := range s.cfg.MaskedPaths {
			if mask == "" {
				continue
			}
			args = append(args, "--mask", mask)
		}
		return s.claudeConfigFlags(s.withEnvForwards(s.withRWMapMounts(s.withReadOnlyMounts(s.withForwardPorts(args)))))
	default:
		return nil
	}
}

// claudeConfigFlags hides the write channel of ~/.claude.json, which carries
// mcpServers and would turn a write into a command at the next Claude Code start.
//
// Why it depends on the cwd: ai-jail binds the process's cwd read-write AFTER the
// caller's mounts, so every mount below it is shadowed. With a work dir the run
// starts outside the home and the map holds (measured: the file stays readable and
// the write is refused, so a sandboxed `claude -p` still starts). A run without a
// work dir never gets here: newSandbox refuses it while the run is prepared (YM-437),
// because it would inherit the daemon's cwd and bind the whole real home over the
// sandbox. The mask branch below therefore covers the remaining shape: a work dir
// that IS the home or an ancestor of the file — the cwd bind covers the config, the
// map is void, and the write reaches the host (measured: with the home as work dir an
// append landed in ~/.claude.json). The mask branch is also what a Sandbox without a
// PrepareErr renders, which means a directly constructed one — or the narrow window in
// which the directory vanished between the prepare step and this rendering: that run
// takes the map branch, because an unresolvable path makes cwdCoversClaudeConfig
// false, and runProcess then falls back to the daemon's cwd (named there).
func (s *Sandbox) claudeConfigFlags(args []string) []string {
	if s.cfg.ClaudeConfig == "" {
		return args
	}
	// A mask on the file itself or on one of its parents wins over the map. The
	// mask list is rendered before this call and ai-jail lays mounts after masks,
	// so a map here would put back exactly the path the mask hides (Befund 3,
	// YM-334). The list is the caller's, so a configured masked_paths entry
	// reaches this point.
	if sitsUnderMask(s.cfg.ClaudeConfig, s.cfg.MaskedPaths) {
		// The mask wins, and that has a cost the operator should see: a masked
		// ~/.claude.json makes the CLI abort at its next start (measured, see
		// claudeGlobalConfig). Logged on every rendering run because the state is a
		// configuration decision, not a transient.
		log.Printf("[sandbox] WARNING: ~/.claude.json is masked, a sandboxed claude start aborts on it")
		return args
	}
	if s.runWorkDir() != "" && !s.cwdCoversClaudeConfig() {
		return append(args, "--map", s.cfg.ClaudeConfig)
	}
	// Same cost as the branch above, and the same reason to log it: a run in this
	// shape loses its sandboxed `claude -p` to a 0-byte config, which is otherwise
	// only visible as an opaque JSON parse error.
	log.Printf("[sandbox] WARNING: ~/.claude.json is masked (no work dir of its own, or the cwd bind covers it), a sandboxed claude start aborts on it")
	return append(args, "--mask", s.cfg.ClaudeConfig)
}

// cwdCoversClaudeConfig reports whether the run's own cwd bind covers the config
// path. ai-jail read-write binds exactly the cwd, so a file at or below it is not
// reachable through a map. Both sides are resolved: the kernel hands ai-jail the
// resolved cwd, and projectLocalClaudeConfigs resolves for the same reason, so a
// work dir that reaches the home through a symlink must take the mask branch too.
func (s *Sandbox) cwdCoversClaudeConfig() bool {
	work := s.runWorkDir()
	if work == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	config := s.cfg.ClaudeConfig
	if resolved, err := filepath.EvalSymlinks(config); err == nil {
		config = resolved
	}
	return samePath(work, config) || pathInside(work, config)
}

// workDirCoversHome reports whether the run's own cwd bind covers the user's home:
// the work dir is the home itself or an ancestor of it.
//
// Both sides are resolved for the reason cwdCoversClaudeConfig resolves them: the
// kernel hands ai-jail the resolved cwd, so a work dir that reaches the home
// through a symlink covers it just as well. A work dir that is itself inside the
// home is not this case: the bind then covers only that subtree, which is the shape
// every cap run has (<dataDir>-run/workdir/cap:NAME), and it shadows no map below
// the home as a whole.
//
// A work dir that is gone is not this case either, but it no longer falls through for
// a sandboxed run: since YM-437 a missing work dir is refused while the run is
// prepared (errNoWorkDir). That refusal names the run's own cause and stays on the
// same gate as this one: every profile that binds the cwd is refused, none is exempt.
// The profiles that bind no cwd at all (none, the unsandboxed fallback) keep the
// runProcess fallback into the daemon's cwd, where no cwd bind can shadow a map.
func workDirCoversHome(work string) bool {
	if work == "" {
		// A missing work dir is refused on its own (errNoWorkDir, YM-437). This
		// function only answers whether the home lies inside the work dir.
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Without a home there is nothing to cover, and no home is already logged
		// where the mask list is built.
		return false
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	return samePath(work, home) || pathInside(work, home)
}

// sitsUnderMask reports whether path is one of masks or lies below one.
func sitsUnderMask(path string, masks []string) bool {
	for _, mask := range masks {
		if mask == "" {
			continue
		}
		if samePath(mask, path) || pathInside(mask, path) {
			return true
		}
	}
	return false
}

// samePath compares two paths lexically after Clean. A mask written with a
// trailing separator names the same path, and the equality branch would miss it
// while the "below" branch (filepath.Rel) cleans on its own.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// withForwardPorts relays host loopback ports into the sandbox. A forwarded port
// joins the Landlock connect set, so it composes with lockdown.
func (s *Sandbox) withForwardPorts(args []string) []string {
	for _, port := range s.cfg.ForwardPorts {
		if port <= 0 || port > 65535 {
			continue
		}
		args = append(args, "--forward-port", strconv.Itoa(port))
	}
	return args
}

func (s *Sandbox) withReadOnlyMounts(args []string) []string {
	for _, mount := range s.cfg.ReadOnlyMounts {
		if mount == "" {
			continue
		}
		args = append(args, "--map", mount)
	}
	return args
}

// AddReadOnlyMounts registers read-only mounts after construction. A caller that
// knows what its run needs (a cap and its CLI binaries) adds the paths here rather
// than rendering flags itself: the invariant from newSandbox holds on this path
// too, so a mount below a masked path is dropped instead of putting that part of
// the path back into the sandbox (YM-276, YM-314). The comparison is lexical, like
// every other mount filter here: a symlink whose target sits below a mask is not
// recognised, which is harmless for the server-controlled paths this is used for.
//
// The dropped paths are returned: without them the drop was only a log line, and a
// caller that depends on one of its mounts failed later with a symptom that named
// nothing (requiredMountErr, Befund 4).
func (s *Sandbox) AddReadOnlyMounts(paths ...string) []string {
	mounts := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		mounts = append(mounts, path)
	}
	if len(mounts) == 0 {
		return nil
	}
	added := appendMissingPaths(s.cfg.ReadOnlyMounts, mounts...)
	kept := dropMountsInside(added, s.cfg.MaskedPaths)
	s.cfg.ReadOnlyMounts = kept
	return missingPaths(added, kept)
}

// missingPaths returns the entries of all that kept does not carry, in the order
// of all.
func missingPaths(all, kept []string) []string {
	if len(kept) == len(all) {
		return nil
	}
	present := make(map[string]bool, len(kept))
	for _, path := range kept {
		present[path] = true
	}
	missing := make([]string, 0, len(all)-len(kept))
	for _, path := range all {
		if !present[path] {
			missing = append(missing, path)
		}
	}
	return missing
}

// requiredMountErr turns the paths AddReadOnlyMounts dropped into a refusal when
// one the run cannot work without is among them. A cap whose yesmem CLI or job
// socket mount vanished does not fail at once: store() and llm() run a command
// the sandbox does not have, so the caller sees a shell error that names the
// symptom and not the mask (Befund 4, YM-334). The message carries no host path
// because it travels to the cap caller (Befund 5); dropMountsInside logs both
// paths for the daemon log.
//
// The check belongs to the profile that renders mounts at all: none starts the
// command directly and strict hides the home with --lockdown, which drops every
// map, so a lost mount changes nothing about those runs. Refusing them would
// report a mask failure for a run that has neither masks nor mounts.
func requiredMountErr(dropped, required []string, profile SandboxProfile) error {
	if len(dropped) == 0 || len(required) == 0 || !profileRendersMasks(profile) {
		return nil
	}
	lost := make(map[string]bool, len(dropped))
	for _, path := range dropped {
		lost[path] = true
	}
	for _, path := range required {
		if path == "" {
			continue
		}
		if lost[path] {
			return fmt.Errorf("%w: a mount the run needs (the yesmem CLI or the job socket) lies inside a masked path, store() and llm() would fail; see the daemon log", errMaskPrepare)
		}
	}
	return nil
}

// maskRefusalForCaller is the text a caller sees for a run that failed before it
// started. A mask refusal names absolute host paths ("mask placeholder <path>:
// <err>") and the caller may itself be a sandboxed run, so the detail stays in
// the daemon log, where newSandbox already writes it (Befund 5, YM-334). Every
// other error passes through unchanged.
//
// The two work-dir refusals are the deliberate exception: their text names the work
// dir, and the caller has to be able to act on it. For a job that value is the
// operator's own project, for a cap or a job without one it is the run dir the daemon
// derived — a path inside the runtime dir that a sandboxed caller already knows from
// YESMEM_SOCK, so it carries no secret either.
func maskRefusalForCaller(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errWorkDirCoversHome) {
		// This one is not a mask, and the work dir is the caller's own input, so the
		// text stays complete: a job result that only said "a sandbox mask could not
		// be applied" would send the operator to the mask list for the one value
		// they have to change. The message is built with %q, so a path cannot add a
		// line to a job result or a cap response.
		return err.Error()
	}
	if errors.Is(err, errMaskPrepare) {
		return "a sandbox mask could not be applied, the run was refused (details in the daemon log)"
	}
	return err.Error()
}

// withRWMapMounts maps a path read-write at the same location inside the sandbox.
// Lockdown drops every mount without a warning, so no strict branch calls this.
func (s *Sandbox) withRWMapMounts(args []string) []string {
	for _, mount := range s.cfg.RWMapPaths {
		if mount == "" {
			continue
		}
		args = append(args, "--rw-map", mount)
	}
	return args
}

// withEnvForwards names the variables the run needs inside the sandbox. ai-jail
// 2.x does not inherit the environment (measured: an exported YESMEM_JOB_DIR
// arrives empty), so a job log path would silently fall back to /tmp. Only the
// name is passed on: a NAME=VALUE would be visible in ai-jail's own argv.
// ai-jail 0.9.x inherits the environment and has no --env flag, so its flag set
// does not use this.
func (s *Sandbox) withEnvForwards(args []string) []string {
	for _, kv := range s.cfg.Env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" {
			continue
		}
		args = append(args, "--env", name)
	}
	return args
}

// runWorkDir is the working directory a run gets. ai-jail treats the process's cwd
// as the project directory it mounts read-write and reads a .ai-jail from there, so
// a run that inherits the daemon's cwd — the home — gets the whole real home bound
// over the sandbox (measured: findmnt reports a tmpfs and an ext4 rw on $HOME, every
// mount below is shadowed, and ~/.claude.json becomes writable on the host).
//
// Until YM-291 the 0.9.x path returned "" and kept inheriting the daemon's cwd; that
// is what made every mount and mask-free path in the home reachable. Both majors now
// use the configured work dir, which callers already provide (one per cap, one per
// job): the run starts outside the home, the home stays a private tmpfs, and a
// read-only map survives instead of being shadowed.
func (s *Sandbox) runWorkDir() string {
	return s.cfg.WorkDir
}

// unsandboxedErr is the fail-closed error: a profile was requested, ai-jail is
// missing, and the configuration forbids running without a sandbox.
// errUnsandboxed is the sentinel inside unsandboxedErr: a run that never started
// must be distinguishable from a script that failed, so the failure is not fed to
// the auto-correct pass as if the script were at fault (YM-286).
var errUnsandboxed = errors.New("ai-jail is unavailable and fallback_unsandboxed is false")

func unsandboxedErr(profile SandboxProfile) error {
	return fmt.Errorf("sandbox profile %s requested but %w", profile, errUnsandboxed)
}

// errMaskPrepare is the sentinel inside a mask refusal. Like errUnsandboxed it
// marks a run that never started, so the failure must not be fed to the
// auto-correct pass as if the script were at fault.
var errMaskPrepare = errors.New("a sandbox mask could not be applied")

// errWorkDirCoversHome is the refusal for a run whose own cwd bind would cover the
// user's home. ai-jail lays the read-write cwd bind after the caller's mounts, so
// every map inside the home is shadowed by it — and the two the run cannot do
// without are exactly those: the yesmem CLI the store()/llm() polyfills call and
// the restricted job socket. Masking them is not an option (the run needs them), so
// the run is refused instead; that keeps the decision fail-closed and stays on the
// line claudeConfigFlags already took for ~/.claude.json one step narrower
// (cwdCoversClaudeConfig, YM-428).
//
// A marker of its own, not errMaskPrepare: the caller-facing text has to name the
// work dir, and "a mask could not be applied" would send the operator to the mask
// list for a value they set on the job.
var errWorkDirCoversHome = errors.New("a run whose working directory is the user's home, or an ancestor of it, is not supported")

// workDirCoversHomeErr names the work dir and the reason. Only the work dir: the
// caller supplied it (a job's project, a cap's run dir), while the home is a host
// path it never held — the same line maskRefusalForCaller draws for every other
// prepare refusal (Befund 5, YM-334). Without the work dir the refusal is not
// actionable. %q keeps a path that carries a line break or an escape out of a job
// result and out of a cap response.
func workDirCoversHomeErr(work string) error {
	return fmt.Errorf("%w (work dir %q)", errWorkDirCoversHome, work)
}

// errNoWorkDir is the refusal for a run that has no working directory it can start
// in: the prepared value was empty, or the directory the caller named is gone. Both
// shapes used to fall through to the daemon's cwd, which for a daemon started from a
// login shell is the home — ai-jail then write-binds that home over its own private
// tmpfs, and every map and mask the run was prepared with is void (YM-437).
//
// A marker of its own, like errWorkDirCoversHome: the caller-facing text has to name
// the work dir, and "a mask could not be applied" would send the operator to the mask
// list for a value they set on the job or that a deleted project took with it.
var errNoWorkDir = errors.New("a run without a usable working directory is not supported")

// noWorkDirErr names the work dir and the reason. An empty value is named as such
// instead of as a quoted empty string: it is the shape a failed default derivation
// leaves behind (YM-286). %q keeps a path that carries a line break or an escape out
// of a job result and out of a cap response.
func noWorkDirErr(work string) error {
	if work == "" {
		return fmt.Errorf("%w (no work dir was configured or derived)", errNoWorkDir)
	}
	return fmt.Errorf("%w (work dir %q is not a directory)", errNoWorkDir, work)
}

// workDirUsable reports whether work can serve as a run's working directory. The
// caller names it (a job's project, a cap's run dir) and it may be gone by the time
// the run is prepared: a project the job outlived, an agent worktree that was
// removed. An empty value is the other shape, a default derivation that failed.
func workDirUsable(work string) bool {
	if work == "" {
		return false
	}
	info, err := os.Stat(work)
	return err == nil && info.IsDir()
}

// runNeverStarted reports whether err marks a run that was refused before it
// executed. All four classes are one class to the caller: there is no script output
// to read, so the auto-correct pass must not rewrite a live cap over an
// infrastructure or configuration error (bash_error_handler, handler_scheduler).
func runNeverStarted(err error) bool {
	return errors.Is(err, errUnsandboxed) || errors.Is(err, errMaskPrepare) ||
		errors.Is(err, errWorkDirCoversHome) || errors.Is(err, errNoWorkDir)
}

// maskFailure is the refusal a run carries for a preparation problem the sandbox
// cannot fix (SandboxConfig.PrepareErr). Three causes share the field and not the gate:
//
//   - A work dir that covers the home is refused for every profile that binds the
//     cwd. strict binds it as well, and a strict run with the home as work dir still
//     reads the real home through it (measured on 0.9.2: a pre-existing file under
//     the home is readable in the run, the write is refused with EROFS), so the
//     mask-rendering gate below would leave that shape open. Only none is exempt: it
//     starts the command directly and binds nothing.
//   - A work dir that is missing is refused on the same gate for the same reason
//     (YM-437): without one the run inherits the daemon's cwd, and every profile that
//     binds the cwd binds that directory then.
//   - A mask placeholder that could not be materialised is refused only where masks
//     are rendered. A strict run carries none of these masks (measured on 0.9.2:
//     ~/.claude does not exist inside the run), so refusing it would cost the run for
//     a channel it does not have.
//
// The predicate is the profile, not the rendered argv: a mask list that came back
// empty because the host has no home directory is exactly the failure that has to be
// refused, and a scan for a --mask flag would wave it through.
func (s *Sandbox) maskFailure(profile SandboxProfile) error {
	if s.cfg.PrepareErr == nil {
		return nil
	}
	if errors.Is(s.cfg.PrepareErr, errNoWorkDir) {
		if profile == ProfileNone {
			return nil
		}
		// Rebuilt from the work dir rather than returned: PrepareErr can carry a mask
		// cause beside this one (both are joined in newSandbox, so a strict run cannot
		// start on the cause that does not refuse it), and the joined text would put a
		// second line into a job result or a cap response. The other cause stays in
		// the daemon log, where newSandbox already writes PrepareErr.
		return noWorkDirErr(s.cfg.WorkDir)
	}
	if errors.Is(s.cfg.PrepareErr, errWorkDirCoversHome) {
		if profile == ProfileNone {
			return nil
		}
		// Not a mask: the run's own cwd bind is the cause. Returned unwrapped so the
		// caller text does not blame the mask list; runNeverStarted covers the
		// "never started" classification the mask sentinel would otherwise carry.
		return s.cfg.PrepareErr
	}
	if !profileRendersMasks(profile) {
		return nil
	}
	return fmt.Errorf("%w: %w", errMaskPrepare, s.cfg.PrepareErr)
}

// profileRendersMasks reports whether the profile's flag set carries --mask at
// all. Both ai-jail majors follow this split: standard renders the mask list,
// strict hides the home with --lockdown and renders none.
func profileRendersMasks(profile SandboxProfile) bool {
	return profile == ProfileStandard
}

// Run executes command under ai-jail when the configured profile and the
// available binary allow it. Otherwise the fallback decides between an error
// (fail closed) and a warned run without a sandbox.
func (s *Sandbox) Run(command string, timeoutSec int, profile SandboxProfile) (string, int, error) {
	if profile == ProfileNone || !s.cfg.Enabled {
		return runShell(command, timeoutSec, s.runWorkDir(), s.cfg.Env)
	}
	if !s.Available() {
		if !s.cfg.FallbackUnsandboxed {
			return "", -1, unsandboxedErr(profile)
		}
		log.Printf("[sandbox] WARNING: ai-jail not available, running %s profile unsandboxed", profile)
		return runShell(command, timeoutSec, s.runWorkDir(), s.cfg.Env)
	}
	if err := s.maskFailure(profile); err != nil {
		return "", -1, err
	}
	args := append(s.sandboxFlags(profile), "bash", "-c", command)
	return runProcess(s.binaryPath, args, timeoutSec, s.runWorkDir(), s.cfg.Env)
}

// RunWithProfile is the profile-aware entry point for scheduled jobs.
func (s *Sandbox) RunWithProfile(command string, timeoutSec int, profile SandboxProfile) (string, int, error) {
	return s.Run(command, timeoutSec, profile)
}

// WrapExecArgs wraps binary and args for execution through ai-jail. It reports an
// error when a profile was requested but ai-jail is unavailable and the
// unsandboxed fallback is disabled — callers must not run the command then.
func (s *Sandbox) WrapExecArgs(binary string, args []string, profile SandboxProfile) (string, []string, error) {
	resolved, err := s.resolve(profile)
	if err != nil {
		return "", nil, err
	}
	if resolved == "" {
		return binary, args, nil
	}
	wrapped := append(s.sandboxFlags(profile), binary)
	wrapped = append(wrapped, args...)
	return resolved, wrapped, nil
}

// resolve returns the ai-jail binary to use, an empty string for a pass-through
// run, or an error when the sandbox is required but unavailable.
func (s *Sandbox) resolve(profile SandboxProfile) (string, error) {
	if profile == ProfileNone {
		return "", nil
	}
	if s.Available() {
		if err := s.maskFailure(profile); err != nil {
			return "", err
		}
		return s.binaryPath, nil
	}
	if !s.cfg.FallbackUnsandboxed {
		return "", unsandboxedErr(profile)
	}
	log.Printf("[sandbox] WARNING: ai-jail not available, running %s profile unsandboxed", profile)
	return "", nil
}

// killProcessGroupOnCancel makes the run's context take down the whole process
// tree, not just the process the daemon started (YM-267). exec.CommandContext
// kills the direct child only, so a run that started `sleep` or a background
// shell left it running past the deadline — an orphan that keeps writing into the
// job dir and keeps the sandbox alive. Setpgid puts the run into its own process
// group; the kill then addresses the group.
//
// WaitDelay bounds the wait for the pipes afterwards. A child that left the group
// (setsid, a double fork) can hold them open after the kill, and without the
// delay the caller — and with CombinedOutput the whole goroutine — would wait on
// a stream nobody closes.
//
// The run no longer belongs to the daemon's own process group, so a group-wide
// signal sent to the daemon (or to a supervisor that signals the group rather
// than the tree) does not reach it. A cgroup kill — what systemd does — still
// does.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 5 * time.Second
	}
}

func runShell(command string, timeoutSec int, workDir string, env []string) (string, int, error) {
	return runProcess("bash", []string{"-c", command}, timeoutSec, workDir, env)
}

// runProcess runs a process with a timeout and reports its exit code. A non-zero
// exit is always an error: callers key "did this job succeed" off it. env is
// appended to the inherited environment, so a run can pass values in (a job's
// YESMEM_JOB_DIR) without losing the caller's own environment.
func runProcess(binary string, args []string, timeoutSec int, workDir string, env []string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	killProcessGroupOnCancel(cmd)
	if len(env) > 0 {
		cmd.Env = mergeEnv(env)
	}
	if workDir != "" {
		// A directory that vanished must not turn the run into a chdir failure. Every
		// caller that binds the cwd refuses such a run before it gets here: newSandbox
		// records errNoWorkDir and maskFailure refuses the run (YM-437). The fallback
		// therefore serves the runs that bind no cwd (none, the unsandboxed fallback)
		// and the window in which a directory is removed between the prepare step and
		// this exec. That window is documented rather than closed at this point: the
		// flag rendering is also exercised directly (NewSandbox without newSandbox),
		// and a work-dir check here would refuse those runs.
		if info, err := os.Stat(workDir); err == nil && info.IsDir() {
			cmd.Dir = workDir
		}
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), -1, fmt.Errorf("timed out after %ds", timeoutSec)
	}
	return string(out), exitCodeOf(err), err
}

// mergeEnv appends extra to the inherited environment and drops inherited entries
// whose key extra overrides. A duplicate key resolves per consumer, and on Linux
// the first match wins, so appending alone would keep the inherited value.
func mergeEnv(extra []string) []string {
	inherited := os.Environ()
	out := inherited[:0]
	for _, kv := range inherited {
		key, _, _ := strings.Cut(kv, "=")
		overridden := false
		for _, e := range extra {
			if extraKey, _, _ := strings.Cut(e, "="); extraKey == key {
				overridden = true
				break
			}
		}
		if !overridden {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
