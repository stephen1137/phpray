package main

import (
	"sync/atomic"
	"testing"
)

// Ring zakleszczony: pozycja czytania przed pozycja zapisu.
//
// Tlo (22.09.2026, h2): pisarz w rozszerzeniu liczy wolne miejsce jako
// `write_pos - read_pos` na uint64. Po odwroceniu pozycji dostaje liczbe rzedu
// 10^19, uznaje bufor za pelny i odrzuca KAZDY kolejny slad — na zawsze.
// Cztery ringi na h2 byly w tym stanie: 24 654 zgubionych z 80 501 (23,4%),
// jedno konto gubilo 90% ruchu. Czytelnik jest jedyna strona, ktora moze to
// naprawic bez wymiany rozszerzenia.
func TestZakleszczonyRingJestWyrownywany(t *testing.T) {
	r := nowyRingDoTestu(t, 4096)

	// Stan niemozliwy, dokladnie taki, jaki zastalismy na produkcji.
	atomic.StoreUint64(&r.header.WritePos, 1000)
	atomic.StoreUint64(&r.header.ReadPos, 1064)

	if _, _, ok := r.ReadRecord(); ok {
		t.Fatal("z zakleszczonego ringu nie ma czego czytac")
	}
	if got := atomic.LoadUint64(&r.header.ReadPos); got != 1000 {
		t.Errorf("pozycja czytania po naprawie = %d, chce 1000 (rowna pozycji zapisu)", got)
	}
	if r.Odwrocenia != 1 {
		t.Errorf("licznik odwrocen = %d, chce 1", r.Odwrocenia)
	}

	// Po wyrownaniu ring MUSI znowu przyjmowac dane — to jest cel naprawy.
	ladunek := []byte(`{"ok":1}`)
	offset := zaklep(r, uint32(len(ladunek))+recordHeaderSize)
	opublikuj(r, offset, ladunek, uint8(1))
	typ, dane, ok := r.ReadRecord()
	if !ok || typ != uint8(1) || string(dane) != string(ladunek) {
		t.Errorf("po naprawie ring nie dziala: ok=%v typ=%d dane=%q", ok, typ, dane)
	}
}

// Zadne przesuniecie pozycji czytania nie moze jej wyniesc przed zapis.
func TestPrzesuniecieNigdyNiePrzeskakujePisarza(t *testing.T) {
	r := nowyRingDoTestu(t, 4096)
	atomic.StoreUint64(&r.header.WritePos, 500)
	atomic.StoreUint64(&r.header.ReadPos, 480)

	for _, skok := range []uint64{8, 64, 4096, 1 << 40} {
		atomic.StoreUint64(&r.header.ReadPos, 480)
		r.przesunCzytanie(480+skok, 500)
		got := atomic.LoadUint64(&r.header.ReadPos)
		if got > 500 {
			t.Errorf("skok %d: pozycja czytania = %d, przeskoczyla zapis (500)", skok, got)
		}
	}
}
