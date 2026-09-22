package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonFlushBatch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	cfg := DefaultDaemonConfig()
	cfg.DBPath = dbPath
	cfg.BatchSize = 3

	d := NewDaemon(cfg)
	d.store = store

	// Add traces to batch
	for i := 0; i < 5; i++ {
		d.batch = append(d.batch, Trace{
			ID:         "test-" + string(rune('a'+i)),
			Ts:         time.Now().Unix(),
			Host:       "example.com",
			Method:     "GET",
			URI:        "/test",
			Status:     200,
			DurationMs: float64(i * 100),
			Level:      "normal",
		})
	}

	d.flushBatch()

	count, err := store.GetTraceCount()
	if err != nil {
		t.Fatalf("GetTraceCount: %v", err)
	}
	if count != 5 {
		t.Errorf("Expected 5 traces stored, got %d", count)
	}
	if d.tracesStored.Load() != 5 {
		t.Errorf("Expected tracesStored=5, got %d", d.tracesStored.Load())
	}
}

func TestDaemonTailJSONL(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	dbPath := filepath.Join(tmpDir, "test.db")
	statePath := filepath.Join(tmpDir, "test.state")

	// Write some test traces
	f, err := os.Create(jsonlPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	traces := []Trace{
		{ID: "t1", Ts: time.Now().Unix(), Host: "a.com", Method: "GET", URI: "/", Status: 200, DurationMs: 10, Level: "summary"},
		{ID: "t2", Ts: time.Now().Unix(), Host: "b.com", Method: "POST", URI: "/api", Status: 201, DurationMs: 50, Level: "normal"},
		{ID: "t3", Ts: time.Now().Unix(), Host: "a.com", Method: "GET", URI: "/slow", Status: 200, DurationMs: 500, Level: "full"},
	}

	enc := json.NewEncoder(f)
	for _, tr := range traces {
		enc.Encode(tr)
	}
	f.Close()

	// Start daemon with short intervals
	cfg := &DaemonConfig{
		JSONLPath:      jsonlPath,
		DBPath:         dbPath,
		PollIntervalMs: 50,
		FlushInterval:  100 * time.Millisecond,
		BatchSize:      10,
		AggInterval:    1 * time.Hour,
		RetentionDays:  30,
		StateFile:      statePath,
		Quiet:          true,
		Mode:           "jsonl",
	}

	daemon := NewDaemon(cfg)

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	daemon.store = store

	// Run JSONL tail in goroutine
	go func() {
		daemon.tailJSONL()
	}()

	// Wait for processing
	time.Sleep(500 * time.Millisecond)

	// Signal stop
	close(daemon.done)
	time.Sleep(200 * time.Millisecond)

	// Flush remaining
	daemon.flushBatch()

	count, err := store.GetTraceCount()
	if err != nil {
		t.Fatalf("GetTraceCount: %v", err)
	}
	if count != 3 {
		t.Errorf("Expected 3 traces, got %d", count)
	}

	store.Close()
}

func TestDaemonStateResume(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "test.state")

	cfg := DefaultDaemonConfig()
	cfg.StateFile = statePath

	d := NewDaemon(cfg)

	// Save state
	d.saveState(12345, 67890)

	// Load state
	state := d.loadState()
	if state.Offset != 12345 {
		t.Errorf("Expected offset=12345, got %d", state.Offset)
	}
	if state.Inode != 67890 {
		t.Errorf("Expected inode=67890, got %d", state.Inode)
	}
	if state.LastRead == 0 {
		t.Error("Expected LastRead to be set")
	}
}

func TestAggregate1m(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	// Insert test traces within a known minute bucket
	bucket := int64(1711612800) // a nice round timestamp
	for i := 0; i < 10; i++ {
		tr := &Trace{
			ID:         "agg-" + string(rune('a'+i)),
			Ts:         bucket + int64(i*5),
			Host:       "test.com",
			Method:     "GET",
			URI:        "/test",
			Status:     200,
			DurationMs: float64((i + 1) * 10), // 10ms to 100ms
			Level:      "normal",
		}
		if i%3 == 0 {
			tr.N1 = 1
		}
		if err := store.StoreTrace(tr); err != nil {
			t.Fatalf("StoreTrace: %v", err)
		}
	}

	// Run aggregation
	if err := store.Aggregate1m(bucket); err != nil {
		t.Fatalf("Aggregate1m: %v", err)
	}

	// Query aggregates
	aggs, err := store.GetAggregates1m(bucket, bucket+60, "test.com")
	if err != nil {
		t.Fatalf("GetAggregates1m: %v", err)
	}

	if len(aggs) != 1 {
		t.Fatalf("Expected 1 aggregate row, got %d", len(aggs))
	}

	a := aggs[0]
	if a.RequestCount != 10 {
		t.Errorf("Expected 10 requests, got %d", a.RequestCount)
	}
	if a.AvgDurationMs < 50 || a.AvgDurationMs > 60 {
		t.Errorf("Expected avg ~55ms, got %.1f", a.AvgDurationMs)
	}
	if a.MaxDurationMs != 100 {
		t.Errorf("Expected max=100ms, got %.1f", a.MaxDurationMs)
	}
	if a.P95DurationMs < 90 {
		t.Errorf("Expected P95 >= 90ms, got %.1f", a.P95DurationMs)
	}
	if a.N1Count != 4 { // i=0,3,6,9
		t.Errorf("Expected 4 N+1, got %d", a.N1Count)
	}
}

