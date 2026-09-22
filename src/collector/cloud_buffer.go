package main

// Disk buffer for the cloud shipper: one file per batch (the exact payload that
// failed to deliver, batch_id included so the console can deduplicate a retry).
// Bounded by a byte limit; when exceeded the oldest *trace* batches are removed
// first — aggregates are never dropped (spec §1).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type CloudBuffer struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
	dropped  int64 // traces dropped by trimming since the last TakeDropped()
}

type bufferedFile struct {
	Name string
	Kind string // "traces" | "aggregates"
	Size int64
}

func NewCloudBuffer(dir string, maxBytes int64) (*CloudBuffer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	return &CloudBuffer{dir: dir, maxBytes: maxBytes}, nil
}

// Store writes a payload and trims trace batches beyond the limit.
func (b *CloudBuffer) Store(kind, batchID string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	name := fmt.Sprintf("%020d-%s-%s.json", time.Now().UnixNano(), kind, strings.TrimPrefix(batchID, "b_"))
	tmp := filepath.Join(b.dir, name+".tmp")
	if err := os.WriteFile(tmp, payload, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(b.dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	b.trimLocked()
	return nil
}

func (b *CloudBuffer) listLocked() []bufferedFile {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil
	}
	var files []bufferedFile
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		kind := "aggregates"
		if strings.Contains(n, "-traces-") {
			kind = "traces"
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, bufferedFile{Name: n, Kind: kind, Size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files
}

// trimLocked removes the oldest trace batches until the buffer fits the limit.
func (b *CloudBuffer) trimLocked() {
	files := b.listLocked()
	var total int64
	for _, f := range files {
		total += f.Size
	}
	for _, f := range files {
		if total <= b.maxBytes {
			break
		}
		if f.Kind != "traces" {
			continue
		}
		if payload, err := os.ReadFile(filepath.Join(b.dir, f.Name)); err == nil {
			b.dropped += int64(countTraces(payload))
		}
		if os.Remove(filepath.Join(b.dir, f.Name)) == nil {
			total -= f.Size
		}
	}
}

// List returns buffered batches, oldest first.
func (b *CloudBuffer) List() []bufferedFile {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listLocked()
}

func (b *CloudBuffer) Read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.dir, filepath.Base(name)))
}

func (b *CloudBuffer) Remove(name string) {
	os.Remove(filepath.Join(b.dir, filepath.Base(name)))
}

// Stats returns the number of buffered batches and their total size.
func (b *CloudBuffer) Stats() (int, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var total int64
	files := b.listLocked()
	for _, f := range files {
		total += f.Size
	}
	return len(files), total
}

// TakeDropped returns and resets the number of traces dropped by trimming.
func (b *CloudBuffer) TakeDropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.dropped
	b.dropped = 0
	return n
}
