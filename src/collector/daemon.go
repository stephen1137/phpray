package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DaemonConfig holds all configuration for the daemon
type DaemonConfig struct {
	// Input sources
	JSONLPath string
	SHMPath   string

	// Storage
	DBPath string

	// Behavior
	PollIntervalMs int
	FlushInterval  time.Duration
	BatchSize      int
	AggInterval    time.Duration
	RetentionDays  int
	RetentionMaxMB int // 0 = no size cap

	// State
	StateFile string

	// Logging
	LogLevel string
	Quiet    bool
	Verbose  bool

	// Mode: "auto" (try ring buffer, fallback JSONL), "ring", "jsonl"
	Mode string

	// Cloud shipping ([cloud] in collector.toml)
	Cloud CloudConfig

	// Shared-memory control table (input.control_path; "" = disabled)
	ControlPath string
}

// DefaultDaemonConfig returns sensible defaults
func DefaultDaemonConfig() *DaemonConfig {
	return &DaemonConfig{
		JSONLPath:      "/tmp/phpray.jsonl",
		SHMPath:        "/dev/shm/phpray",
		DBPath:         "/var/lib/phpray/traces.db",
		PollIntervalMs: 100,
		FlushInterval:  5 * time.Second,
		BatchSize:      50,
		AggInterval:    60 * time.Second,
		RetentionDays:  30,
		RetentionMaxMB: 4096,
		StateFile:      "/var/lib/phpray/collector.state",
		LogLevel:       "info",
		Mode:           "auto",
	}
}

// DaemonState tracks persistent reading position for JSONL mode
type DaemonState struct {
	Offset   int64  `json:"offset"`
	Inode    uint64 `json:"inode"`
	LastRead int64  `json:"last_read"`
}

// Daemon is the main collector service
type Daemon struct {
	cfg   *DaemonConfig
	store *Storage
	hub   *Hub

	// Atomic stats
	tracesRead   atomic.Int64
	tracesStored atomic.Int64
	tracesFailed atomic.Int64
	bytesRead    atomic.Int64
	ringDrops    atomic.Int64

	// Batch buffer
	batch []Trace
	mu    sync.Mutex

	// Aggregation state
	lastAggBucket int64

	// Ring buffer drop monitoring
	lastDropCount uint64
	lastDropCheck time.Time

	// Cloud shipper (nil when [cloud] enabled = false)
	shipper *Shipper

	// Control table (nil when disabled or unavailable)
	control *ControlTable

	// Ring buffers matched by an shm_path glob (nil for a single ring path) — ringset.go
	rings            *ringSet
	ringScanInterval time.Duration // how often the glob is rescanned (0 = 5 s)

	// Lifecycle
	done chan struct{}
	wg   sync.WaitGroup
}

// NewDaemon creates a new daemon instance
func NewDaemon(cfg *DaemonConfig) *Daemon {
	d := &Daemon{
		cfg:   cfg,
		batch: make([]Trace, 0, cfg.BatchSize),
		done:  make(chan struct{}),
	}
	if isGlobPattern(cfg.SHMPath) {
		d.rings = newRingSet(cfg.SHMPath)
	}
	return d
}

// startReader launches the input goroutine for the configured mode: JSONL tail,
// one ring buffer, or every ring matching an shm_path glob (ring/auto modes).
func (d *Daemon) startReader() {
	d.wg.Add(1)
	switch {
	case d.cfg.Mode == "jsonl":
		go d.jsonlWorker()
	case d.rings != nil:
		go d.ringSetWorker()
	case d.cfg.Mode == "ring":
		go d.ringWorker()
	default: // "auto"
		go d.autoWorker()
	}
}

// IngestTrace feeds a trace received on POST /api/v1/ingest through the same
// path as ring/JSONL traces (live hub, cloud shipper, SQLite batch).
func (d *Daemon) IngestTrace(t Trace) {
	d.tracesRead.Add(1)
	d.addToBatch(t)
}

