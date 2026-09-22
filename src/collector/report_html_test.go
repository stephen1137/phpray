package main

import (
	"strings"
	"testing"
)

func przykladowyRaport() *DiagReport {
	return &DiagReport{
		Domain: "sklep.example", WindowMin: 1440, TraceCount: 534, HealthScore: 27,
		Summary: "sklep.example: three rules tripped; the database is the dominant cost.",
		Findings: []DiagFinding{
			{Rule: "n1", Severity: SevCritical, Title: "N+1 Query Pattern Detected",
				Description: "261 of 289 requests repeat the same query shape.",
				Impact:      "Adds about 1.4 s to the median request.",
				Fix:         "Cache the lookup or batch it.",
				Evidence:    []string{"sklep.example/wp-json/wp-statistics/v2/hit"}},
			{Rule: "slow", Severity: SevWarning, Title: "Slow External API Calls"},
		},
	}
}

func TestRaportHTMLJestSamowystarczalny(t *testing.T) {
	h := GenerateHTMLReport(przykladowyRaport(), false, "0.15.3")
	if !strings.HasPrefix(h, "<!doctype html>") {
		t.Fatalf("raport musi być pełnym dokumentem")
	}
	// Żadnych zewnętrznych zasobów: plik ma działać z załącznika i bez sieci.
	for _, zle := range []string{"<script", "src=\"http", "href=\"http://", "@import", "<link"} {
		if strings.Contains(h, zle) {
			t.Fatalf("raport nie może zawierać %q — ma być jednym plikiem bez zasobów zewnętrznych", zle)
		}
	}
	for _, musi := range []string{"sklep.example", "Query Pattern Detected", "27",
		"Cache the lookup or batch it.", "PHPRay 0.15.3", "phpray.dev"} {
		if !strings.Contains(h, musi) {
			t.Fatalf("brak %q w raporcie", musi)
		}
	}
}

func TestRaportAnonimowyNieZdradzaKlienta(t *testing.T) {
	h := GenerateHTMLReport(przykladowyRaport(), true, "0.15.3")
	if strings.Contains(h, "sklep.example") {
		t.Fatalf("tryb anonimowy zostawił nazwę domeny — także w dowodach")
	}
	if !strings.Contains(h, "the site") {
		t.Fatalf("tryb anonimowy powinien podstawić neutralną nazwę")
	}
	// Reszta treści musi zostać, inaczej anonimizacja psuje raport.
	if !strings.Contains(h, "Query Pattern Detected") {
		t.Fatalf("tryb anonimowy zgubił ustalenia")
	}
}

func TestOknoSlownie(t *testing.T) {
	for wejscie, oczekiwane := range map[int]string{
		60: "the last hour", 120: "the last 2 hours",
		1440: "the last 24 hours", 10080: "the last 7 days", 45: "the last 45 minutes",
	} {
		if got := oknoSlownie(wejscie); got != oczekiwane {
			t.Fatalf("oknoSlownie(%d) = %q, chcę %q", wejscie, got, oczekiwane)
		}
	}
}
