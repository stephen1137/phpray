package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type topEntry struct {
	Host       string
	URI        string
	Level      string
	DurationMs float64
	DBCount    uint16
	DBMs       float64
	HTTPCount  uint16
	HTTPMs     float64
	Status     uint16
	N1         bool
	WP         bool
	Errors     int
	Ts         int64
}

func cmdTop(args []string) {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	filePath := fs.String("f", "", "read traces from a JSONL file instead of the collector")
	cfgPath := fs.String("c", "/etc/phpray/collector.toml", "collector config (for the API address and auth secret)")
	addr := fs.String("addr", "127.0.0.1:9191", "collector API address")
	count := fs.Int("n", 20, "Number of entries to show")
	refreshSec := fs.Int("r", 2, "Refresh interval (seconds)")
	windowSec := fs.Int64("w", 60, "Time window (seconds)")
	domain := fs.String("d", "", "Filter by domain")
	fs.Parse(args)

	// Skąd czytać. Do 0.15.2 „top" zaglądał wyłącznie do /tmp/phpray.jsonl,
	// a produkcyjna instalacja pisze do pierścienia w pamięci dzielonej —
	// polecenie pokazywało więc pustą listę na każdym normalnym serwerze
	// i nie mówiło dlaczego. Teraz domyślnie pyta kolektor, a plik jest
	// awaryjnym źródłem przez -f.
	zrodlo := *filePath
	przezAPI := zrodlo == ""
	if przezAPI {
		if problem := sprawdzKolektor(*addr); problem != "" {
			fmt.Fprintln(os.Stderr, problem)
			os.Exit(1)
		}
	} else if _, err := os.Stat(zrodlo); err != nil {
		fmt.Fprintf(os.Stderr, "phpray top: cannot open %s: %v\n", zrodlo, err)
		os.Exit(1)
	}

	fmt.Print("\033[?25l")         // hide cursor
	defer fmt.Print("\033[?25h\n") // show cursor on exit

	for {
		var entries []topEntry
		var bladAPI string
		if przezAPI {
			entries, bladAPI = sladyZAPI(*addr, *cfgPath, *windowSec, *domain, *count)
		} else {
			entries = readRecentTraces(zrodlo, *windowSec, *domain)
		}

		// Sort by duration descending
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].DurationMs > entries[j].DurationMs
		})

		// Truncate to count
		if len(entries) > *count {
			entries = entries[:*count]
		}

		// Clear screen
		cmd := exec.Command("clear")
		cmd.Stdout = os.Stdout
		cmd.Run()

		// Header
		now := time.Now().Format("15:04:05")
		fmt.Printf("🔦 PHPRay Top — %s  (%d requests in last %ds)\n", now, len(entries), *windowSec)
		if *domain != "" {
			fmt.Printf("   Filter: %s\n", *domain)
		}
		fmt.Println(strings.Repeat("─", 120))
		fmt.Printf("%-7s %8s %-7s %-30s %-40s %5s %5s %5s %s\n",
			"STATUS", "TIME", "LEVEL", "HOST", "URI", "DB", "HTTP", "ERR", "FLAGS")
		fmt.Println(strings.Repeat("─", 120))

		for _, e := range entries {
			uri := e.URI
			if len(uri) > 40 {
				uri = uri[:37] + "..."
			}
			host := e.Host
			if len(host) > 30 {
				host = host[:27] + "..."
			}

			color := levelColor(e.Level)
			flags := ""
			if e.N1 {
				flags += "N+1 "
			}
			if e.WP {
				flags += "WP "
			}

			dbStr := "-"
			if e.DBCount > 0 {
				dbStr = fmt.Sprintf("%d", e.DBCount)
			}
			httpStr := "-"
			if e.HTTPCount > 0 {
				httpStr = fmt.Sprintf("%d", e.HTTPCount)
			}
			errStr := "-"
			if e.Errors > 0 {
				errStr = fmt.Sprintf("%d", e.Errors)
			}

			fmt.Printf("%s%-7d %7.1fms %-7s %-30s %-40s %5s %5s %5s %s%s\n",
				color, e.Status, e.DurationMs, e.Level, host, uri,
				dbStr, httpStr, errStr, flags, resetColor)
		}

		if len(entries) == 0 {
			// Puste okno wygląda dokładnie tak samo jak zepsuta konfiguracja,
			// więc trzeba powiedzieć, gdzie szukaliśmy i co dalej.
			if bladAPI != "" {
				fmt.Printf("  %s\n", bladAPI)
			} else if przezAPI {
				fmt.Printf("  Nothing in the last %ds. The collector is answering on %s, so it is\n", *windowSec, *addr)
				fmt.Println("  recording — this window is simply quiet. Widen it with  -w 3600,")
				fmt.Println("  or check that the extension is loaded:  phpray status")
			} else {
				fmt.Printf("  Nothing in the last %ds in %s.\n", *windowSec, zrodlo)
			}
		}

		fmt.Printf("\n  Press Ctrl+C to exit. Refreshing every %ds...\n", *refreshSec)

		time.Sleep(time.Duration(*refreshSec) * time.Second)
	}
}

