package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/codex2api/cmd/sqlite2pg"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "copy":
		return runCopy(args[1:])
	case "sync-credentials":
		return runSync(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "sqlite2pg: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func runCopy(args []string) int {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	sqlitePath := fs.String("sqlite", "", "SQLite database path")
	nowText := fs.String("now", "", "cutoff time, RFC3339")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "sqlite2pg: unexpected arguments")
		return 2
	}
	var now time.Time
	if *nowText != "" {
		parsed, err := time.Parse(time.RFC3339, *nowText)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sqlite2pg: -now must be RFC3339")
			return 2
		}
		now = parsed
	}
	report, err := sqlite2pg.Copy(context.Background(), sqlite2pg.Options{
		SQLitePath:  *sqlitePath,
		PostgresDSN: os.Getenv("SQLITE2PG_PG_DSN"),
		Now:         now,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	writeReport(os.Stdout, report)
	return 0
}

func runSync(args []string) int {
	fs := flag.NewFlagSet("sync-credentials", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	sqlitePath := fs.String("sqlite", "", "SQLite database path")
	apply := fs.Bool("apply", false, "write credential updates; default is dry-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "sqlite2pg: unexpected arguments")
		return 2
	}
	report, err := sqlite2pg.SyncCredentials(context.Background(), sqlite2pg.Options{
		SQLitePath:  *sqlitePath,
		PostgresDSN: os.Getenv("SQLITE2PG_PG_DSN"),
		Apply:       *apply,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	writeReport(os.Stdout, report)
	return 0
}

func writeReport(out interface{ Write([]byte) (int, error) }, report sqlite2pg.Report) {
	fmt.Fprintf(out, "pg_only=%d sequences=%d\n", len(report.PGOnly), report.Sequences)
	if len(report.PGOnly) > 0 {
		fmt.Fprintf(out, "pg_only_tables=%s\n", join(report.PGOnly))
	}
	names := make([]string, 0, len(report.Tables))
	for name := range report.Tables {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		table := report.Tables[name]
		fmt.Fprintf(out, "table=%s read=%d written=%d", name, table.Read, table.Written)
		for _, reason := range sortedKeys(table.Skipped) {
			fmt.Fprintf(out, " skipped_%s=%d", reason, table.Skipped[reason])
		}
		if table.SequenceSet {
			fmt.Fprintf(out, " sequence=%s last=%d", table.Sequence, table.SequenceLast)
		}
		fmt.Fprintln(out)
	}
	for _, kind := range sortedKeys(report.Conversions) {
		fmt.Fprintf(out, "conversion=%s count=%d\n", kind, report.Conversions[kind])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sqlite2pg copy -sqlite PATH [-now RFC3339]
    PostgreSQL DSN is read from SQLITE2PG_PG_DSN. The target must already be migrated.

sqlite2pg sync-credentials -sqlite PATH [-apply]
    Copy accounts.credentials from PostgreSQL to SQLite, matching by id.
    Dry-run unless -apply is set. Output is counts only.
`)
}

func join(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ","
		}
		out += value
	}
	return out
}

func sortedKeys(values map[string]int64) []string {
	keys := make([]string, 0, len(values))
	for key, count := range values {
		if count == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
