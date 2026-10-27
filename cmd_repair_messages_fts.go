package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/carsteneu/yesmem/internal/repair"
)

// runRepairMessagesFTS backfills the messages_fts rows of messages that
// message_search cannot see. It is a dry run by default and prints what it found;
// --apply writes the rows. It deliberately has no daemon-start counterpart: a
// repair that rewrites an index of a multi-gigabyte file belongs to an operator
// run with a dry run in front of it, not to an upgrade.
func runRepairMessagesFTS() {
	dbPath := filepath.Join(yesmemDataDir(), "messages.db")
	apply := false
	examples := 5

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
			dbPath = args[i]
		case "--examples":
			if i+1 >= len(args) {
				log.Fatal("--examples needs a number")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 || n > maxFTSExamples {
				log.Fatalf("--examples needs a number between 0 and %d, got %q", maxFTSExamples, args[i])
			}
			examples = n
		default:
			log.Fatalf("unknown argument %q", args[i])
		}
	}

	// The report scans the whole store (measured 2m41s on a 10 GB messages.db), so
	// the run says what it is doing instead of staying silent until the report.
	fmt.Fprintf(os.Stderr, "repair-messages-fts: scanning %s, this reads the whole file\n", dbPath)
	if err := repairMessagesFTS(dbPath, apply, examples, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "repair-messages-fts: %v\n", err)
		os.Exit(1)
	}
}

// maxFTSExamples bounds --examples: the samples are read from the store and held
// in memory, so the flag must not be able to ask for the whole table.
const maxFTSExamples = 1000

// repairMessagesFTS is the testable body: no data directory, output to out, and
// the apply only on request.
func repairMessagesFTS(dbPath string, apply bool, examples int, out io.Writer) error {
	// The apply discards the samples, and collecting them would be a second full
	// scan of a store that can be 10 GB. Only the report that prints them pays for
	// them.
	if apply {
		examples = 0
	}
	report, err := repair.PlanMessagesFTS(dbPath, examples)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "messages_fts backfill for %s\n", report.DBPath)
	fmt.Fprintf(out, "messages: %d  without fts row: %d  backfillable: %d  empty: %d  orphan fts rows: %d\n",
		report.Messages, report.Missing, report.Backfillable, report.Empty, report.OrphanFTS)
	if report.Empty > 0 {
		fmt.Fprintln(out, "the empty rows carry neither content nor blob: they have nothing to index and the write path skips them too")
	}

	if apply {
		inserted, err := repair.ApplyMessagesFTS(dbPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "inserted: %d rows into messages_fts\n", inserted)
		return nil
	}

	// The samples are transcript content. An operator run prints a few of them to
	// identify the gap in the report; an apply prints none.
	if len(report.Examples) > 0 {
		fmt.Fprintln(out, "examples:")
		for _, s := range report.Examples {
			fmt.Fprintf(out, "  %d  %s  %s  %s  %s\n", s.ID, s.SessionID, s.MessageType, s.Timestamp, s.Snippet)
		}
	}
	fmt.Fprintln(out, "dry run: nothing written. Pass --apply to insert the missing rows.")
	return nil
}
