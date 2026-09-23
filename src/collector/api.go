package main

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// API server for PHPRay dashboard
type API struct {
	store        *Storage
	hub          *Hub
	router       chi.Router
	jwtSecret    string
	metrics      MetricsProvider
	cloud        CloudStatusProvider
	metricsCache dbMetricsCache
	startTime    time.Time

	// POST /api/v1/ingest (ingest.go): where accepted traces go, batch_id memory, counters
	sink   TraceSink
	dedup  *batchDedup
	ingest ingestCounters
}

// NewAPI creates a new API server
func NewAPI(store *Storage, hub *Hub, jwtSecret string) *API {
	a := &API{
		store:     store,
		hub:       hub,
		jwtSecret: jwtSecret,
		metrics:   NilMetrics{},
		cloud:     NilCloud{},
		startTime: time.Now(),
		dedup:     newBatchDedup(),
	}
	a.setupRoutes()
	return a
}

//go:embed dashboard.html
var dashboardHTML []byte

func (a *API) setupRoutes() {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(corsMiddleware)

	// Public routes — no auth
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})

	// Auth status endpoint — tells the dashboard whether auth is enabled
	r.Get("/api/v1/auth/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"auth_enabled": a.jwtSecret != ""})
	})

	// Health — public
	r.Get("/api/v1/health", a.handleHealth)

	// Prometheus metrics — public
	r.Get("/metrics", a.handleMetrics)

	// WebSocket — uses query param auth
	if a.hub != nil {
		r.With(WSAuthMiddleware(a.jwtSecret)).Get("/ws/traces", a.handleWebSocket)
	}

	// HTTP ingest (WordPress plugin "collector" mode, third-party agents) — ingest.go.
	// Own guard: Bearer JWT when a secret is set, loopback-only otherwise.
	r.With(ingestGuard(a.jwtSecret)).Post("/api/v1/ingest", a.handleIngest)

	// Protected API routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(AuthMiddleware(a.jwtSecret))
		r.Get("/overview", a.handleOverview)
		r.Get("/traces", a.handleTraces)
		r.Get("/traces/{id}", a.handleTraceDetail)
		r.Get("/domains", a.handleDomains)
		r.Get("/domains/{domain}/stats", a.handleDomainStats)
		r.Get("/domain-owners", a.handleDomainOwners)
		r.Get("/queries/slow", a.handleSlowQueries)
		r.Get("/queries/frequent", a.handleFrequentQueries)
		r.Get("/aggregates", a.handleAggregates)
		r.Get("/timeseries", a.handleTimeseries)
		r.Get("/users", a.handleUsers)
		r.Get("/users/{uid}", a.handleUserDetail)
		r.Get("/diagnostics", a.handleDiagnostics)
		r.Get("/diagnostics/{domain}", a.handleDiagnostics)
		r.Get("/diagnostics/{domain}/report", a.handleDiagReport)
		// Publikacja linkiem. Lokalny panel jest jedynym miejscem, ktore
		// widzi KAZDY, kto zainstalowal PHPRaya sam — a to tam jest wejscie
		// do petli wzrostu. Flaga w CLI, o ktorej nikt nie wie, nie roznosi
		// produktu.
		r.Post("/diagnostics/{domain}/share", a.handleDiagShare)
		r.Get("/components", a.handleComponents)
		r.Get("/php-versions", a.handlePhpVersions)
		r.Get("/alerts", a.handleAlerts)
		r.Get("/alerts/count", a.handleAlertCount)
		r.Post("/alerts/{id}/ack", a.handleAlertAck)
	})

	a.router = r
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// GET /api/v1/overview — server summary
func (a *API) handleOverview(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	cutoff := time.Now().Unix() - int64(windowMin)*60

	var overview struct {
		TotalRequests int64            `json:"total_requests"`
		AvgDuration   float64          `json:"avg_duration_ms"`
		MaxDuration   float64          `json:"max_duration_ms"`
		P95Duration   float64          `json:"p95_duration_ms"`
		ErrorCount    int64            `json:"error_count"`
		ErrorRate     float64          `json:"error_rate_pct"`
		N1Count       int64            `json:"n1_count"`
		N1Rate        float64          `json:"n1_rate_pct"`
		TotalQueries  int64            `json:"total_queries"`
		TotalHTTP     int64            `json:"total_http_calls"`
		UniqueDomains int64            `json:"unique_domains"`
		LevelCounts   map[string]int64 `json:"level_counts"`
		WindowMinutes int              `json:"window_minutes"`
	}
	overview.LevelCounts = make(map[string]int64)
	overview.WindowMinutes = windowMin

	domClause, domArgs := DomainFilter(r, "host")

	overviewQuery := `
		SELECT COUNT(*), COALESCE(AVG(duration_ms),0), COALESCE(MAX(duration_ms),0),
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END),
			SUM(n_plus_one), SUM(db_count), SUM(http_count),
			COUNT(DISTINCT host)
		FROM traces WHERE timestamp >= ?` + domClause
	overviewArgs := append([]interface{}{cutoff}, domArgs...)
	a.store.db.QueryRow(overviewQuery, overviewArgs...).Scan(
		&overview.TotalRequests, &overview.AvgDuration, &overview.MaxDuration,
		&overview.ErrorCount, &overview.N1Count, &overview.TotalQueries,
		&overview.TotalHTTP, &overview.UniqueDomains,
	)

	if overview.TotalRequests > 0 {
		overview.ErrorRate = float64(overview.ErrorCount) / float64(overview.TotalRequests) * 100
		overview.N1Rate = float64(overview.N1Count) / float64(overview.TotalRequests) * 100
	}

	// P95 approximation
	p95Query := `SELECT duration_ms FROM traces WHERE timestamp >= ?` + domClause + ` ORDER BY duration_ms DESC LIMIT 1 OFFSET ?`
	p95Args := append([]interface{}{cutoff}, domArgs...)
	p95Args = append(p95Args, int(float64(overview.TotalRequests)*0.05))
	a.store.db.QueryRow(p95Query, p95Args...).Scan(&overview.P95Duration)

	// Level counts
	lvlQuery := `SELECT trace_level, COUNT(*) FROM traces WHERE timestamp >= ?` + domClause + ` GROUP BY trace_level`
	lvlArgs := append([]interface{}{cutoff}, domArgs...)
	rows, _ := a.store.db.Query(lvlQuery, lvlArgs...)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var level string
			var count int64
			rows.Scan(&level, &count)
			overview.LevelCounts[level] = count
		}
	}

	writeJSON(w, overview)
}

