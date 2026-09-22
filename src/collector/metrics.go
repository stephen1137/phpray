package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MetricsProvider supplies daemon counters for the metrics endpoint.
// In serve mode, this is backed by the real Daemon; in standalone API mode it returns zeros.
type MetricsProvider interface {
	TracesRead() int64
	TracesStored() int64
	TracesFailed() int64
	BytesRead() int64
	RingDrops() int64
	Input() InputStatus
}

// InputStatus describes the trace inputs of the running daemon
// (/api/v1/health "input", `phpray status`).
type InputStatus struct {
	Mode    string       `json:"mode"`
	SHMPath string       `json:"shm_path"`
	Glob    bool         `json:"glob"`  // shm_path is a pattern: one ring per PHP-FPM pool
	Rings   []RingStatus `json:"rings"` // rings currently open (glob mode)
}

// RingStatus is one open ring buffer.
type RingStatus struct {
	Path    string  `json:"path"`
	Version uint32  `json:"version"`
	Records uint64  `json:"records"`
	Drops   uint64  `json:"drops"`
	FillPct float64 `json:"fill_pct"`
}

// DaemonMetrics adapts a Daemon to the MetricsProvider interface.
type DaemonMetrics struct {
	daemon *Daemon
}

func (m *DaemonMetrics) TracesRead() int64   { return m.daemon.tracesRead.Load() }
func (m *DaemonMetrics) TracesStored() int64 { return m.daemon.tracesStored.Load() }
func (m *DaemonMetrics) TracesFailed() int64 { return m.daemon.tracesFailed.Load() }
func (m *DaemonMetrics) BytesRead() int64    { return m.daemon.bytesRead.Load() }
func (m *DaemonMetrics) RingDrops() int64    { return m.daemon.ringDrops.Load() }
func (m *DaemonMetrics) Input() InputStatus  { return m.daemon.inputStatus() }

// NilMetrics returns zeros when no daemon is running (standalone API mode).
type NilMetrics struct{}

func (NilMetrics) TracesRead() int64   { return 0 }
func (NilMetrics) TracesStored() int64 { return 0 }
func (NilMetrics) TracesFailed() int64 { return 0 }
func (NilMetrics) BytesRead() int64    { return 0 }
func (NilMetrics) RingDrops() int64    { return 0 }
func (NilMetrics) Input() InputStatus  { return InputStatus{} }

// dbMetricsCache holds cached results from SQLite queries.
type dbMetricsCache struct {
	mu        sync.Mutex
	lastFetch time.Time
	data      string
}

// handleMetrics serves Prometheus text exposition format at /metrics.
func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	// --- Daemon counters (always available, zero if no daemon) ---
	writeCounter(&b, "phpray_traces_read_total", "Total traces read from input", a.metrics.TracesRead())
	writeCounter(&b, "phpray_traces_stored_total", "Total traces stored to database", a.metrics.TracesStored())
	writeCounter(&b, "phpray_traces_failed_total", "Total traces that failed to parse or store", a.metrics.TracesFailed())
	writeCounter(&b, "phpray_bytes_read_total", "Total bytes read from input", a.metrics.BytesRead())
	writeGauge(&b, "phpray_ring_drops_total", "Total traces dropped due to ring buffer overflow", fmt.Sprintf("%d", a.metrics.RingDrops()))
	writeGauge(&b, "phpray_ring_buffers_open", "Ring buffers currently open (shm_path glob mode)", fmt.Sprintf("%d", len(a.metrics.Input().Rings)))

	// --- HTTP ingest (POST /api/v1/ingest) ---
	in := a.ingest.stats()
	writeCounter(&b, "phpray_ingest_batches_total", "Batches accepted on /api/v1/ingest", in.Batches)
	writeCounter(&b, "phpray_ingest_traces_total", "Trace records accepted on /api/v1/ingest", in.Traces)
	writeCounter(&b, "phpray_ingest_dropped_total", "Trace records skipped on /api/v1/ingest (malformed)", in.Dropped)
	writeCounter(&b, "phpray_ingest_rejected_total", "Requests rejected on /api/v1/ingest (auth, size, format)", in.Rejected)

	// --- Process metrics ---
	writeGauge(&b, "phpray_uptime_seconds", "Seconds since process start", fmt.Sprintf("%.0f", time.Since(a.startTime).Seconds()))
	writeGaugeLabels(&b, "phpray_info", "PHPRay collector version info", map[string]string{"version": version}, "1")

	// --- DB metrics (cached) ---
	dbBlock := a.cachedDBMetrics()
	b.WriteString(dbBlock)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

