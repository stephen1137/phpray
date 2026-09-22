package main

import (
	"regexp"
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

// Raport z kolektora jest tym, ktory widza klienci IQhostu przez wtyczke
// DirectAdmin i kazdy, kto uruchomi `phpray report` u siebie. To nasz jedyny
// artefakt krazacy poza nami, wiec musi niesc znacznik zrodla — inaczej nie
// wiemy, czy przekazany raport kogokolwiek przyprowadza. 22.09.2026 konsola
// dostala ten znacznik, a kolektor nie: dwa szablony rozjechaly sie po cichu.
func TestRaportNiesieZnacznikZrodla(t *testing.T) {
	html := GenerateHTMLReport(przykladowyRaport(), false, "0.15.6")
	// KAZDY odnosnik do phpray.dev musi niesc znacznik, nie "co najmniej jeden".
	// Pierwsza wersja tego testu sprawdzala samo wystapienie ciagu i przeszla
	// nawet po tym, jak recznie usunalem znacznik z pierwszego odnosnika —
	// bo zostal w drugim. Bramka, ktora nie pada przy zepsutym wejsciu,
	// niczego nie pilnuje.
	odnosniki := regexp.MustCompile(`href="(https://phpray\.dev[^"]*)"`).FindAllStringSubmatch(html, -1)
	if len(odnosniki) < 2 {
		t.Fatalf("w stopce raportu ma byc adres strony i adres instalacji, jest %d", len(odnosniki))
	}
	for _, o := range odnosniki {
		if !strings.Contains(o[1], "utm_source=report") {
			t.Errorf("odnosnik bez znacznika zrodla: %s", o[1])
		}
	}
	if !strings.Contains(html, "to run it yourself") {
		t.Error("w stopce brakuje zdania kierujacego odbiorce do instalacji")
	}
}
