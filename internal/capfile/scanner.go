package capfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckDirName holds the documented invariant that a CAP.md's directory name
// equals its frontmatter name. Without it a stray copy — the live
// ~/.claude/caps/"telegram (Kopie)" is one — imports the original cap's name
// and supersedes its version on every scan.
func CheckDirName(dirName, capName string) error {
	if dirName == capName {
		return nil
	}
	return fmt.Errorf("directory name %q does not match frontmatter name %q", dirName, capName)
}

// ScanDirectory reads every <dir>/<name>/CAP.md with the shareable scope
// default. Callers that know the directory is project-bound use
// ScanDirectoryWithScope.
func ScanDirectory(dir string) ([]CapFile, []error) {
	return ScanDirectoryWithScope(dir, "user")
}

// ScanDirectoryWithScope is ScanDirectory with an explicit default for CAP.md
// files that omit the scope field. The scope follows from where the file lives:
// a scope-less file below a project directory is project-bound, and reading it
// as "user" would make the cap load project-agnostic — unsandboxed, if its
// script asks for sandbox=none.
//
// defaultScope must be "user" or "project".
func ScanDirectoryWithScope(dir, defaultScope string) ([]CapFile, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("read dir %s: %w", dir, err)}
	}

	var caps []CapFile
	var errs []error

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		capPath := filepath.Join(dir, entry.Name(), "CAP.md")
		// ReadNoFollow, not os.ReadFile: a link planted in an import directory
		// would feed the version gate and the cap store content from outside the
		// caps tree. The export path already refuses it (YM-359), and this is the
		// same read (YM-373).
		data, err := ReadNoFollow(capPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("read %s: %w", capPath, err))
			continue
		}
		cf, err := ParseWithDefaultScope(data, defaultScope)
		if err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", capPath, err))
			continue
		}
		if err := CheckDirName(entry.Name(), cf.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", capPath, err))
			continue
		}
		cf.SourcePath = capPath
		caps = append(caps, *cf)
	}
	return caps, errs
}

func ScanAll(userDir, projectDir string) ([]CapFile, []error) {
	var allCaps []CapFile
	var allErrs []error

	caps, errs := ScanDirectoryWithScope(userDir, "user")
	allCaps = append(allCaps, caps...)
	allErrs = append(allErrs, errs...)

	caps, errs = ScanDirectoryWithScope(projectDir, "project")
	allCaps = append(allCaps, caps...)
	allErrs = append(allErrs, errs...)

	return allCaps, allErrs
}
