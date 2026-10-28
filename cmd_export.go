package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

func runExport() {
	path := "yesmem-export.json"
	if len(os.Args) > 2 {
		path = os.Args[2]
	}

	dataDir := yesmemDataDir()
	// Export only reads. storage.Open would create the runtime/messages siblings
	// and run the schema migrations, so a backup command changed the live DB.
	store, err := storage.OpenReadOnly(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	exported, err := store.ExportLearnings(path)
	if err != nil {
		log.Fatalf("export: %v", err)
	}

	fmt.Printf("Exported %d learnings to %q\n", exported, path)
}

const importUsage = "Usage: yesmem import <file.json> [--distrust-file-sources]"

// parseImportArgs splits `yesmem import` arguments into the file path and the
// distrust switch, in either order. Anything else is an error and not a silent
// no-op: an unrecognised switch (--distrust-file-sourcesX, the =true form) would
// otherwise leave the file's own provenance claim intact, and a second path
// would import only the first file while exiting 0.
func parseImportArgs(args []string) (path string, distrust bool, err error) {
	for _, arg := range args {
		if arg == "--distrust-file-sources" {
			distrust = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", distrust, fmt.Errorf("unknown flag %q", arg)
		}
		if path != "" {
			return "", distrust, fmt.Errorf("unexpected argument %q", arg)
		}
		path = arg
	}
	if path == "" {
		return "", distrust, errors.New("missing file")
	}
	return path, distrust, nil
}

func runImport() {
	path, distrust, err := parseImportArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n%s\n", err, importUsage)
		os.Exit(1)
	}

	dataDir := yesmemDataDir()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	imported, skipped, err := store.ImportLearningsWithOptions(path, storage.ImportOptions{DistrustFileSources: distrust})
	if err != nil {
		log.Fatalf("import: %v", err)
	}
	note := ""
	if distrust {
		note = " [file sources distrusted]"
	}
	fmt.Printf("Imported %d learnings, skipped %d entries (duplicates, rejected or conflicting entries, persona overrides of a distrusted file) from %q%s\n", imported, skipped, path, note)
}

// costRow is one day of spend, grouped into the four cost groups the daemon
// tracks. The proxy_*/fork_* buckets store raw token counts, so the values come
// from GetSpendSummary, which converts them with the shared price constants.
// Calls counts the daemon's own LLM calls (extract + quality); the proxy/fork
// counters increment once per non-empty token bucket, so they are not comparable.
type costRow struct {
	Day     string
	Extract float64
	Quality float64
	Proxy   float64
	Fork    float64
	Total   float64
	Calls   int
}

// costRows summarises every day in days (in the order given). llmCalls holds the
// extract+quality call count per day.
func costRows(store *storage.Store, days []string, llmCalls map[string]int) ([]costRow, error) {
	rows := make([]costRow, 0, len(days))
	for _, day := range days {
		start, err := time.Parse("2006-01-02", day)
		if err != nil {
			return nil, fmt.Errorf("parse day %q: %w", day, err)
		}
		sum, err := store.GetSpendSummary(storage.StatsFilter{
			Since:  day,
			Before: start.AddDate(0, 0, 1).Format("2006-01-02"),
		})
		if err != nil {
			return nil, fmt.Errorf("summarize %s: %w", day, err)
		}
		rows = append(rows, costRow{
			Day:     day,
			Extract: sum.ExtractUSD,
			Quality: sum.QualityUSD,
			Proxy:   sum.ProxyUSD,
			Fork:    sum.ForkUSD,
			Total:   sum.TotalUSD,
			Calls:   llmCalls[day],
		})
	}
	return rows, nil
}

func runCost() {
	store, err := storage.OpenReadOnly(filepath.Join(yesmemDataDir(), "yesmem.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	days := 7
	if len(os.Args) > 2 {
		if d, err := fmt.Sscanf(os.Args[2], "%d", &days); d == 0 || err != nil {
			days = 7
		}
	}

	history, err := store.GetSpendHistory(days)
	if err != nil {
		log.Fatalf("get spend: %v", err)
	}

	if len(history) == 0 {
		fmt.Println("No spend data yet. Budget tracking starts after next daemon restart.")
		return
	}

	var dayOrder []string
	llmCalls := map[string]int{}
	seen := make(map[string]bool, len(history))
	for _, ds := range history {
		if !seen[ds.Day] {
			seen[ds.Day] = true
			dayOrder = append(dayOrder, ds.Day)
		}
		if ds.Bucket == "extract" || ds.Bucket == "quality" {
			llmCalls[ds.Day] += ds.Calls
		}
	}

	rows, err := costRows(store, dayOrder, llmCalls)
	if err != nil {
		log.Fatalf("cost rows: %v", err)
	}

	fmt.Printf("%-12s %10s %10s %10s %10s %10s %7s\n", "Day", "Extract $", "Quality $", "Proxy $", "Fork $", "Total $", "Calls")
	fmt.Println(strings.Repeat("─", 82))

	var tot costRow
	for _, r := range rows {
		tot.Extract += r.Extract
		tot.Quality += r.Quality
		tot.Proxy += r.Proxy
		tot.Fork += r.Fork
		tot.Total += r.Total
		tot.Calls += r.Calls
		fmt.Printf("%-12s %10.2f %10.2f %10.2f %10.2f %10.2f %7d\n",
			r.Day, r.Extract, r.Quality, r.Proxy, r.Fork, r.Total, r.Calls)
	}

	fmt.Println(strings.Repeat("─", 82))
	fmt.Printf("%-12s %10.2f %10.2f %10.2f %10.2f %10.2f %7d\n",
		"Total", tot.Extract, tot.Quality, tot.Proxy, tot.Fork, tot.Total, tot.Calls)
	if len(rows) > 1 {
		n := float64(len(rows))
		fmt.Printf("%-12s %10.2f %10.2f %10.2f %10.2f %10.2f %7s\n",
			"Avg/day", tot.Extract/n, tot.Quality/n, tot.Proxy/n, tot.Fork/n, tot.Total/n, "")
	}
}
