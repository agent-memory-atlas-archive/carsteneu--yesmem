package main

import (
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/carsteneu/yesmem/internal/setup"
)

// runReapStaleMCP terminates the `yesmem mcp` helpers that still map a replaced
// binary. A deploy swaps the inode; a helper keeps serving the old tool schema
// until it dies and its client forks a new one (Learning #81467).
func runReapStaleMCP() {
	fs := flag.NewFlagSet("reap-stale-mcp", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "list the stale helpers without signalling them")
	fs.BoolVar(dryRun, "n", false, "shorthand for --dry-run")
	fs.Parse(os.Args[2:])

	var kill func(pid int) error
	if !*dryRun {
		kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	}
	pids, err := setup.ReapStaleMCPHelpers("/proc", time.Now(), os.Getpid(), kill)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reap-stale-mcp: %v\n", err)
		os.Exit(1)
	}
	if len(pids) == 0 {
		return
	}
	verb := "reaped"
	if *dryRun {
		verb = "would reap"
	}
	for _, pid := range pids {
		fmt.Printf("  %s PID %d\n", verb, pid)
	}
	fmt.Printf("%s %d stale yesmem-mcp process(es) holding old binary\n", verb, len(pids))
}