func readRecentTraces(path string, windowSec int64, domain string) []topEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	cutoff := time.Now().Unix() - windowSec
	var entries []topEntry

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var t Trace
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			continue
		}

		if t.Ts < cutoff {
			continue
		}

		if domain != "" && !strings.Contains(t.Host, domain) {
			continue
		}

		e := topEntry{
			Host:       t.Host,
			URI:        t.URI,
			Level:      t.Level,
			DurationMs: t.DurationMs,
			Status:     t.Status,
			N1:         t.N1 == 1,
			WP:         t.WP == 1,
			Errors:     len(t.Errors),
			Ts:         t.Ts,
		}
		if t.DBCount != nil {
			e.DBCount = *t.DBCount
			e.DBMs = t.DBMs
		}
		if t.HTTPCount != nil {
			e.HTTPCount = *t.HTTPCount
			e.HTTPMs = t.HTTPMs
		}

		entries = append(entries, e)
	}

	return entries
}

// sprawdzKolektor zwraca pusty łańcuch, gdy API odpowiada, a w przeciwnym razie
// gotowy komunikat dla człowieka. Bez tego „phpray top" na serwerze bez
// działającego kolektora kończył się pustym ekranem.
func sprawdzKolektor(addr string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		return "phpray top: the collector is not answering on " + addr + " (" + err.Error() + ")\n" +
			"  Start it:        systemctl start phpray-collector\n" +
			"  Check the setup: phpray status\n" +
			"  Or read a JSONL file instead:  phpray top -f /path/to/phpray.jsonl"
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("phpray top: the collector on %s answered %d on /api/v1/health", addr, resp.StatusCode)
	}
	return ""
}

// sladyZAPI pobiera ostatnie ślady z kolektora. Gdy API wymaga uwierzytelnienia,
// podpisujemy krótkotrwały token sekretem z konfiguracji — to samo, co robi
// wtyczka panelu, tyle że lokalnie i na kilka sekund.
func sladyZAPI(addr, cfgPath string, windowSec int64, domain string, limit int) ([]topEntry, string) {
	// API liczy okno w PEŁNYCH minutach, więc przy window=1 bieżąca, niepełna
	// minuta wypada poza zakresem — a to w niej są najnowsze żądania. Domyślne
	// „phpray top" (60 s) pokazywało z tego powodu zero, choć kolektor zapisywał
	// ruch na bieżąco: pierwsze polecenie nowego użytkownika milczało.
	// Bierzemy więc z zapasem i docinamy dokładnie po stronie klienta.
	okno := int(windowSec/60) + 2
	u := fmt.Sprintf("http://%s/api/v1/traces?window=%d&limit=%d", addr, okno, limit*8)
	if domain != "" {
		u += "&domain=" + url.QueryEscape(domain)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "phpray top: " + err.Error()
	}
	if tok := tokenLokalny(cfgPath); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, "phpray top: the collector stopped answering (" + err.Error() + ")"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, "phpray top: the collector requires authentication and the token could not be signed.\n" +
			"  Run as root, or point at the config:  phpray top -c /etc/phpray/collector.toml"
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("phpray top: /api/v1/traces answered %d", resp.StatusCode)
	}
	var odp struct {
		Traces []struct {
			Ts         int64   `json:"timestamp"`
			Host       string  `json:"host"`
			URI        string  `json:"uri"`
			Status     uint16  `json:"status"`
			DurationMs float64 `json:"duration_ms"`
			DBCount    uint16  `json:"db_count"`
			DBMs       float64 `json:"db_ms"`
			HTTPCount  uint16  `json:"http_count"`
			HTTPMs     float64 `json:"http_ms"`
			Level      string  `json:"level"`
			N1         int     `json:"n1"`
			WP         int     `json:"wp"`
			Errors     int     `json:"error_count"`
		} `json:"traces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&odp); err != nil {
		return nil, "phpray top: could not read the collector response (" + err.Error() + ")"
	}
	// Docinamy do żądanego okna co do sekundy — API oddało z zapasem.
	granica := time.Now().Unix() - windowSec
	entries := make([]topEntry, 0, len(odp.Traces))
	for _, t := range odp.Traces {
		if t.Ts > 0 && t.Ts < granica {
			continue
		}
		entries = append(entries, topEntry{
			Host: t.Host, URI: t.URI, Level: t.Level, DurationMs: t.DurationMs,
			DBCount: t.DBCount, DBMs: t.DBMs, HTTPCount: t.HTTPCount, HTTPMs: t.HTTPMs,
			Status: t.Status, N1: t.N1 == 1, WP: t.WP == 1, Errors: t.Errors, Ts: t.Ts,
		})
	}
	return entries, ""
}

// tokenLokalny podpisuje token administratora na minutę. Pusty łańcuch, gdy
// konfiguracji nie da się odczytać — wtedy i tak próbujemy bez nagłówka, bo
// kolektor bywa uruchomiony bez uwierzytelnienia.
func tokenLokalny(cfgPath string) string {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return ""
	}
	secret := cfg.ResolveAuthSecret()
	if secret == "" {
		return ""
	}
	tok, err := GenerateToken(secret, JWTClaims{Sub: "phpray-top", Role: "admin",
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		return ""
	}
	return tok
}
