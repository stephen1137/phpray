package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Several ring buffers at once. With `[input] shm_path = "/run/phpray/ring-*"`
// (the extension's phpray.shm_path with %u → one file per PHP-FPM pool uid) the
// collector rescans the pattern every ringScanInterval, opens rings that appeared,
// drops rings whose file went away and reads all of them in one loop.

const defaultRingScanInterval = 5 * time.Second

// isGlobPattern reports whether an shm_path names a set of files rather than one.
func isGlobPattern(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// ringSource is one open ring buffer of the set.
type ringSource struct {
	path      string
	ring      *RingReader
	inode     uint64
	lastDrops uint64 // drop counter at the last overflow check
}

// ringSet is the reconciled set of rings for a pattern. rescan/closeAll run on
// the reader goroutine; status() may be called from the API goroutine.
type ringSet struct {
	pattern string
	mu      sync.Mutex
	rings   map[string]*ringSource
	lastErr map[string]string // last open error logged per path (avoid a log line per scan)
}

func newRingSet(pattern string) *ringSet {
	return &ringSet{
		pattern: pattern,
		rings:   make(map[string]*ringSource),
		lastErr: make(map[string]string),
	}
}

// rescan globs the pattern and reconciles the set: opens new files (and files
// recreated with a new inode), closes the ones that disappeared.
func (s *ringSet) rescan() (added, removed int) {
	matches, err := filepath.Glob(s.pattern)
	if err != nil {
		log.Printf("Ring buffer pattern %q: %v", s.pattern, err)
		return 0, 0
	}
	sort.Strings(matches)

	s.mu.Lock()
	defer s.mu.Unlock()

	present := make(map[string]bool, len(matches))
	for _, path := range matches {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		present[path] = true
		ino := fileInode(st)
		if src, ok := s.rings[path]; ok {
			if src.inode == ino {
				continue
			}
			log.Printf("Ring buffer %s was recreated — reopening", path)
			src.ring.Close()
			delete(s.rings, path)
			removed++
		}
		ring, err := OpenRing(path)
		if err != nil {
			if s.lastErr[path] != err.Error() { // e.g. still being initialised by the first PHP worker
				log.Printf("Ring buffer %s not usable yet: %v", path, err)
				s.lastErr[path] = err.Error()
			}
			continue
		}
		delete(s.lastErr, path)
		records, drops, fill := ring.Stats()
		s.rings[path] = &ringSource{path: path, ring: ring, inode: ino, lastDrops: drops}
		added++
		log.Printf("✅ Ring buffer added: %s (v%d, %d records, %d drops, %.1f%% full) — %d open",
			path, ring.Version(), records, drops, fill, len(s.rings))
	}
	for path, src := range s.rings {
		if present[path] {
			continue
		}
		src.ring.Close()
		delete(s.rings, path)
		removed++
		log.Printf("Ring buffer removed: %s (file gone) — %d open", path, len(s.rings))
	}
	for path := range s.lastErr {
		if !present[path] {
			delete(s.lastErr, path)
		}
	}
	return added, removed
}

// sources returns the open rings in path order (a snapshot for the reader loop).
func (s *ringSet) sources() []*ringSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*ringSource, 0, len(s.rings))
	for _, src := range s.rings {
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// paths lists the open ring files in order.
func (s *ringSet) paths() []string {
	srcs := s.sources()
	out := make([]string, len(srcs))
	for i, src := range srcs {
		out[i] = src.path
	}
	return out
}

// status snapshots every open ring for /api/v1/health.
func (s *ringSet) status() []RingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RingStatus, 0, len(s.rings))
	for path, src := range s.rings {
		records, drops, fill := src.ring.Stats()
		out = append(out, RingStatus{Path: path, Version: src.ring.Version(), Records: records, Drops: drops, FillPct: fill})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// closeAll unmaps every ring (shutdown).
func (s *ringSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, src := range s.rings {
		src.ring.Close()
		delete(s.rings, path)
	}
}

// ─── Daemon side ────────────────────────────────────────────────────────────

// ringSetWorker reads every ring matching the shm_path glob and rescans the
// pattern every ringScanInterval for pools that started or stopped.
func (d *Daemon) ringSetWorker() {
	defer d.wg.Done()

	set := d.rings
	pollInterval := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond
	scanEvery := d.ringScanInterval
	if scanEvery <= 0 {
		scanEvery = defaultRingScanInterval
	}
	log.Printf("   Rings:     %s (rescan every %v)", set.pattern, scanEvery)

	var lastScan time.Time
	for {
		select {
		case <-d.done:
			set.closeAll()
			return
		default:
		}

		if time.Since(lastScan) >= scanEvery {
			set.rescan()
			lastScan = time.Now()
		}

		read := 0
		for _, src := range set.sources() {
			read += d.drainRing(src.ring, 100)
		}
		d.checkRingSetDrops(set)

		if read == 0 {
			select {
			case <-d.done:
				set.closeAll()
				return
			case <-time.After(pollInterval):
			}
		}
	}
}

// checkRingSetDrops logs overflow per ring (at most every 10 seconds).
func (d *Daemon) checkRingSetDrops(set *ringSet) {
	if time.Since(d.lastDropCheck) < 10*time.Second {
		return
	}
	d.lastDropCheck = time.Now()
	for _, src := range set.sources() {
		_, drops, fillPct := src.ring.Stats()
		if drops > src.lastDrops {
			newDrops := drops - src.lastDrops
			d.ringDrops.Add(int64(newDrops))
			log.Printf("⚠️ Ring buffer overflow in %s: %d traces dropped (total: %d, buffer %.1f%% full)",
				src.path, newDrops, drops, fillPct)
		}
		src.lastDrops = drops
	}
}
