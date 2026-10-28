package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	los04iDispatchArgsEnv = "YESMEM_LOS04I_DISPATCH_ARGS"
	los04iDispatchPathEnv = "YESMEM_LOS04I_DISPATCH_PATH"
)

// los04iCleanDispatchEnv drops every variable that would point the child at
// another installation than the scenario's HOME.
func los04iCleanDispatchEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "HOME", "YESMEM_DATA_DIR", "YESMEM_SOCK", los04iDispatchArgsEnv, los04iDispatchPathEnv:
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestLos04iDispatchCommand is the child process entry point: it runs main() with
// the scenario's subcommand, so the fatal formatting of the dispatcher is under
// test and not a helper.
func TestLos04iDispatchCommand(t *testing.T) {
	// A path carrying a newline cannot travel in the space separated args, so it
	// comes in its own variable.
	if path := os.Getenv(los04iDispatchPathEnv); path != "" {
		os.Args = []string{"yesmem", "add-docs", "--name", "los04i-probe", "--path", path, "--project", "p"}
		main()
		t.Fatal("the dispatched command returned, want a fatal exit")
	}
	if args := os.Getenv(los04iDispatchArgsEnv); args != "" {
		os.Args = append([]string{"yesmem"}, strings.Fields(args)...)
		main()
		t.Fatal("the dispatched command returned, want a fatal exit")
	}
	t.Skip("helper process only")
}

// The dispatcher prints the error of a subcommand with log.Fatalf. A path built
// from HOME reaches that line raw through %v, so a newline in HOME ended the
// stderr line and the rest read as its own log entry (YM-332, escaped from
// Los 04h).
func TestDispatch_QuotesThePathsOfSubcommandErrors(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home\nwarn: forged dispatch line")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	// A file where the data directory belongs, so the store cannot be opened.
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write .claude: %v", err)
	}

	for _, args := range []string{
		"wiki-render --project /tmp/los04i-project --out /tmp/los04i-wiki",
		"sync-docs --all",
		"status",
		"consolidate",
	} {
		t.Run(strings.Fields(args)[0], func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLos04iDispatchCommand$")
			cmd.Env = append(los04iCleanDispatchEnv(), "HOME="+home, los04iDispatchArgsEnv+"="+args)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the subcommand reported success although its data directory cannot be created:\n%s", out)
			}

			printed := string(out)
			if strings.Contains(printed, "\nwarn: forged dispatch line") {
				t.Fatalf("a path forged a stderr line:\n%s", printed)
			}
			if !strings.Contains(printed, `home\nwarn: forged dispatch line`) {
				t.Fatalf("the path is missing from the fatal message:\n%s", printed)
			}
		})
	}
}

// 'add-docs' reports a failed ingest through log.Fatalf, and the error carries
// the source path from --path. A directory that cannot be read makes the walk
// fail; root ignores that mode, so this test only runs for a regular user.
func TestDispatch_QuotesTheIngestErrorOfAddDocs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode this test relies on")
	}
	home := t.TempDir()
	locked := filepath.Join(t.TempDir(), "docs\nwarn: forged ingest line")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "guide.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod source: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestLos04iDispatchCommand$")
	cmd.Env = append(los04iCleanDispatchEnv(), "HOME="+home, los04iDispatchPathEnv+"="+locked)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the command reported success although the source cannot be read:\n%s", out)
	}

	printed := string(out)
	if strings.Contains(printed, "\nwarn: forged ingest line") {
		t.Fatalf("the source path forged a stderr line:\n%s", printed)
	}
	if !strings.Contains(printed, `docs\nwarn: forged ingest line`) {
		t.Fatalf("the source path is missing from the fatal message:\n%s", printed)
	}
}
