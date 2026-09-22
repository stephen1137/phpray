package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// Testy umowy publikacji rekordu w ringu (v6).
//
// Tlo: pisarz najpierw zaklepuje miejsce przez CAS na write_pos, a dopiero
// potem zapisuje dane. Przez to czytelnik widzial slot jako gotowy, gdy lezala
// w nim jeszcze tresc z poprzedniego okrazenia bufora — 21.09.2026 trafil tak
// do produkcyjnej bazy slad z tekstem zapytania SQL w polu host, statusem
// 28521 i czasem trwania 8 529 657 644 052 ms.

// nowyRingDoTestu tworzy plik ringu o zadanej pojemnosci i otwiera czytelnika.
func nowyRingDoTestu(t *testing.T, pojemnosc uint64) *RingReader {
	t.Helper()
	naglowek := alignUp(uint64(unsafe.Sizeof(RingHeader{})), ringCacheLine)
	sciezka := filepath.Join(t.TempDir(), "ring.shm")
	if err := os.WriteFile(sciezka, make([]byte, naglowek+pojemnosc), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(sciezka, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := RingHeader{Magic: ringMagic, Version: ringVersion, Capacity: pojemnosc}
	if err := binary.Write(f, binary.LittleEndian, &h); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := OpenRing(sciezka)
	if err != nil {
		t.Fatalf("OpenRing: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// zaklep odwzorowuje pierwszy krok pisarza z ringbuffer.c: CAS przesuwa
// write_pos, zanim w slocie cokolwiek stanie.
func zaklep(r *RingReader, dlugosc uint32) uint64 {
	wp := atomic.LoadUint64(&r.header.WritePos)
	atomic.StoreUint64(&r.header.WritePos, wp+((uint64(dlugosc)+7) & ^uint64(7)))
	return wp % r.capacity
}

// opublikuj odwzorowuje pozostale kroki: typ RESERVED, dlugosc, dane, typ.
func opublikuj(r *RingReader, offset uint64, ladunek []byte, typ uint8) {
	dlugosc := uint32(len(ladunek)) + recordHeaderSize
	b := r.dataAt(offset, uint64(dlugosc))
	b[4] = recordTypeReserved
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), dlugosc)
	copy(b[recordHeaderSize:], ladunek)
	b[4] = typ
}

// TestSlotZaklepanyNieJestCzytany to regresja na sam blad: slot zwolniony przez
// czytelnika i ponownie zaklepany przez pisarza nie moze oddac starej tresci.
func TestSlotZaklepanyNieJestCzytany(t *testing.T) {
	pierwszy := buildTraceRecordFor(5, "sklep.example", "/koszyk/", nil, 1, 0)
	pojemnosc := (uint64(len(pierwszy))+recordHeaderSize+7) & ^uint64(7)
	r := nowyRingDoTestu(t, pojemnosc)

	// Pisarz zapisuje rekord, czytelnik go konsumuje.
	opublikuj(r, zaklep(r, uint32(len(pierwszy)+recordHeaderSize)), pierwszy, recordTypTrace)
	if _, _, ok := r.ReadRecord(); !ok {
		t.Fatal("pierwszy rekord powinien byc odczytany")
	}

	// Pojemnosc to dokladnie jeden rekord, wiec pisarz zaklepuje TEN SAM slot.
	// Dane jeszcze w nim nie leza — lezy to, co czytelnik wlasnie skonsumowal.
	offset := zaklep(r, uint32(len(pierwszy)+recordHeaderSize))

	if _, _, ok := r.ReadRecord(); ok {
		t.Fatal("czytelnik oddal zawartosc slotu, ktory pisarz dopiero zaklepal " +
			"— to jest ten blad: stary rekord wraca jako nowy")
	}

	// Gdy pisarz skonczy, ten sam slot ma sie odczytac normalnie.
	drugi := buildTraceRecordFor(5, "inny.example", "/kasa/", nil, 1, 0)
	opublikuj(r, offset, drugi, recordTypTrace)
	typ, ladunek, ok := r.ReadRecord()
	if !ok || typ != recordTypTrace {
		t.Fatal("po publikacji rekord powinien byc widoczny")
	}
	slad, err := DeserializeTrace(ladunek, 5)
	if err != nil {
		t.Fatalf("DeserializeTrace: %v", err)
	}
	if slad.Host != "inny.example" {
		t.Fatalf("odczytano %q, oczekiwano nowego rekordu", slad.Host)
	}
}

// TestSlotPoMartwymPisarzuNieBlokujeRingu: gdy proces PHP zginie miedzy
// rezerwacja a zapisem, czytelnik po czasie ma isc dalej, a nie stanac na zawsze.
func TestSlotPoMartwymPisarzuNieBlokujeRingu(t *testing.T) {
	stary := utkniecieSlotu
	utkniecieSlotu = 20 * time.Millisecond
	defer func() { utkniecieSlotu = stary }()

	r := nowyRingDoTestu(t, 8192)
	dlugosc := uint32(128)
	zaklep(r, dlugosc) // pisarz zaklepal i zginal

	if _, _, ok := r.ReadRecord(); ok {
		t.Fatal("niedokonczony slot nie moze byc odczytany")
	}
	time.Sleep(40 * time.Millisecond)
	if _, _, ok := r.ReadRecord(); ok {
		t.Fatal("po porzuceniu slotu nie ma juz czego czytac")
	}
	if r.Porzucone != 1 {
		t.Fatalf("porzucone sloty = %d, oczekiwano 1", r.Porzucone)
	}
	if got := atomic.LoadUint64(&r.header.ReadPos); got == 0 {
		t.Fatal("czytelnik utknal na martwym slocie")
	}
}

// TestOdrzucamyNiemozliwySlad opisuje wiersz, ktory realnie trafil do bazy.
func TestOdrzucamyNiemozliwySlad(t *testing.T) {
	przypadki := []struct {
		nazwa string
		slad  *Trace
	}{
		{"status z produkcji", &Trace{Status: 28521}},
		{"czas z produkcji", &Trace{Status: 200, DurationMs: 8529657644052.67}},
		{"znacznik czasu z produkcji", &Trace{Status: 200, Ts: 2329560872168805120}},
		{"pamiec", &Trace{Status: 200, MemoryMB: 9e9}},
	}
	for _, p := range przypadki {
		t.Run(p.nazwa, func(t *testing.T) {
			if err := rozsadnySlad(p.slad); err == nil {
				t.Fatal("taki slad nie moze przejsc do bazy")
			}
		})
	}
	zwykly := &Trace{Status: 200, DurationMs: 412.5, MemoryMB: 64, Ts: time.Now().Unix()}
	if err := rozsadnySlad(zwykly); err != nil {
		t.Fatalf("zwykly slad odrzucony: %v", err)
	}
}
