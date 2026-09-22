package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func cmdStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	filePath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	windowMin := fs.Int("w", 60, "Time window (minutes)")
	fs.Parse(args)

	f, err := os.Open(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening %s: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer f.Close()

	cutoff := time.Now().Unix() - int64(*windowMin)*60

	var (
		total      int
		levelCount = map[string]int{}
		domainMs   = map[string][]float64{}
		domainReq  = map[string]int{}
		n1Count    int
		errCount   int
		maxMs      float64
		maxURI     string
		maxHost    string
		totalMs    float64
		totalDB    int
		totalHTTP  int
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var t Trace
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			continue
		}
		if t.Ts < cutoff {
			continue
		}

		total++
		levelCount[t.Level]++
		domainMs[t.Host] = append(domainMs[t.Host], t.DurationMs)
		domainReq[t.Host]++
		totalMs += t.DurationMs

		if t.DurationMs > maxMs {
			maxMs = t.DurationMs
			maxURI = t.URI
			maxHost = t.Host
		}
		if t.N1 == 1 {
			n1Count++
		}
		if len(t.Errors) > 0 {
			errCount++
		}
		if t.DBCount != nil {
			totalDB += int(*t.DBCount)
		}
		if t.HTTPCount != nil {
			totalHTTP += int(*t.HTTPCount)
		}
	}

	if total == 0 {
		fmt.Printf("No traces found in last %d minutes.\n", *windowMin)
		return
	}

	// Overall stats
	avgMs := totalMs / float64(total)
	fmt.Printf("🔦 PHPRay Stats — last %d minutes\n", *windowMin)
	fmt.Println(strings.Repeat("─", 80))
	fmt.Printf("  Total requests:  %d\n", total)
	fmt.Printf("  Avg duration:    %.1fms\n", avgMs)
	fmt.Printf("  Max duration:    %.1fms (%s%s)\n", maxMs, maxHost, maxURI)
	fmt.Printf("  Total queries:   %d\n", totalDB)
	fmt.Printf("  Total HTTP:      %d\n", totalHTTP)
	fmt.Printf("  N+1 detected:    %d (%.1f%%)\n", n1Count, float64(n1Count)/float64(total)*100)
	fmt.Printf("  With errors:     %d (%.1f%%)\n", errCount, float64(errCount)/float64(total)*100)
	fmt.Println()

	// Level distribution
	fmt.Println("  Trace levels:")
	for _, level := range []string{"summary", "normal", "full", "alert"} {
		if c, ok := levelCount[level]; ok {
			bar := strings.Repeat("█", c*40/total)
			fmt.Printf("    %-8s %5d (%5.1f%%) %s\n", level, c, float64(c)/float64(total)*100, bar)
		}
	}
	fmt.Println()

	// Top 10 slowest domains
	type domainStat struct {
		name  string
		count int
		avgMs float64
		p95Ms float64
	}
	var dstats []domainStat
	for name, times := range domainMs {
		sort.Float64s(times)
		avg := 0.0
		for _, t := range times {
			avg += t
		}
		avg /= float64(len(times))
		p95idx := int(float64(len(times)) * 0.95)
		if p95idx >= len(times) {
			p95idx = len(times) - 1
		}
		dstats = append(dstats, domainStat{
			name:  name,
			count: len(times),
			avgMs: avg,
			p95Ms: times[p95idx],
		})
	}
	sort.Slice(dstats, func(i, j int) bool {
		return dstats[i].p95Ms > dstats[j].p95Ms
	})

	fmt.Println("  Top domains by P95 latency:")
	fmt.Printf("    %-35s %6s %8s %8s\n", "DOMAIN", "REQS", "AVG", "P95")
	fmt.Println("    " + strings.Repeat("─", 65))
	for i, d := range dstats {
		if i >= 10 {
			break
		}
		name := d.name
		if len(name) > 35 {
			name = name[:32] + "..."
		}
		fmt.Printf("    %-35s %6d %7.1fms %7.1fms\n", name, d.count, d.avgMs, d.p95Ms)
	}
}
