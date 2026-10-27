package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// runtimeDirSuffix names the directory beside the data dir that a sandboxed run
// may see. It exists because the job sandbox masks the data dir as a whole
// (YM-276): the restricted socket and the job log dirs have to live outside it,
// or the mask would hide them from the very runs that need them. internal/setup
// uninstalls the same path; keep the two in step.
const runtimeDirSuffix = "-run"

// RuntimeDir returns that sibling, or "" for an empty data dir. A sibling, so an
// install that moves the data dir moves this with it.
func RuntimeDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Clean(dataDir) + runtimeDirSuffix
}

// EnsureRuntimeDir creates the runtime dir with the mode the socket needs. The
// daemon listens on the job socket before any job exists, so it cannot wait for a
// job to create the directory.
func EnsureRuntimeDir(dataDir string) (string, error) {
	dir := RuntimeDir(dataDir)
	if dir == "" {
		return "", fmt.Errorf("no data dir")
	}
	// The directory mode is the only access control on the restricted socket, and
	// a host where ~/.claude is world-writable hands the directory to anyone. A
	// symlink would move the socket and the job dirs to its target; MkdirAll
	// follows one silently, so refuse it rather than accept it.
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return "", fmt.Errorf("runtime dir %s exists and is not a directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves the mode of an existing directory untouched.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// runWorkDirName names the working directory of a run that belongs to no project.
const runWorkDirName = "workdir"

// runWorkDirBase is the parent of the per-key run work dirs. The sweep walks it,
// and the job log dirs live beside it under the same runtime dir.
func runWorkDirBase(dataDir string) string {
	dir := RuntimeDir(dataDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, runWorkDirName)
}

// EnsureRunWorkDir creates and returns that directory. It sits inside the runtime
// dir but in a subdirectory of its own: ai-jail mounts the process's cwd
// read-write, and the restricted job socket is one level up, so a run must not
// have the runtime dir itself as its cwd (YM-286).
func EnsureRunWorkDir(dataDir, jobKey string) (string, error) {
	dir, err := EnsureRuntimeDir(dataDir)
	if err != nil {
		return "", err
	}
	base := filepath.Join(dir, runWorkDirName)
	// Same reasoning as for the runtime dir itself: a symlink here would move the
	// writable cwd of every project-less run to its target.
	if info, err := os.Lstat(base); err == nil && !info.IsDir() {
		return "", fmt.Errorf("run work dir %s exists and is not a directory", base)
	}
	// One directory per job, so one job's leftovers are not the next one's cwd.
	work := base
	if jobKey != "" {
		work = filepath.Join(base, jobDirName(jobKey))
	}
	// The base is writable inside a run without a job, so the per-job element can
	// be a planted symlink by the time the next run asks for it.
	if info, err := os.Lstat(work); err == nil && !info.IsDir() {
		return "", fmt.Errorf("run work dir %s exists and is not a directory", work)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves the mode of an existing directory untouched.
	if err := os.Chmod(work, 0o700); err != nil {
		return "", err
	}
	return work, nil
}
