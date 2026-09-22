package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
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
	// Konsola czyta okno z ?from i ?to (sekundy uniksowe), NIE z ?window.
	//
	// Do 22.09.2026 wysylalismy tu ?window=<minuty>. Konsola ten parametr
	// ignorowala i brala swoje domyslne 24 godziny, wiec window_minutes —
	// ogloszone w schemacie KAZDEGO narzedzia — nie robilo nic. Widac to
	// bylo dopiero po porownaniu: okno 5 minut i okno 1440 minut oddawaly
	// identyczne liczby (6846 zadan, 21 adresow). Agent zapytany "czy cos
	// sie pogorszylo w ostatniej godzinie" porownywal dwa razy te sama dobe
	// i odpowiadal "nic sie nie zmienilo" — z pelnym przekonaniem.
	okno := func(args map[string]any, domyslne int) url.Values {
		minut := liczba(args, "window_minutes", domyslne)
		if minut <= 0 {
			minut = domyslne
		}
		teraz := time.Now().Unix()
		q := url.Values{}
		q.Set("from", fmt.Sprintf("%d", teraz-int64(minut)*60))
		q.Set("to", fmt.Sprintf("%d", teraz))
		return q
	}
	idSerwisu := func(args map[string]any) (string, error) {
		id := tekst(args, "site_id", "")
		if id == "" {
			return "", fmt.Errorf("site_id is required; call phpray_sites first to get one")
		}
		return id, nil
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
			Name: "phpray_errors",
			// Opis mowil "PHP errors and warnings ... with file, line". Koncowka
			// zwraca ZADANIA, ktore sie nie powiodly: te z bledem PHP maja plik
			// i linie, ale odpowiedzi 5xx bez bledu PHP nie maja ich wcale.
			// Na demie wszystkie sto wierszy mialo puste error_message, wiec
			// agent zobaczylby "bledy" bez tresci i opowiedzialby o nich
			// uzytkownikowi. Opis musi mowic, co naprawde przychodzi.
			Description: "Requests that failed on a site: PHP errors and warnings with file and line " +
				"where PHP raised one, plus 5xx responses that carry no PHP error. " +
				"Pass an id to phpray_trace to see what one of them did.",
			InputSchema: schemat(map[string]any{
				"site_id":        map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"window_minutes": map[string]any{"type": "integer", "description": "How far back to look. Default 1440."},
				"limit":          map[string]any{"type": "integer", "description": "How many to return. Default 20."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/errors", okno(args, 1440), nil)
				if err != nil {
					return "", err
				}
				return tabelaBledow(r, liczba(args, "limit", 20))
			},
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
				return tabelaSladow(r, liczba(args, "limit", 50))
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
				return kartaSladu(r)
			},
		},
		{
			Name: "phpray_compare",
			Description: "Two time ranges for one site side by side. Use it to check whether a deployment, " +
				"a plugin update or a configuration change made things better or worse.",
			InputSchema: schemat(map[string]any{
				"site_id": map[string]any{"type": "string", "description": "Site id from phpray_sites."},
				"from_a":  map[string]any{"type": "integer", "description": "Start of range A (the earlier one), unix seconds. Without any range: the day before yesterday's 24 h against the last 24 h."},
				"to_a":    map[string]any{"type": "integer", "description": "End of range A, unix seconds."},
				"from_b":  map[string]any{"type": "integer", "description": "Start of range B, unix seconds."},
				"to_b":    map[string]any{"type": "integer", "description": "End of range B, unix seconds."},
			}, "site_id"),
			call: func(args map[string]any) (string, error) {
				id, err := idSerwisu(args)
				if err != nil {
					return "", err
				}
				// Koncowka oczekuje a_from/a_to/b_from/b_to. Wysylalismy
				// from_a/to_a/from_b/to_b, wiec byly po cichu ignorowane:
				// obie polowy porownania wychodzily z tego samego domyslnego
				// okna i narzedzie ZAWSZE odpowiadalo "bez zmian". Sprawdzone
				// na zywo 22.09.2026: z wlasciwymi nazwami p95 561,3 wobec
				// 541,6 ms, z naszymi — 541,6 wobec 541,6.
				nazwy := map[string]string{"from_a": "a_from", "to_a": "a_to",
					"from_b": "b_from", "to_b": "b_to"}
				q := url.Values{}
				for nasz, ichni := range nazwy {
					if v := liczba(args, nasz, 0); v > 0 {
						q.Set(ichni, fmt.Sprintf("%d", v))
					}
				}
				// Bez zakresow porownanie samego siebie ze soba nie odpowiada
				// na zadne pytanie. Domyslnie: poprzednia doba kontra ostatnia.
				if len(q) == 0 {
					teraz := time.Now().Unix()
					q.Set("a_from", fmt.Sprintf("%d", teraz-2*86400))
					q.Set("a_to", fmt.Sprintf("%d", teraz-86400))
					q.Set("b_from", fmt.Sprintf("%d", teraz-86400))
					q.Set("b_to", fmt.Sprintf("%d", teraz))
				}
				r, err := k.zapytaj("GET", "/console/v1/sites/"+id+"/compare", q, nil)
				if err != nil {
					return "", err
				}
				return tabelaPorownania(r)
			},
		},
		{
			Name:        "phpray_alerts",
			Description: "Alerts currently firing across every site this token can see, worst overrun first.",
			InputSchema: schemat(map[string]any{}),
			call: func(args map[string]any) (string, error) {
				r, err := k.zapytaj("GET", "/console/v1/alerts", nil, nil)
				if err != nil {
					return "", err
				}
				return tabelaAlertow(r)
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
		// Naglowek mowi "Slowest URLs", wiec lista MUSI byc posortowana po p95.
		// Na demie szla w kolejnosci z API i /zamowienie/ (875 ms) stalo nad /
		// (2357 ms) — agent czyta pierwszy wiersz jako odpowiedz na "ktora
		// strona jest wolna" i podalby bledna.
		sort.SliceStable(o.TopURIs, func(i, j int) bool {
			return o.TopURIs[i].P95Ms > o.TopURIs[j].P95Ms
		})
		fmt.Fprintf(&b, "\nSlowest URLs (%d of %d, slowest first by p95):\n%-56s %8s %9s\n", min(ileURI, len(o.TopURIs)), len(o.TopURIs), "url", "requests", "p95 ms")
		for i, u := range o.TopURIs {
			if i >= ileURI {
				break
			}
			fmt.Fprintf(&b, "%-56s %8d %9.1f\n", przytnij(u.URI, 56), u.Requests, u.P95Ms)
		}
	}
	if ileKomponentow > 0 && len(o.Components) > 0 {
		// Czasy przez czasSumy, tak samo jak w phpray_components.
		//
		// Do 22.09.2026 stalo tu %11.0f na surowym SelfMs i agent dostawal
		// "2536828" bez jednostki. To sa milisekundy, czyli czterdziesci dwie
		// minuty — a dedykowane narzedzie te sama liczbe pokazuje jako
		// "42.3 min". Dwa nasze narzedzia podawaly ten sam fakt w dwoch
		// formatach, z ktorych jeden byl nieczytelny: model, ktory przeczyta
		// "2536828 self total", poda uzytkownikowi dwa i pol miliona czegos.
		fmt.Fprintf(&b, "\nWhere the time goes (%d of %d components, profiled requests only):\n%-44s %11s %11s %13s\n",
			min(ileKomponentow, len(o.Components)), len(o.Components), "component", "self total", "incl total", "avg self/req")
		for i, c := range o.Components {
			if i >= ileKomponentow {
				break
			}
			fmt.Fprintf(&b, "%-44s %11s %11s %10.2f ms\n",
				przytnij(c.Name, 44), czasSumy(c.SelfMs), czasSumy(c.InclMs), c.AvgSelfMs)
		}
		fmt.Fprintf(&b, "\nphpray_components has the full list with how many requests were profiled.\n")
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
	// Sortujemy po LACZNYM czasie (avg x liczba wywolan) i pokazujemy go jako
	// pierwsza liczbe. Zapytanie trwajace 0,3 ms, ale wywolane piec tysiecy
	// razy, kosztuje bazę wiecej niz jednorazowe 6 ms — a lista szla wczesniej
	// w kolejnosci z API i zaczynala sie od odcisku wartego 0,13 s, podczas gdy
	// najdrozszy zjadal 3,6 s. Kolumny z czasem lacznym w ogole nie bylo, wiec
	// agent nie mial jak tego zauwazyc.
	sort.SliceStable(o.Queries, func(i, j int) bool {
		return o.Queries[i].AvgMs*float64(o.Queries[i].Count) >
			o.Queries[j].AvgMs*float64(o.Queries[j].Count)
	})
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d slow query fingerprints, showing %d, ordered by total time in the window "+
		"(avg x calls) — that is what costs the database. Literal values are masked as ?.\n\n",
		len(o.Queries), min(ile, len(o.Queries)))
	for i, q := range o.Queries {
		if i >= ile {
			break
		}
		fmt.Fprintf(&b, "%2d. total %s · avg %.1f ms · max %.1f ms · %d calls",
			i+1, czasSumy(q.AvgMs*float64(q.Count)), q.AvgMs, q.MaxMs, q.Count)
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
	// Sortujemy po SELF malejaco, bo to jest odpowiedz na pytanie "ktora
	// wtyczka spowalnia strone". Wczesniej szla kolejnosc z konsoli (po incl)
	// i pierwszy wiersz NIE byl najdrozszy: na demie WooCommerce stalo nad
	// Wordfence'em, majac 41,9 min wlasnego czasu wobec jego 49,0 min. Agent
	// czyta pierwszy wiersz jako odpowiedz, wiec kolejnosc jest trescia, nie
	// ozdoba. `incl` sumuje tez to, co komponent wywoluje, wiec podwaja czas
	// przy zagniezdzeniu — do wskazania winnego sluzy `self`.
	sort.SliceStable(o.Components, func(i, j int) bool {
		return o.Components[i].SelfMs > o.Components[j].SelfMs
	})
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d components, showing %d, ordered by self time (most expensive first). "+
		"self = time in that component's own code, incl = including what it calls; "+
		"self is the one that says which component to look at.\n\n",
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

// zadanie to jeden wiersz z /traces i /errors — te same pola.
type zadanie struct {
	ID         int64   `json:"id"`
	TS         string  `json:"ts"`
	URI        string  `json:"uri"`
	Status     int     `json:"status"`
	DurationMs float64 `json:"duration_ms"`
	DBCount    int     `json:"db_count"`
	N1         bool    `json:"n1"`
	Profiled   bool    `json:"profiled"`
	Level      string  `json:"level"`
	ErrFile    string  `json:"error_file"`
	ErrLine    int     `json:"error_line"`
	ErrMessage string  `json:"error_message"`
}

// wierszZadania pisze jedno żądanie w jednej linii. `id` jest pierwszy, bo to
// jedyna rzecz, którą agent musi przepisać do phpray_trace.
func wierszZadania(b *bytes.Buffer, z zadanie) {
	n1 := ""
	if z.N1 {
		n1 = " N+1"
	}
	fmt.Fprintf(b, "%-9d %7.0f ms  %3d %5d db%-4s %s\n",
		z.ID, z.DurationMs, z.Status, z.DBCount, n1, przytnij(z.URI, 48))
}

// tabelaSladow zastępuje surowy JSON: przy domyślnym limicie 50 miał 12,8 kB,
// czyli kilka tysięcy tokenów, i szedł w kolejności zwracania przez API —
// pierwszy wiersz nie był najwolniejszym żądaniem.
func tabelaSladow(surowe json.RawMessage, ile int) (string, error) {
	var o struct {
		Traces []zadanie `json:"traces"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Traces) == 0 {
		return "No requests recorded in this window.", nil
	}
	sort.SliceStable(o.Traces, func(i, j int) bool {
		return o.Traces[i].DurationMs > o.Traces[j].DurationMs
	})
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d requests, showing %d, slowest first. Pass an id to phpray_trace.\n\n",
		len(o.Traces), min(ile, len(o.Traces)))
	fmt.Fprintf(&b, "%-9s %10s  %3s %5s     %s\n", "id", "duration", "st", "db", "url")
	for i, z := range o.Traces {
		if i >= ile {
			break
		}
		wierszZadania(&b, z)
	}
	return b.String(), nil
}

// tabelaBledow pokazuje nieudane żądania i MÓWI WPROST, gdy żadne z nich nie
// niesie błędu PHP. Bez tego agent dostawał sto wierszy z pustym
// error_message i opowiadał użytkownikowi o „błędach”, których treści nie ma.
func tabelaBledow(surowe json.RawMessage, ile int) (string, error) {
	var o struct {
		Errors []zadanie `json:"errors"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Errors) == 0 {
		return "No failed requests in this window: no PHP errors and no 5xx responses.", nil
	}
	zBledemPHP := 0
	for _, z := range o.Errors {
		if z.ErrMessage != "" {
			zBledemPHP++
		}
	}
	sort.SliceStable(o.Errors, func(i, j int) bool {
		if (o.Errors[i].ErrMessage != "") != (o.Errors[j].ErrMessage != "") {
			return o.Errors[i].ErrMessage != ""
		}
		return o.Errors[i].DurationMs > o.Errors[j].DurationMs
	})
	var b bytes.Buffer
	if zBledemPHP == 0 {
		fmt.Fprintf(&b, "%d failed requests, showing %d. NONE of them raised a PHP error — "+
			"these are 5xx responses, so the cause is outside PHP's error handler "+
			"(a fatal before the handler, a timeout, the web server, or an upstream). "+
			"Open one with phpray_trace to see the queries and outbound calls it made.\n\n",
			len(o.Errors), min(ile, len(o.Errors)))
	} else {
		fmt.Fprintf(&b, "%d failed requests, showing %d; %d of them raised a PHP error "+
			"(those come first, with file and line).\n\n",
			len(o.Errors), min(ile, len(o.Errors)), zBledemPHP)
	}
	fmt.Fprintf(&b, "%-9s %10s  %3s %5s     %s\n", "id", "duration", "st", "db", "url")
	for i, z := range o.Errors {
		if i >= ile {
			break
		}
		wierszZadania(&b, z)
		if z.ErrMessage != "" {
			fmt.Fprintf(&b, "          %s — %s:%d\n",
				przytnij(jednaLinia(z.ErrMessage), 120), przytnij(z.ErrFile, 60), z.ErrLine)
		}
	}
	return b.String(), nil
}

// tabelaPorownania odpowiada na pytanie „czy po zmianie jest lepiej”, zamiast
// oddawać 4,6 kB JSON-a, z którego agent musi sam policzyć różnice.
func tabelaPorownania(surowe json.RawMessage) (string, error) {
	type okres struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	type staty struct {
		Requests  int64   `json:"requests"`
		AvgMs     float64 `json:"avg_ms"`
		P95Ms     float64 `json:"p95_ms"`
		ErrorRate float64 `json:"error_rate"`
		Errors5xx int64   `json:"errors_5xx"`
		ErrorsPHP int64   `json:"errors_php"`
		DBShare   float64 `json:"db_ms_share"`
	}
	var o struct {
		A  okres `json:"a"`
		B  okres `json:"b"`
		AS staty `json:"a_stats"`
		BS staty `json:"b_stats"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "A (before): %s → %s\nB (after):  %s → %s\n\n", o.A.From, o.A.To, o.B.From, o.B.To)
	fmt.Fprintf(&b, "%-14s %12s %12s %12s\n", "", "A", "B", "change")
	wiersz := func(nazwa string, a, bb float64, jed string, mniejLepiej bool) {
		zmiana := "—"
		if a != 0 {
			p := (bb - a) / a * 100
			znak := ""
			if p > 0 {
				znak = "+"
			}
			ocena := ""
			// Mowimy WPROST, czy zmiana jest dobra: agent nie musi wiedziec,
			// ze przy p95 mniej znaczy lepiej, a przy liczbie zadan nie.
			if mniejLepiej && p <= -5 {
				ocena = " better"
			} else if mniejLepiej && p >= 5 {
				ocena = " worse"
			}
			zmiana = fmt.Sprintf("%s%.1f%%%s", znak, p, ocena)
		}
		fmt.Fprintf(&b, "%-14s %12s %12s %12s\n", nazwa,
			fmt.Sprintf("%.1f%s", a, jed), fmt.Sprintf("%.1f%s", bb, jed), zmiana)
	}
	fmt.Fprintf(&b, "%-14s %12d %12d %12s\n", "requests", o.AS.Requests, o.BS.Requests, "—")
	wiersz("p95", o.AS.P95Ms, o.BS.P95Ms, " ms", true)
	wiersz("avg", o.AS.AvgMs, o.BS.AvgMs, " ms", true)
	wiersz("error rate", o.AS.ErrorRate*100, o.BS.ErrorRate*100, "%", true)
	wiersz("database", o.AS.DBShare*100, o.BS.DBShare*100, "%", true)
	fmt.Fprintf(&b, "%-14s %12d %12d\n", "5xx", o.AS.Errors5xx, o.BS.Errors5xx)
	fmt.Fprintf(&b, "%-14s %12d %12d\n", "PHP errors", o.AS.ErrorsPHP, o.BS.ErrorsPHP)
	fmt.Fprintf(&b, "\n\"better\"/\"worse\" is marked from 5%% change upward; below that treat it as noise.\n")
	return b.String(), nil
}

// tabelaAlertow: alerty po przekroczeniu progu, najgorszy pierwszy. Surowy
// JSON zmuszał agenta do samodzielnego dzielenia value przez threshold, żeby
// wiedzieć, który alert jest poważny — a przy sześciu regułach o różnych
// jednostkach (ms, procenty, sztuki) to jedyny sposób, by je porównać.
func tabelaAlertow(surowe json.RawMessage) (string, error) {
	var o struct {
		Alerts []struct {
			Host      string  `json:"host"`
			SiteID    string  `json:"site_id"`
			Rule      string  `json:"rule"`
			Value     float64 `json:"value"`
			Threshold float64 `json:"threshold"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	if len(o.Alerts) == 0 {
		return "No alerts firing on any site this token can see.", nil
	}
	krotnosc := func(i int) float64 {
		if o.Alerts[i].Threshold == 0 {
			return 0
		}
		return o.Alerts[i].Value / o.Alerts[i].Threshold
	}
	sort.SliceStable(o.Alerts, func(i, j int) bool { return krotnosc(i) > krotnosc(j) })
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d alerts firing, worst overrun first "+
		"(over = how many times the value exceeds its threshold).\n\n", len(o.Alerts))
	fmt.Fprintf(&b, "%-24s %-18s %12s %12s %8s\n", "site", "rule", "value", "threshold", "over")
	for i, a := range o.Alerts {
		// error_rate to ulamek (0,061 wobec progu 0,05). Przy %.0f oba
		// wychodzily jako "0", a obok stalo "1,2x" — wiersz sam sobie
		// przeczyl. Format dobieramy do wielkosci liczby.
		fmt.Fprintf(&b, "%-24s %-18s %12s %12s %7.1fx\n",
			przytnij(a.Host, 24), przytnij(a.Rule, 18),
			liczbaAlertu(a.Value), liczbaAlertu(a.Threshold), krotnosc(i))
	}
	fmt.Fprintf(&b, "\nPass a site_id to phpray_overview or phpray_traces to see what is behind one.\n")
	return b.String(), nil
}

// kartaSladu odpowiada na „dlaczego to żądanie było wolne”: najpierw podział
// czasu, potem najdroższe zapytania i komponenty. Surowy JSON miał 6,1 kB,
// z czego większość to pola techniczne (pid, uid, rid, docroot), których agent
// i tak nie użyje w odpowiedzi dla człowieka.
func kartaSladu(surowe json.RawMessage) (string, error) {
	var o struct {
		ID         int64   `json:"id"`
		TS         string  `json:"ts"`
		URI        string  `json:"uri"`
		Status     int     `json:"status"`
		DurationMs float64 `json:"duration_ms"`
		Record     struct {
			Method       string  `json:"method"`
			Host         string  `json:"host"`
			PHPVer       string  `json:"php_ver"`
			DurationMs   float64 `json:"duration_ms"`
			DBMs         float64 `json:"db_ms"`
			DBCount      int     `json:"db_count"`
			FileMs       float64 `json:"file_ms"`
			CPUUserMs    float64 `json:"cpu_user_ms"`
			CPUSysMs     float64 `json:"cpu_sys_ms"`
			MemoryPeakMB float64 `json:"memory_peak_mb"`
			// n1 i profiled przychodza jako 0/1, nie jako true/false —
			// zadeklarowanie ich jako bool wywalalo cale json.Unmarshal,
			// a zejscie awaryjne po cichu oddawalo surowy JSON i ukrywalo blad.
			N1       int `json:"n1"`
			Profiled int `json:"profiled"`
			Queries  []struct {
				Ms  float64 `json:"ms"`
				SQL string  `json:"sql"`
			} `json:"queries"`
			Components []struct {
				Name string  `json:"name"`
				Ms   float64 `json:"ms"`
				Pct  float64 `json:"pct"`
			} `json:"components"`
			Errors []struct {
				Message string `json:"message"`
				File    string `json:"file"`
				Line    int    `json:"line"`
			} `json:"errors"`
			HTTP []struct {
				Ms  float64 `json:"ms"`
				URL string  `json:"url"`
			} `json:"http"`
		} `json:"record"`
	}
	if err := json.Unmarshal(surowe, &o); err != nil {
		return ladnie(surowe), nil
	}
	r := o.Record
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s → %d, %.0f ms, %s (PHP %s)\n\n",
		r.Method, o.URI, o.Status, o.DurationMs, o.TS, r.PHPVer)
	inne := r.DurationMs - r.DBMs
	fmt.Fprintf(&b, "where the time went: database %.0f ms in %d queries · everything else %.0f ms\n",
		r.DBMs, r.DBCount, inne)
	fmt.Fprintf(&b, "cpu %.0f ms user + %.0f ms sys · peak memory %.1f MB · files %.0f ms",
		r.CPUUserMs, r.CPUSysMs, r.MemoryPeakMB, r.FileMs)
	if r.N1 != 0 {
		fmt.Fprint(&b, " · N+1 pattern detected")
	}
	if r.Profiled == 0 {
		fmt.Fprint(&b, " · not profiled, so no component breakdown")
	}
	fmt.Fprint(&b, "\n")

	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "\nerrors (%d):\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "  %s — %s:%d\n", przytnij(jednaLinia(e.Message), 140), przytnij(e.File, 60), e.Line)
		}
	}
	if len(r.HTTP) > 0 {
		sort.SliceStable(r.HTTP, func(i, j int) bool { return r.HTTP[i].Ms > r.HTTP[j].Ms })
		fmt.Fprintf(&b, "\noutbound HTTP (%d, slowest first):\n", len(r.HTTP))
		for i, h := range r.HTTP {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "  %7.1f ms  %s\n", h.Ms, przytnij(h.URL, 90))
		}
	}
	if len(r.Components) > 0 {
		sort.SliceStable(r.Components, func(i, j int) bool { return r.Components[i].Ms > r.Components[j].Ms })
		fmt.Fprintf(&b, "\ncomponents (%d, most expensive first):\n", len(r.Components))
		for i, c := range r.Components {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&b, "  %7.1f ms %5.1f%%  %s\n", c.Ms, c.Pct, przytnij(c.Name, 60))
		}
	}
	if len(r.Queries) > 0 {
		sort.SliceStable(r.Queries, func(i, j int) bool { return r.Queries[i].Ms > r.Queries[j].Ms })
		fmt.Fprintf(&b, "\nslowest queries (%d recorded, showing %d):\n", len(r.Queries), min(8, len(r.Queries)))
		for i, q := range r.Queries {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&b, "  %7.1f ms  %s\n", q.Ms, przytnij(jednaLinia(q.SQL), 150))
		}
	}
	return b.String(), nil
}

// liczbaAlertu pisze wartość progu tak, żeby była widoczna niezależnie od
// jednostki: liczby żądań idą bez części ułamkowej, wskaźniki (error_rate)
// z trzema miejscami, bo inaczej 0,061 i 0,05 wyglądają identycznie jak "0".
func liczbaAlertu(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 1:
		return fmt.Sprintf("%.3f", v)
	case v < 100:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}
