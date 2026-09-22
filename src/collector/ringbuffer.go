package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Ring buffer constants — must match ringbuffer.h
const (
	ringMagic         = 0x50485259 // "PHRY"
	ringVersion       = 5          // v4: app + profiled in header, incl/self per component; v5: docroot string
	ringVersionV4     = 4
	ringVersionV3     = 3
	ringVersionV2     = 2
	ringVersionLegacy = 1
	ringCacheLine     = 64
	recordTypTrace    = 0
	recordTypePadding = 0xFF
	// Slot zaklepany przez pisarza, dane jeszcze w locie. Czytelnik czeka.
	recordTypeReserved = 0xFE
)

// Po tym czasie uznajemy, ze pisarz zginal miedzy rezerwacja a zapisem
// (fatal, OOM killer, SIGKILL) i slot nigdy sie nie dokonczy. Zywy pisarz
// domyka rekord w mikrosekundach, wiec dwie sekundy nikogo nie dotycza.
// Zmienna, bo testy skracaja to do milisekund.
var utkniecieSlotu = 2 * time.Second

// RingHeader matches phpray_ring_header_t (C struct, cache-line aligned)
type RingHeader struct {
	// Cache line 0: identification
	Magic    uint32
	Version  uint32
	Capacity uint64
	Total    uint64
	_pad0    [ringCacheLine - 24]byte

	// Cache line 1: writer state
	WritePos    uint64
	RecordCount uint64
	DropCount   uint64
	_pad1       [ringCacheLine - 24]byte

	// Cache line 2: reader state
	ReadPos uint64
	_pad2   [ringCacheLine - 8]byte
}

// RecordHeader matches phpray_ring_record_t (packed)
type RecordHeader struct {
	RecordLen  uint32
	RecordType uint8
}

const recordHeaderSize = 5 // packed: 4 + 1

// RingReader reads from a shared memory ring buffer created by phpray.so
type RingReader struct {
	data     []byte      // mmap'd region (header + data)
	header   *RingHeader // points into data
	dataBase uintptr     // start of data region
	capacity uint64
	file     *os.File // kept open for the mapping's lifetime; closed by Close (never by a finalizer)
	version  uint32   // ring buffer version (1..6)

	// Bezpiecznik na niedokonczony rekord: zapamietujemy, od kiedy stoimy
	// w tym samym miejscu, zeby martwy pisarz nie zatrzymal calego ringu.
	czekaOd    time.Time
	czekaPoz   uint64
	Porzucone  uint64 // sloty pominiete po utknieciu — do metryk
	Odwrocenia uint64 // ile razy ring byl zakleszczony i zostal wyrownany
}

// OpenRing opens an existing ring buffer for reading
func OpenRing(path string) (*RingReader, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat: %w", err)
	}

	size := info.Size()
	if size < int64(unsafe.Sizeof(RingHeader{})) {
		f.Close()
		return nil, fmt.Errorf("file too small: %d bytes", size)
	}

	// mmap the file
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: %w", err)
	}

	// f stays open until Close(); closing its raw fd behind os.File's back would
	// let the File finalizer close a reused descriptor later (double close).

	header := (*RingHeader)(unsafe.Pointer(&data[0]))

	if header.Magic != ringMagic {
		syscall.Munmap(data)
		f.Close()
		return nil, fmt.Errorf("bad magic: 0x%08X (expected 0x%08X)", header.Magic, ringMagic)
	}
	if header.Version < ringVersionLegacy || header.Version > ringVersion {
		syscall.Munmap(data)
		f.Close()
		return nil, fmt.Errorf("version mismatch: %d (expected %d..%d)", header.Version, ringVersionLegacy, ringVersion)
	}

	headerSize := alignUp(uint64(unsafe.Sizeof(RingHeader{})), ringCacheLine)

	return &RingReader{
		data:     data,
		header:   header,
		dataBase: uintptr(unsafe.Pointer(&data[headerSize])),
		capacity: header.Capacity,
		file:     f,
		version:  header.Version,
	}, nil
}

