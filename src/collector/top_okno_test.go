package main

import "testing"

// Okno pytania do API musi być szersze niż okno wyświetlania, bo kolektor
// kubełkuje po pełnych minutach: przy window=1 bieżąca, niepełna minuta wypada
// poza zakresem i domyślne „phpray top" pokazuje zero mimo napływającego ruchu.
func TestOknoZapytaniaJestSzerszeNizOknoWyswietlania(t *testing.T) {
	przypadki := []struct {
		sekundy   int64
		minMinuty int
	}{
		{60, 2}, // domyślne wywołanie — to ono było zepsute
		{30, 2},
		{300, 6},
		{3600, 61},
	}
	for _, p := range przypadki {
		got := int(p.sekundy/60) + 2
		if got < p.minMinuty {
			t.Errorf("dla -w %ds pytamy o %d min, za mało (min %d)", p.sekundy, got, p.minMinuty)
		}
		// Zapas nie może być tak duży, żeby zalewać kolektor.
		if got > int(p.sekundy/60)+3 {
			t.Errorf("dla -w %ds zapas %d min jest przesadny", p.sekundy, got)
		}
	}
}
