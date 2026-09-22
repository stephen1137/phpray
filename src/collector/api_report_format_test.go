package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Koncowka /diagnostics/{domena}/report oddawala TYLKO markdown i tekst, mimo
// ze generator HTML istnial i uzywaly go CLI, wtyczka DirectAdmin i konsola
// Cloud. Z tej koncowki korzysta LOKALNY PANEL — pierwsza powierzchnia kazdego,
// kto zainstalowal PHPRaya sam. Jedyny artefakt do wyslania komus byl tam
// plikiem .md bez ani jednego odnosnika do nas, a caly sens raportu polega na
// tym, ze wedruje dalej.
//
// Test wola PRAWDZIWA TRASE przez router. Pierwsza wersja powtarzala switcha
// z handlera i przeszlaby nawet po jego zepsuciu — czyli nie chronila niczego.
func zadajRaport(t *testing.T, format string) *httptest.ResponseRecorder {
	t.Helper()
	store, err := OpenStorage(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	api := NewAPI(store, nil, "")

	adres := "/api/v1/diagnostics/sklep.example/report?window=60"
	if format != "" {
		adres += "&format=" + format
	}
	rr := httptest.NewRecorder()
	api.router.ServeHTTP(rr, httptest.NewRequest("GET", adres, nil))
	return rr
}

func TestKoncowkaRaportuOddajeHTML(t *testing.T) {
	for _, p := range []struct{ format, typ, rozsz string }{
		{"html", "text/html", ".html"},
		{"text", "text/plain", ".txt"},
		{"markdown", "text/markdown", ".md"},
		{"", "text/markdown", ".md"}, // brak parametru = markdown, jak dotad
	} {
		t.Run("format="+p.format, func(t *testing.T) {
			rr := zadajRaport(t, p.format)
			if rr.Code != 200 {
				t.Fatalf("kod %d, tresc: %s", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, p.typ) {
				t.Errorf("typ tresci %q, chcialem %q", ct, p.typ)
			}
			if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, p.rozsz) {
				t.Errorf("nazwa pliku %q, chcialem rozszerzenie %q", cd, p.rozsz)
			}
		})
	}
}

// To jest powod, dla ktorego HTML ma tu byc: plik niesie droge powrotna.
func TestRaportHTMLZKoncowkiNiesieDrogePowrotna(t *testing.T) {
	rr := zadajRaport(t, "html")
	h := rr.Body.String()
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "<!doctype html>") {
		t.Fatalf("to nie jest pelny dokument HTML: %.120s", h)
	}
	for _, musi := range []string{"utm_source=report", "phpray.dev"} {
		if !strings.Contains(h, musi) {
			t.Errorf("raport HTML musi niesc %q — inaczej nie domyka petli", musi)
		}
	}
}
