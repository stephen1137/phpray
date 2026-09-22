package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Tryb lokalny: agent rozmawia z kolektorem na tym samym serwerze, bez konta
// w chmurze. Kolektor przyjmuje podpisany token HS256 z sekretem z sekcji
// [auth] w /etc/phpray/collector.toml — ten sam, którego używa wtyczka
// DirectAdmin. Bez sekretu w konfiguracji kolektor nie wymaga niczego.
type tokenLokalny struct {
	sekret string
	rola   string
	sub    string
}

func (t tokenLokalny) podpisz() string {
	if t.sekret == "" {
		return ""
	}
	naglowek := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	teraz := time.Now().Unix()
	roszczenia, _ := json.Marshal(map[string]any{
		"sub": t.sub, "role": t.rola, "iat": teraz, "exp": teraz + 300,
	})
	wejscie := naglowek + "." + base64url(roszczenia)
	m := hmac.New(sha256.New, []byte(t.sekret))
	m.Write([]byte(wejscie))
	return wejscie + "." + base64url(m.Sum(nil))
}

func base64url(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// narzedziaLokalne odwzorowuje ten sam zestaw narzędzi na API kolektora.
// Nazwy narzędzi są identyczne jak w trybie chmurowym, żeby agent nie musiał
// wiedzieć, z czym rozmawia; różni się tylko to, że kluczem jest domena.
func narzedziaLokalne(k *klient) []toolSpec {
	oknoQ := func(args map[string]any, domyslne int) url.Values {
		q := url.Values{}
		q.Set("window", fmt.Sprintf("%d", liczba(args, "window_minutes", domyslne)))
		return q
	}
	domena := func(args map[string]any) (string, error) {
		d := tekst(args, "site_id", tekst(args, "domain", ""))
		if d == "" {
			return "", fmt.Errorf("site_id (the domain) is required; call phpray_sites first")
		}
		return d, nil
	}
	polaD := map[string]any{
		"site_id":        map[string]any{"type": "string", "description": "Domain name, as returned by phpray_sites."},
		"window_minutes": map[string]any{"type": "integer", "description": "How far back to look, in minutes."},
	}

	return []toolSpec{
		{
			Name:        "phpray_sites",
			Description: "List the domains this collector sees, with requests, p95 latency and error rate. Start here.",
			InputSchema: schemat(map[string]any{
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"limit":          map[string]any{"type": "integer", "description": "How many domains to return, slowest first. Default 40."},
			}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/api/v1/domains", oknoQ(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return tabelaDomen(r, liczba(args, "limit", 40))
			},
		},
		{
			Name:        "phpray_overview",
			Description: "Headline numbers for one domain: requests, errors, latency percentiles and database share.",
			InputSchema: schemat(polaD, "site_id"),
			call: func(args map[string]any) (string, error) {
				d, err := domena(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/api/v1/domains/"+url.PathEscape(d)+"/stats", oknoQ(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name: "phpray_diagnose",
			Description: "The collector's own read of what is wrong with a domain: a health score, findings in plain " +
				"language and what to look at first. The fastest way to start.",
			InputSchema: schemat(polaD, "site_id"),
			call: func(args map[string]any) (string, error) {
				d, err := domena(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/api/v1/diagnostics/"+url.PathEscape(d), oknoQ(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:        "phpray_slow_queries",
			Description: "SQL fingerprints ordered by time spent, with call counts. Literals are masked.",
			InputSchema: schemat(polaD),
			call: func(args map[string]any) (string, error) {
				q := oknoQ(args, 1440)
				if d := tekst(args, "site_id", ""); d != "" {
					q.Set("domain", d)
				}
				r, err := k.zapytaj("GET", "/api/v1/queries/slow", q, nil)
				if err != nil {
					return "", err
				}
				return tabelaZapytan(r, liczba(args, "limit", 20))
			},
		},
		{
			Name:        "phpray_components",
			Description: "Time attributed to each plugin, theme or vendor package on profiled requests.",
			InputSchema: schemat(polaD),
			call: func(args map[string]any) (string, error) {
				q := oknoQ(args, 1440)
				if d := tekst(args, "site_id", ""); d != "" {
					q.Set("domain", d)
				}
				r, err := k.zapytaj("GET", "/api/v1/components", q, nil)
				if err != nil {
					return "", err
				}
				return tabelaKomponentow(r, liczba(args, "limit", 20))
			},
		},
		{
			Name:        "phpray_traces",
			Description: "Recent requests. Filter by domain, minimum duration and status, then pass an id to phpray_trace.",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Domain name."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"min_ms":         map[string]any{"type": "integer", "description": "Only requests slower than this."},
				"status":         map[string]any{"type": "integer", "description": "Only this HTTP status."},
				"limit":          map[string]any{"type": "integer", "description": "How many to return. Default 50."},
			}),
			call: func(args map[string]any) (string, error) {
				q := oknoQ(args, 1440)
				q.Set("limit", fmt.Sprintf("%d", liczba(args, "limit", 50)))
				if d := tekst(args, "site_id", ""); d != "" {
					q.Set("domain", d)
				}
				if v := liczba(args, "min_ms", 0); v > 0 {
					q.Set("min_ms", fmt.Sprintf("%d", v))
				}
				if v := liczba(args, "status", 0); v > 0 {
					q.Set("status", fmt.Sprintf("%d", v))
				}
				r, err := k.zapytaj("GET", "/api/v1/traces", q, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:        "phpray_trace",
			Description: "One request in full: timing split, every query, outbound calls, errors and components.",
			InputSchema: schemat(map[string]any{
				"trace_id": map[string]any{"type": "string", "description": "Trace id from phpray_traces."},
			}, "trace_id"),
			call: func(args map[string]any) (string, error) {
				id := tekst(args, "trace_id", "")
				if id == "" {
					return "", fmt.Errorf("trace_id is required; get one from phpray_traces")
				}
				r, err := k.zapytaj("GET", "/api/v1/traces/"+url.PathEscape(id), nil, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:        "phpray_alerts",
			Description: "Alerts currently firing on this server.",
			InputSchema: schemat(map[string]any{}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/api/v1/alerts", nil, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:        "phpray_php_versions",
			Description: "Which PHP versions the traced sites run, and how much traffic each one carries.",
			InputSchema: schemat(map[string]any{}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/api/v1/php-versions", oknoQ(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
	}
}

// tabelaDomen skraca /api/v1/domains. Na hoście współdzielonym bywa tam
// sto kilkadziesiąt domen po dziesięć pól, czyli kilkadziesiąt kilobajtów.
// Sortujemy po średnim czasie, bo agent szuka tego, co wolne.
func tabelaDomen(surowe json.RawMessage, ile int) (string, error) {
	var o struct {
		Count   int `json:"count"`
		Domains []struct {
			Host     string  `json:"host"`
			App      string  `json:"app"`
			Owner    string  `json:"owner"`
			Requests int     `json:"requests"`
			Errors   int     `json:"errors"`
			AvgMs    float64 `json:"avg_duration_ms"`
			MaxMs    float64 `json:"max_duration_ms"`
			N1       int     `json:"n1_count"`
			TotalQ   int     `json:"total_queries"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Domains) == 0 {
		return "No domains have reported traces in this window.", nil
	}
	sort.Slice(o.Domains, func(i, j int) bool { return o.Domains[i].AvgMs > o.Domains[j].AvgMs })
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d domains reporting, showing the %d slowest by average time. Pass host as site_id.\n\n",
		o.Count, min(ile, len(o.Domains)))
	fmt.Fprintf(&b, "%-36s %-10s %-10s %8s %8s %9s %7s %8s\n",
		"host", "app", "owner", "requests", "avg ms", "max ms", "N+1", "queries")
	for i, d := range o.Domains {
		if i >= ile {
			break
		}
		fmt.Fprintf(&b, "%-36s %-10s %-10s %8d %8.0f %9.0f %7d %8d\n",
			przytnij(d.Host, 36), przytnij(d.App, 10), przytnij(d.Owner, 10),
			d.Requests, d.AvgMs, d.MaxMs, d.N1, d.TotalQ)
	}
	return b.String(), nil
}