// inputStatus reports the configured input and the rings currently open.
func (d *Daemon) inputStatus() InputStatus {
	st := InputStatus{Mode: d.cfg.Mode, SHMPath: d.cfg.SHMPath,
		JSONLPath: d.cfg.JSONLPath, Glob: d.rings != nil}
	if d.rings != nil {
		st.Rings = d.rings.status()
	}
	return st
}

// Run starts the daemon — blocks until signal
func (d *Daemon) Run() error {
	log.Printf("🔦 PHPRay Collector Daemon v%s", version)
	log.Printf("   Mode:      %s", d.cfg.Mode)
	log.Printf("   Database:  %s", d.cfg.DBPath)
	log.Printf("   Batch:     %d / %v flush", d.cfg.BatchSize, d.cfg.FlushInterval)
	log.Printf("   Agg:       every %v", d.cfg.AggInterval)
	log.Printf("   Retention: %d days, cap %d MB", d.cfg.RetentionDays, d.cfg.RetentionMaxMB)

	// Open storage
	var err error
	d.store, err = OpenStorage(d.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer d.store.Close()

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Cloud shipper
	d.startShipper()

	// Start workers
	d.wg.Add(2) // flush + agg
	go d.flushWorker()
	go d.aggWorker()

	// Start reader based on mode (JSONL, one ring, or every ring matching a glob)
	d.startReader()

	log.Printf("   Collecting... (send SIGTERM to stop)")

	// Wait for signal
	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				log.Printf("SIGHUP — reloading (no-op for now)")
				continue
			default:
				log.Printf("Signal %v — graceful shutdown", sig)
				close(d.done)
				d.wg.Wait()
				d.flushBatch()
				d.stopShipper()
				d.logStats()
				log.Printf("Daemon stopped cleanly")
				return nil
			}
		}
	}
}

// ─── Ring Buffer Worker ─────────────────────────────────────────────────────