func TestConfigLoad(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "test.toml")

	content := `# Test config
[input]
jsonl_path = "/custom/path.jsonl"
shm_path = "/dev/shm/custom"

[storage]
db_path = "/custom/traces.db"

[collector]
poll_interval_ms = 200
flush_interval_s = 10
batch_size = 100
agg_interval_s = 120
mode = "jsonl"

[retention]
days = 7

[logging]
level = "debug"
quiet = true
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Input.JSONLPath != "/custom/path.jsonl" {
		t.Errorf("jsonl_path: got %s", cfg.Input.JSONLPath)
	}
	if cfg.Storage.DBPath != "/custom/traces.db" {
		t.Errorf("db_path: got %s", cfg.Storage.DBPath)
	}
	if cfg.Collector.PollIntervalMs != 200 {
		t.Errorf("poll_interval_ms: got %d", cfg.Collector.PollIntervalMs)
	}
	if cfg.Collector.BatchSize != 100 {
		t.Errorf("batch_size: got %d", cfg.Collector.BatchSize)
	}
	if cfg.Collector.Mode != "jsonl" {
		t.Errorf("mode: got %s", cfg.Collector.Mode)
	}
	if cfg.Retention.Days != 7 {
		t.Errorf("retention days: got %d", cfg.Retention.Days)
	}
	if !cfg.Logging.Quiet {
		t.Error("Expected quiet=true")
	}

	// Test conversion
	dc := cfg.ToDaemonConfig()
	if dc.JSONLPath != "/custom/path.jsonl" {
		t.Errorf("DaemonConfig JSONLPath: got %s", dc.JSONLPath)
	}
	if dc.BatchSize != 100 {
		t.Errorf("DaemonConfig BatchSize: got %d", dc.BatchSize)
	}
	if dc.RetentionDays != 7 {
		t.Errorf("DaemonConfig RetentionDays: got %d", dc.RetentionDays)
	}
	if dc.Mode != "jsonl" {
		t.Errorf("DaemonConfig Mode: got %s", dc.Mode)
	}
}

func TestConfigLoadWithServerSection(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "test.toml")

	content := `[input]
jsonl_path = "/tmp/phpray.jsonl"
shm_path = "/dev/shm/phpray"

[storage]
db_path = "/tmp/test.db"

[collector]
mode = "auto"

[retention]
days = 14

[logging]
level = "info"

[auth]
secret = "test-secret"

[server]
da_data_path = "/custom/da/users"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Auth.Secret != "test-secret" {
		t.Errorf("auth.secret: got %s", cfg.Auth.Secret)
	}
	if cfg.Server.DADataPath != "/custom/da/users" {
		t.Errorf("server.da_data_path: got %s, want /custom/da/users", cfg.Server.DADataPath)
	}
}

func TestDaemonAddToBatch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	cfg := DefaultDaemonConfig()
	cfg.DBPath = dbPath
	cfg.BatchSize = 3

	d := NewDaemon(cfg)
	d.store = store

	// Add 2 traces — shouldn't flush yet
	for i := 0; i < 2; i++ {
		d.addToBatch(Trace{
			ID: "x", Ts: time.Now().Unix(), Host: "test.com",
			Method: "GET", URI: "/", Status: 200, DurationMs: 1, Level: "summary",
		})
	}

	count, _ := store.GetTraceCount()
	if count != 0 {
		t.Errorf("Expected 0 stored (batch not full), got %d", count)
	}

	// Add 1 more — should auto-flush (batch size = 3)
	d.addToBatch(Trace{
		ID: "x", Ts: time.Now().Unix(), Host: "test.com",
		Method: "GET", URI: "/", Status: 200, DurationMs: 1, Level: "summary",
	})

	// Give a moment for flush
	time.Sleep(50 * time.Millisecond)

	count, _ = store.GetTraceCount()
	if count != 3 {
		t.Errorf("Expected 3 stored after auto-flush, got %d", count)
	}
}
