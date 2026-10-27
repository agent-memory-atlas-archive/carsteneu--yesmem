package main

import (
	"fmt"
	"os"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/textutil"
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
		fmt.Printf("%s\n", cbmFindingLine(codescan.FormatCBMByteSize(f.SizeBytes), f.Path, f.Reason))
	}
	mode := "deleted"
	if dryRun {
		mode = "dry-run"
	}
	fmt.Printf("cbm-gc: %d items, %s total (%s)\n", len(findings), codescan.FormatCBMByteSize(total), mode)
}

// cbmFindingLine renders one cbm-gc finding on exactly two lines. The path and the reason
// come from the filesystem and from the scanner; a line break inside either one would
// invent a third line that reads as its own finding (YM-389).
func cbmFindingLine(size, path, reason string) string {
	return fmt.Sprintf("%12s  %s\n            %s",
		size, textutil.SingleLine(path, 300), textutil.SingleLine(reason, 300))
}