// ReadRecord reads one record from the ring buffer.
// Returns the record type, payload data, and whether a record was available.
func (r *RingReader) ReadRecord() (uint8, []byte, bool) {
	for {
		readPos := atomic.LoadUint64(&r.header.ReadPos)
		writePos := atomic.LoadUint64(&r.header.WritePos)

		// Pozycja czytania PRZED pozycja zapisu to stan niemozliwy, ktory
		// zakleszcza ring NA ZAWSZE: pisarz w rozszerzeniu liczy wolne
		// miejsce jako `write_pos - read_pos` na uint64, wiec po odwroceniu
		// dostaje liczbe rzedu 10^19, uznaje bufor za pelny i odrzuca KAZDY
		// kolejny slad. 22.09.2026 na h2 tak zakleszczone byly cztery ringi:
		// 24 654 zgubionych sladow z 80 501 (23,4%), jedno konto traci 90%.
		// Czytelnik jest jedyna strona, ktora moze to naprawic — i robi to
		// tutaj, wyrownujac pozycje zamiast czekac na nowe rozszerzenie.
		if readPos > writePos {
			atomic.StoreUint64(&r.header.ReadPos, writePos)
			r.Odwrocenia++
			log.Printf("ring: pozycja czytania wyprzedzila zapis (%d > %d) — ring byl zakleszczony, wyrownuje",
				readPos, writePos)
			return 0, nil, false
		}

		if readPos >= writePos {
			return 0, nil, false // No data
		}

		offset := readPos % r.capacity
		tailSpace := r.capacity - offset

		// Check if we can read a record header
		if tailSpace < recordHeaderSize {
			// Skip gap (was padding from wraparound)
			r.przesunCzytanie(readPos+tailSpace, writePos)
			continue
		}

		// Naglowek czytamy atomowo. Pisarz zapisuje dlugosc jako ostatnia
		// z pary (typ, dlugosc) i ze zwolnieniem bariery, wiec niezerowa
		// dlugosc oznacza, ze pasujacy typ jest juz widoczny.
		headerBytes := r.dataAt(offset, recordHeaderSize)
		var hdr RecordHeader
		hdr.RecordLen = atomic.LoadUint32((*uint32)(unsafe.Pointer(&headerBytes[0])))
		hdr.RecordType = headerBytes[4]

		// Dlugosc 0 = pisarz zaklepal slot przez CAS na write_pos, ale nie
		// zdazyl go opisac. Tego NIE WOLNO przeskoczyc: pod spodem lezy tresc
		// z poprzedniego okrazenia bufora i przeczytalibysmy ja jako nowy slad.
		// Tak wlasnie 21.09.2026 trafil do bazy rekord z tekstem SQL w polu
		// host i czasem 8 529 657 644 052 ms.
		if hdr.RecordLen == 0 || hdr.RecordType == recordTypeReserved {
			if !r.utknal(readPos) {
				return 0, nil, false // wroci za chwile, rekord jest w locie
			}
			// Pisarz zginal miedzy rezerwacja a zapisem. Gdy znamy dlugosc,
			// pomijamy dokladnie ten rekord; gdy nie — przesuwamy sie o slowo.
			skok := uint64(8)
			if hdr.RecordLen > 0 && uint64(hdr.RecordLen) <= r.capacity {
				skok = (uint64(hdr.RecordLen) + 7) & ^uint64(7)
			}
			log.Printf("ring: porzucam niedokonczony rekord na %d (%d B) — pisarz nie wrocil przez %s",
				readPos, skok, utkniecieSlotu)
			r.Porzucone++
			r.wyczyscNaglowek(offset)
			r.przesunCzytanie(readPos+skok, writePos)
			r.czekaOd = time.Time{}
			continue
		}
		r.czekaOd = time.Time{}

		if uint64(hdr.RecordLen) > r.capacity {
			// Corrupt — skip 8 bytes
			r.przesunCzytanie(readPos+8, writePos)
			continue
		}

		paddedTotal := (uint64(hdr.RecordLen) + 7) & ^uint64(7)

		// Skip padding records
		if hdr.RecordType == recordTypePadding {
			r.wyczyscNaglowek(offset)
			r.przesunCzytanie(readPos+paddedTotal, writePos)
			continue
		}

		payloadLen := hdr.RecordLen - recordHeaderSize

		// Copy payload
		payload := make([]byte, payloadLen)
		copy(payload, r.dataAt(offset+recordHeaderSize, uint64(payloadLen)))

		// Naglowek zerujemy PRZED oddaniem miejsca pisarzowi, zeby slot, ktory
		// zaraz zaklepie, mial dlugosc 0 zamiast resztki po tym rekordzie.
		r.wyczyscNaglowek(offset)

		// Advance read position
		r.przesunCzytanie(readPos+paddedTotal, writePos)

		return hdr.RecordType, payload, true
	}
}