func (d *Daemon) ringWorker() {
	defer d.wg.Done()

	var ring *RingReader
	var err error
	pollInterval := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond

	for {
		select {
		case <-d.done:
			if ring != nil {
				ring.Close()
			}
			return
		default:
		}

		// Connect to ring buffer
		if ring == nil {
			ring, err = OpenRing(d.cfg.SHMPath)
			if err != nil {
				select {
				case <-d.done:
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			records, drops, fill := ring.Stats()
			log.Printf("✅ Ring buffer connected: %d records, %d drops, %.1f%% full",
				records, drops, fill)
		}

		// Read up to 100 records per cycle
		batchCount := d.drainRing(ring, 100)

		// Monitor ring buffer drops (at most every 10 seconds)
		d.checkRingDrops(ring)

		if batchCount == 0 {
			select {
			case <-d.done:
				if ring != nil {
					ring.Close()
				}
				return
			case <-time.After(pollInterval):
			}
		}
	}
}

// drainRing reads up to max records from one ring buffer into the batch and
// returns how many records (of any type) it consumed.
func (d *Daemon) drainRing(ring *RingReader, max int) int {
	n := 0
	for n < max {
		recType, payload, ok := ring.ReadRecord()
		if !ok {
			break
		}
		n++
		d.tracesRead.Add(1)

		if recType != recordTypTrace {
			continue
		}

		trace, err := DeserializeTrace(payload, ring.Version())
		if err != nil {
			d.tracesFailed.Add(1)
			if d.cfg.Verbose {
				log.Printf("Deserialize error: %v", err)
			}
			continue
		}

		d.addToBatch(*trace)
	}
	return n
}

// ─── JSONL Tail Worker ──────────────────────────────────────────────────────

func (d *Daemon) jsonlWorker() {
	defer d.wg.Done()

	for {
		select {
		case <-d.done:
			return
		default:
		}

		if err := d.tailJSONL(); err != nil {
			log.Printf("JSONL tail error: %v — retrying in 5s", err)
			select {
			case <-d.done:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (d *Daemon) tailJSONL() error {
	f, err := os.Open(d.cfg.JSONLPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", d.cfg.JSONLPath, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	currentInode := fileInode(fi)

	// Resume from saved position
	state := d.loadState()
	if state.Inode == currentInode && state.Offset > 0 {
		if state.Offset <= fi.Size() {
			f.Seek(state.Offset, io.SeekStart)
			log.Printf("Resumed JSONL from offset %d", state.Offset)
		} else {
			log.Printf("JSONL truncated (%d→%d) — reading from start", state.Offset, fi.Size())
		}
	} else if state.Inode != 0 && state.Inode != currentInode {
		log.Printf("JSONL rotated (inode %d→%d) — reading from start", state.Inode, currentInode)
	}

	reader := bufio.NewReaderSize(f, 256*1024)
	pollInterval := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond
	lastSave := time.Now()

	for {
		select {
		case <-d.done:
			offset, _ := f.Seek(0, io.SeekCurrent)
			d.saveState(offset, currentInode)
			return nil
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read: %w", err)
			}

			// Check for file rotation
			newFi, statErr := os.Stat(d.cfg.JSONLPath)
			if statErr != nil {
				select {
				case <-d.done:
					return nil
				case <-time.After(time.Second):
				}
				continue
			}
			if fileInode(newFi) != currentInode {
				offset, _ := f.Seek(0, io.SeekCurrent)
				d.saveState(offset, currentInode)
				log.Printf("JSONL rotated — reopening")
				return nil
			}

			// Wait for more data
			select {
			case <-d.done:
				offset, _ := f.Seek(0, io.SeekCurrent)
				d.saveState(offset, currentInode)
				return nil
			case <-time.After(pollInterval):
			}

			// Periodic state save
			if time.Since(lastSave) > 10*time.Second {
				offset, _ := f.Seek(0, io.SeekCurrent)
				d.saveState(offset, currentInode)
				lastSave = time.Now()
			}
			continue
		}

		d.bytesRead.Add(int64(len(line)))

		line = trimRight(line)
		if len(line) == 0 {
			continue
		}

		var t Trace
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			d.tracesFailed.Add(1)
			continue
		}

		d.tracesRead.Add(1)
		d.addToBatch(t)
	}
}

// ─── Auto Worker (ring buffer preferred, JSONL fallback) ────────────────────

func (d *Daemon) autoWorker() {
	defer d.wg.Done()

	// Try ring buffer first
	ring, err := OpenRing(d.cfg.SHMPath)
	if err == nil {
		records, drops, fill := ring.Stats()
		log.Printf("✅ Ring buffer connected: %d records, %d drops, %.1f%% full", records, drops, fill)
		log.Printf("   Reading from ring buffer (%s)", d.cfg.SHMPath)

		// Run ring buffer reader inline
		pollInterval := time.Duration(d.cfg.PollIntervalMs) * time.Millisecond
		for {
			select {
			case <-d.done:
				ring.Close()
				return
			default:
			}

			batchCount := d.drainRing(ring, 100)

			// Monitor ring buffer drops (at most every 10 seconds)
			d.checkRingDrops(ring)

			if batchCount == 0 {
				select {
				case <-d.done:
					ring.Close()
					return
				case <-time.After(pollInterval):
				}
			}
		}
	}

	// Ring buffer not available — fall back to JSONL
	log.Printf("⚠️  Ring buffer not available (%v) — falling back to JSONL", err)
	log.Printf("   Reading from %s", d.cfg.JSONLPath)

	// Na świeżej instalacji pliku JSONL jeszcze nie ma: PHP zapisze go dopiero
	// przy pierwszym żądaniu. To jest stan normalny, a nie błąd — a przez to,
	// że pętla ponawia co 5 s, pierwszą rzeczą, jaką nowy użytkownik widział
	// w dzienniku swojej świeżej instalacji, było powtarzane "JSONL error".
	// Mówimy o tym raz, spokojnie, i dopiero prawdziwy błąd krzyczy.
	brakZgloszony := false
	for {
		select {
		case <-d.done:
			return
		default:
		}

		if err := d.tailJSONL(); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if !brakZgloszony {
					log.Printf("   Waiting for %s — PHP has not written a trace yet. "+
						"This is normal on a fresh install; make one request to your site.", d.cfg.JSONLPath)
					brakZgloszony = true
				}
			} else {
				brakZgloszony = false
				log.Printf("JSONL error: %v — retrying in 5s", err)
			}
			select {
			case <-d.done:
				return
			case <-time.After(5 * time.Second):
			}
		} else {
			brakZgloszony = false
		}
	}
}

// ─── Batch Management ───────────────────────────────────────────────────────

func (d *Daemon) addToBatch(t Trace) {
	if d.hub != nil {
		d.hub.Broadcast(t)
	}
	if d.shipper != nil {
		d.shipper.Observe(&t)
	}

	d.mu.Lock()
	d.batch = append(d.batch, t)
	shouldFlush := len(d.batch) >= d.cfg.BatchSize
	d.mu.Unlock()

	if shouldFlush {
		d.flushBatch()
	}
}

func (d *Daemon) flushBatch() {
	d.mu.Lock()
	if len(d.batch) == 0 {
		d.mu.Unlock()
		return
	}
	batch := d.batch
	d.batch = make([]Trace, 0, d.cfg.BatchSize)
	d.mu.Unlock()

	stored, err := d.store.StoreBatch(batch)
	if err != nil {
		log.Printf("Batch store error: %v", err)
	}
	d.tracesStored.Add(int64(stored))
	if stored != len(batch) {
		d.tracesFailed.Add(int64(len(batch) - stored))
	}
}

// ─── Flush & Aggregation Workers ────────────────────────────────────────────

func (d *Daemon) flushWorker() {
	defer d.wg.Done()

	flushTicker := time.NewTicker(d.cfg.FlushInterval)
	defer flushTicker.Stop()

	statsTicker := time.NewTicker(60 * time.Second)
	defer statsTicker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-flushTicker.C:
			d.flushBatch()
		case <-statsTicker.C:
			if !d.cfg.Quiet {
				d.logStats()
			}
		}
	}
}

func (d *Daemon) aggWorker() {
	defer d.wg.Done()

	aggTicker := time.NewTicker(d.cfg.AggInterval)
	defer aggTicker.Stop()

	retentionTicker := time.NewTicker(time.Hour)
	defer retentionTicker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-aggTicker.C:
			d.runAggregation()
		case <-retentionTicker.C:
			d.runRetention()
		}
	}
}

