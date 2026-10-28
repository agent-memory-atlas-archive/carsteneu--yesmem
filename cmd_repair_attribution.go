package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/repair"
)

// runRepairAttribution repairs learnings whose stored session (and project)
// disagree with the session that actually wrote them. It is a dry run by
// default and prints the id / old / new / basis table; --apply writes the
// unambiguous rows after copying them into repair.BackupTable.
func runRepairAttribution() {
	yesmemDB := filepath.Join(yesmemDataDir(), "yesmem.db")
	opencodeDB := defaultOpencodeDB()
	since := repair.DefaultSince
	apply := false

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--apply":
			apply = true
		case "--db":
			if i+1 >= len(args) {
				log.Fatal("--db needs a path")
			}
			i++
			yesmemDB = args[i]
		case "--opencode-db":
			if i+1 >= len(args) {
				log.Fatal("--opencode-db needs a path")
			}
			i++
			opencodeDB = args[i]
		case "--since":
			if i+1 >= len(args) {
				log.Fatal("--since needs a date (YYYY-MM-DD)")
			}
			i++
			since = args[i]
		default:
			log.Fatalf("unknown argument %q", args[i])
		}
	}

	report, err := repair.Plan(yesmemDB, opencodeDB, since)
	if err != nil {
		log.Fatalf("plan session attribution repair: %v", err)
	}
	repairable, already, unresolved := report.Counts()
	fmt.Printf("session attribution repair (since %s)\n", report.Since)
	fmt.Printf("candidates: %d  repair: %d  already: %d  unresolved: %d\n", report.Candidates, repairable, already, unresolved)
	fmt.Print(report.Table())

	if !apply {
		fmt.Println("dry run — nothing written. Pass --apply to write the REPAIR rows.")
		return
	}
	changed, err := repair.Apply(yesmemDB, report)
	if err != nil {
		log.Fatalf("apply session attribution repair: %v", err)
	}
	fmt.Printf("applied: %d rows updated, originals kept in %s\n", changed, repair.BackupTable)
}

func defaultOpencodeDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "opencode.db"
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}
