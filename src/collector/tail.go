package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Trace represents a parsed JSONL trace line
type Trace struct {
	Ts          int64   `json:"ts"`
	UID         uint32  `json:"uid"`
	PID         uint32  `json:"pid"`
	RID         uint64  `json:"rid"`
	ID          string  `json:"id"`
	Host        string  `json:"host"`
	Method      string  `json:"method"`
	URI         string  `json:"uri"`
	URIFp       string  `json:"uri_fp,omitempty"`
	Status      uint16  `json:"status"`
	DurationMs  float64 `json:"duration_ms"`
	CPUUserMs   float64 `json:"cpu_user_ms"`
	CPUSysMs    float64 `json:"cpu_sys_ms"`
	MemoryMB    float64 `json:"memory_peak_mb"`
	WP          uint8   `json:"wp"`
	App         string  `json:"app,omitempty"`  // detected application (wordpress, prestashop, ...)
	Profiled    uint8   `json:"profiled"`       // 1 = function profile collected for this request
	Docroot     string  `json:"docroot,omitempty"` // DOCUMENT_ROOT (site_id = sha1(host + "\0" + docroot)[:16])
	SiteID      string  `json:"site_id,omitempty"` // set by agents that compute it themselves (WP plugin via /api/v1/ingest); empty for the extension
	N1          uint8   `json:"n1,omitempty"`
	Level       string  `json:"level"`
	PhpVer      string  `json:"php_ver,omitempty"`
	DBCount     *uint16 `json:"db_count,omitempty"`
	DBMs        float64 `json:"db_ms,omitempty"`
	HTTPCount   *uint16 `json:"http_count,omitempty"`
	HTTPMs      float64 `json:"http_ms,omitempty"`
	FileCount   *uint16 `json:"file_count,omitempty"`
	FileMs      float64 `json:"file_ms,omitempty"`
	RedisCount  *uint16 `json:"redis_count,omitempty"`
	RedisMs     float64 `json:"redis_ms,omitempty"`
	Marks       []Mark  `json:"marks,omitempty"`
	Errors      []Error `json:"errors,omitempty"`
	Queries     []Query `json:"queries,omitempty"`
	HTTPCalls   []HTTP      `json:"http_calls,omitempty"`
	Components  []Component `json:"components,omitempty"`
}

// Component is one entry of the function profile (plugin / theme / mu-plugin / composer package).
// Ms and Pct are inclusive (component boundary); SelfNs excludes nested observed components.
type Component struct {
	Name   string  `json:"name"`
	Ms     float64 `json:"ms"`
	InclNs uint64  `json:"incl_ns,omitempty"`
	SelfNs uint64  `json:"self_ns,omitempty"`
	Calls  int     `json:"calls"`
	Pct    float64 `json:"pct"`
}