func (d *Daemon) runAggregation() {
	now := time.Now().Unix()
	bucket := ((now / 60) * 60) - 60 // previous completed minute

	if bucket <= d.lastAggBucket {
		return
	}

	if err := d.store.Aggregate1m(bucket); err != nil {
		log.Printf("Aggregation error (bucket %d): %v", bucket, err)
		return
	}

	d.lastAggBucket = bucket
	if d.cfg.Verbose {
		log.Printf("Aggregated bucket %d (%s)", bucket,
			time.Unix(bucket, 0).Format("15:04"))
	}

	// Every aggregation cycle, check alert rules
	d.checkAlerts()
}

// checkAlerts evaluates default alert rules and stores any new alerts
func (d *Daemon) checkAlerts() {
	rules := DefaultAlertRules()
	events := EvaluateAlerts(d.store, rules)

	for _, event := range events {
		id, err := d.store.StoreAlert(event)
		if err != nil {
			log.Printf("Alert store error: %v", err)
			continue
		}
		switch event.Severity {
		case "critical":
			log.Printf("🚨 CRITICAL ALERT #%d: %s", id, event.Message)
		case "warning":
			log.Printf("⚠️ WARNING ALERT #%d: %s", id, event.Message)
		default:
			log.Printf("ℹ️ ALERT #%d: %s", id, event.Message)
		}
	}
}