// przesunCzytanie przesuwa pozycje czytania, NIGDY poza pozycje zapisu.
//
// Kazde z pieciu miejsc, ktore przesuwaly ja wprost, moglo ja przeskoczyc:
// przy pomijaniu luki na koncu bufora, przy porzucaniu niedokonczonego slotu,
// przy uszkodzonej dlugosci rekordu i przy rekordzie wypelniajacym. Przeskok
// o jeden bajt wystarczy, zeby pisarz przestal zapisywac cokolwiek — patrz
// komentarz przy wykryciu odwrocenia w ReadRecord.
func (r *RingReader) przesunCzytanie(nowa, writePos uint64) {
	if nowa > writePos {
		nowa = writePos
	}
	atomic.StoreUint64(&r.header.ReadPos, nowa)
}

// utknal mowi, czy stoimy na tej samej pozycji dluzej niz utkniecieSlotu.
// Pierwsze wywolanie dla danej pozycji tylko zapamietuje czas.
func (r *RingReader) utknal(pos uint64) bool {
	if r.czekaOd.IsZero() || r.czekaPoz != pos {
		r.czekaOd, r.czekaPoz = time.Now(), pos
		return false
	}
	return time.Since(r.czekaOd) > utkniecieSlotu
}

// wyczyscNaglowek zeruje dlugosc i typ skonsumowanego rekordu.
func (r *RingReader) wyczyscNaglowek(offset uint64) {
	b := r.dataAt(offset, recordHeaderSize)
	b[4] = 0
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), 0)
}

// Stats returns current ring buffer statistics
func (r *RingReader) Stats() (records, drops uint64, fillPct float64) {
	records = atomic.LoadUint64(&r.header.RecordCount)
	drops = atomic.LoadUint64(&r.header.DropCount)

	// Kolejnosc ma znaczenie: czytamy NAJPIERW pozycje czytania, potem zapisu,
	// zeby wp bylo co najmniej tak swieze jak rp. Obie wartosci pobieramy
	// osobnymi operacjami atomowymi, wiec i tak moga sie rozjechac w czasie.
	rp := atomic.LoadUint64(&r.header.ReadPos)
	wp := atomic.LoadUint64(&r.header.WritePos)
	fillPct = zapelnienie(wp, rp, r.capacity)
	return
}

// zapelnienie liczy procent zajetosci ringu, odporny na rozjazd pozycji.
//
// 22.09.2026 na h2 dziennik pisal "buffer 109952421083179.7% full": odejmowanie
// wp-rp na uint64 przewinelo sie pod zero, bo pozycja czytania wyprzedzila
// zapis miedzy dwoma odczytami atomowymi. Bezsensowny procent ukrywal
// prawdziwa sprawe — ring NAPRAWDE gubil slady (2037 sztuk) i nie dalo sie
// zobaczyc, jak bardzo jest pelny.
func zapelnienie(wp, rp uint64, capacity uint64) float64 {
	if capacity == 0 {
		return 0
	}
	if wp < rp {
		// Czytelnik dogonil lub wyprzedzil zapis — nic nie zalega.
		return 0
	}
	used := wp - rp
	if used > capacity {
		used = capacity
	}
	return float64(used) / float64(capacity) * 100.0
}

