package main

// Usuwanie nazwy strony z TREŚCI raportu.
//
// Wydzielone 22.09.2026, po tym jak napisałem drugą ścieżkę (publikacja
// linkiem) i powtórzyłem w niej błąd, który był opisany komentarzem trzy
// linie obok, w pierwszej ścieżce. Usunąłem domenę z POLA ładunku i uznałem
// sprawę za załatwioną — a nazwa przeciekła TREŚCIĄ: kolektor pisze w
// podsumowaniu „sklep-tvsat.com: 1 critical issue(s) found". Opublikowany
// dokument zdradzał klienta w pierwszym zdaniu.
//
// Wiedza zamknięta w domknięciu wewnątrz funkcji nie dziedziczy się do
// następnej ścieżki. Tutaj jest funkcją pakietu, żeby nie dało się jej
// pominąć przez przeoczenie — można ją tylko świadomie nie wywołać.

import (
	"sort"
	"strings"
)

// BezNazwyStrony zastępuje domenę w dowolnym tekście raportu.
//
// Obejmuje też postać z "www." i bez, bo podsumowania i dowody cytują raz
// jedną, raz drugą.
func BezNazwyStrony(tekst, domena string) string {
	if domena == "" || tekst == "" {
		return tekst
	}
	warianty := []string{domena}
	if strings.HasPrefix(domena, "www.") {
		warianty = append(warianty, strings.TrimPrefix(domena, "www."))
	} else {
		warianty = append(warianty, "www."+domena)
	}
	// Najdluzszy wariant pierwszy: inaczej "www.sklep.pl/koszyk" zamienia sie
	// w "www./koszyk", bo krotszy wariant trafia w srodek dluzszego.
	sort.Slice(warianty, func(i, j int) bool { return len(warianty[i]) > len(warianty[j]) })

	out := tekst
	// Nazwa ze sciezka to adres, nie zdanie. "sklep.pl/checkout/" ma zostac
	// "/checkout/", a nie "the site/checkout/" — opublikowany raport czyta
	// obcy czlowiek i taki zlepek wyglada na blad narzedzia. Znalezione
	// 23.09.2026 na zywym raporcie, nie w tescie.
	for _, w := range warianty {
		for _, s := range []string{"https://", "http://", "//", ""} {
			out = strings.ReplaceAll(out, s+w+"/", "/")
		}
	}
	for _, w := range warianty {
		out = strings.ReplaceAll(out, w, "the site")
	}
	return out
}