// GET /api/v1/traces — list traces with filters
func (a *API) handleTraces(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	domain := r.URL.Query().Get("domain")
	minMs := queryFloat(r, "min_ms", 0)
	level := r.URL.Query().Get("level")
	n1Only := r.URL.Query().Get("n1") == "1"
	app := r.URL.Query().Get("app") // wordpress | prestashop | laravel | magento | symfony | "" (unknown)
	windowMin := queryInt(r, "window", 60)
	cutoff := time.Now().Unix() - int64(windowMin)*60

	query := `SELECT id, phpray_id, timestamp, uid, COALESCE(username,'') as username,
		host, method, uri, status,
		duration_ms, cpu_user_ms, cpu_sys_ms, memory_peak_mb,
		db_count, db_ms, http_count, http_ms, file_count, file_ms,
		COALESCE(redis_count,0), COALESCE(redis_ms,0),
		wp, n_plus_one, trace_level, error_count, COALESCE(php_version,'') as php_version,
		COALESCE(profiled,0), COALESCE(app,'') as app
		FROM traces WHERE timestamp >= ?`
	args := []interface{}{cutoff}

	// Role-based domain restriction
	domClause, domArgs := DomainFilter(r, "host")
	query += domClause
	args = append(args, domArgs...)

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}
	if minMs > 0 {
		query += " AND duration_ms >= ?"
		args = append(args, minMs)
	}
	if level != "" {
		query += " AND trace_level = ?"
		args = append(args, level)
	}
	if n1Only {
		query += " AND n_plus_one = 1"
	}
	if app != "" {
		if app == "unknown" || app == "-" {
			query += " AND app = ''"
		} else {
			query += " AND app = ?"
			args = append(args, app)
		}
	}

	query += " ORDER BY timestamp DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type TraceRow struct {
		ID         int64   `json:"id"`
		PhprayID   string  `json:"phpray_id"`
		Timestamp  int64   `json:"timestamp"`
		UID        int     `json:"uid"`
		Username   string  `json:"username"`
		Host       string  `json:"host"`
		Method     string  `json:"method"`
		URI        string  `json:"uri"`
		Status     int     `json:"status"`
		DurationMs float64 `json:"duration_ms"`
		CPUUserMs  float64 `json:"cpu_user_ms"`
		CPUSysMs   float64 `json:"cpu_sys_ms"`
		MemoryMB   float64 `json:"memory_peak_mb"`
		DBCount    int     `json:"db_count"`
		DBMs       float64 `json:"db_ms"`
		HTTPCount  int     `json:"http_count"`
		HTTPMs     float64 `json:"http_ms"`
		FileCount  int     `json:"file_count"`
		FileMs     float64 `json:"file_ms"`
		RedisCount int     `json:"redis_count"`
		RedisMs    float64 `json:"redis_ms"`
		WP         int     `json:"wp"`
		N1         int     `json:"n1"`
		Level      string  `json:"level"`
		ErrorCount int     `json:"error_count"`
		PhpVersion string  `json:"php_version,omitempty"`
		Profiled   int     `json:"profiled"`
		App        string  `json:"app,omitempty"`
	}

	var traces []TraceRow
	for rows.Next() {
		var t TraceRow
		rows.Scan(&t.ID, &t.PhprayID, &t.Timestamp, &t.UID, &t.Username, &t.Host, &t.Method, &t.URI, &t.Status,
			&t.DurationMs, &t.CPUUserMs, &t.CPUSysMs, &t.MemoryMB,
			&t.DBCount, &t.DBMs, &t.HTTPCount, &t.HTTPMs, &t.FileCount, &t.FileMs,
			&t.RedisCount, &t.RedisMs,
			&t.WP, &t.N1, &t.Level, &t.ErrorCount, &t.PhpVersion, &t.Profiled, &t.App)
		// Enrich username from resolver if missing
		if t.Username == "" && a.store.users != nil {
			t.Username = a.store.users.Resolve(uint32(t.UID))
		}
		traces = append(traces, t)
	}

	writeJSON(w, map[string]interface{}{
		"traces": traces,
		"count":  len(traces),
		"offset": offset,
		"limit":  limit,
	})
}