// Version returns the ring buffer format version (1..4)
func (r *RingReader) Version() uint32 {
	return r.version
}

// Close unmaps and closes the ring buffer
func (r *RingReader) Close() error {
	if r.data != nil {
		syscall.Munmap(r.data)
		r.data = nil
	}
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	return nil
}

// dataAt returns a slice of the data region at offset, length bytes
func (r *RingReader) dataAt(offset, length uint64) []byte {
	headerSize := alignUp(uint64(unsafe.Sizeof(RingHeader{})), ringCacheLine)
	start := headerSize + offset
	return r.data[start : start+length]
}

func alignUp(val, alignment uint64) uint64 {
	return (val + alignment - 1) & ^(alignment - 1)
}

// DeserializeTrace parses a binary trace record into a Trace struct.
// ringVersion: 1 legacy, 2 adds php_version, 3 adds redis + components,
// 4 adds app/profiled/prof_overflow in the header and incl/self per component,
// 5 adds the DOCUMENT_ROOT string (site identity for the cloud shipper).
// Pass 0 to assume v2.
func DeserializeTrace(data []byte, ringVer ...uint32) (*Trace, error) {
	ver := uint32(2) // default to v2
	if len(ringVer) > 0 && ringVer[0] > 0 {
		ver = ringVer[0]
	}
	_ = ver             // used below
	if len(data) < 90 { // minimum fixed header size
		return nil, fmt.Errorf("data too short: %d bytes", len(data))
	}

	// Read fixed header fields (packed, little-endian)
	pos := 0
	read := func(size int) []byte {
		if pos+size > len(data) {
			return nil
		}
		b := data[pos : pos+size]
		pos += size
		return b
	}

	readU8 := func() uint8 {
		b := read(1)
		if b == nil {
			return 0
		}
		return b[0]
	}
	readU16 := func() uint16 {
		b := read(2)
		if b == nil {
			return 0
		}
		return binary.LittleEndian.Uint16(b)
	}
	readU32 := func() uint32 {
		b := read(4)
		if b == nil {
			return 0
		}
		return binary.LittleEndian.Uint32(b)
	}
	readU64 := func() uint64 {
		b := read(8)
		if b == nil {
			return 0
		}
		return binary.LittleEndian.Uint64(b)
	}
	readI32 := func() int32 {
		b := read(4)
		if b == nil {
			return 0
		}
		return int32(binary.LittleEndian.Uint32(b))
	}
	readStr := func(length uint16) string {
		b := read(int(length))
		if b == nil {
			return ""
		}
		return string(b)
	}

	t := &Trace{}

	// Fixed header — matches phpray_trace_record_t
	t.RID = readU64()
	t.UID = readU32()
	t.PID = readU32()
	t.Ts = int64(readU64())
	durationNs := readU64()
	t.DurationMs = float64(durationNs) / 1e6
	cpuUserNs := readU64()
	t.CPUUserMs = float64(cpuUserNs) / 1e6
	cpuSysNs := readU64()
	t.CPUSysMs = float64(cpuSysNs) / 1e6
	t.Status = readU16()
	memPeak := readU64()
	t.MemoryMB = float64(memPeak) / (1024 * 1024)

	qCount := readU16()
	dbTotalNs := readU64()
	t.DBMs = float64(dbTotalNs) / 1e6
	if qCount > 0 {
		t.DBCount = &qCount
	}

	hCount := readU16()
	httpTotalNs := readU64()
	t.HTTPMs = float64(httpTotalNs) / 1e6
	if hCount > 0 {
		t.HTTPCount = &hCount
	}

	fCount := readU16()
	fileTotalNs := readU64()
	t.FileMs = float64(fileTotalNs) / 1e6
	if fCount > 0 {
		t.FileCount = &fCount
	}

	// v3: redis counts (only in ring buffer v3+)
	if ver >= 3 {
		redisCount := readU16()
		redisTotalNs := readU64()
		t.RedisMs = float64(redisTotalNs) / 1e6
		if redisCount > 0 {
			t.RedisCount = &redisCount
		}
	}

	t.WP = readU8()
	traceLevel := readU8()
	switch traceLevel {
	case 0:
		t.Level = "summary"
	case 1:
		t.Level = "normal"
	case 2:
		t.Level = "full"
	case 3:
		t.Level = "alert"
	}

	markCount := readU16()
	errorCount := readU16()
	t.N1 = readU8()

	// Variable string lengths
	serverNameLen := readU16()
	requestURILen := readU16()
	requestMethodLen := readU16()
	phprayIDLen := readU16()

	// v2: php_version_len (uint8) added after phpray_id_len
	var phpVersionLen uint8
	if ver >= 2 {
		phpVersionLen = readU8()
	}

	// Serialized sub-record counts
	serializedQueryCount := readU16()
	serializedHTTPCount := readU16()
	serializedMarkCount := readU16()
	serializedErrorCount := readU16()
	// v3: component count (only in ring buffer v3+)
	var serializedComponentCount uint16
	if ver >= 3 {
		serializedComponentCount = readU16()
	}
	// v4: detected application, profiled flag, profile stack overflow flag
	if ver >= 4 {
		t.App = appName(readU8())
		t.Profiled = readU8()
		_ = readU8() // prof_overflow (self times approximate) — not surfaced yet
	}
	// v5: DOCUMENT_ROOT length (string follows php_version)
	var docrootLen uint16
	if ver >= 5 {
		docrootLen = readU16()
	}

	// Read strings
	t.Host = readStr(serverNameLen)
	t.URI = readStr(requestURILen)
	t.Method = readStr(requestMethodLen)
	t.ID = readStr(phprayIDLen)

	// v2: read php version string
	if ver >= 2 && phpVersionLen > 0 {
		t.PhpVer = readStr(uint16(phpVersionLen))
	}
	// v5: docroot
	if ver >= 5 && docrootLen > 0 {
		t.Docroot = readStr(docrootLen)
	}

	// Read queries (with optional backtrace frames)
	for i := uint16(0); i < serializedQueryCount; i++ {
		sqlLen := readU16()
		durNs := readU64()
		offNs := readU64()
		rows := readU32()
		src := readU8()
		btCount := readU8()
		sqlText := readStr(sqlLen)

		var frames []Frame
		for b := uint8(0); b < btCount; b++ {
			fileLen := readU8()
			line := readU32()
			file := readStr(uint16(fileLen))
			frames = append(frames, Frame{File: file, Line: line})
		}

		t.Queries = append(t.Queries, Query{
			SQL:    sqlText,
			Ms:     float64(durNs) / 1e6,
			T:      float64(offNs) / 1e6,
			Rows:   rows,
			Source: src,
			BT:     frames,
		})
	}

	// Read HTTP calls (with optional backtrace frames)
	for i := uint16(0); i < serializedHTTPCount; i++ {
		urlLen := readU16()
		durNs := readU64()
		offNs := readU64()
		respCode := readU16()
		btCount := readU8()
		urlText := readStr(urlLen)

		var frames []Frame
		for b := uint8(0); b < btCount; b++ {
			fileLen := readU8()
			line := readU32()
			file := readStr(uint16(fileLen))
			frames = append(frames, Frame{File: file, Line: line})
		}

		t.HTTPCalls = append(t.HTTPCalls, HTTP{
			URL:    urlText,
			Ms:     float64(durNs) / 1e6,
			T:      float64(offNs) / 1e6,
			Status: respCode,
			BT:     frames,
		})
	}

	// Read marks
	for i := uint16(0); i < serializedMarkCount; i++ {
		nameLen := readU8()
		offNs := readU64()
		name := readStr(uint16(nameLen))

		t.Marks = append(t.Marks, Mark{
			Name: name,
			T:    float64(offNs) / 1e6,
		})
	}

	// Read errors
	for i := uint16(0); i < serializedErrorCount; i++ {
		msgLen := readU16()
		errType := readI32()
		offNs := readU64()
		msg := readStr(msgLen)

		t.Errors = append(t.Errors, Error{
			Type: errorTypeString(int(errType)),
			Msg:  msg,
			T:    float64(offNs) / 1e6,
		})
	}

	// Read components (function profile)
	for i := uint16(0); i < serializedComponentCount; i++ {
		nameLen := readU8()
		var inclNs, selfNs uint64
		if ver >= 4 {
			inclNs = readU64()
			selfNs = readU64()
		} else {
			inclNs = readU64() // v3: exclusive bucket time, reported as ms
		}
		callCount := readU32()
		name := readStr(uint16(nameLen))

		c := Component{
			Name:  name,
			Ms:    float64(inclNs) / 1e6,
			Calls: int(callCount),
			Pct:   0, // will be computed below
		}
		if ver >= 4 {
			c.InclNs = inclNs
			c.SelfNs = selfNs
		}
		t.Components = append(t.Components, c)
	}
	// Traces older than v4 carried components only when the (always-on) profiler ran
	if ver < 4 && len(t.Components) > 0 {
		t.Profiled = 1
	}

	// Compute component percentages
	if t.DurationMs > 0 {
		for i := range t.Components {
			t.Components[i].Pct = t.Components[i].Ms / t.DurationMs * 100
		}
	}

	_ = markCount
	_ = errorCount

	if err := rozsadnySlad(t); err != nil {
		return nil, err
	}

	return t, nil
}