func (d *Daemon) runRetention() {
	d.enforceSizeCap()
	if d.cfg.RetentionDays <= 0 {
		return
	}

	maxAge := time.Duration(d.cfg.RetentionDays) * 24 * time.Hour

	deleted, err := d.store.PurgeOld(maxAge)
	if err != nil {
		log.Printf("Retention error: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("Retention: purged %d traces (>%d days)", deleted, d.cfg.RetentionDays)
	}

	aggDeleted, err := d.store.PurgeOldAggregates(maxAge)
	if err != nil {
		log.Printf("Aggregate retention error: %v", err)
	} else if aggDeleted > 0 {
		log.Printf("Retention: purged %d aggregate rows", aggDeleted)
	}
}

// ─── Ring Buffer Drop Monitoring ────────────────────────────────────────────

func (d *Daemon) checkRingDrops(ring *RingReader) {
	if time.Since(d.lastDropCheck) < 10*time.Second {
		return
	}
	d.lastDropCheck = time.Now()

	_, drops, fillPct := ring.Stats()
	if drops > d.lastDropCount {
		newDrops := drops - d.lastDropCount
		d.ringDrops.Add(int64(newDrops))
		log.Printf("⚠️ Ring buffer overflow: %d traces dropped (total: %d, buffer %.1f%% full)",
			newDrops, drops, fillPct)
	}
	d.lastDropCount = drops
}

// ─── Stats & State ──────────────────────────────────────────────────────────

func (d *Daemon) logStats() {
	read := d.tracesRead.Load()
	stored := d.tracesStored.Load()
	failed := d.tracesFailed.Load()
	bytes := d.bytesRead.Load()
	drops := d.ringDrops.Load()

	count, _ := d.store.GetTraceCount()

	log.Printf("📊 read=%d stored=%d failed=%d drops=%d bytes=%.1fMB db_total=%d",
		read, stored, failed, drops, float64(bytes)/1024/1024, count)
}

func (d *Daemon) loadState() DaemonState {
	var state DaemonState
	data, err := os.ReadFile(d.cfg.StateFile)
	if err != nil {
		return state
	}
	json.Unmarshal(data, &state)
	return state
}

func (d *Daemon) saveState(offset int64, inode uint64) {
	state := DaemonState{
		Offset:   offset,
		Inode:    inode,
		LastRead: time.Now().Unix(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	dir := filepath.Dir(d.cfg.StateFile)
	os.MkdirAll(dir, 0755)
	os.WriteFile(d.cfg.StateFile, data, 0644)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func fileInode(fi os.FileInfo) uint64 {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ino
}

func trimRight(s string) string {
	for len(s) > 0 {
		ch := s[len(s)-1]
		if ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t' {
			s = s[:len(s)-1]
		} else {
			break
		}
	}
	return s
}

// ─── CLI Entry Point ────────────────────────────────────────────────────────

func cmdDaemonRun(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("c", "", "Config file path (TOML)")
	jsonlPath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	shmPath := fs.String("shm", "/dev/shm/phpray", "Shared memory ring buffer path")
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	pollMs := fs.Int("poll", 100, "Poll interval (ms)")
	flushSec := fs.Int("flush", 5, "Flush interval (seconds)")
	batchSize := fs.Int("batch", 50, "Batch size")
	retentionDays := fs.Int("retain", 30, "Retention (days)")
	mode := fs.String("mode", "auto", "Input mode: auto, ring, jsonl")
	quiet := fs.Bool("q", false, "Suppress periodic stats")
	verbose := fs.Bool("v", false, "Verbose output")
	fs.Parse(args)

	var cfg *DaemonConfig

	if *configPath != "" {
		fileCfg, err := LoadConfig(*configPath)
		if err != nil {
			log.Fatalf("Config error: %v", err)
		}
		cfg = fileCfg.ToDaemonConfig()
		log.Printf("Config loaded: %s", *configPath)
	} else {
		cfg = &DaemonConfig{
			JSONLPath:      *jsonlPath,
			SHMPath:        *shmPath,
			DBPath:         *dbPath,
			PollIntervalMs: *pollMs,
			FlushInterval:  time.Duration(*flushSec) * time.Second,
			BatchSize:      *batchSize,
			AggInterval:    60 * time.Second,
			RetentionDays:  *retentionDays,
			RetentionMaxMB: 4096,
			StateFile:      *dbPath + ".state",
			LogLevel:       "info",
			Quiet:          *quiet,
			Verbose:        *verbose,
			Mode:           *mode,
		}
	}

	daemon := NewDaemon(cfg)
	if err := daemon.Run(); err != nil {
		log.Fatalf("Daemon error: %v", err)
	}
}

// cmdServe runs daemon + API in the same process with a shared WebSocket hub
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("c", "", "Config file path (TOML)")
	jsonlPath := fs.String("f", "/tmp/phpray.jsonl", "JSONL file path")
	shmPath := fs.String("shm", "/dev/shm/phpray", "Shared memory ring buffer path")
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	addr := fs.String("addr", ":9191", "API listen address")
	pollMs := fs.Int("poll", 100, "Poll interval (ms)")
	flushSec := fs.Int("flush", 5, "Flush interval (seconds)")
	batchSize := fs.Int("batch", 50, "Batch size")
	retentionDays := fs.Int("retain", 30, "Retention (days)")
	mode := fs.String("mode", "auto", "Input mode: auto, ring, jsonl")
	quiet := fs.Bool("q", false, "Suppress periodic stats")
	verbose := fs.Bool("v", false, "Verbose output")
	jwtSecret := fs.String("secret", "", "JWT secret (or set PHPRAY_JWT_SECRET)")
	fs.Parse(args)

	var cfg *DaemonConfig
	var jwtSec string
	var daDataPath string
	if *configPath != "" {
		fileCfg, err := LoadConfig(*configPath)
		if err != nil {
			log.Fatalf("Config error: %v", err)
		}
		cfg = fileCfg.ToDaemonConfig()
		jwtSec = fileCfg.ResolveAuthSecret()
		daDataPath = fileCfg.Server.DADataPath
		log.Printf("Config loaded: %s", *configPath)
	} else {
		cfg = &DaemonConfig{
			JSONLPath:      *jsonlPath,
			SHMPath:        *shmPath,
			DBPath:         *dbPath,
			PollIntervalMs: *pollMs,
			FlushInterval:  time.Duration(*flushSec) * time.Second,
			BatchSize:      *batchSize,
			AggInterval:    60 * time.Second,
			RetentionDays:  *retentionDays,
			RetentionMaxMB: 4096,
			StateFile:      *dbPath + ".state",
			LogLevel:       "info",
			Quiet:          *quiet,
			Verbose:        *verbose,
			Mode:           *mode,
		}
		jwtSec = *jwtSecret
		if jwtSec == "" {
			jwtSec = os.Getenv("PHPRAY_JWT_SECRET")
		}
	}

	// Shared hub for live WS streaming
	hub := NewHub()

	// Open storage (shared between daemon and API)
	store, err := OpenStorage(cfg.DBPath)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer store.Close()

	// Initialize domain→user mapping (DirectAdmin)
	store.InitDomainResolver(daDataPath)

	// Wire user resolver to hub for WebSocket username enrichment
	hub.users = store.users
	go hub.Run()

	// Start API server in background
	api := NewAPI(store, hub, jwtSec)

	// Create daemon and wire its metrics, cloud status and HTTP ingest into the API
	daemon := NewDaemon(cfg)
	api.metrics = &DaemonMetrics{daemon: daemon}
	api.cloud = &DaemonCloud{daemon: daemon}
	api.sink = daemon // POST /api/v1/ingest → same path as ring/JSONL traces

	go func() {
		log.Printf("🌐 API server listening on %s", *addr)
		if err := http.ListenAndServe(*addr, api.router); err != nil {
			log.Fatalf("API server error: %v", err)
		}
	}()

	// Run daemon (blocks until signal)
	daemon.hub = hub
	daemon.store = store
	// Run inline — daemon.Run() opens its own storage, so we run the workers directly
	log.Printf("🔦 PHPRay Serve v%s (daemon + API + WebSocket)", version)
	log.Printf("   Mode:      %s", cfg.Mode)
	log.Printf("   Database:  %s", cfg.DBPath)
	log.Printf("   API:       %s", *addr)
	if jwtSec != "" {
		log.Printf("   Auth:      JWT enabled (HS256)")
		log.Printf("   Ingest:    POST %s/api/v1/ingest (Bearer token required)", *addr)
	} else {
		log.Printf("   Auth:      disabled (no secret configured)")
		log.Printf("   Ingest:    POST %s/api/v1/ingest (loopback clients only)", *addr)
	}
	log.Printf("   Batch:     %d / %v flush", cfg.BatchSize, cfg.FlushInterval)
	log.Printf("   Retention: %d days", cfg.RetentionDays)

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Cloud shipper (serve mode runs the workers inline, so start it here too)
	daemon.startShipper()

	// Start workers
	daemon.wg.Add(2)
	go daemon.flushWorker()
	go daemon.aggWorker()

	daemon.startReader()

	log.Printf("   Collecting... (send SIGTERM to stop)")

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			log.Printf("SIGHUP — reloading (no-op for now)")
			continue
		default:
			log.Printf("Signal %v — graceful shutdown", sig)
			close(daemon.done)
			daemon.wg.Wait()
			daemon.flushBatch()
			daemon.stopShipper()
			daemon.logStats()
			log.Printf("Serve stopped cleanly")
			return
		}
	}
}

