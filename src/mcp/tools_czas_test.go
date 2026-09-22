package main

import "testing"

// Sumy czasu trafiają wprost do odpowiedzi modelu, więc muszą być odporne na
// przeczytanie na głos. Kolumna „self ms" z wartością 2528110 jest formalnie
// poprawna, ale agent powie z niej użytkownikowi bzdurę albo pomyli rząd
// wielkości.
func TestSumyCzasuSaCzytelne(t *testing.T) {
	przypadki := []struct {
		ms   float64
		chce string
	}{
		{2528110, "42.1 min"},
		{4016688, "1.1 h"},
		{3500, "3.5 s"},
		{420, "420 ms"},
		{0, "0 ms"},
	}
	for _, p := range przypadki {
		if got := czasSumy(p.ms); got != p.chce {
			t.Errorf("czasSumy(%.0f) = %q, oczekiwano %q", p.ms, got, p.chce)
		}
	}
}