// GET /api/v1/traces/{id} — trace detail with queries, marks, errors
func (a *API) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, 400, "invalid trace id")
		return
	}

	// Main trace
	var t struct {
		ID         int64         `json:"id"`
		PhprayID   string        `json:"phpray_id"`
		Timestamp  int64         `json:"timestamp"`
		UID        int           `json:"uid"`
		Username   string        `json:"username"`
		Host       string        `json:"host"`
		Method     string        `json:"method"`
		URI        string        `json:"uri"`
		URIFp      string        `json:"uri_fingerprint"`
		Status     int           `json:"status"`
		DurationMs float64       `json:"duration_ms"`
		CPUUserMs  float64       `json:"cpu_user_ms"`
		CPUSysMs   float64       `json:"cpu_sys_ms"`
		MemoryMB   float64       `json:"memory_peak_mb"`
		DBCount    int           `json:"db_count"`
		DBMs       float64       `json:"db_ms"`
		HTTPCount  int           `json:"http_count"`
		HTTPMs     float64       `json:"http_ms"`
		FileCount  int           `json:"file_count"`
		FileMs     float64       `json:"file_ms"`
		RedisCount int           `json:"redis_count"`
		RedisMs    float64       `json:"redis_ms"`
		WP         int           `json:"wp"`
		N1         int           `json:"n1"`
		Level      string        `json:"level"`
		PhpVersion string        `json:"php_version,omitempty"`
		Profiled   int           `json:"profiled"`
		App        string        `json:"app,omitempty"`
		Queries    []interface{} `json:"queries"`
		HTTPCalls  []interface{} `json:"http_calls"`
		Marks      []interface{} `json:"marks"`
		Errors     []interface{} `json:"errors"`
		Components []Component   `json:"components,omitempty"`
	}

	var componentsJSON string
	err = a.store.db.QueryRow(`
		SELECT id, phpray_id, timestamp, uid, COALESCE(username,'') as username,
			host, method, uri, uri_fingerprint, status,
			duration_ms, cpu_user_ms, cpu_sys_ms, memory_peak_mb,
			db_count, db_ms, http_count, http_ms, file_count, file_ms,
			COALESCE(redis_count,0), COALESCE(redis_ms,0),
			wp, n_plus_one, trace_level, COALESCE(components_json, '[]'),
			COALESCE(php_version, '') as php_version, COALESCE(profiled, 0), COALESCE(app, '')
		FROM traces WHERE id = ?
	`, id).Scan(&t.ID, &t.PhprayID, &t.Timestamp, &t.UID, &t.Username, &t.Host, &t.Method, &t.URI, &t.URIFp,
		&t.Status, &t.DurationMs, &t.CPUUserMs, &t.CPUSysMs, &t.MemoryMB,
		&t.DBCount, &t.DBMs, &t.HTTPCount, &t.HTTPMs, &t.FileCount, &t.FileMs,
		&t.RedisCount, &t.RedisMs,
		&t.WP, &t.N1, &t.Level, &componentsJSON, &t.PhpVersion, &t.Profiled, &t.App)
	// Parse components JSON
	if componentsJSON != "" && componentsJSON != "[]" {
		json.Unmarshal([]byte(componentsJSON), &t.Components)
	}
	// Enrich username from resolver if missing
	if t.Username == "" && a.store.users != nil {
		t.Username = a.store.users.Resolve(uint32(t.UID))
	}
	if err == sql.ErrNoRows {
		writeError(w, 404, "trace not found")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Queries (with backtrace)
	qRows, _ := a.store.db.Query(`
		SELECT sql_text, sql_fingerprint, duration_ms, offset_ms, affected_rows, source, backtrace
		FROM queries WHERE trace_id = ? ORDER BY offset_ms
	`, id)
	if qRows != nil {
		defer qRows.Close()
		for qRows.Next() {
			var q struct {
				SQL         string  `json:"sql"`
				Fingerprint string  `json:"fingerprint"`
				DurationMs  float64 `json:"duration_ms"`
				OffsetMs    float64 `json:"offset_ms"`
				Rows        int     `json:"affected_rows"`
				Source      string  `json:"source"`
				Backtrace   []Frame `json:"backtrace,omitempty"`
			}
			var btJSON string
			qRows.Scan(&q.SQL, &q.Fingerprint, &q.DurationMs, &q.OffsetMs, &q.Rows, &q.Source, &btJSON)
			if btJSON != "" {
				json.Unmarshal([]byte(btJSON), &q.Backtrace)
			}
			t.Queries = append(t.Queries, q)
		}
	}

	// HTTP calls (with backtrace)
	hRows, _ := a.store.db.Query(`
		SELECT url, duration_ms, offset_ms, status, backtrace FROM http_calls WHERE trace_id = ? ORDER BY offset_ms
	`, id)
	if hRows != nil {
		defer hRows.Close()
		for hRows.Next() {
			var h struct {
				URL        string  `json:"url"`
				DurationMs float64 `json:"duration_ms"`
				OffsetMs   float64 `json:"offset_ms"`
				Status     int     `json:"status"`
				Backtrace  []Frame `json:"backtrace,omitempty"`
			}
			var btJSON string
			hRows.Scan(&h.URL, &h.DurationMs, &h.OffsetMs, &h.Status, &btJSON)
			if btJSON != "" {
				json.Unmarshal([]byte(btJSON), &h.Backtrace)
			}
			t.HTTPCalls = append(t.HTTPCalls, h)
		}
	}

	// Marks
	mRows, _ := a.store.db.Query(`
		SELECT name, offset_ms FROM marks WHERE trace_id = ? ORDER BY offset_ms
	`, id)
	if mRows != nil {
		defer mRows.Close()
		for mRows.Next() {
			var m struct {
				Name     string  `json:"name"`
				OffsetMs float64 `json:"offset_ms"`
			}
			mRows.Scan(&m.Name, &m.OffsetMs)
			t.Marks = append(t.Marks, m)
		}
	}

	// Errors
	eRows, _ := a.store.db.Query(`
		SELECT error_type, message, offset_ms FROM errors WHERE trace_id = ? ORDER BY offset_ms
	`, id)
	if eRows != nil {
		defer eRows.Close()
		for eRows.Next() {
			var e struct {
				Type     string  `json:"type"`
				Message  string  `json:"message"`
				OffsetMs float64 `json:"offset_ms"`
			}
			eRows.Scan(&e.Type, &e.Message, &e.OffsetMs)
			t.Errors = append(t.Errors, e)
		}
	}

	writeJSON(w, t)
}

