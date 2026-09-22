package main

import "testing"

func TestZapelnienie(t *testing.T) {
	const cap0 = 1000
	przypadki := []struct {
		nazwa    string
		wp, rp   uint64
		capacity uint64
		chce     float64
	}{
		{"pusty", 100, 100, cap0, 0},
		{"polowa", 600, 100, cap0, 50},
		{"pelny", 1100, 100, cap0, 100},
		// To jest ta wpadka: na h2 dziennik pisal 109952421083179.7% full,
		// bo wp-rp na uint64 przewinelo sie pod zero.
		{"czytelnik wyprzedzil zapis", 100, 101, cap0, 0},
		{"czytelnik daleko przed", 0, 1 << 40, cap0, 0},
		{"wiecej niz pojemnosc — obcinamy do 100", 9000, 100, cap0, 100},
		{"zerowa pojemnosc nie dzieli przez zero", 500, 100, 0, 0},
	}
	for _, p := range przypadki {
		if got := zapelnienie(p.wp, p.rp, p.capacity); got != p.chce {
			t.Errorf("%s: zapelnienie(%d,%d,%d) = %v, chce %v",
				p.nazwa, p.wp, p.rp, p.capacity, got, p.chce)
		}
	}
}

// Wynik MUSI byc procentem, ktory da sie pokazac czlowiekowi.
func TestZapelnienieZawszeWZakresie(t *testing.T) {
	wartosci := []uint64{0, 1, 999, 1000, 1001, 1 << 32, 1<<64 - 1}
	for _, wp := range wartosci {
		for _, rp := range wartosci {
			g := zapelnienie(wp, rp, 4096)
			if g < 0 || g > 100 {
				t.Errorf("zapelnienie(%d,%d,4096) = %v — poza zakresem 0..100", wp, rp, g)
			}
		}
	}
}