// startShipper opens the control table and starts cloud shipping when [cloud] enabled = true
// (daemon and serve modes).
func (d *Daemon) startShipper() {
	if d.cfg.ControlPath != "" && d.control == nil {
		ct, err := OpenControlTable(d.cfg.ControlPath)
		if err != nil {
			log.Printf("Control table disabled: %v", err)
		} else {
			d.control = ct
			log.Printf("   Control:   %s (%d entries)", d.cfg.ControlPath, len(ct.List()))
		}
	}
	if !d.cfg.Cloud.Enabled || d.shipper != nil {
		return
	}
	sh, err := NewShipper(d.cfg.Cloud)
	if err != nil {
		log.Printf("Cloud shipping disabled: %v", err)
		return
	}
	sh.control = d.control
	d.shipper = sh
	sh.Start()
	log.Printf("   Cloud:     %s (traces sample %d %%, buffer %d MB)", d.cfg.Cloud.Endpoint, d.cfg.Cloud.TraceSampleRate, d.cfg.Cloud.BufferMaxMB)
}

// stopShipper flushes and stops cloud shipping (no-op when disabled).
func (d *Daemon) stopShipper() {
	if d.shipper != nil {
		d.shipper.Stop()
	}
	if d.control != nil {
		d.control.Close()
		d.control = nil
	}
}