// rozsadnySlad odrzuca rekordy, ktore nie moga pochodzic z prawdziwego zadania.
// To druga linia obrony za protokolem publikacji w ringu: nawet gdyby jakas
// przyszla zmiana znow wpuscila polowe cudzego rekordu, nie trafi ona do bazy
// ani na oczy klienta. Progi sa celowo absurdalnie szerokie — chodzi o
// odsianie smiecia, nie o ocenianie, czy zadanie bylo wolne.
func rozsadnySlad(t *Trace) error {
	if t.Status != 0 && (t.Status < 100 || t.Status > 599) {
		return fmt.Errorf("niemozliwy status HTTP: %d", t.Status)
	}
	if t.DurationMs < 0 || t.DurationMs > 24*60*60*1000 {
		return fmt.Errorf("niemozliwy czas trwania: %.0f ms", t.DurationMs)
	}
	if t.MemoryMB < 0 || t.MemoryMB > 1024*1024 {
		return fmt.Errorf("niemozliwa pamiec: %.0f MB", t.MemoryMB)
	}
	// Znacznik czasu to SEKUNDY epoki (phpray_trace_record_t.timestamp), nie
	// nanosekundy — dopuszczamy od 2020 roku do doby w przod, bo zegar na
	// serwerze klienta bywa przestawiony.
	const rok2020 = int64(1577836800)
	if t.Ts != 0 && (t.Ts < rok2020 || t.Ts > time.Now().Add(24*time.Hour).Unix()) {
		return fmt.Errorf("znacznik czasu poza zakresem: %d", t.Ts)
	}
	return nil
}

// appName maps PHPRAY_APP_* from the extension to the JSONL "app" value
func appName(app uint8) string {
	switch app {
	case 1:
		return "wordpress"
	case 2:
		return "prestashop"
	case 3:
		return "laravel"
	case 4:
		return "magento"
	case 5:
		return "symfony"
	default:
		return ""
	}
}

func errorTypeString(t int) string {
	switch t {
	case 1:
		return "E_ERROR"
	case 2:
		return "E_WARNING"
	case 4:
		return "E_PARSE"
	case 8:
		return "E_NOTICE"
	case 256:
		return "E_USER_ERROR"
	case 512:
		return "E_USER_WARNING"
	case 1024:
		return "E_USER_NOTICE"
	case 8192:
		return "E_DEPRECATED"
	case 16384:
		return "E_USER_DEPRECATED"
	default:
		return fmt.Sprintf("E_%d", t)
	}
}
