package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func cmdTrace(args []string) {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	filePath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	domain := fs.String("d", "", "Filter by domain (required)")
	count := fs.Int("n", 20, "Number of recent traces")
	minMs := fs.Float64("min", 0, "Minimum duration (ms)")
	level := fs.String("level", "", "Filter by level (summary/normal/full/alert)")
	n1Only := fs.Bool("n1", false, "Show only N+1 detected requests")
	jsonOut := fs.Bool("json", false, "Output as JSON lines")
	fs.Parse(args)

	if *domain == "" {
		fmt.Fprintln(os.Stderr, "Error: -d <domain> is required")
		fmt.Fprintln(os.Stderr, "Usage: phpray-collector trace -d example.com [-n 20] [-min 200]")
		os.Exit(1)
	}

	f, err := os.Open(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening %s: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer f.Close()

	var traces []Trace

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var t Trace
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			continue
		}

		if !strings.Contains(t.Host, *domain) {
			continue
		}
		if *minMs > 0 && t.DurationMs < *minMs {
			continue
		}
		if *level != "" && t.Level != *level {
			continue
		}
		if *n1Only && t.N1 != 1 {
			continue
		}

		traces = append(traces, t)
	}

	// Keep last N traces
	if len(traces) > *count {
		traces = traces[len(traces)-*count:]
	}

	if *jsonOut {
		for _, t := range traces {
			data, _ := json.Marshal(t)
			fmt.Println(string(data))
		}
		return
	}

	// Pretty output
	fmt.Printf("🔦 PHPRay Traces — %s (%d results)\n", *domain, len(traces))
	fmt.Println(strings.Repeat("─", 120))

	for _, t := range traces {
		ts := time.Unix(t.Ts, 0).Format("2006-01-02 15:04:05")
		color := levelColor(t.Level)

		flags := ""
		if t.N1 == 1 {
			flags += " ⚠️N+1"
		}
		if t.WP == 1 {
			flags += " 🔷WP"
		}

		fmt.Printf("\n%s[%s] %s %s %s  %.1fms  [%d]%s%s\n",
			color, ts, t.ID, t.Method, t.URI, t.DurationMs, t.Status, flags, resetColor)

		// Timing breakdown
		dbMs := t.DBMs
		httpMs := t.HTTPMs
		phpMs := t.DurationMs - dbMs - httpMs
		if phpMs < 0 {
			phpMs = 0
		}

		if t.DBCount != nil && *t.DBCount > 0 {
			fmt.Printf("  📊 DB:   %d queries, %.1fms (%.0f%%)\n",
				*t.DBCount, dbMs, dbMs/t.DurationMs*100)
		}
		if t.HTTPCount != nil && *t.HTTPCount > 0 {
			fmt.Printf("  🌐 HTTP: %d calls, %.1fms (%.0f%%)\n",
				*t.HTTPCount, httpMs, httpMs/t.DurationMs*100)
		}
		fmt.Printf("  🖥️  PHP:  %.1fms (%.0f%%)\n", phpMs, phpMs/t.DurationMs*100)
		fmt.Printf("  💾 Mem:   %.2f MB\n", t.MemoryMB)

		// Marks (waterfall)
		if len(t.Marks) > 0 {
			fmt.Println("  📍 Marks:")
			for _, m := range t.Marks {
				barLen := int(m.T / t.DurationMs * 50)
				if barLen > 50 {
					barLen = 50
				}
				bar := strings.Repeat("░", barLen) + "█"
				fmt.Printf("    %6.1fms %s %s\n", m.T, bar, m.Name)
			}
		}

		// Slowest queries
		if len(t.Queries) > 0 {
			fmt.Println("  💾 Queries:")
			for i, q := range t.Queries {
				if i >= 5 {
					fmt.Printf("    ... and %d more\n", len(t.Queries)-5)
					break
				}
				sql := q.SQL
				if len(sql) > 80 {
					sql = sql[:77] + "..."
				}
				src := "mysqli"
				if q.Source == 1 {
					src = "pdo"
				}
				fmt.Printf("    %.2fms [%s] %s\n", q.Ms, src, sql)
				// Show backtrace if available
				for _, f := range q.BT {
					fmt.Printf("      → %s:%d\n", f.File, f.Line)
				}
			}
		}

		// HTTP calls
		if len(t.HTTPCalls) > 0 {
			fmt.Println("  🌐 HTTP Calls:")
			for _, h := range t.HTTPCalls {
				url := h.URL
				if len(url) > 70 {
					url = url[:67] + "..."
				}
				fmt.Printf("    %.0fms [%d] %s\n", h.Ms, h.Status, url)
				// Show backtrace if available
				for _, f := range h.BT {
					fmt.Printf("      → %s:%d\n", f.File, f.Line)
				}
			}
		}

		// Errors
		if len(t.Errors) > 0 {
			fmt.Println("  ❌ Errors:")
			for _, e := range t.Errors {
				msg := e.Msg
				if len(msg) > 80 {
					msg = msg[:77] + "..."
				}
				fmt.Printf("    %.1fms %s: %s\n", e.T, e.Type, msg)
			}
		}
	}
}
