package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	filePath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	fs.Parse(args)

	store, err := OpenStorage(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	f, err := os.Open(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening %s: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	start := time.Now()
	imported := 0
	errors := 0

	for scanner.Scan() {
		var t Trace
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			errors++
			continue
		}

		if err := store.StoreTrace(&t); err != nil {
			errors++
			continue
		}
		imported++

		if imported%100 == 0 {
			fmt.Printf("\r  Imported %d traces...", imported)
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("\r🔦 PHPRay Import Complete\n")
	fmt.Printf("  Traces imported: %d\n", imported)
	if errors > 0 {
		fmt.Printf("  Errors: %d\n", errors)
	}
	fmt.Printf("  Time: %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Rate: %.0f traces/sec\n", float64(imported)/elapsed.Seconds())
	fmt.Printf("  Database: %s\n", *dbPath)

	count, _ := store.GetTraceCount()
	fmt.Printf("  Total traces in DB: %d\n", count)
}

func cmdSlowQueries(args []string) {
	fs := flag.NewFlagSet("slow-queries", flag.ExitOnError)
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	limit := fs.Int("n", 20, "Number of slow queries to show")
	fs.Parse(args)

	store, err := OpenStorage(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	queries, err := store.GetSlowQueries(*limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("🔦 PHPRay Slow Queries (top %d by avg duration)\n", *limit)
	fmt.Printf("────────────────────────────────────────────────────────────────────────────────\n")

	for i, q := range queries {
		fp := q.Fingerprint
		if len(fp) > 80 {
			fp = fp[:77] + "..."
		}
		sample := q.SampleSQL
		if len(sample) > 80 {
			sample = sample[:77] + "..."
		}

		fmt.Printf("\n%s#%d%s  avg:%.2fms  max:%.2fms  count:%d  host:%s\n",
			"\033[93m", i+1, resetColor, q.AvgMs, q.MaxMs, q.ExecCount, q.Host)
		fmt.Printf("  FP:     %s\n", fp)
		fmt.Printf("  Sample: %s\n", sample)
	}

	if len(queries) == 0 {
		fmt.Println("  No slow queries found. Import traces first: phpray-collector import -f /tmp/phpray.jsonl")
	}
}