// GET /api/v1/domains — list domains with stats
func (a *API) handleDomains(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	cutoff := time.Now().Unix() - int64(windowMin)*60

	domClause, domArgs := DomainFilter(r, "host")
	app := r.URL.Query().Get("app")
	appClause := ""
	if app == "unknown" || app == "-" {
		appClause = " AND app = ''"
	} else if app != "" {
		appClause = " AND app = ?"
		domArgs = append(domArgs, app)
	}
	domainQuery := `
		SELECT host, COUNT(*) as reqs,
			AVG(duration_ms) as avg_ms, MAX(duration_ms) as max_ms,
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as errs,
			SUM(n_plus_one) as n1,
			SUM(db_count) as total_queries,
			COALESCE(MAX(app), '') as app
		FROM traces WHERE timestamp >= ? AND host != ''` + domClause + appClause + `
		GROUP BY host ORDER BY avg_ms DESC`
	domainArgs := append([]interface{}{cutoff}, domArgs...)
	rows, err := a.store.db.Query(domainQuery, domainArgs...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type DomainRow struct {
		Host         string  `json:"host"`
		Requests     int64   `json:"requests"`
		AvgMs        float64 `json:"avg_duration_ms"`
		MaxMs        float64 `json:"max_duration_ms"`
		Errors       int64   `json:"errors"`
		N1Count      int64   `json:"n1_count"`
		TotalQueries int64   `json:"total_queries"`
		App          string  `json:"app,omitempty"`
		Owner        string  `json:"owner,omitempty"`
		OwnerUID     uint32  `json:"owner_uid,omitempty"`
	}

	var domains []DomainRow
	for rows.Next() {
		var d DomainRow
		rows.Scan(&d.Host, &d.Requests, &d.AvgMs, &d.MaxMs, &d.Errors, &d.N1Count, &d.TotalQueries, &d.App)
		// Enrich with domain owner from DirectAdmin mapping
		if a.store.domains != nil {
			if owner := a.store.domains.Resolve(d.Host); owner != nil {
				d.Owner = owner.Username
				d.OwnerUID = owner.UID
			}
		}
		domains = append(domains, d)
	}

	writeJSON(w, map[string]interface{}{"domains": domains, "count": len(domains)})
}

// GET /api/v1/domains/{domain}/stats — per-domain detailed stats
func (a *API) handleDomainStats(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	windowMin := queryInt(r, "window", 60)
	cutoff := time.Now().Unix() - int64(windowMin)*60

	// For user role, check domain is in allowed list
	if allowed := AllowedDomains(r); allowed != nil {
		found := false
		for _, d := range allowed {
			if strings.Contains(domain, d) || strings.Contains(d, domain) {
				found = true
				break
			}
		}
		if !found {
			writeError(w, 403, "domain not in your allowed list")
			return
		}
	}

	// Use LIKE to allow partial matching (e.g., "example" matches "example.com:8080")
	domainFilter := "%" + domain + "%"

	var stats struct {
		Domain       string        `json:"domain"`
		Requests     int64         `json:"requests"`
		AvgMs        float64       `json:"avg_duration_ms"`
		MaxMs        float64       `json:"max_duration_ms"`
		P95Ms        float64       `json:"p95_duration_ms"`
		Errors       int64         `json:"errors"`
		N1Count      int64         `json:"n1_count"`
		TotalQueries int64         `json:"total_queries"`
		TotalHTTP    int64         `json:"total_http_calls"`
		Owner        string        `json:"owner,omitempty"`
		OwnerUID     uint32        `json:"owner_uid,omitempty"`
		TopURIs      []interface{} `json:"top_uris"`
		SlowQueries  []interface{} `json:"slow_queries"`
	}
	stats.Domain = domain

	// Enrich with domain owner from DirectAdmin mapping
	if a.store.domains != nil {
		if owner := a.store.domains.Resolve(domain); owner != nil {
			stats.Owner = owner.Username
			stats.OwnerUID = owner.UID
		}
	}

	a.store.db.QueryRow(`
		SELECT COUNT(*), COALESCE(AVG(duration_ms),0), COALESCE(MAX(duration_ms),0),
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END),
			SUM(n_plus_one), SUM(db_count), SUM(http_count)
		FROM traces WHERE timestamp >= ? AND host LIKE ?
	`, cutoff, domainFilter).Scan(
		&stats.Requests, &stats.AvgMs, &stats.MaxMs,
		&stats.Errors, &stats.N1Count, &stats.TotalQueries, &stats.TotalHTTP,
	)

	// P95 for this domain (same approximation as the overview endpoint: the
	// value 5 % of the way down the sorted list). Without it the per-site view
	// in the panel plugin always showed 0.
	if stats.Requests > 0 {
		a.store.db.QueryRow(`
			SELECT duration_ms FROM traces WHERE timestamp >= ? AND host LIKE ?
			ORDER BY duration_ms DESC LIMIT 1 OFFSET ?
		`, cutoff, domainFilter, int(float64(stats.Requests)*0.05)).Scan(&stats.P95Ms)
	}

	// Top URIs
	uriRows, _ := a.store.db.Query(`
		SELECT uri, COUNT(*) as reqs, AVG(duration_ms), MAX(duration_ms)
		FROM traces WHERE timestamp >= ? AND host LIKE ?
		GROUP BY uri ORDER BY AVG(duration_ms) DESC LIMIT 10
	`, cutoff, domainFilter)
	type topURI struct {
		URI   string  `json:"uri"`
		Reqs  int64   `json:"requests"`
		AvgMs float64 `json:"avg_ms"`
		MaxMs float64 `json:"max_ms"`
		P95Ms float64 `json:"p95_ms"`
	}
	var uris []topURI
	if uriRows != nil {
		defer uriRows.Close()
		for uriRows.Next() {
			var u topURI
			uriRows.Scan(&u.URI, &u.Reqs, &u.AvgMs, &u.MaxMs)
			uris = append(uris, u)
		}
		uriRows.Close()
		for i := range uris {
			if uris[i].Reqs > 0 {
				a.store.db.QueryRow(`
					SELECT duration_ms FROM traces WHERE timestamp >= ? AND host LIKE ? AND uri = ?
					ORDER BY duration_ms DESC LIMIT 1 OFFSET ?
				`, cutoff, domainFilter, uris[i].URI, int(float64(uris[i].Reqs)*0.05)).Scan(&uris[i].P95Ms)
			}
			stats.TopURIs = append(stats.TopURIs, uris[i])
		}
	}

	// Slow queries for this domain
	sqRows, _ := a.store.db.Query(`
		SELECT fingerprint, sample_sql, avg_ms, max_ms, exec_count
		FROM slow_queries WHERE host LIKE ?
		ORDER BY avg_ms DESC LIMIT 10
	`, domainFilter)
	if sqRows != nil {
		defer sqRows.Close()
		for sqRows.Next() {
			var sq struct {
				Fingerprint string  `json:"fingerprint"`
				Sample      string  `json:"sample_sql"`
				AvgMs       float64 `json:"avg_ms"`
				MaxMs       float64 `json:"max_ms"`
				Count       int64   `json:"exec_count"`
			}
			sqRows.Scan(&sq.Fingerprint, &sq.Sample, &sq.AvgMs, &sq.MaxMs, &sq.Count)
			stats.SlowQueries = append(stats.SlowQueries, sq)
		}
	}

	writeJSON(w, stats)
}

// GET /api/v1/domain-owners — list all known domain→user mappings from DirectAdmin
func (a *API) handleDomainOwners(w http.ResponseWriter, r *http.Request) {
	if a.store.domains == nil || a.store.domains.Count() == 0 {
		writeJSON(w, map[string]interface{}{
			"owners": []interface{}{},
			"count":  0,
			"source": "none",
			"note":   "DirectAdmin data not available (no /usr/local/directadmin/data/users/ found)",
		})
		return
	}

	// Optional filter by username
	filterUser := r.URL.Query().Get("user")

	allOwners := a.store.domains.GetAll()

	type OwnerEntry struct {
		Domain   string `json:"domain"`
		Username string `json:"username"`
		UID      uint32 `json:"uid"`
	}

	entries := make([]OwnerEntry, 0, len(allOwners))
	for domain, owner := range allOwners {
		if filterUser != "" && owner.Username != filterUser {
			continue
		}
		entries = append(entries, OwnerEntry{
			Domain:   domain,
			Username: owner.Username,
			UID:      owner.UID,
		})
	}

	// Sort by username then domain for deterministic output
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Username != entries[j].Username {
			return entries[i].Username < entries[j].Username
		}
		return entries[i].Domain < entries[j].Domain
	})

	writeJSON(w, map[string]interface{}{
		"owners": entries,
		"count":  len(entries),
		"source": "directadmin",
	})
}

