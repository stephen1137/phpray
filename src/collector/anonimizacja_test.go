package main

import "testing"

// Test pilnuje obu ksztaltow, w jakich nazwa strony wchodzi do raportu:
// jako podmiot zdania i jako poczatek adresu.
func TestBezNazwyStrony(t *testing.T) {
	przypadki := []struct{ tekst, domena, chce string }{
		{"sklep.pl: 2 warning(s) found", "sklep.pl", "the site: 2 warning(s) found"},
		{"sklep.pl/checkout/: 104 N+1 requests", "sklep.pl", "/checkout/: 104 N+1 requests"},
		{"https://sklep.pl/cart/ 842 ms", "sklep.pl", "/cart/ 842 ms"},
		{"www.sklep.pl/checkout/", "sklep.pl", "/checkout/"},
		{"sklep.pl/checkout/ on sklep.pl", "sklep.pl", "/checkout/ on the site"},
		{"www.sklep.pl slow", "sklep.pl", "the site slow"},
		{"nothing to strip", "sklep.pl", "nothing to strip"},
		{"sklep.pl", "", "sklep.pl"},
	}
	for _, p := range przypadki {
		if got := BezNazwyStrony(p.tekst, p.domena); got != p.chce {
			t.Errorf("BezNazwyStrony(%q, %q) = %q, chce %q", p.tekst, p.domena, got, p.chce)
		}
	}
}
