package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cmdStatus prints a one-screen health check: which PHP binaries load the
// extension, whether the collector API answers, where traces are written and
// whether Cloud shipping is on. It is what the docs call `phpray status`.
func cmdStatus(args []string) {
	cfgPath := "/etc/phpray/collector.toml"
	addr := "127.0.0.1:9191"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 < len(args) {
				i++
				cfgPath = args[i]
			}
		case "-addr":
			if i+1 < len(args) {
				i++
				addr = args[i]
			}
		}
	}
	ok := func(b bool) string {
		if b {
			return "ok  "
		}
		return "MISSING"
	}
	fmt.Printf("phpray-collector v%s\n\n", version)

	// PHP binaries: the usual system binary plus per-version installs.
	fmt.Println("PHP extension")
	found := 0
	for _, php := range findPHPBinaries() {
		out, err := exec.Command(php, "-r", `echo PHP_VERSION, " ", extension_loaded("phpray") ? phpversion("phpray") : "-";`).Output()
		if err != nil {
			continue
		}
		f := strings.Fields(string(out))
		if len(f) < 2 {
			continue
		}
		state := "not loaded"
		if f[1] != "-" {
			state = "phpray " + f[1]
			found++
		}
		fmt.Printf("  %-40s PHP %-8s %s\n", php, f[0], state)
	}
	if found == 0 {
		fmt.Println("  no PHP binary loads phpray.so - run the installer or check zz-phpray.ini / a PHP-FPM reload")
	}

	// Config + trace inputs.
	fmt.Println("\nCollector")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Printf("  config %-34s %s (%v)\n", cfgPath, "MISSING", err)
	} else {
		fmt.Printf("  config %-34s ok\n", cfgPath)
		shm := cfg.Input.SHMPath
		if shm == "" {
			shm = "/dev/shm/phpray"
		}
		jsonl := cfg.Input.JSONLPath
		if jsonl == "" {
			jsonl = "/tmp/phpray.jsonl"
		}
		if isGlobPattern(shm) {
			// one ring per PHP-FPM pool (phpray.shm_path with %u): list what matches now
			matches, _ := filepath.Glob(shm)
			fmt.Printf("  ring buffers %-28s %d found\n", shm, len(matches))
			for _, m := range matches {
				fmt.Printf("    %-38s ok\n", m)
			}
			if len(matches) == 0 {
				fmt.Println("    none yet - a pool creates its ring on the first PHP request")
			}
		} else {
			fmt.Printf("  ring buffer %-29s %s\n", shm, ok(exists(shm)))
		}
		fmt.Printf("  jsonl %-35s %s\n", jsonl, ok(exists(jsonl)))
		db := cfg.Storage.DBPath
		if db == "" {
			db = "/var/lib/phpray/traces.db"
		}
		if st, err := os.Stat(db); err == nil {
			fmt.Printf("  database %-32s ok   (%.1f MB)\n", db, float64(st.Size())/1e6)
		} else {
			fmt.Printf("  database %-32s not created yet\n", db)
		}
	}

	// API / dashboard.
	fmt.Println("\nDashboard / API")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		fmt.Printf("  http://%s/  not answering (%v)\n  start it:  systemctl enable --now phpray-collector\n", addr, err)
		return
	}
	defer resp.Body.Close()
	var h struct {
		Traces      int64       `json:"trace_count"`
		Traces2     int64       `json:"traces"`
		AuthEnabled bool        `json:"auth_enabled"`
		Ingest      IngestStats `json:"ingest"`
		Input       InputStatus `json:"input"`
		Cloud       struct {
			Enabled     bool   `json:"enabled"`
			Connected   bool   `json:"connected"`
			AuthFailed  bool   `json:"auth_failed"`
			Endpoint    string `json:"endpoint"`
			SentTraces  int64  `json:"sent_traces"`
			LastStatus  int    `json:"last_status"`
			DroppedTrac int64  `json:"dropped_traces"`
		} `json:"cloud"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Traces == 0 {
		h.Traces = h.Traces2
	}
	fmt.Printf("  http://%s/  ok   (HTTP %d, %d traces stored)\n", addr, resp.StatusCode, h.Traces)
	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Println("  auth is on: open the dashboard with the token from /etc/phpray/dashboard-token")
	}
	// Rings the running collector has open (shm_path glob) and the HTTP ingest endpoint.
	if h.Input.Glob {
		fmt.Printf("  ring buffers open in the collector: %d\n", len(h.Input.Rings))
		for _, r := range h.Input.Rings {
			fmt.Printf("    %-38s v%d  %d records, %d dropped, %.0f%% full\n", r.Path, r.Version, r.Records, r.Drops, r.FillPct)
		}
	}
	access := "loopback clients only"
	if h.AuthEnabled {
		access = "Bearer token required"
	}
	if h.Ingest.Batches > 0 || h.Ingest.Rejected > 0 {
		fmt.Printf("  ingest http://%s/api/v1/ingest  ok   (%d batches, %d traces, %d records dropped, %d requests rejected; %s)\n",
			addr, h.Ingest.Batches, h.Ingest.Traces, h.Ingest.Dropped, h.Ingest.Rejected, access)
	} else {
		fmt.Printf("  ingest http://%s/api/v1/ingest  ready (no batches yet; %s)\n", addr, access)
	}

	// Cloud.
	fmt.Println("\nCloud")
	switch {
	case !h.Cloud.Enabled:
		fmt.Println("  off  - add [cloud] with your server key to " + cfgPath + " and restart phpray-collector")
	case h.Cloud.AuthFailed:
		fmt.Printf("  %s  KEY REJECTED (HTTP %d) - check server_key\n", h.Cloud.Endpoint, h.Cloud.LastStatus)
	case h.Cloud.Connected:
		fmt.Printf("  %s  connected (sent %d traces, dropped %d)\n", h.Cloud.Endpoint, h.Cloud.SentTraces, h.Cloud.DroppedTrac)
	default:
		fmt.Printf("  %s  configured, waiting for the first send (last status %d)\n", h.Cloud.Endpoint, h.Cloud.LastStatus)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// findPHPBinaries returns candidate php executables: PATH plus the common
// per-version locations of Debian, Remi, cPanel/EA, Plesk, LiteSpeed and DirectAdmin.
func findPHPBinaries() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if seen[real] {
				return
			}
			seen[real] = true
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			out = append(out, p)
		}
	}
	if p, err := exec.LookPath("php"); err == nil {
		add(p)
	}
	for _, pat := range []string{"/usr/bin/php8.*", "/opt/remi/php*/root/usr/bin/php", "/opt/cpanel/ea-php*/root/usr/bin/php", "/opt/plesk/php/*/bin/php", "/usr/local/lsws/lsphp*/bin/php", "/usr/local/php*/bin/php", "/opt/alt/php*/usr/bin/php"} {
		m, _ := filepath.Glob(pat)
		for _, p := range m {
			add(p)
		}
	}
	return out
}
