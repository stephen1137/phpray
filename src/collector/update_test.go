package main

import "testing"

func TestNowszy(t *testing.T) {
	przypadki := []struct {
		tag, obecna string
		chce        bool
		blad        bool
	}{
		{"v0.15.7", "0.15.6", true, false},
		{"v0.15.6", "0.15.6", false, false},
		{"v0.15.5", "0.15.6", false, false},
		{"v0.16.0", "0.15.99", true, false},
		{"v1.0.0", "0.99.99", true, false},
		// Porownanie MUSI byc liczbowe, nie tekstowe: "0.15.10" < "0.15.9"
		// leksykograficznie, wiec wersja tekstowa przegapilaby aktualizacje.
		{"v0.15.10", "0.15.9", true, false},
		{"v0.9.0", "0.10.0", false, false},
		// Wersja wbudowana bywa z przyrostkiem przy budowaniu z gita.
		{"v0.15.7", "0.15.6-dirty", true, false},
		{"0.15.7", "0.15.6", false, true}, // brak "v"
		{"v0.15", "0.15.6", false, true},  // trzy czlony wymagane
		{"smieci", "0.15.6", false, true},
	}
	for _, p := range przypadki {
		got, err := nowszy(p.tag, p.obecna)
		if p.blad {
			if err == nil {
				t.Errorf("nowszy(%q,%q): oczekiwano bledu, jest nil", p.tag, p.obecna)
			}
			continue
		}
		if err != nil {
			t.Errorf("nowszy(%q,%q): nieoczekiwany blad %v", p.tag, p.obecna, err)
			continue
		}
		if got != p.chce {
			t.Errorf("nowszy(%q,%q) = %v, chce %v", p.tag, p.obecna, got, p.chce)
		}
	}
}

func TestSumaZManifestu(t *testing.T) {
	m := "aaa  phpray-collector-linux-amd64\nbbb  phpray-collector-linux-arm64\nccc  SHA256SUMS\n"
	if s, err := sumaZManifestu(m, "phpray-collector-linux-arm64"); err != nil || s != "bbb" {
		t.Errorf("arm64: %q %v", s, err)
	}
	// Dopasowanie MUSI byc pelne. "linux-amd64" jest podciagiem innej nazwy
	// i dopasowanie czesciowe wzieloby zla sume — a wtedy aktualizacja
	// przerwalaby sie na sumie kontrolnej, wygladajac na atak.
	if _, err := sumaZManifestu(m, "linux-amd64"); err == nil {
		t.Error("dopasowanie czesciowe nie powinno przejsc")
	}
	if _, err := sumaZManifestu(m, "phpray-collector-linux-riscv"); err == nil {
		t.Error("brak wpisu powinien byc bledem")
	}
	if _, err := sumaZManifestu("", "cokolwiek"); err == nil {
		t.Error("pusty manifest powinien byc bledem")
	}
}
