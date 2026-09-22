package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func schemat(wlasciwosci map[string]any, wymagane ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": wlasciwosci}
	if len(wymagane) > 0 {
		s["required"] = wymagane
	}
	return s
}

var (
	polaSerwis = map[string]any{
		"site_id": map[string]any{"type": "string", "description": "Site id from phpray_sites."},
	}
	polaOkno = map[string]any{
		"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
		"window_minutes": map[string]any{"type": "integer", "description": "How far back to look, in minutes. Default 60."},
	}
)

func zbudujNarzedzia(k *klient) []toolSpec {
	okno := func(args map[string]any, domyslne int) url.Values {
		q := url.Values{}
		q.Set("window", fmt.Sprintf("%d", liczba(args, "window_minutes", domyslne)))
		return q
	}
	idSerwisu := func(args map[string]any) (string, error) {
		id := tekst(args, "site_id", "")
		if id == "" {
			return "", fmt.Errorf("site_id is required; call phpray_sites first to get one")
		}
		return id, nil
	}
	prosty := func(sciezka string, domyslneOkno int) func(map[string]any) (string, error) {
		return func(args map[string]any) (string, error) {
			id, err := idSerwisu(args)
			if err != nil {
				return "", err
			}
			r, err := k.zapytaj("GET", fmt.Sprintf(sciezka, id), okno(args, domyslneOkno), nil)
			if err != nil {
				return "", err
			}
			return ladnie(r), nil
		}
	}

	return []toolSpec{
		{
			Name: "phpray_sites",
			Description: "List the PHP sites this token can see, with requests, p95 latency and error rate. " +
				"Start here: every other tool needs a site_id from this list.",
			InputSchema: schemat(map[string]any{}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/console/v1/sites", nil, nil)
				if err != nil {
					return "", err
				}
				return tabelaSerwisow(r)
			},
		},
		{
			Name: "phpray_overview",
			Description: "Headline numbers for one site over a time window: requests, errors, p50/p95/p99 latency, " +
				"database share and N+1 count. Use this before digging into anything else.",
			InputSchema: schemat(polaOkno, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/overview", okno(args, 60), nil)
				if err != nil {
					return "", err
				}
				return podsumowaniePrzegladu(r, 12, 10)
			},
		},
		{
			Name: "phpray_slow_pages",
			Description: "The slowest URLs on a site, with request counts and p95, from the request timeseries. " +
				"Answers \"which page is slow\".",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"limit":          map[string]any{"type": "integer", "description": "How many URLs to return. Default 25."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/overview", okno(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return podsumowaniePrzegladu(r, liczba(args, "limit", 25), 0)
			},
		},
		{
			Name: "phpray_slow_queries",
			Description: "SQL fingerprints ordered by time spent, with call counts and the caller when known. " +
				"Literals are already masked. Answers \"which query is slow\".",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"limit":          map[string]any{"type": "integer", "description": "How many queries to return. Default 20."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/queries/slow", okno(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return tabelaZapytan(r, liczba(args, "limit", 20))
			},
		},
		{
			Name: "phpray_components",
			Description: "Time attributed to each plugin, theme or vendor package on profiled requests. " +
				"Answers \"which plugin is slowing the site down\". Empty when function profiling is off.",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"limit":          map[string]any{"type": "integer", "description": "How many components to return. Default 20."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/components", okno(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return tabelaKomponentow(r, liczba(args, "limit", 20))
			},
		},
		{
			Name:        "phpray_errors",
			Description: "Recent PHP errors and warnings for a site, with file, line and the request they happened in.",
			InputSchema: schemat(polaOkno, "site_id"),
			call:        prosty("/console/v1/sites/%s/errors", 1440),
		},
		{
			Name: "phpray_traces",
			Description: "Recent requests for a site. Filter with min_ms and status to find the bad ones, " +
				"then pass an id to phpray_trace.",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"min_ms":         map[string]any{"type": "integer", "description": "Only requests slower than this."},
				"status":         map[string]any{"type": "integer", "description": "Only this HTTP status, e.g. 500."},
				"limit":          map[string]any{"type": "integer", "description": "How many to return. Default 50."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				q := okno(args, 1440)
				q.Set("limit", fmt.Sprintf("%d", liczba(args, "limit", 50)))
				if v := liczba(args, "min_ms", 0); v > 0 {
					q.Set("min_ms", fmt.Sprintf("%d", v))
				}
				if v := liczba(args, "status", 0); v > 0 {
					q.Set("status", fmt.Sprintf("%d", v))
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/traces", q, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name: "phpray_trace",
			Description: "One request in full: wall time split into PHP, database and outbound HTTP, every query, " +
				"every external call, errors, and the component breakdown when the request was profiled.",
			InputSchema: schemat(map[string]any{
				"trace_id": map[string]any{"type": "string", "description": "Trace id from phpray_traces."},
			}, "trace_id"),
			call: func(args map[string]any) (string, error) {
				id := tekst(args, "trace_id", "")
				if id == "" {
					return "", fmt.Errorf("trace_id is required; get one from phpray_traces")
				}
				r, err := k.zapytaj("GET", "/console/v1/traces/"+url.PathEscape(id), nil, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name: "phpray_compare",
			Description: "Two time ranges for one site side by side. Use it to check whether a deployment, " +
				"a plugin update or a configuration change made things better or worse.",
			InputSchema: schemat(map[string]any{
				"site_id": map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"from_a":  map[string]any{"type": "integer", "description": "Start of range A, unix seconds."},
				"to_a":    map[string]any{"type": "integer", "description": "End of range A, unix seconds."},
				"from_b":  map[string]any{"type": "integer", "description": "Start of range B, unix seconds."},
				"to_b":    map[string]any{"type": "integer", "description": "End of range B, unix seconds."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				q := url.Values{}
				for _, k2 := range []string{"from_a", "to_a", "from_b", "to_b"} {
					if v := liczba(args, k2, 0); v > 0 {
						q.Set(k2, fmt.Sprintf("%d", v))
					}
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/compare", q, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:        "phpray_alerts",
			Description: "Alerts currently firing across every site this token can see.",
			InputSchema: schemat(map[string]any{}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/console/v1/alerts", nil, nil)
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
		{
			Name:  "phpray_profile_url",
			zapis: true,
			Description: "Turn on per-function profiling for one URL prefix for a few minutes, so the next requests " +
				"to it get a plugin-level breakdown. This is the only tool that changes anything, and it needs an " +
				"owner or member token. Profiling costs a few percent while it is on and switches itself off.",
			InputSchema: schemat(map[string]any{
				"site_id":    map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"url_prefix": map[string]any{"type": "string", "description": "Path prefix to profile, for example /checkout."},
				"minutes":    map[string]any{"type": "integer", "description": "How long to keep it on. Default 10."},
			}, "site_id", "url_prefix"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				prefiks := tekst(args, "url_prefix", "")
				if prefiks == "" {
					return "", fmt.Errorf("url_prefix is required, for example /checkout")
				}
				cialo, _ := json.Marshal(map[string]any{
					"url_prefix": prefiks,
					"minutes":    liczba(args, "minutes", 10),
				})
				r, err := k.zapytaj("POST", "/console/v1/sites/"+id+"/profile", nil, bytes.NewReader(cialo))
				if err != nil {
					return "", err
				}
				return ladnie(r), nil
			},
		},
	}
}

// tabelaSerwisow zamienia pełną odpowiedź /sites na zwięzłą tabelę. Surowa
// odpowiedź niesie dla każdego serwisu listy adresów i komponentów, co przy
// osiemdziesięciu serwisach daje kilkadziesiąt kilobajtów i zjada okno agenta.
func tabelaSerwisow(surowe json.RawMessage) (string, error) {
	var odp struct {
		Sites []struct {
			Site struct {
				ID   string `json:"id"`
				Host string `json:"host"`
				App  string `json:"app"`
			} `json:"site"`
			ServerName string `json:"server_name"`
			LastSeenAt string `json:"last_seen_at"`
			Stats      *struct {
				Requests  int     `json:"requests"`
				P95Ms     float64 `json:"p95_ms"`
				AvgMs     float64 `json:"avg_ms"`
				ErrorRate float64 `json:"error_rate"`
				DBShare   float64 `json:"db_ms_share"`
			} `json:"stats"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(surowe, &odp); err != nil {
		return ladnie(surowe), nil // nie udajemy mądrzejszych niż jesteśmy
	}
	if len(odp.Sites) == 0 {
		return "No sites report to this account yet. Install the collector on a server and connect it with a server key.", nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d sites. Pass site_id to the other tools.\n\n", len(odp.Sites))
	fmt.Fprintf(&b, "%-38s %-32s %-10s %-9s %8s %9s %8s %7s\n",
		"site_id", "host", "app", "server", "requests", "p95 ms", "errors", "db %")
	for _, w := range odp.Sites {
		req, p95, er, db := 0, 0.0, 0.0, 0.0
		if w.Stats != nil {
			req, p95, er, db = w.Stats.Requests, w.Stats.P95Ms, w.Stats.ErrorRate, w.Stats.DBShare
		}
		fmt.Fprintf(&b, "%-38s %-32s %-10s %-9s %8d %9.1f %7.1f%% %6.0f%%\n",
			w.Site.ID, przytnij(w.Site.Host, 32), przytnij(w.Site.App, 10),
			przytnij(w.ServerName, 9), req, p95, er*100, db*100)
	}
	return b.String(), nil
}

func przytnij(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// podsumowaniePrzegladu skraca odpowiedź /overview do tego, co agent naprawdę
// czyta. Surowa odpowiedź ma 1587 pozycji w top_uris i potrafi mieć 136 kB.
func podsumowaniePrzegladu(surowe json.RawMessage, ileURI, ileKomponentow int) (string, error) {
	var o struct {
		Site struct {
			Host string `json:"host"`
			App  string `json:"app"`
		} `json:"site"`
		ServerName string `json:"server_name"`
		LastSeenAt string `json:"last_seen_at"`
		Stats      *struct {
			Requests  int     `json:"requests"`
			Errors5xx int     `json:"errors_5xx"`
			ErrorsPHP int     `json:"errors_php"`
			Profiled  int     `json:"profiled"`
			AvgMs     float64 `json:"avg_ms"`
			P95Ms     float64 `json:"p95_ms"`
			DBMs      float64 `json:"db_ms"`
			HTTPMs    float64 `json:"http_ms"`
			ErrorRate float64 `json:"error_rate"`
			DBShare   float64 `json:"db_ms_share"`
		} `json:"stats"`
		TopURIs []struct {
			URI      string  `json:"uri"`
			Requests int     `json:"requests"`
			P95Ms    float64 `json:"p95_ms"`
		} `json:"top_uris"`
		Components []struct {
			Name      string  `json:"name"`
			SelfMs    float64 `json:"self_ms"`
			InclMs    float64 `json:"incl_ms"`
			Calls     int64   `json:"calls"`
			Profiled  int     `json:"profiled"`
			AvgSelfMs float64 `json:"avg_self_ms"`
		} `json:"components"`
		PHPVersions []string `json:"php_versions"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s (%s) on %s, last seen %s\n", o.Site.Host, o.Site.App, o.ServerName, o.LastSeenAt)
	if o.Stats != nil {
		st := o.Stats
		fmt.Fprintf(&b, "\nrequests %d · p95 %.0f ms · avg %.0f ms · errors %.2f%% (%d 5xx, %d PHP) · database %.0f%% of time · profiled %d\n",
			st.Requests, st.P95Ms, st.AvgMs, st.ErrorRate*100, st.Errors5xx, st.ErrorsPHP, st.DBShare*100, st.Profiled)
	}
	if len(o.PHPVersions) > 0 {
		fmt.Fprintf(&b, "PHP: %v\n", o.PHPVersions)
	}
	if ileURI > 0 && len(o.TopURIs) > 0 {
		fmt.Fprintf(&b, "\nSlowest URLs (%d of %d):\n%-56s %8s %9s\n", min(ileURI, len(o.TopURIs)), len(o.TopURIs), "url", "requests", "p95 ms")
		for i, u := range o.TopURIs {
			if i >= ileURI {
				break
			}
			fmt.Fprintf(&b, "%-56s %8d %9.1f\n", przytnij(u.URI, 56), u.Requests, u.P95Ms)
		}
	}
	if ileKomponentow > 0 && len(o.Components) > 0 {
		fmt.Fprintf(&b, "\nWhere the time goes (%d of %d components, profiled requests only):\n%-44s %11s %11s %7s\n",
			min(ileKomponentow, len(o.Components)), len(o.Components), "component", "self total", "incl total", "avg self")
		for i, c := range o.Components {
			if i >= ileKomponentow {
				break
			}
			fmt.Fprintf(&b, "%-44s %11.0f %11.0f %7.1f\n", przytnij(c.Name, 44), c.SelfMs, c.InclMs, c.AvgSelfMs)
		}
	}
	if o.Stats != nil && o.Stats.Profiled == 0 {
		b.WriteString("\nNo profiled requests in this window, so there is no component breakdown. " +
			"Use phpray_profile_url to turn profiling on for one URL prefix for a few minutes.\n")
	}
	return b.String(), nil
}

// tabelaZapytan skraca /queries/slow: 200 odcisków SQL to około 50 kB.
func tabelaZapytan(surowe json.RawMessage, ile int) (string, error) {
	var o struct {
		Queries []struct {
			SQL      string  `json:"sql"`
			Count    int     `json:"count"`
			AvgMs    float64 `json:"avg_ms"`
			MaxMs    float64 `json:"max_ms"`
			LastSeen string  `json:"last_seen"`
			Caller   string  `json:"caller"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Queries) == 0 {
		return "No slow queries recorded in this window.", nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d slow query fingerprints, showing %d. Literal values are masked as ?.\n\n",
		len(o.Queries), min(ile, len(o.Queries)))
	for i, q := range o.Queries {
		if i >= ile {
			break
		}
		fmt.Fprintf(&b, "%2d. avg %.1f ms · max %.1f ms · %d calls", i+1, q.AvgMs, q.MaxMs, q.Count)
		if q.Caller != "" {
			fmt.Fprintf(&b, " · from %s", q.Caller)
		}
		fmt.Fprintf(&b, "\n    %s\n", przytnij(jednaLinia(q.SQL), 260))
	}
	return b.String(), nil
}

// tabelaKomponentow skraca /components do listy, którą da się przeczytać.
func tabelaKomponentow(surowe json.RawMessage, ile int) (string, error) {
	var o struct {
		Components []struct {
			Name      string  `json:"name"`
			SelfMs    float64 `json:"self_ms"`
			InclMs    float64 `json:"incl_ms"`
			Calls     int64   `json:"calls"`
			Profiled  int     `json:"profiled"`
			AvgSelfMs float64 `json:"avg_self_ms"`
		} `json:"components"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Components) == 0 {
		return "No component data in this window. The breakdown only exists for profiled requests; " +
			"use phpray_profile_url to turn profiling on for a URL prefix.", nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d components, showing %d, ordered as the console returns them. "+
		"self = time in that component's own code, incl = including what it calls.\n\n",
		len(o.Components), min(ile, len(o.Components)))
	fmt.Fprintf(&b, "%-46s %11s %11s %13s %8s\n", "component", "self total", "incl total", "avg self/req", "profiled")
	for i, c := range o.Components {
		if i >= ile {
			break
		}
		fmt.Fprintf(&b, "%-46s %11s %11s %10.2f ms %8d\n",
			przytnij(c.Name, 46), czasSumy(c.SelfMs), czasSumy(c.InclMs), c.AvgSelfMs, c.Profiled)
	}
	return b.String(), nil
}

// czasSumy podaje sumę czasu w jednostce, którą da się przeczytać. Kolumna
// „self ms" z wartością 2528110 jest formalnie poprawna (suma milisekund po
// 30 tys. żądań), ale agent czytający to użytkownikowi powie „dwa i pół
// miliona milisekund" albo pomyli rząd wielkości. Liczba, która trafia do
// odpowiedzi modelu, musi być odporna na takie przeczytanie.
func czasSumy(ms float64) string {
	switch {
	case ms >= 3600000:
		return fmt.Sprintf("%.1f h", ms/3600000)
	case ms >= 60000:
		return fmt.Sprintf("%.1f min", ms/60000)
	case ms >= 1000:
		return fmt.Sprintf("%.1f s", ms/1000)
	default:
		return fmt.Sprintf("%.0f ms", ms)
	}
}

func jednaLinia(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}