// GET /api/v1/queries/slow — top slow query fingerprints
func (a *API) handleSlowQueries(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20)
	domain := r.URL.Query().Get("domain")

	query := `SELECT fingerprint, sample_sql, host, avg_ms, max_ms, exec_count, total_ms, last_seen
		FROM slow_queries WHERE 1=1`
	var args []interface{}

	// Role-based domain restriction
	domClause, domArgs := DomainFilter(r, "host")
	query += domClause
	args = append(args, domArgs...)

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}

	query += " ORDER BY avg_ms DESC LIMIT ?"
	args = append(args, limit)

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type SQ struct {
		Fingerprint string  `json:"fingerprint"`
		Sample      string  `json:"sample_sql"`
		Host        string  `json:"host"`
		AvgMs       float64 `json:"avg_ms"`
		MaxMs       float64 `json:"max_ms"`
		Count       int64   `json:"exec_count"`
		TotalMs     float64 `json:"total_ms"`
		LastSeen    int64   `json:"last_seen"`
	}

	var queries []SQ
	for rows.Next() {
		var q SQ
		rows.Scan(&q.Fingerprint, &q.Sample, &q.Host, &q.AvgMs, &q.MaxMs, &q.Count, &q.TotalMs, &q.LastSeen)
		queries = append(queries, q)
	}

	writeJSON(w, map[string]interface{}{"queries": queries, "count": len(queries)})
}

// GET /api/v1/queries/frequent — most frequently executed query fingerprints
func (a *API) handleFrequentQueries(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20)
	domain := r.URL.Query().Get("domain")
	windowMin := queryInt(r, "window", 0)

	query := `SELECT fingerprint, sample_sql, host, avg_ms, max_ms, exec_count, total_ms, last_seen
		FROM slow_queries WHERE 1=1`
	var args []interface{}

	// Role-based domain restriction
	domClause, domArgs := DomainFilter(r, "host")
	query += domClause
	args = append(args, domArgs...)

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}

	if windowMin > 0 {
		cutoff := time.Now().Unix() - int64(windowMin)*60
		query += " AND last_seen >= ?"
		args = append(args, cutoff)
	}

	query += " ORDER BY exec_count DESC LIMIT ?"
	args = append(args, limit)

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type SQ struct {
		Fingerprint string  `json:"fingerprint"`
		Sample      string  `json:"sample_sql"`
		Host        string  `json:"host"`
		AvgMs       float64 `json:"avg_ms"`
		MaxMs       float64 `json:"max_ms"`
		Count       int64   `json:"exec_count"`
		TotalMs     float64 `json:"total_ms"`
		LastSeen    int64   `json:"last_seen"`
	}

	var queries []SQ
	for rows.Next() {
		var q SQ
		rows.Scan(&q.Fingerprint, &q.Sample, &q.Host, &q.AvgMs, &q.MaxMs, &q.Count, &q.TotalMs, &q.LastSeen)
		queries = append(queries, q)
	}

	writeJSON(w, map[string]interface{}{"queries": queries, "count": len(queries)})
}

// GET /api/v1/users — per-user statistics
func (a *API) handleUsers(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	limit := queryInt(r, "limit", 50)
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "avg_ms"
	}

	users, err := a.store.GetUserStats(windowMin, limit, sortBy)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	writeJSON(w, map[string]interface{}{
		"users":   users,
		"count":   len(users),
		"window":  windowMin,
		"sort_by": sortBy,
	})
}

// GET /api/v1/users/{uid} — detailed stats for a specific user
func (a *API) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	uidStr := chi.URLParam(r, "uid")
	uid64, err := strconv.ParseUint(uidStr, 10, 32)
	if err != nil {
		writeError(w, 400, "invalid UID")
		return
	}
	uid := uint32(uid64)

	windowMin := queryInt(r, "window", 60)

	user, domains, err := a.store.GetUserDetail(uid, windowMin)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	writeJSON(w, map[string]interface{}{
		"user":    user,
		"domains": domains,
		"window":  windowMin,
	})
}