// UnmarshalJSON also accepts the WordPress plugin's component shape
// ({"name","incl_ms","self_ms"?,"calls"}) next to the extension's
// ({"name","ms","incl_ns","self_ns","calls","pct"}), so plugin traces read from
// JSONL or /api/v1/ingest keep their per-plugin times.
func (c *Component) UnmarshalJSON(data []byte) error {
	type plain Component
	var aux struct {
		plain
		InclMs *float64 `json:"incl_ms"`
		SelfMs *float64 `json:"self_ms"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = Component(aux.plain)
	if aux.InclMs != nil && c.Ms == 0 && c.InclNs == 0 {
		c.Ms = *aux.InclMs
		c.InclNs = uint64(*aux.InclMs * 1e6)
	}
	if aux.SelfMs != nil && c.SelfNs == 0 {
		c.SelfNs = uint64(*aux.SelfMs * 1e6)
	}
	return nil
}

type Mark struct {
	Name string  `json:"n"`
	T    float64 `json:"t"`
}

type Error struct {
	Type string  `json:"type"`
	Msg  string  `json:"msg"`
	T    float64 `json:"t"`
}

// Frame represents a single PHP backtrace stack frame
type Frame struct {
	File string `json:"f"`
	Line uint32 `json:"l"`
}

type Query struct {
	SQL    string  `json:"sql"`
	Ms     float64 `json:"ms"`
	T      float64 `json:"t"`
	Rows   uint32  `json:"rows,omitempty"`
	Source uint8   `json:"src"`
	BT     []Frame `json:"bt,omitempty"`
}

type HTTP struct {
	URL    string  `json:"url"`
	Ms     float64 `json:"ms"`
	Status uint16  `json:"status"`
	T      float64 `json:"t"`
	BT     []Frame `json:"bt,omitempty"`
}

// levelColor returns ANSI color for trace level
func levelColor(level string) string {
	switch level {
	case "alert":
		return "\033[91m" // bright red
	case "full":
		return "\033[93m" // yellow
	case "normal":
		return "\033[96m" // cyan
	default:
		return "\033[90m" // gray
	}
}

const resetColor = "\033[0m"

// formatTrace formats a trace for terminal display
func formatTrace(t *Trace) string {
	var b strings.Builder

	color := levelColor(t.Level)
	ts := time.Unix(t.Ts, 0).Format("15:04:05")

	// Main line
	fmt.Fprintf(&b, "%s%s %7.1fms %-7s %-30s %s %d",
		color, ts, t.DurationMs, t.Level, t.Host, t.URI, t.Status)

	// DB stats
	if t.DBCount != nil && *t.DBCount > 0 {
		fmt.Fprintf(&b, " 📊%dq/%.0fms", *t.DBCount, t.DBMs)
	}

	// HTTP stats
	if t.HTTPCount != nil && *t.HTTPCount > 0 {
		fmt.Fprintf(&b, " 🌐%d/%.0fms", *t.HTTPCount, t.HTTPMs)
	}

	// File stats
	if t.FileCount != nil && *t.FileCount > 0 {
		fmt.Fprintf(&b, " 📁%d", *t.FileCount)
	}

	// Redis stats
	if t.RedisCount != nil && *t.RedisCount > 0 {
		fmt.Fprintf(&b, " 🔴%dc/%.0fms", *t.RedisCount, t.RedisMs)
	}

	// N+1 flag
	if t.N1 == 1 {
		fmt.Fprintf(&b, " ⚠️N+1")
	}

	// WP flag
	if t.WP == 1 {
		fmt.Fprintf(&b, " 🔷WP")
	}

	// Errors
	if len(t.Errors) > 0 {
		fmt.Fprintf(&b, " ❌%d errs", len(t.Errors))
	}

	b.WriteString(resetColor)

	return b.String()
}

func cmdTail(args []string) {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	filePath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	domain := fs.String("d", "", "Filter by domain")
	minMs := fs.Float64("min", 0, "Minimum duration (ms)")
	follow := fs.Bool("F", true, "Follow file (like tail -f)")
	fs.Parse(args)

	f, err := os.Open(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening %s: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer f.Close()

	// Seek to end if following
	if *follow {
		f.Seek(0, io.SeekEnd)
	}

	reader := bufio.NewReader(f)

	fmt.Printf("🔦 PHPRay tail — %s", *filePath)
	if *domain != "" {
		fmt.Printf(" (domain: %s)", *domain)
	}
	if *minMs > 0 {
		fmt.Printf(" (min: %.0fms)", *minMs)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 120))

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if !*follow {
					return
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			fmt.Fprintf(os.Stderr, "Read error: %v\n", err)
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var t Trace
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			continue
		}

		// Apply filters
		if *domain != "" && !strings.Contains(t.Host, *domain) {
			continue
		}
		if *minMs > 0 && t.DurationMs < *minMs {
			continue
		}

		fmt.Println(formatTrace(&t))

		// Show query details for non-summary traces
		if len(t.Queries) > 0 {
			for i, q := range t.Queries {
				if i >= 5 && t.Level != "full" && t.Level != "alert" {
					fmt.Printf("    ... and %d more queries\n", len(t.Queries)-5)
					break
				}
				sql := q.SQL
				if len(sql) > 80 {
					sql = sql[:77] + "..."
				}
				fmt.Printf("    💾 %.2fms %s\n", q.Ms, sql)
			}
		}

		// Show HTTP call details
		if len(t.HTTPCalls) > 0 {
			for _, h := range t.HTTPCalls {
				url := h.URL
				if len(url) > 60 {
					url = url[:57] + "..."
				}
				fmt.Printf("    🌐 %.0fms [%d] %s\n", h.Ms, h.Status, url)
			}
		}

		// Show errors
		if len(t.Errors) > 0 {
			for _, e := range t.Errors {
				msg := e.Msg
				if len(msg) > 80 {
					msg = msg[:77] + "..."
				}
				fmt.Printf("    ❌ %s: %s\n", e.Type, msg)
			}
		}
	}
}