func (a *API) cachedDBMetrics() string {
	a.metricsCache.mu.Lock()
	defer a.metricsCache.mu.Unlock()

	if time.Since(a.metricsCache.lastFetch) < 10*time.Second && a.metricsCache.data != "" {
		return a.metricsCache.data
	}

	data := a.queryDBMetrics()
	a.metricsCache.lastFetch = time.Now()
	a.metricsCache.data = data
	return data
}

func (a *API) queryDBMetrics() string {
	var b strings.Builder

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// traces by level
	b.WriteString("# HELP phpray_traces_total Total traces in database by level\n")
	b.WriteString("# TYPE phpray_traces_total gauge\n")
	rows, err := a.store.db.QueryContext(ctx,
		`SELECT trace_level, COUNT(*) FROM traces GROUP BY trace_level`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var level string
			var count int64
			rows.Scan(&level, &count)
			fmt.Fprintf(&b, "phpray_traces_total{level=%q} %d\n", level, count)
		}
	}

	// total requests
	var totalRequests int64
	a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traces`).Scan(&totalRequests)
	writeGauge(&b, "phpray_requests_total", "Total requests in database", fmt.Sprintf("%d", totalRequests))

	// last-hour cutoff
	cutoff := time.Now().Unix() - 3600

	// active domains (last hour)
	var activeDomains int64
	a.store.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT host) FROM traces WHERE timestamp >= ?`, cutoff).Scan(&activeDomains)
	writeGauge(&b, "phpray_domains_active", "Distinct domains active in last hour", fmt.Sprintf("%d", activeDomains))

	// avg duration (last hour)
	var avgDuration float64
	a.store.db.QueryRowContext(ctx,
		`SELECT COALESCE(AVG(duration_ms), 0) FROM traces WHERE timestamp >= ?`, cutoff).Scan(&avgDuration)
	writeGauge(&b, "phpray_avg_duration_ms", "Average request duration in ms (last hour)", fmt.Sprintf("%.2f", avgDuration))

	// max duration (last hour)
	var maxDuration float64
	a.store.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(duration_ms), 0) FROM traces WHERE timestamp >= ?`, cutoff).Scan(&maxDuration)
	writeGauge(&b, "phpray_max_duration_ms", "Max request duration in ms (last hour)", fmt.Sprintf("%.2f", maxDuration))

	// p95 from latest aggregation
	var p95Duration float64
	a.store.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(p95_duration_ms), 0) FROM aggregates_1m
		 WHERE bucket >= ? ORDER BY bucket DESC LIMIT 1`, cutoff).Scan(&p95Duration)
	writeGauge(&b, "phpray_p95_duration_ms", "P95 request duration from latest aggregation", fmt.Sprintf("%.2f", p95Duration))

	// errors (last hour)
	var errors int64
	a.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traces WHERE timestamp >= ? AND status >= 500`, cutoff).Scan(&errors)
	writeGauge(&b, "phpray_errors_total", "Traces with HTTP status >= 500 (last hour)", fmt.Sprintf("%d", errors))

	// N+1 detections (last hour)
	var n1 int64
	a.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traces WHERE timestamp >= ? AND n_plus_one = 1`, cutoff).Scan(&n1)
	writeGauge(&b, "phpray_n1_detections_total", "Traces with N+1 query pattern detected (last hour)", fmt.Sprintf("%d", n1))

	// slow queries total
	var slowQueries int64
	a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM slow_queries`).Scan(&slowQueries)
	writeGauge(&b, "phpray_slow_queries_total", "Total slow query fingerprints tracked", fmt.Sprintf("%d", slowQueries))

	return b.String()
}

// --- formatting helpers ---

func writeCounter(b *strings.Builder, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s %d\n", name, value)
}

func writeGauge(b *strings.Builder, name, help, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s %s\n", name, value)
}

func writeGaugeLabels(b *strings.Builder, name, help string, labels map[string]string, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)

	var labelParts []string
	for k, v := range labels {
		labelParts = append(labelParts, fmt.Sprintf("%s=%q", k, v))
	}
	fmt.Fprintf(b, "%s{%s} %s\n", name, strings.Join(labelParts, ","), value)
}
