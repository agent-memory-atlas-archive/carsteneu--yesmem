package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/config"
)

// PrintConfigCheck reports the config.yaml keys the schema cannot read or that sit at the wrong nesting
// level. `yesmem update` and `yesmem setup` both call it, so the two entry points cannot drift apart in
// what they tell the user about the same file (YM-463). Unknown keys are reported, never removed.
func PrintConfigCheck(path string, out, warn io.Writer) {
	// A missing file has no issues by contract, but "OK" about a file that does not exist reads like a
	// passed check. A fresh install reaches this line before it writes the template.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintln(out, "  Config check: no config.yaml yet")
		return
	}
	issues, err := config.CheckKeys(path)
	if err != nil {
		fmt.Fprintf(warn, "  Config check warning: %v\n", err)
		return
	}
	if len(issues) == 0 {
		fmt.Fprintln(out, "  Config check: OK")
		return
	}
	for _, issue := range issues {
		fmt.Fprintf(out, "  Config check: %s\n", issue)
	}
}

// writeConfigForInstall reports what the replaced config.yaml carried and then writes the template. The
// order is the point and is the reason for this function: after the write the file is the template, which
// is generated from the schema, so a check at that point could only ever report on itself (YM-463).
func writeConfigForInstall(dataDir, content string, out, warn io.Writer) error {
	PrintConfigCheck(filepath.Join(dataDir, "config.yaml"), out, warn)
	return backupAndWriteConfigFile(dataDir, content)
}
