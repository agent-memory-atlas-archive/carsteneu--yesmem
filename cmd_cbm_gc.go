package main

import (
	"fmt"
	"os"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// runCBMGc GCs orphaned CBM worktree-index DBs. Default is a real run;
// --dry-run only lists candidates.
func runCBMGc(args []string) {
	dryRun := hasFlag(args, "--dry-run")
	findings, err := codescan.RunCBMGC(dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cbm-gc: %v\n", err)
		os.Exit(1)
	}
	var total int64
	for _, f := range findings {
		total += f.SizeBytes
		fmt.Printf("%12s  %s\n            %s\n", codescan.FormatCBMByteSize(f.SizeBytes), f.Path, f.Reason)
	}
	mode := "deleted"
	if dryRun {
		mode = "dry-run"
	}
	fmt.Printf("cbm-gc: %d items, %s total (%s)\n", len(findings), codescan.FormatCBMByteSize(total), mode)
}
