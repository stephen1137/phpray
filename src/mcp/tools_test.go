package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Dwa nasze narzedzia podawaly ten sam fakt w dwoch formatach, z ktorych
// jeden byl nieczytelny: phpray_components pokazywalo "42.3 min", a
// phpray_overview te sama liczbe jako gole "2536828". To sa milisekundy,
// ale w tabeli nie ma jednostki — model, ktory to przeczyta, poda
// uzytkownikowi dwa i pol miliona czegos.
func TestPrzegladPodajeCzasySkladnikowWJednostkach(t *testing.T) {
	surowe := []byte(`{
      "site": {"host":"sklep.example","app":"wordpress"},
      "server_name": "web-01",
      "last_seen_at": "2026-09-22T09:00:00Z",
      "stats": {"requests":30730,"p95_ms":541,"avg_ms":426,"error_rate":0.029,
                "errors_5xx":898,"errors_php":0,"db_ms_share":0.10,"profiled":30730},
      "components": [
        {"name":"plugins/woocommerce","self_ms":2536828,"incl_ms":4037457,"avg_self_ms":82.6,"profiled":30730}
      ]
    }`)
	out, err := podsumowaniePrzegladu(surowe, 12, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "2536828") {
		t.Errorf("surowe milisekundy w tabeli skladnikow:\n%s", out)
	}
	if !strings.Contains(out, "42.3 min") {
		t.Errorf("brak czasu w czytelnej jednostce, jest:\n%s", out)
	}
	// I odnosnik do narzedzia, ktore ma pelna liste.
	if !strings.Contains(out, "phpray_components") {
		t.Errorf("przeglad nie kieruje do phpray_components:\n%s", out)
	}
}

// window_minutes bylo ogloszone w schemacie KAZDEGO narzedzia i nie robilo
// nic: wysylalismy ?window=<minuty>, a konsola czyta ?from i ?to (sekundy
// uniksowe) i bez nich bierze swoje domyslne 24 godziny. Okno 5 minut
// i okno 1440 minut oddawaly identyczne liczby.
func TestOknoIdzieJakoFromITo(t *testing.T) {
	var widziane []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		widziane = append(widziane, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"site":{"host":"x"},"stats":{"requests":1}}`))
	}))
	defer srv.Close()

	narzedzia := zbudujNarzedzia(nowyKlient(srv.URL, "t"))
	var przeglad *toolSpec
	for i := range narzedzia {
		if narzedzia[i].Name == "phpray_overview" {
			przeglad = &narzedzia[i]
		}
	}
	if przeglad == nil {
		t.Fatal("brak phpray_overview")
	}

	if _, err := przeglad.call(map[string]any{"site_id": "s1", "window_minutes": 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := przeglad.call(map[string]any{"site_id": "s1", "window_minutes": 1440}); err != nil {
		t.Fatal(err)
	}
	if len(widziane) != 2 {
		t.Fatalf("oczekiwalem dwoch zapytan, jest %d", len(widziane))
	}
	for _, q := range widziane {
		if strings.Contains(q, "window=") {
			t.Errorf("konsola nie zna parametru window, a wyslalismy: %s", q)
		}
		if !strings.Contains(q, "from=") || !strings.Contains(q, "to=") {
			t.Errorf("brak from/to w zapytaniu: %s", q)
		}
	}
	// I najwazniejsze: dwa rozne okna MUSZA dac rozne zapytania.
	if widziane[0] == widziane[1] {
		t.Errorf("okno 5 min i 1440 min daly TO SAMO zapytanie: %s", widziane[0])
	}
}
