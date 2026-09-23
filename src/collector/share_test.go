package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Opublikowany raport NIE MOZE zawierac nazwy strony w ZADNYM polu.
//
// 22.09.2026 pierwsza wersja usuwala domene z pola "domain" (ktorego
// w kontrakcie nawet nie ma) i opublikowala na produkcji dokument
// z "sklep-tvsat.com: 1 critical issue(s) found" w podsumowaniu. Nazwa
// przeciekla TRESCIA, nie polem. Ten test chodzi po kazdym polu tekstowym.
func TestPublikacjaWycinaNazweStronyZeWszystkichPol(t *testing.T) {
	const domena = "sklep-tvsat.com"
	report := &DiagReport{
		Domain:      domena,
		WindowMin:   120,
		TraceCount:  1000,
		HealthScore: 65,
		Summary:     domena + ": 1 critical issue(s) found in 1000 requests.",
		Findings: []DiagFinding{{
			Rule:        "errors_5xx",
			Severity:    SevCritical,
			Title:       "Requests on " + domena + " are failing",
			Description: "The shop at www." + domena + " returned 500.",
			Impact:      "Visitors of " + domena + " saw an error.",
			Fix:         "Check " + domena + " logs.",
			Evidence:    []string{"HTTP 500 — https://" + domena + "/sklep/", "HTTP 503 — www." + domena},
		}},
	}

	var odebrane []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		odebrane, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://app.phpray.dev/r/xyz","expires_at":"2026-10-22T00:00:00Z"}`))
	}))
	defer srv.Close()

	url, _, err := opublikujRaport(report, srv.URL)
	if err != nil {
		t.Fatalf("publikacja: %v", err)
	}
	if url == "" {
		t.Fatal("brak adresu")
	}
	tresc := string(odebrane)
	if strings.Contains(tresc, domena) {
		// Pokazujemy GDZIE, zeby nastepnym razem nie trzeba bylo szukac.
		var l map[string]any
		_ = json.Unmarshal(odebrane, &l)
		t.Errorf("WYCIEK: nazwa strony %q trafila do ladunku publikacji.\nCale ciało: %s", domena, tresc)
	}
	// Tresc ma zostac, tylko bez nazwy — "bezpieczny" nie moze znaczyc "pusty".
	if !strings.Contains(tresc, "the site") {
		t.Error("nazwa nie zostala zastapiona — ladunek wyglada na wyczyszczony do zera")
	}
	if !strings.Contains(tresc, "critical") || !strings.Contains(tresc, "1000") {
		t.Error("z ladunku zniknely dane merytoryczne")
	}
}
