package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	// Insert some test traces
	for i := 0; i < 5; i++ {
		tr := &Trace{
			ID:         "m-" + string(rune('a'+i)),
			Ts:         time.Now().Unix(),
			Host:       "example.com",
			Method:     "GET",
			URI:        "/test",
			Status:     200,
			DurationMs: float64((i + 1) * 100),
			Level:      "normal",
		}
		if i == 4 {
			tr.Status = 500
			tr.Level = "alert"
		}
		if i == 3 {
			tr.N1 = 1
		}
		if err := store.StoreTrace(tr); err != nil {
			t.Fatalf("StoreTrace: %v", err)
		}
	}

	api := NewAPI(store, nil, "")

	// Simulate daemon counters
	cfg := DefaultDaemonConfig()
	d := NewDaemon(cfg)
	d.tracesRead.Store(100)
	d.tracesStored.Store(95)
	d.tracesFailed.Store(5)
	d.bytesRead.Store(1024000)
	api.metrics = &DaemonMetrics{daemon: d}

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Expected text/plain content type, got %s", ct)
	}

	body := rec.Body.String()

	// Check daemon counters
	requireMetric(t, body, "phpray_traces_read_total", "100")
	requireMetric(t, body, "phpray_traces_stored_total", "95")
	requireMetric(t, body, "phpray_traces_failed_total", "5")
	requireMetric(t, body, "phpray_bytes_read_total", "1024000")

	// Check process metrics
	requireContains(t, body, "phpray_uptime_seconds")
	requireContains(t, body, "phpray_info{")
	requireContains(t, body, version)

	// Check DB metrics
	requireContains(t, body, "phpray_requests_total")
	requireContains(t, body, "phpray_domains_active")
	requireContains(t, body, "phpray_avg_duration_ms")
	requireContains(t, body, "phpray_max_duration_ms")
	requireContains(t, body, "phpray_p95_duration_ms")
	requireContains(t, body, "phpray_errors_total")
	requireContains(t, body, "phpray_n1_detections_total")
	requireContains(t, body, "phpray_slow_queries_total")

	// Check that traces_total has level labels
	requireContains(t, body, `phpray_traces_total{level="normal"}`)
	requireContains(t, body, `phpray_traces_total{level="alert"}`)

	// Check HELP and TYPE lines exist
	requireContains(t, body, "# HELP phpray_traces_read_total")
	requireContains(t, body, "# TYPE phpray_traces_read_total counter")
	requireContains(t, body, "# TYPE phpray_uptime_seconds gauge")

	// Verify specific counts from our test data
	requireMetric(t, body, "phpray_requests_total", "5")
}

func TestMetricsNilProvider(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	api := NewAPI(store, nil, "")
	// metrics defaults to NilMetrics{}

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	requireMetric(t, body, "phpray_traces_read_total", "0")
	requireMetric(t, body, "phpray_traces_stored_total", "0")
}

func TestMetricsCaching(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	api := NewAPI(store, nil, "")

	// First call populates cache
	req1 := httptest.NewRequest("GET", "/metrics", nil)
	rec1 := httptest.NewRecorder()
	api.router.ServeHTTP(rec1, req1)
	body1 := rec1.Body.String()

	// Insert a trace
	store.StoreTrace(&Trace{
		ID: "cache-test", Ts: time.Now().Unix(), Host: "test.com",
		Method: "GET", URI: "/", Status: 200, DurationMs: 50, Level: "summary",
	})

	// Second call should return cached data (within 10s window)
	req2 := httptest.NewRequest("GET", "/metrics", nil)
	rec2 := httptest.NewRecorder()
	api.router.ServeHTTP(rec2, req2)
	body2 := rec2.Body.String()

	// DB metrics portion should be identical (cached)
	// Daemon counters may differ but DB portion is cached
	if !strings.Contains(body1, "phpray_requests_total 0") {
		t.Error("First request should show 0 requests")
	}
	if !strings.Contains(body2, "phpray_requests_total 0") {
		t.Error("Second request should still show 0 (cached)")
	}
}

func TestMetricsPublicNoAuth(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := OpenStorage(dbPath)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer store.Close()

	// Create API with JWT auth enabled
	api := NewAPI(store, nil, "super-secret-key")

	// /metrics should work without auth
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected /metrics to be public (200), got %d", rec.Code)
	}

	// Protected endpoint should fail without auth
	req2 := httptest.NewRequest("GET", "/api/v1/overview", nil)
	rec2 := httptest.NewRecorder()
	api.router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("Expected /api/v1/overview to require auth (401), got %d", rec2.Code)
	}
}

// --- helpers ---

func requireMetric(t *testing.T, body, name, value string) {
	t.Helper()
	expected := name + " " + value
	if !strings.Contains(body, expected) {
		t.Errorf("Expected metric %q in body, not found.\nBody:\n%s", expected, body)
	}
}

func requireContains(t *testing.T, body, substr string) {
	t.Helper()
	if !strings.Contains(body, substr) {
		t.Errorf("Expected %q in body, not found.\nBody:\n%s", substr, body)
	}
}
