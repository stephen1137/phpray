package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

// writeRingFile creates a ring buffer file in the on-disk layout of
// src/extension/ringbuffer.h (header of three cache lines + data region) holding
// the given trace payloads, exactly as phpray.so would leave it before the
// collector reads it.
func writeRingFile(t *testing.T, path string, payloads [][]byte) {
	t.Helper()
	const capacity = 64 * 1024
	headerSize := alignUp(uint64(unsafe.Sizeof(RingHeader{})), ringCacheLine)
	buf := make([]byte, int(headerSize)+capacity)
	binary.LittleEndian.PutUint32(buf[0:], ringMagic)
	binary.LittleEndian.PutUint32(buf[4:], ringVersion)
	binary.LittleEndian.PutUint64(buf[8:], capacity)
	binary.LittleEndian.PutUint64(buf[16:], uint64(len(buf)))
	pos := uint64(0)
	for _, p := range payloads {
		recLen := uint32(recordHeaderSize + len(p))
		off := int(headerSize + pos)
		binary.LittleEndian.PutUint32(buf[off:], recLen)
		buf[off+4] = recordTypTrace
		copy(buf[off+recordHeaderSize:], p)
		pos += (uint64(recLen) + 7) &^ 7
	}
	binary.LittleEndian.PutUint64(buf[64:], pos)                   // write_pos
	binary.LittleEndian.PutUint64(buf[72:], uint64(len(payloads))) // record_count
	// drop_count (offset 80) and read_pos (offset 128) stay 0
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write ring %s: %v", path, err)
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestIsGlobPattern(t *testing.T) {
	for p, want := range map[string]bool{
		"/dev/shm/phpray":        false,
		"/run/phpray/ring-*":     true,
		"/run/phpray/ring-?":     true,
		"/run/phpray/ring-[0-9]": true,
		"":                       false,
	} {
		if got := isGlobPattern(p); got != want {
			t.Errorf("isGlobPattern(%q) = %v, want %v", p, got, want)
		}
	}
	if d := NewDaemon(DefaultDaemonConfig()); d.rings != nil {
		t.Fatalf("single shm_path must not create a ring set")
	}
}

func TestRingSetRescan(t *testing.T) {
	dir := t.TempDir()
	rec := buildTraceRecordFor(5, "a.local", "/", nil, 1, 0)
	writeRingFile(t, filepath.Join(dir, "ring-1000"), [][]byte{rec})
	writeRingFile(t, filepath.Join(dir, "ring-1001"), [][]byte{rec})
	// a file matching the pattern that is not a ring (too small) must be skipped, not fatal
	if err := os.WriteFile(filepath.Join(dir, "ring-broken"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := newRingSet(filepath.Join(dir, "ring-*"))
	defer set.closeAll()

	added, removed := set.rescan()
	if added != 2 || removed != 0 {
		t.Fatalf("first rescan: added=%d removed=%d, want 2/0", added, removed)
	}
	if got := set.paths(); len(got) != 2 || got[0] != filepath.Join(dir, "ring-1000") || got[1] != filepath.Join(dir, "ring-1001") {
		t.Fatalf("paths after first rescan: %v", got)
	}
	// nothing changed → nothing happens
	if added, removed = set.rescan(); added != 0 || removed != 0 {
		t.Fatalf("idle rescan: added=%d removed=%d", added, removed)
	}
	// the broken file becomes a real ring → picked up
	writeRingFile(t, filepath.Join(dir, "ring-broken"), nil)
	if added, removed = set.rescan(); added != 1 || removed != 0 {
		t.Fatalf("rescan after fixing the file: added=%d removed=%d", added, removed)
	}
	// a pool goes away
	os.Remove(filepath.Join(dir, "ring-1000"))
	if added, removed = set.rescan(); added != 0 || removed != 1 {
		t.Fatalf("rescan after removal: added=%d removed=%d", added, removed)
	}
	// recreated with a new inode → reopened (counts as removed + added)
	os.Remove(filepath.Join(dir, "ring-1001"))
	writeRingFile(t, filepath.Join(dir, "ring-1001"), [][]byte{rec, rec})
	if added, removed = set.rescan(); added != 1 || removed != 1 {
		t.Fatalf("rescan after recreation: added=%d removed=%d", added, removed)
	}
	st := set.status()
	if len(st) != 2 || st[0].Path != filepath.Join(dir, "ring-1001") || st[0].Records != 2 || st[0].Version != ringVersion {
		t.Fatalf("status: %+v", st)
	}
}

func TestRingSetWorkerReadsEveryRing(t *testing.T) {
	dir := t.TempDir()
	writeRingFile(t, filepath.Join(dir, "ring-1000"), [][]byte{
		buildTraceRecordFor(5, "a.local", "/a", nil, 1, 0),
	})
	writeRingFile(t, filepath.Join(dir, "ring-1001"), [][]byte{
		buildTraceRecordFor(5, "b.local", "/b", nil, 1, 0),
		buildTraceRecordFor(5, "b.local", "/b2", nil, 1, 0),
	})

	store, err := OpenStorage(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	cfg := DefaultDaemonConfig()
	cfg.SHMPath = filepath.Join(dir, "ring-*")
	cfg.Mode = "ring"
	cfg.PollIntervalMs = 10
	cfg.BatchSize = 100
	cfg.Quiet = true
	d := NewDaemon(cfg)
	d.store = store
	d.ringScanInterval = 30 * time.Millisecond
	if d.rings == nil {
		t.Fatalf("glob shm_path must create a ring set")
	}

	d.startReader()
	waitFor(t, "3 records from two rings", 3*time.Second, func() bool { return d.tracesRead.Load() >= 3 })

	// a third pool starts later and is picked up by the rescan
	writeRingFile(t, filepath.Join(dir, "ring-1002"), [][]byte{
		buildTraceRecordFor(5, "c.local", "/c", nil, 0, 0),
	})
	waitFor(t, "record from the new ring", 3*time.Second, func() bool { return d.tracesRead.Load() >= 4 })
	waitFor(t, "3 rings open", 3*time.Second, func() bool { return len(d.rings.status()) == 3 })

	// a pool stops: its file disappears and the ring is dropped
	os.Remove(filepath.Join(dir, "ring-1000"))
	waitFor(t, "ring removed", 3*time.Second, func() bool { return len(d.rings.status()) == 2 })
	if in := d.inputStatus(); !in.Glob || in.SHMPath != cfg.SHMPath || len(in.Rings) != 2 || in.Rings[0].Path != filepath.Join(dir, "ring-1001") {
		t.Fatalf("inputStatus: %+v", in)
	}

	close(d.done)
	d.wg.Wait()
	d.flushBatch()

	count, _ := store.GetTraceCount()
	if count != 4 {
		t.Fatalf("stored traces: got %d, want 4", count)
	}
	rows, err := store.db.Query("SELECT host, COUNT(*) FROM traces GROUP BY host ORDER BY host")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var host string
		var n int
		rows.Scan(&host, &n)
		got[host] = n
	}
	if got["a.local"] != 1 || got["b.local"] != 2 || got["c.local"] != 1 {
		t.Fatalf("traces per host: %v", got)
	}
	if len(d.rings.status()) != 0 {
		t.Fatalf("rings must be closed after shutdown")
	}
}
