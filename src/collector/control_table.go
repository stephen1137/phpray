package main

// Shared-memory control table (writer side). Mirrors src/extension/control.h:
//
//   header (64 B): magic u32 "PHRC", version u32, seq u32 (seqlock), entry_count u32,
//                  max_entries u32, reserved u32, updated_at u64, pad
//   entry (160 B): docroot_hash u64 (FNV-1a 64), until u64 (0 = empty), sample_rate u16,
//                  flags u16 (bit0 active), reserved u32, url_prefix[136] (NUL-terminated)
//
// One writer (the collector), many readers (PHP workers mapping the file read-only).
// Writes are wrapped in a seqlock: seq becomes odd before the change and even after,
// so a reader that sees an odd or changed seq discards its snapshot and retries.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	ctrlMagic      = 0x50485243
	ctrlVersion    = 1
	ctrlMaxEntries = 256
	ctrlPrefixLen  = 136
	ctrlHeaderSize = 64
	ctrlEntrySize  = 160
	ctrlFileSize   = ctrlHeaderSize + ctrlMaxEntries*ctrlEntrySize
	ctrlFlagActive = 1
)

// ControlEntry is one on-demand profiling request.
type ControlEntry struct {
	DocrootHash uint64 `json:"docroot_hash"`
	Docroot     string `json:"docroot,omitempty"` // known only to the writer (not stored)
	URLPrefix   string `json:"url_prefix"`
	SampleRate  int    `json:"sample_rate"`
	Until       int64  `json:"until"`
	Active      bool   `json:"active"`
}

// ControlTable is an mmap'd control table.
type ControlTable struct {
	path string
	data []byte
	mu   sync.Mutex
}

