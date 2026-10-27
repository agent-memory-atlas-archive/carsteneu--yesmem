package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// cpOfLiveDatabase matches a copy invocation that takes the database file as an
// argument. Prose that mentions cp as the wrong tool ("a cp of the live file is
// torn") must not match, so the pattern requires a whitespace-separated path.
var cpOfLiveDatabase = regexp.MustCompile(`\bcp\s+[^\s` + "`" + `]*yesmem\.db`)

// YM-361 [DEV-DT56]: the backfill plan told the operator to copy the live
// database file. In WAL mode that copy is torn, because the writer may be
// checkpointing into the middle of it; `yesmem backup` uses VACUUM INTO
// (cmd_backup.go) and is the only supported way. The plan is a document an
// operator follows at a real decision, so the wrong instruction is a trap.
func TestBackfillPlanRecommendsYesmemBackupLos55e(t *testing.T) {
	// Resolved from this file, not from the working directory: the test binary
	// also runs outside the repo root, and a missing path there would read like
	// a documentation bug.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed, cannot locate the repository root")
	}
	plan := filepath.Join(filepath.Dir(self), "yesdocs", "plans", "2026-07-05-language-backfill.md")
	data, err := os.ReadFile(plan)
	if err != nil {
		t.Fatalf("read %s: %v", plan, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if cpOfLiveDatabase.MatchString(line) {
			t.Errorf("%s tells the operator to copy the live database, which is torn in WAL mode: %q", plan, strings.TrimSpace(line))
		}
	}
	if !strings.Contains(string(data), "yesmem backup") {
		t.Errorf("%s never names the command that takes a consistent backup", plan)
	}
}