// GET /api/v1/aggregates — per-minute aggregates
func (a *API) handleAggregates(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	domain := r.URL.Query().Get("domain")

	// For user role, restrict domain
	if allowed := AllowedDomains(r); allowed != nil {
		if domain == "" && len(allowed) == 1 {
			domain = allowed[0]
		} else if domain != "" {
			found := false
			for _, d := range allowed {
				if strings.Contains(domain, d) || strings.Contains(d, domain) {
					found = true
					break
				}
			}
			if !found {
				writeError(w, 403, "domain not in your allowed list")
				return
			}
		}
	}

	now := time.Now().Unix()
	from := ((now - int64(windowMin)*60) / 60) * 60

	aggs, err := a.store.GetAggregates1m(from, now, domain)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	writeJSON(w, map[string]interface{}{"aggregates": aggs, "count": len(aggs)})
}

// GET /api/v1/health
// GET /api/v1/timeseries — compute time-series from raw traces (works without daemon aggregation)
func (a *API) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	domain := r.URL.Query().Get("domain")
	bucketSec := queryInt(r, "bucket", 0) // 0 = auto

	// Auto bucket size based on window
	if bucketSec == 0 {
		switch {
		case windowMin <= 15:
			bucketSec = 60 // 1-minute buckets for <=15m
		case windowMin <= 120:
			bucketSec = 300 // 5-minute buckets for <=2h
		case windowMin <= 1440:
			bucketSec = 900 // 15-minute buckets for <=24h
		default:
			bucketSec = 3600 // 1-hour buckets for >24h
		}
	}

	cutoff := time.Now().Unix() - int64(windowMin)*60

	domClause, domArgs := DomainFilter(r, "host")

	query := `
		SELECT (timestamp / ?) * ? as bucket,
			COUNT(*) as request_count,
			COALESCE(AVG(duration_ms), 0) as avg_ms,
			COALESCE(MAX(duration_ms), 0) as max_ms,
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as error_count,
			SUM(n_plus_one) as n1_count,
			SUM(db_count) as total_queries,
			SUM(http_count) as total_http
		FROM traces WHERE timestamp >= ?` + domClause
	args := append([]interface{}{bucketSec, bucketSec, cutoff}, domArgs...)

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}

	query += " GROUP BY bucket ORDER BY bucket ASC"

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type TSPoint struct {
		Bucket       int64   `json:"t"`
		Requests     int     `json:"reqs"`
		AvgMs        float64 `json:"avg_ms"`
		MaxMs        float64 `json:"max_ms"`
		Errors       int     `json:"errors"`
		N1Count      int     `json:"n1"`
		TotalQueries int     `json:"queries"`
		TotalHTTP    int     `json:"http"`
	}

	var points []TSPoint
	for rows.Next() {
		var p TSPoint
		rows.Scan(&p.Bucket, &p.Requests, &p.AvgMs, &p.MaxMs,
			&p.Errors, &p.N1Count, &p.TotalQueries, &p.TotalHTTP)
		points = append(points, p)
	}

	// Also compute P95 per bucket (separate query for efficiency)
	if len(points) > 0 {
		p95Query := `
			SELECT (timestamp / ?) * ? as bucket, duration_ms
			FROM traces WHERE timestamp >= ?` + domClause
		p95Args := append([]interface{}{bucketSec, bucketSec, cutoff}, domArgs...)
		if domain != "" {
			p95Query += " AND host LIKE ?"
			p95Args = append(p95Args, "%"+domain+"%")
		}
		p95Query += " ORDER BY bucket, duration_ms ASC"

		p95Rows, err := a.store.db.Query(p95Query, p95Args...)
		if err == nil {
			defer p95Rows.Close()
			// Group durations by bucket and calculate P95
			bucketDurations := make(map[int64][]float64)
			for p95Rows.Next() {
				var bucket int64
				var dur float64
				p95Rows.Scan(&bucket, &dur)
				bucketDurations[bucket] = append(bucketDurations[bucket], dur)
			}

			// Create a p95 map for quick lookup
			p95Map := make(map[int64]float64)
			for bucket, durations := range bucketDurations {
				idx := int(float64(len(durations)) * 0.95)
				if idx >= len(durations) {
					idx = len(durations) - 1
				}
				p95Map[bucket] = durations[idx]
			}

			// Attach P95 to response
			type TSPointP95 struct {
				Bucket       int64   `json:"t"`
				Requests     int     `json:"reqs"`
				AvgMs        float64 `json:"avg_ms"`
				MaxMs        float64 `json:"max_ms"`
				P95Ms        float64 `json:"p95_ms"`
				Errors       int     `json:"errors"`
				N1Count      int     `json:"n1"`
				TotalQueries int     `json:"queries"`
				TotalHTTP    int     `json:"http"`
			}
			var enriched []TSPointP95
			for _, p := range points {
				enriched = append(enriched, TSPointP95{
					Bucket:       p.Bucket,
					Requests:     p.Requests,
					AvgMs:        p.AvgMs,
					MaxMs:        p.MaxMs,
					P95Ms:        p95Map[p.Bucket],
					Errors:       p.Errors,
					N1Count:      p.N1Count,
					TotalQueries: p.TotalQueries,
					TotalHTTP:    p.TotalHTTP,
				})
			}

			writeJSON(w, map[string]interface{}{
				"points":     enriched,
				"count":      len(enriched),
				"bucket_sec": bucketSec,
				"window_min": windowMin,
			})
			return
		}
	}

	writeJSON(w, map[string]interface{}{
		"points":     points,
		"count":      len(points),
		"bucket_sec": bucketSec,
		"window_min": windowMin,
	})
}

// GET /api/v1/diagnostics[/{domain}] — AI diagnostics report
func (a *API) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	if domain == "" {
		domain = r.URL.Query().Get("domain")
	}
	windowMin := queryInt(r, "window", 60)

	// For user role, restrict to allowed domains
	if allowed := AllowedDomains(r); allowed != nil {
		if domain == "" {
			// User must specify a domain from their allowed list
			if len(allowed) == 1 {
				domain = allowed[0]
			} else {
				writeError(w, 403, "specify a domain from your allowed list")
				return
			}
		} else {
			found := false
			for _, d := range allowed {
				if strings.Contains(domain, d) || strings.Contains(d, domain) {
					found = true
					break
				}
			}
			if !found {
				writeError(w, 403, "domain not in your allowed list")
				return
			}
		}
	}

	ctx, err := a.store.LoadTracesForDiag(domain, windowMin)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	if ctx.TotalReqs == 0 {
		writeJSON(w, map[string]interface{}{
			"domain":       domain,
			"window_min":   windowMin,
			"trace_count":  0,
			"findings":     []interface{}{},
			"health_score": 100,
			"summary":      "No traces found for the specified domain and time window.",
		})
		return
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)
	report.WindowMin = windowMin

	writeJSON(w, report)
}

