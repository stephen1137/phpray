// PHPRay Collector — reads traces from JSONL files and shared memory ring buffer,
// stores them in SQLite, provides CLI tools.
//
// Usage:
//
//	phpray-collector tail          — live trace stream
//	phpray-collector top           — live top slow requests (htop-style)
//	phpray-collector stats         — aggregate statistics
//	phpray-collector trace         — per-domain trace detail with waterfall
//	phpray-collector import        — import JSONL file into SQLite
//	phpray-collector slow-queries  — top slow query fingerprints from SQLite
//	phpray-collector daemon        — run as background collector service
//	phpray-collector serve         — daemon + API + WebSocket live streaming
//	phpray-collector agg-query     — query per-minute aggregates from SQLite
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

const version = "0.15.5" // 0.15.5: wyścig przy zapisie do ringu, top, panel lokalny, MCP; ingest protocol v1 frozen since 0.14

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "tail":
		cmdTail(os.Args[2:])
	case "top":
		cmdTop(os.Args[2:])
	case "stats":
		cmdStats(os.Args[2:])
	case "trace":
		cmdTrace(os.Args[2:])
	case "import":
		cmdImport(os.Args[2:])
	case "slow-queries":
		cmdSlowQueries(os.Args[2:])
	case "daemon":
		cmdDaemonRun(os.Args[2:])
	case "agg-query":
		cmdAggQuery(os.Args[2:])
	case "api":
		cmdAPI(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "token":
		cmdToken(os.Args[2:])
	case "report":
		cmdReport(os.Args[2:])
	case "control":
		cmdControl(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("phpray-collector v%s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `PHPRay Collector v%s

Usage: phpray-collector <command> [options]

JSONL Commands (no database needed):
  tail          Live trace stream (like tail -f, with colors)
  top           Auto-refreshing top slow requests (htop for PHP)
  stats         Aggregate statistics from JSONL file
  trace         Per-domain trace detail with waterfall breakdown

Database Commands (SQLite):
  import        Import JSONL traces into SQLite database
  slow-queries  Top slow query fingerprints (deduplicated, aggregated)
  agg-query     Query per-minute aggregates

Service:
  status        One-screen health check: extension, collector, dashboard, cloud
  daemon        Run as background collector service
                (reads ring buffer or JSONL → SQLite + aggregation)
  serve         Daemon + API + WebSocket in one process
                (live trace streaming to dashboard via WS,
                 POST /api/v1/ingest for the WordPress plugin and other agents)
  api           REST API server + dashboard (read-only)

Auth:
  token         Generate a JWT token for API authentication

Reports:
  report        Generate diagnostic report (markdown/text) to stdout

Control table (on-demand profiling, shared memory read by the extension at RINIT):
  control list                                  Show entries
  control set -docroot <dir> [-prefix /p] [-rate 100] [-ttl 600s]
  control clear -docroot <dir>
  control expire                                Drop expired entries
                (-path <file>, default /dev/shm/phpray-control)

Other:
  version       Show version

Common Options:
  -f <path>    JSONL file path (default: /tmp/phpray.jsonl)
  -s <path>    SQLite database path (default: /var/lib/phpray/traces.db)
  -d <domain>  Filter by domain
  -n <count>   Number of results (default: 20)

Daemon Options:
  -c <path>    Config file (TOML)
  -mode <m>    Input mode: auto (default), ring, jsonl
  --shm <path> Ring buffer path (default: /dev/shm/phpray); a glob such as
               /run/phpray/ring-* reads every matching ring (one per PHP-FPM pool)

Examples:
  phpray-collector tail -f /tmp/phpray.jsonl -d example.com
  phpray-collector top -w 300
  phpray-collector stats -w 60
  phpray-collector trace -d shop.example.com -min 500
  phpray-collector import -f /tmp/phpray.jsonl -s ./traces.db
  phpray-collector slow-queries -s ./traces.db -n 10
  phpray-collector daemon -c /etc/phpray/collector.toml
  phpray-collector daemon -mode jsonl -f /tmp/phpray.jsonl
  phpray-collector serve -addr :9191 -s ./traces.db
  phpray-collector agg-query -s ./traces.db -w 60
  phpray-collector token -secret mykey -sub admin -role admin -exp 720h
  phpray-collector token -secret mykey -sub user1 -role user -domains "a.com,b.com"
  phpray-collector report -domain shop.example.com -s ./traces.db -w 60

`, version)
}

// cmdAggQuery queries per-minute aggregate data from SQLite
func cmdAggQuery(args []string) {
	fs := flag.NewFlagSet("agg-query", flag.ExitOnError)
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	windowMin := fs.Int("w", 60, "Time window (minutes)")
	domain := fs.String("d", "", "Filter by domain")
	fs.Parse(args)

	store, err := OpenStorage(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	now := time.Now().Unix()
	from := ((now - int64(*windowMin)*60) / 60) * 60

	aggs, err := store.GetAggregates1m(from, now, *domain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("🔦 PHPRay Aggregates — last %d minutes\n", *windowMin)
	if *domain != "" {
		fmt.Printf("   Filter: %s\n", *domain)
	}
	fmt.Println("────────────────────────────────────────────────────────────────────────────────")
	fmt.Printf("%-8s %-30s %6s %5s %8s %8s %8s %6s %4s\n",
		"TIME", "HOST", "REQS", "ERRS", "AVG", "P95", "MAX", "DB_Q", "N+1")
	fmt.Println("────────────────────────────────────────────────────────────────────────────────")

	for _, a := range aggs {
		ts := time.Unix(a.Bucket, 0).Format("15:04")
		host := a.Host
		if len(host) > 30 {
			host = host[:27] + "..."
		}
		fmt.Printf("%-8s %-30s %6d %5d %7.1fms %7.1fms %7.1fms %6d %4d\n",
			ts, host, a.RequestCount, a.ErrorCount,
			a.AvgDurationMs, a.P95DurationMs, a.MaxDurationMs,
			a.TotalDBQueries, a.N1Count)
	}

	if len(aggs) == 0 {
		fmt.Println("  No aggregate data. Run the daemon to generate aggregates.")
	}
}

// cmdReport generates a diagnostic report and prints to stdout
func cmdReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	domain := fs.String("domain", "", "Domain to analyze (required)")
	windowMin := fs.Int("w", 60, "Time window (minutes)")
	format := fs.String("format", "markdown", "Output format: markdown, text or html")
	anonim := fs.Bool("anonymize", false, "replace the domain name in the output, so the report can be shared publicly")
	out := fs.String("o", "", "write to this file instead of stdout")
	fs.Parse(args)

	if *domain == "" {
		fmt.Fprintf(os.Stderr, "Error: -domain is required\n")
		fmt.Fprintf(os.Stderr, "Usage: phpray-collector report -domain <domain> [-s <db>] [-w <minutes>] [-format markdown|text]\n")
		os.Exit(1)
	}

	store, err := OpenStorage(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx, err := store.LoadTracesForDiag(*domain, *windowMin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading traces: %v\n", err)
		os.Exit(1)
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)
	report.WindowMin = *windowMin

	var tresc string
	switch *format {
	case "text":
		tresc = GeneratePlainTextReport(report)
	case "html":
		tresc = GenerateHTMLReport(report, *anonim, version)
	default:
		tresc = GenerateMarkdownReport(report)
	}
	if *out == "" {
		fmt.Print(tresc)
		return
	}
	if err := os.WriteFile(*out, []byte(tresc), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Report written to %s\n", *out)
}

// cmdControl manages the shared-memory control table from the command line
// (the DirectAdmin plugin and the console use the same writer through the daemon).
func cmdControl(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: phpray-collector control <list|set|clear|expire> [-path <file>] [options]")
		os.Exit(1)
	}
	sub := args[0]
	fs := flag.NewFlagSet("control "+sub, flag.ExitOnError)
	path := fs.String("path", "/dev/shm/phpray-control", "Control table path")
	docroot := fs.String("docroot", "", "DOCUMENT_ROOT of the site")
	prefix := fs.String("prefix", "", "URI prefix to profile (empty = every URI)")
	rate := fs.Int("rate", 100, "Percent of matching requests to profile")
	ttl := fs.Duration("ttl", 10*time.Minute, "How long the entry stays active")
	fs.Parse(args[1:])

	t, err := OpenControlTable(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control table: %v\n", err)
		os.Exit(1)
	}
	defer t.Close()
	switch sub {
	case "list":
		entries := t.List()
		fmt.Printf("%s: %d entries (seq %d)\n", *path, len(entries), t.Seq())
		for _, e := range entries {
			state := "active"
			if !e.Active {
				state = "inactive"
			}
			if e.Until <= time.Now().Unix() {
				state = "expired"
			}
			fmt.Printf("  %016x  prefix=%-30q rate=%3d%%  until=%s  %s\n", e.DocrootHash, e.URLPrefix, e.SampleRate, time.Unix(e.Until, 0).Format("2006-01-02 15:04:05"), state)
		}
	case "set":
		if *docroot == "" {
			fmt.Fprintln(os.Stderr, "control set: -docroot is required")
			os.Exit(1)
		}
		until := time.Now().Add(*ttl).Unix()
		if err := t.Set(*docroot, *prefix, *rate, until); err != nil {
			fmt.Fprintf(os.Stderr, "control set: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("ok: %s (hash %016x) prefix=%q rate=%d%% until=%s\n", *docroot, FNVHash64(*docroot), *prefix, *rate, time.Unix(until, 0).Format("15:04:05"))
	case "clear":
		if *docroot == "" {
			fmt.Fprintln(os.Stderr, "control clear: -docroot is required")
			os.Exit(1)
		}
		fmt.Printf("cleared: %v\n", t.Clear(*docroot))
	case "expire":
		fmt.Printf("expired: %d\n", t.Expire(time.Now().Unix()))
	default:
		fmt.Fprintf(os.Stderr, "control: unknown subcommand %q\n", sub)
		os.Exit(1)
	}
}