// DaemonCloud exposes the daemon's shipper status to the API (nil shipper = disabled).
type DaemonCloud struct {
	daemon *Daemon
}

func (c *DaemonCloud) CloudStatus() CloudStatus {
	if c.daemon == nil || c.daemon.shipper == nil {
		st := CloudStatus{Enabled: c.daemon != nil && c.daemon.cfg.Cloud.Enabled}
		if st.Enabled {
			st.LastError = "shipper not started"
		}
		return st
	}
	return c.daemon.shipper.CloudStatus()
}

// enforceSizeCap keeps the SQLite file under retention.max_mb by purging the
// oldest traces (10 % of rows per pass, a few passes at most). Freed pages are
// reused, so the file stops growing; it is not shrunk in place.
func (d *Daemon) enforceSizeCap() {
	if d.cfg.RetentionMaxMB <= 0 {
		return
	}
	limit := int64(d.cfg.RetentionMaxMB) << 20
	for pass := 0; pass < 5; pass++ {
		size, err := d.store.DBSizeBytes()
		if err != nil || size <= limit {
			return
		}
		deleted, err := d.store.PurgeOldestFraction(0.10)
		if err != nil || deleted == 0 {
			if err != nil {
				log.Printf("Retention (size cap) error: %v", err)
			}
			return
		}
		log.Printf("Retention: database %d MB over the %d MB cap, purged %d oldest traces", size>>20, d.cfg.RetentionMaxMB, deleted)
	}
}