// GET /api/v1/diagnostics/{domain}/report — export diagnostic report
func (a *API) handleDiagReport(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	windowMin := queryInt(r, "window", 60)
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "markdown"
	}

	// Role-based domain restriction
	if allowed := AllowedDomains(r); allowed != nil {
		found := false
		for _, d := range allowed {
			if strings.Contains(domain, d) || strings.Contains(d, domain) {
				found = true
				break
			}
		}
		if !found {
			writeError(w, 403, "domain not in your allowed list")
			return
		}
	}

	ctx, err := a.store.LoadTracesForDiag(domain, windowMin)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)
	report.WindowMin = windowMin

	// HTML jest formatem, ktory WEDRUJE DALEJ: jeden samowystarczalny plik ze
	// stopka "safe to forward" i odnosnikami z utm_source=report. Ma go CLI
	// (phpray report -format html), wtyczka DirectAdmin i konsola Cloud —
	// a ta koncowka, z ktorej korzysta lokalny panel, oddawala tylko markdown.
	// Czyli akurat tam, gdzie trafia KAZDY, kto zainstalowal PHPRaya sam,
	// jedyny artefakt do wyslania komus byl plikiem .md bez ani jednego
	// odnosnika do nas. Zauwazone 22.09.2026.
	var content, typ, rozsz string
	switch format {
	case "text":
		content, typ, rozsz = GeneratePlainTextReport(report), "text/plain; charset=utf-8", "txt"
	case "html":
		content = GenerateHTMLReport(report, r.URL.Query().Get("anonymize") == "1", version)
		typ, rozsz = "text/html; charset=utf-8", "html"
	default:
		content, typ, rozsz = GenerateMarkdownReport(report), "text/markdown; charset=utf-8", "md"
	}

	w.Header().Set("Content-Type", typ)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"phpray-report-%s.%s\"", domain, rozsz))
	w.Write([]byte(content))
}

// GET /api/v1/components — aggregated component/plugin breakdown across traces
func (a *API) handleComponents(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	limit := queryInt(r, "limit", 50)
	domain := r.URL.Query().Get("domain")
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "total_ms"
	}
	category := r.URL.Query().Get("category") // filter: plugin, theme, vendor, core, wp-core

	domClause, domArgs := DomainFilter(r, "host")

	comps, profiledTraces, err := a.store.GetComponentStats(windowMin, domain, domClause, domArgs, 0, sortBy)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Category filter (post-query since we aggregate in Go)
	if category != "" {
		filtered := make([]ComponentAgg, 0)
		for _, c := range comps {
			if c.Category == category {
				filtered = append(filtered, c)
			}
		}
		comps = filtered
	}

	// Apply limit after category filter
	if limit > 0 && len(comps) > limit {
		comps = comps[:limit]
	}

	// Compute totals for context
	var totalMs float64
	var totalTraces int64
	for _, c := range comps {
		totalMs += c.TotalMs
		if c.TraceCount > totalTraces {
			totalTraces = c.TraceCount
		}
	}

	writeJSON(w, map[string]interface{}{
		"components":      comps,
		"count":           len(comps),
		"profiled_traces": profiledTraces, // requests with a function profile in the window
		"window_minutes":  windowMin,
		"sort_by":         sortBy,
	})
}

// GET /api/v1/php-versions — unique PHP versions with request counts
func (a *API) handlePhpVersions(w http.ResponseWriter, r *http.Request) {
	windowMin := queryInt(r, "window", 60)
	cutoff := time.Now().Unix() - int64(windowMin)*60

	domClause, domArgs := DomainFilter(r, "host")

	query := `SELECT php_version, COUNT(*) as request_count
		FROM traces WHERE timestamp >= ? AND php_version != ''` + domClause + `
		GROUP BY php_version ORDER BY request_count DESC`
	args := append([]interface{}{cutoff}, domArgs...)

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type VersionRow struct {
		Version      string `json:"version"`
		RequestCount int64  `json:"request_count"`
	}
	var versions []VersionRow
	for rows.Next() {
		var v VersionRow
		rows.Scan(&v.Version, &v.RequestCount)
		versions = append(versions, v)
	}

	writeJSON(w, map[string]interface{}{
		"versions":       versions,
		"count":          len(versions),
		"window_minutes": windowMin,
	})
}

// GET /api/v1/alerts — list alerts
func (a *API) handleAlerts(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	ack := queryInt(r, "ack", 0)
	severity := r.URL.Query().Get("severity")

	// Role-based domain restriction
	domClause, domArgs := DomainFilter(r, "domain")

	if len(domArgs) == 0 {
		// No domain restriction, use storage method directly
		alerts, err := a.store.GetAlerts(limit, ack == 1, severity)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"alerts": alerts, "count": len(alerts)})
		return
	}

	// Domain-restricted: query with domain filter
	query := `SELECT id, rule_name, severity, metric, current_value, threshold_value,
		domain, message, created_at, acknowledged, ack_at
		FROM alerts WHERE 1=1`
	var args []interface{}

	if ack != 1 {
		query += " AND acknowledged = 0"
	}
	if severity != "" {
		query += " AND severity = ?"
		args = append(args, severity)
	}
	query += domClause
	args = append(args, domArgs...)
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := a.store.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	var alerts []AlertEvent
	for rows.Next() {
		var al AlertEvent
		var acked int
		var ackAt *int64
		if err := rows.Scan(&al.ID, &al.RuleName, &al.Severity, &al.Metric,
			&al.CurrentValue, &al.ThresholdValue, &al.Domain, &al.Message,
			&al.CreatedAt, &acked, &ackAt); err != nil {
			continue
		}
		al.Acknowledged = acked == 1
		al.AckAt = ackAt
		alerts = append(alerts, al)
	}
	writeJSON(w, map[string]interface{}{"alerts": alerts, "count": len(alerts)})
}