// FNVHash64 is FNV-1a 64 — the same function as phpray_ctrl_hash() in the extension.
func FNVHash64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// OpenControlTable opens (creating and initialising when missing) the table at path.
func OpenControlTable(path string) (*ControlTable, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fresh := info.Size() != ctrlFileSize
	if fresh {
		if err := f.Truncate(ctrlFileSize); err != nil {
			return nil, err
		}
	}
	// PHP workers of other users must be able to map it read-only
	os.Chmod(path, 0o644)
	data, err := syscall.Mmap(int(f.Fd()), 0, ctrlFileSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	t := &ControlTable{path: path, data: data}
	if fresh || t.magic() != ctrlMagic || t.version() != ctrlVersion {
		t.initLocked()
	}
	return t, nil
}

func (t *ControlTable) seqPtr() *uint32 { return (*uint32)(unsafe.Pointer(&t.data[8])) }
func (t *ControlTable) magic() uint32   { return binary.LittleEndian.Uint32(t.data[0:4]) }
func (t *ControlTable) version() uint32 { return binary.LittleEndian.Uint32(t.data[4:8]) }
func (t *ControlTable) entryCount() uint32 {
	return binary.LittleEndian.Uint32(t.data[12:16])
}

func (t *ControlTable) initLocked() {
	for i := range t.data {
		t.data[i] = 0
	}
	binary.LittleEndian.PutUint32(t.data[0:4], ctrlMagic)
	binary.LittleEndian.PutUint32(t.data[4:8], ctrlVersion)
	binary.LittleEndian.PutUint32(t.data[16:20], ctrlMaxEntries)
	atomic.StoreUint32(t.seqPtr(), 0)
}

func (t *ControlTable) entryAt(i int) []byte {
	off := ctrlHeaderSize + i*ctrlEntrySize
	return t.data[off : off+ctrlEntrySize]
}

func decodeEntry(b []byte) ControlEntry {
	e := ControlEntry{
		DocrootHash: binary.LittleEndian.Uint64(b[0:8]),
		Until:       int64(binary.LittleEndian.Uint64(b[8:16])),
		SampleRate:  int(binary.LittleEndian.Uint16(b[16:18])),
		Active:      binary.LittleEndian.Uint16(b[18:20])&ctrlFlagActive != 0,
	}
	p := b[24 : 24+ctrlPrefixLen]
	n := 0
	for n < len(p) && p[n] != 0 {
		n++
	}
	e.URLPrefix = string(p[:n])
	return e
}

func (t *ControlTable) beginWrite() {
	s := atomic.LoadUint32(t.seqPtr())
	atomic.StoreUint32(t.seqPtr(), s+1) // odd: write in progress
}

func (t *ControlTable) endWrite() {
	binary.LittleEndian.PutUint64(t.data[24:32], uint64(time.Now().Unix()))
	s := atomic.LoadUint32(t.seqPtr())
	atomic.StoreUint32(t.seqPtr(), s+1) // even: stable
}

// Set adds or replaces the entry for docroot. rate is 0–100 %, until a unix time.
func (t *ControlTable) Set(docroot, urlPrefix string, rate int, until int64) error {
	if docroot == "" {
		return errors.New("control: docroot is empty")
	}
	if len(urlPrefix) >= ctrlPrefixLen {
		return fmt.Errorf("control: url_prefix longer than %d bytes", ctrlPrefixLen-1)
	}
	if rate < 0 {
		rate = 0
	}
	if rate > 100 {
		rate = 100
	}
	h := FNVHash64(docroot)
	now := time.Now().Unix()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := int(t.entryCount())
	slot := -1
	for i := 0; i < n; i++ {
		e := t.entryAt(i)
		if binary.LittleEndian.Uint64(e[0:8]) == h {
			slot = i
			break
		}
	}
	if slot < 0 { // first expired/empty slot, else append
		for i := 0; i < n; i++ {
			e := t.entryAt(i)
			if u := int64(binary.LittleEndian.Uint64(e[8:16])); u == 0 || u <= now {
				slot = i
				break
			}
		}
	}
	if slot < 0 {
		if n >= ctrlMaxEntries {
			return errors.New("control: table full")
		}
		slot = n
		n++
	}
	t.beginWrite()
	e := t.entryAt(slot)
	for i := range e {
		e[i] = 0
	}
	binary.LittleEndian.PutUint64(e[0:8], h)
	binary.LittleEndian.PutUint64(e[8:16], uint64(until))
	binary.LittleEndian.PutUint16(e[16:18], uint16(rate))
	binary.LittleEndian.PutUint16(e[18:20], ctrlFlagActive)
	copy(e[24:24+ctrlPrefixLen-1], urlPrefix)
	binary.LittleEndian.PutUint32(t.data[12:16], uint32(n))
	t.endWrite()
	return nil
}

// Clear deactivates the entry for docroot (if any).
func (t *ControlTable) Clear(docroot string) bool {
	h := FNVHash64(docroot)
	t.mu.Lock()
	defer t.mu.Unlock()
	n := int(t.entryCount())
	for i := 0; i < n; i++ {
		e := t.entryAt(i)
		if binary.LittleEndian.Uint64(e[0:8]) == h && binary.LittleEndian.Uint64(e[8:16]) != 0 {
			t.beginWrite()
			binary.LittleEndian.PutUint16(e[18:20], 0)
			binary.LittleEndian.PutUint64(e[8:16], 0)
			t.endWrite()
			return true
		}
	}
	return false
}

// Expire clears entries whose `until` has passed; returns how many.
func (t *ControlTable) Expire(now int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := int(t.entryCount())
	cleared := 0
	for i := 0; i < n; i++ {
		e := t.entryAt(i)
		u := int64(binary.LittleEndian.Uint64(e[8:16]))
		if u != 0 && u <= now {
			if cleared == 0 {
				t.beginWrite()
			}
			binary.LittleEndian.PutUint16(e[18:20], 0)
			binary.LittleEndian.PutUint64(e[8:16], 0)
			cleared++
		}
	}
	if cleared > 0 {
		t.endWrite()
	}
	return cleared
}

// List returns the live entries.
func (t *ControlTable) List() []ControlEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := int(t.entryCount())
	var out []ControlEntry
	for i := 0; i < n; i++ {
		e := decodeEntry(t.entryAt(i))
		if e.Until != 0 {
			out = append(out, e)
		}
	}
	return out
}

// Seq returns the current seqlock value (even = stable).
func (t *ControlTable) Seq() uint32 { return atomic.LoadUint32(t.seqPtr()) }

func (t *ControlTable) Close() {
	if t.data != nil {
		syscall.Munmap(t.data)
		t.data = nil
	}
}