// GET /api/v1/alerts/count — unacknowledged alert count (for badge polling)
func (a *API) handleAlertCount(w http.ResponseWriter, r *http.Request) {
	count, err := a.store.GetActiveAlertCount()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"count": count})
}

// POST /api/v1/alerts/{id}/ack — acknowledge an alert
func (a *API) handleAlertAck(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, 400, "invalid alert id")
		return
	}
	if err := a.store.AcknowledgeAlert(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "id": id})
}

// GET /api/v1/health
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	count, _ := a.store.GetTraceCount()
	out := map[string]interface{}{
		"status":       "ok",
		"version":      version,
		"traces":       count,
		"auth_enabled": a.jwtSecret != "",
		"ingest":       a.ingest.stats(), // POST /api/v1/ingest counters
	}
	if a.cloud != nil {
		out["cloud"] = a.cloud.CloudStatus()
	}
	if in := a.metrics.Input(); in.SHMPath != "" || in.Mode != "" {
		out["input"] = in
	}
	writeJSON(w, out)
}

// handleWebSocket upgrades HTTP to WebSocket and registers the client
func (a *API) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	client := &wsClient{
		hub:  a.hub,
		conn: conn,
		send: make(chan []byte, wsSendBufSize),
	}
	a.hub.register <- client
	go client.writePump()
	go client.readPump()
}

// Helpers
func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func queryFloat(r *http.Request, key string, defaultVal float64) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

// cmdAPI starts the API server
func cmdAPI(args []string) {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	dbPath := fs.String("s", "/var/lib/phpray/traces.db", "SQLite database path")
	addr := fs.String("addr", ":9191", "Listen address")
	jwtSecret := fs.String("secret", "", "JWT secret (or set PHPRAY_JWT_SECRET)")
	fs.Parse(args)

	secret := *jwtSecret
	if secret == "" {
		secret = os.Getenv("PHPRAY_JWT_SECRET")
	}

	store, err := OpenStorage(*dbPath)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer store.Close()

	// Initialize domain→user mapping (DirectAdmin, default path)
	store.InitDomainResolver("")

	hub := NewHub()
	go hub.Run()

	api := NewAPI(store, hub, secret)
	api.sink = &storeSink{store: store, hub: hub} // no daemon here: ingested traces go straight to SQLite

	count, _ := store.GetTraceCount()
	fmt.Printf("🔦 PHPRay API Server v%s\n", version)
	fmt.Printf("  Database: %s (%d traces)\n", *dbPath, count)
	fmt.Printf("  Listen:   %s\n", *addr)
	if secret != "" {
		fmt.Println("  Auth:     JWT enabled (HS256)")
	} else {
		fmt.Println("  Auth:     disabled (no secret configured)")
	}
	fmt.Println()
	fmt.Println("Endpoints:")
	fmt.Println("  GET /api/v1/overview              — server summary")
	fmt.Println("  GET /api/v1/traces                — list traces (filterable)")
	fmt.Println("  GET /api/v1/traces/{id}           — trace detail + queries + marks")
	fmt.Println("  GET /api/v1/domains               — domain list with stats + owner")
	fmt.Println("  GET /api/v1/domains/{d}/stats     — per-domain detail + owner")
	fmt.Println("  GET /api/v1/domain-owners         — domain→user mapping (DirectAdmin)")
	fmt.Println("  GET /api/v1/queries/slow          — top slow query fingerprints")
	fmt.Println("  GET /api/v1/queries/frequent      — most common queries by exec count")
	fmt.Println("  GET /api/v1/users                 — per-user statistics (sorted)")
	fmt.Println("  GET /api/v1/users/{uid}           — user detail with domains")
	fmt.Println("  GET /api/v1/aggregates            — per-minute aggregates")
	fmt.Println("  GET /api/v1/timeseries            — time-series data for charts")
	fmt.Println("  GET /api/v1/diagnostics           — AI diagnostics (all domains)")
	fmt.Println("  GET /api/v1/diagnostics/{d}       — AI diagnostics for domain")
	fmt.Println("  GET /api/v1/diagnostics/{d}/report — export diagnostic report (md/txt)")
	fmt.Println("  GET /api/v1/components            — PHP component/plugin breakdown")
	fmt.Println("  GET /api/v1/php-versions          — unique PHP versions with counts")
	fmt.Println("  GET /api/v1/alerts                — list alerts (filterable)")
	fmt.Println("  GET /api/v1/alerts/count           — unacknowledged alert count")
	fmt.Println("  POST /api/v1/alerts/{id}/ack       — acknowledge an alert")
	fmt.Println("  GET /api/v1/health                — health check")
	fmt.Println("  POST /api/v1/ingest               — trace batches from agents (ingest v1 envelope or array)")
	fmt.Println("  GET /metrics                      — Prometheus metrics")
	fmt.Println("  WS  /ws/traces                    — live trace stream")
	fmt.Println()

	if err := http.ListenAndServe(*addr, api.router); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// handleDiagShare publikuje raport i oddaje adres do wyslania komus.
//
// Ta sama droga co `phpray-collector report --share`, tylko z panelu:
// wysylamy DANE ustalen, serwer renderuje je wlasnym szablonem. Nazwa
// strony jest wycinana z kazdego pola tekstowego przez BezNazwyStrony().
func (a *API) handleDiagShare(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	windowMin := queryInt(r, "window", 60)

	if allowed := AllowedDomains(r); allowed != nil {
		found := false
		for _, d := range allowed {
			if strings.Contains(domain, d) || strings.Contains(d, domain) {
				found = true
				break
			}
		}
		if !found {
			writeError(w, 403, "domain not in your allowed list")
			return
		}
	}

	ctx, err := a.store.LoadTracesForDiag(domain, windowMin)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	engine := NewDiagEngine()
	report := engine.Analyze(ctx)
	report.WindowMin = windowMin

	url, wygasa, err := opublikujRaport(report, "")
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]any{"url": url, "expires_at": wygasa})
}
