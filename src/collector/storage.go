package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Storage wraps SQLite database for trace persistence
type Storage struct {
	db *sql.DB

	// User resolver (UID → username)
	users *UserResolver

	// Domain resolver (domain → owner username+UID via DirectAdmin)
	domains *DomainResolver

	// Prepared statements
	insertTrace    *sql.Stmt
	insertQuery    *sql.Stmt
	insertHTTPCall *sql.Stmt
	insertMark     *sql.Stmt
	insertError    *sql.Stmt
}

// OpenStorage opens or creates the SQLite database with schema
func OpenStorage(path string) (*Storage, error) {
	// Create directory if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create dir %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// WAL mode for concurrent reads during writes
	db.SetMaxOpenConns(1) // SQLite is single-writer

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	s := &Storage{db: db, users: NewUserResolver()}
	// DomainResolver initialized separately via SetDADataPath after config is loaded

	// Run migrations for existing databases
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}

	if err := s.prepareStatements(); err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare statements: %w", err)
	}

	return s, nil
}

func createSchema(db *sql.DB) error {
	schema := `
	-- Raw request traces
	CREATE TABLE IF NOT EXISTS traces (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		phpray_id TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		uid INTEGER NOT NULL,
		pid INTEGER NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT 'GET',
		uri TEXT NOT NULL DEFAULT '',
		uri_fingerprint TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 200,
		duration_ms REAL NOT NULL,
		cpu_user_ms REAL NOT NULL DEFAULT 0,
		cpu_sys_ms REAL NOT NULL DEFAULT 0,
		memory_peak_mb REAL NOT NULL DEFAULT 0,
		db_count INTEGER NOT NULL DEFAULT 0,
		db_ms REAL NOT NULL DEFAULT 0,
		http_count INTEGER NOT NULL DEFAULT 0,
		http_ms REAL NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0,
		file_ms REAL NOT NULL DEFAULT 0,
		redis_count INTEGER NOT NULL DEFAULT 0,
		redis_ms REAL NOT NULL DEFAULT 0,
		wp INTEGER NOT NULL DEFAULT 0,
		n_plus_one INTEGER NOT NULL DEFAULT 0,
		trace_level TEXT NOT NULL DEFAULT 'summary',
		mark_count INTEGER NOT NULL DEFAULT 0,
		error_count INTEGER NOT NULL DEFAULT 0,
		components_json TEXT NOT NULL DEFAULT '[]',
		username TEXT NOT NULL DEFAULT '',
		php_version TEXT NOT NULL DEFAULT '',
		profiled INTEGER NOT NULL DEFAULT 0,
		app TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- Individual queries (only for normal+ traces)
	CREATE TABLE IF NOT EXISTS queries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id INTEGER NOT NULL,
		sql_text TEXT NOT NULL,
		sql_fingerprint TEXT NOT NULL,
		duration_ms REAL NOT NULL,
		offset_ms REAL NOT NULL DEFAULT 0,
		affected_rows INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL DEFAULT 'mysqli',
		backtrace TEXT NOT NULL DEFAULT '',
		FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
	);

	-- HTTP calls
	CREATE TABLE IF NOT EXISTS http_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id INTEGER NOT NULL,
		url TEXT NOT NULL,
		duration_ms REAL NOT NULL,
		offset_ms REAL NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0,
		backtrace TEXT NOT NULL DEFAULT '',
		FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
	);

	-- User marks
	CREATE TABLE IF NOT EXISTS marks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		offset_ms REAL NOT NULL,
		FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
	);

	-- PHP errors
	CREATE TABLE IF NOT EXISTS errors (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id INTEGER NOT NULL,
		error_type TEXT NOT NULL,
		message TEXT NOT NULL,
		offset_ms REAL NOT NULL DEFAULT 0,
		FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
	);

	-- Aggregated slow queries (fingerprinted, deduplicated)
	CREATE TABLE IF NOT EXISTS slow_queries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		fingerprint TEXT NOT NULL,
		sample_sql TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		total_ms REAL NOT NULL DEFAULT 0,
		exec_count INTEGER NOT NULL DEFAULT 0,
		avg_ms REAL NOT NULL DEFAULT 0,
		max_ms REAL NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL,
		UNIQUE(fingerprint, host)
	);

	-- Per-minute aggregates
	CREATE TABLE IF NOT EXISTS aggregates_1m (
		bucket INTEGER NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		request_count INTEGER NOT NULL DEFAULT 0,
		error_count INTEGER NOT NULL DEFAULT 0,
		avg_duration_ms REAL NOT NULL DEFAULT 0,
		p95_duration_ms REAL NOT NULL DEFAULT 0,
		max_duration_ms REAL NOT NULL DEFAULT 0,
		total_db_queries INTEGER NOT NULL DEFAULT 0,
		n1_count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, host)
	);

	-- Alerts
	CREATE TABLE IF NOT EXISTS alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		rule_name TEXT NOT NULL,
		severity TEXT NOT NULL DEFAULT 'warning',
		metric TEXT NOT NULL,
		current_value REAL NOT NULL,
		threshold_value REAL NOT NULL,
		domain TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		acknowledged INTEGER NOT NULL DEFAULT 0,
		ack_at INTEGER
	);

	-- Indexes
	CREATE INDEX IF NOT EXISTS idx_traces_timestamp ON traces(timestamp);
	CREATE INDEX IF NOT EXISTS idx_traces_host ON traces(host);
	CREATE INDEX IF NOT EXISTS idx_traces_host_ts ON traces(host, timestamp);
	CREATE INDEX IF NOT EXISTS idx_traces_duration ON traces(duration_ms DESC);
	CREATE INDEX IF NOT EXISTS idx_traces_level ON traces(trace_level);
	CREATE INDEX IF NOT EXISTS idx_queries_trace ON queries(trace_id);
	CREATE INDEX IF NOT EXISTS idx_queries_fingerprint ON queries(sql_fingerprint);
	CREATE INDEX IF NOT EXISTS idx_traces_php_version ON traces(php_version);
	CREATE INDEX IF NOT EXISTS idx_traces_app ON traces(app);
	CREATE INDEX IF NOT EXISTS idx_slow_queries_fp ON slow_queries(fingerprint);
	CREATE INDEX IF NOT EXISTS idx_aggregates_bucket ON aggregates_1m(bucket);
	CREATE INDEX IF NOT EXISTS idx_alerts_created ON alerts(created_at);
	CREATE INDEX IF NOT EXISTS idx_alerts_ack ON alerts(acknowledged);
	`

	return applySchema(db, schema)
}

// runMigrations applies schema changes to existing databases
func runMigrations(db *sql.DB) error {
	// Migration 1: Add username column to traces
	var colCount int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traces') WHERE name='username'`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN username TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add username column: %w", err)
		}
		// Create index for username-based queries
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_traces_username ON traces(username)`)
		if err != nil {
			return fmt.Errorf("create username index: %w", err)
		}
		log.Printf("Migration: added username column to traces table")
	}

	// Migration 2: Add backtrace column to queries table
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('queries') WHERE name='backtrace'`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE queries ADD COLUMN backtrace TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add queries backtrace column: %w", err)
		}
		log.Printf("Migration: added backtrace column to queries table")
	}

	// Migration 3: Add php_version column to traces
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traces') WHERE name='php_version'`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN php_version TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add php_version column: %w", err)
		}
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_traces_php_version ON traces(php_version)`)
		if err != nil {
			return fmt.Errorf("create php_version index: %w", err)
		}
		log.Printf("Migration: added php_version column to traces table")
	}

	// Migration 4: Add backtrace column to http_calls table
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('http_calls') WHERE name='backtrace'`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE http_calls ADD COLUMN backtrace TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add http_calls backtrace column: %w", err)
		}
		log.Printf("Migration: added backtrace column to http_calls table")
	}

	// Migration 5: Add redis_count and redis_ms columns to traces
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traces') WHERE name='redis_count'`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN redis_count INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add redis_count column: %w", err)
		}
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN redis_ms REAL NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add redis_ms column: %w", err)
		}
		log.Printf("Migration: added redis_count and redis_ms columns to traces table")
	}

	// Migration 6: Add profiled column to traces (function profile collected for the request).
	// Component aggregation only counts profiled requests — otherwise averages lie.
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traces') WHERE name='profiled'`).Scan(&colCount)
	if err != nil {
		return fmt.Errorf("check profiled column: %w", err)
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN profiled INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add profiled column: %w", err)
		}
		// Traces stored before ring v4 carried components only when the (always-on) profiler ran
		_, err = db.Exec(`UPDATE traces SET profiled = 1 WHERE components_json != '[]' AND components_json != ''`)
		if err != nil {
			return fmt.Errorf("backfill profiled column: %w", err)
		}
		log.Printf("Migration: added profiled column to traces table")
	}

	// Migration 7: Add app column (application detected by the extension: wordpress, prestashop, ...)
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('traces') WHERE name='app'`).Scan(&colCount)
	if err != nil {
		return fmt.Errorf("check app column: %w", err)
	}
	if colCount == 0 {
		_, err = db.Exec(`ALTER TABLE traces ADD COLUMN app TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add app column: %w", err)
		}
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_traces_app ON traces(app)`)
		if err != nil {
			return fmt.Errorf("create app index: %w", err)
		}
		log.Printf("Migration: added app column to traces table")
	}

	return nil
}

func (s *Storage) prepareStatements() error {
	var err error

	s.insertTrace, err = s.db.Prepare(`
		INSERT INTO traces (phpray_id, timestamp, uid, pid, host, method, uri, uri_fingerprint,
			status, duration_ms, cpu_user_ms, cpu_sys_ms, memory_peak_mb,
			db_count, db_ms, http_count, http_ms, file_count, file_ms,
			redis_count, redis_ms,
			wp, n_plus_one, trace_level, mark_count, error_count, components_json, username, php_version,
			profiled, app)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	s.insertQuery, err = s.db.Prepare(`
		INSERT INTO queries (trace_id, sql_text, sql_fingerprint, duration_ms, offset_ms, affected_rows, source, backtrace)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	s.insertHTTPCall, err = s.db.Prepare(`
		INSERT INTO http_calls (trace_id, url, duration_ms, offset_ms, status, backtrace)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	s.insertMark, err = s.db.Prepare(`
		INSERT INTO marks (trace_id, name, offset_ms)
		VALUES (?, ?, ?)
	`)
	if err != nil {
		return err
	}

	s.insertError, err = s.db.Prepare(`
		INSERT INTO errors (trace_id, error_type, message, offset_ms)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	return nil
}

// StoreTrace stores a complete trace with all sub-records in a transaction
func (s *Storage) StoreTrace(t *Trace) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// URI fingerprint
	uriFp := t.URIFp
	if uriFp == "" {
		uriFp = t.URI
	}

	// Insert main trace
	dbCount := uint16(0)
	if t.DBCount != nil {
		dbCount = *t.DBCount
	}
	httpCount := uint16(0)
	if t.HTTPCount != nil {
		httpCount = *t.HTTPCount
	}
	fileCount := uint16(0)
	if t.FileCount != nil {
		fileCount = *t.FileCount
	}
	redisCount := uint16(0)
	if t.RedisCount != nil {
		redisCount = *t.RedisCount
	}

	// Serialize components to JSON
	componentsJSON := "[]"
	if len(t.Components) > 0 {
		if cj, err := json.Marshal(t.Components); err == nil {
			componentsJSON = string(cj)
		}
	}

	// Resolve UID to username
	username := ""
	if s.users != nil {
		username = s.users.Resolve(t.UID)
	}

	result, err := tx.Stmt(s.insertTrace).Exec(
		t.ID, t.Ts, t.UID, t.PID, t.Host, t.Method, t.URI, uriFp,
		t.Status, t.DurationMs, t.CPUUserMs, t.CPUSysMs, t.MemoryMB,
		dbCount, t.DBMs, httpCount, t.HTTPMs, fileCount, t.FileMs,
		redisCount, t.RedisMs,
		t.WP, t.N1, t.Level, len(t.Marks), len(t.Errors), componentsJSON, username, t.PhpVer,
		t.Profiled, appValue(t.App),
	)
	if err != nil {
		return err
	}

	traceID, err := result.LastInsertId()
	if err != nil {
		return err
	}

	// Insert queries with fingerprinting
	for _, q := range t.Queries {
		fp := FingerprintSQL(q.SQL)
		source := "mysqli"
		if q.Source == 1 {
			source = "pdo"
		}
		btJSON := ""
		if len(q.BT) > 0 {
			if bj, err := json.Marshal(q.BT); err == nil {
				btJSON = string(bj)
			}
		}
		if _, err := tx.Stmt(s.insertQuery).Exec(traceID, q.SQL, fp, q.Ms, q.T, q.Rows, source, btJSON); err != nil {
			return err
		}

		// Update slow_queries aggregate
		if _, err := tx.Exec(`
			INSERT INTO slow_queries (fingerprint, sample_sql, host, total_ms, exec_count, avg_ms, max_ms, last_seen)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?)
			ON CONFLICT(fingerprint, host) DO UPDATE SET
				total_ms = total_ms + excluded.total_ms,
				exec_count = exec_count + 1,
				avg_ms = (total_ms + excluded.total_ms) / (exec_count + 1),
				max_ms = MAX(max_ms, excluded.max_ms),
				last_seen = excluded.last_seen,
				sample_sql = CASE WHEN excluded.max_ms > max_ms THEN excluded.sample_sql ELSE sample_sql END
		`, fp, q.SQL, t.Host, q.Ms, q.Ms, q.Ms, t.Ts); err != nil {
			log.Printf("Warning: slow_queries upsert failed: %v", err)
		}
	}

	// Insert HTTP calls
	for _, h := range t.HTTPCalls {
		btJSON := ""
		if len(h.BT) > 0 {
			if bj, err := json.Marshal(h.BT); err == nil {
				btJSON = string(bj)
			}
		}
		if _, err := tx.Stmt(s.insertHTTPCall).Exec(traceID, h.URL, h.Ms, h.T, h.Status, btJSON); err != nil {
			return err
		}
	}

	// Insert marks
	for _, m := range t.Marks {
		if _, err := tx.Stmt(s.insertMark).Exec(traceID, m.Name, m.T); err != nil {
			return err
		}
	}

	// Insert errors
	for _, e := range t.Errors {
		if _, err := tx.Stmt(s.insertError).Exec(traceID, e.Type, e.Msg, e.T); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// StoreBatch stores multiple traces efficiently
func (s *Storage) StoreBatch(traces []Trace) (int, error) {
	stored := 0
	for i := range traces {
		if err := s.StoreTrace(&traces[i]); err != nil {
			log.Printf("Warning: failed to store trace %s: %v", traces[i].ID, err)
			continue
		}
		stored++
	}
	return stored, nil
}

// PurgeOld removes traces older than the given duration
func (s *Storage) PurgeOld(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).Unix()

	// Delete cascade handles queries, http_calls, marks, errors
	result, err := s.db.Exec("DELETE FROM traces WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Close closes the database
// InitDomainResolver initializes the domain→user mapping from DirectAdmin data.
// Call after OpenStorage with the configured DA data path (empty = default).
func (s *Storage) InitDomainResolver(daPath string) {
	s.domains = NewDomainResolver(daPath, s.users)
}

func (s *Storage) Close() error {
	if s.insertTrace != nil {
		s.insertTrace.Close()
	}
	if s.insertQuery != nil {
		s.insertQuery.Close()
	}
	if s.insertHTTPCall != nil {
		s.insertHTTPCall.Close()
	}
	if s.insertMark != nil {
		s.insertMark.Close()
	}
	if s.insertError != nil {
		s.insertError.Close()
	}
	return s.db.Close()
}

// Aggregate1m computes per-minute aggregates for a given bucket (Unix timestamp rounded to minute)
func (s *Storage) Aggregate1m(bucket int64) error {
	bucketEnd := bucket + 60

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO aggregates_1m (bucket, host, request_count, error_count,
			avg_duration_ms, p95_duration_ms, max_duration_ms, total_db_queries, n1_count)
		SELECT
			? as bucket,
			host,
			COUNT(*) as request_count,
			SUM(CASE WHEN error_count > 0 THEN 1 ELSE 0 END) as error_count,
			AVG(duration_ms) as avg_duration_ms,
			0 as p95_duration_ms,
			MAX(duration_ms) as max_duration_ms,
			SUM(db_count) as total_db_queries,
			SUM(CASE WHEN n_plus_one = 1 THEN 1 ELSE 0 END) as n1_count
		FROM traces
		WHERE timestamp >= ? AND timestamp < ?
		GROUP BY host
	`, bucket, bucket, bucketEnd)
	if err != nil {
		return err
	}

	// Update P95 — SQLite doesn't have PERCENTILE_CONT, so we compute it manually per host
	rows, err := s.db.Query(`
		SELECT DISTINCT host FROM traces WHERE timestamp >= ? AND timestamp < ?
	`, bucket, bucketEnd)
	if err != nil {
		return err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var host string
		rows.Scan(&host)
		hosts = append(hosts, host)
	}

	for _, host := range hosts {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM traces WHERE timestamp >= ? AND timestamp < ? AND host = ?`,
			bucket, bucketEnd, host).Scan(&count)

		if count == 0 {
			continue
		}

		p95Offset := int(float64(count) * 0.95)
		if p95Offset >= count {
			p95Offset = count - 1
		}

		var p95 float64
		s.db.QueryRow(`
			SELECT duration_ms FROM traces
			WHERE timestamp >= ? AND timestamp < ? AND host = ?
			ORDER BY duration_ms ASC
			LIMIT 1 OFFSET ?
		`, bucket, bucketEnd, host, p95Offset).Scan(&p95)

		s.db.Exec(`UPDATE aggregates_1m SET p95_duration_ms = ? WHERE bucket = ? AND host = ?`,
			p95, bucket, host)
	}

	return nil
}

// PurgeOldAggregates removes aggregate rows older than the given duration
func (s *Storage) PurgeOldAggregates(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).Unix()
	result, err := s.db.Exec("DELETE FROM aggregates_1m WHERE bucket < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// GetAggregates1m returns aggregate data for a time range
func (s *Storage) GetAggregates1m(from, to int64, host string) ([]Aggregate1m, error) {
	query := `SELECT bucket, host, request_count, error_count, avg_duration_ms, 
		p95_duration_ms, max_duration_ms, total_db_queries, n1_count
		FROM aggregates_1m WHERE bucket >= ? AND bucket < ?`
	args := []interface{}{from, to}

	if host != "" {
		query += " AND host = ?"
		args = append(args, host)
	}
	query += " ORDER BY bucket ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Aggregate1m
	for rows.Next() {
		var a Aggregate1m
		if err := rows.Scan(&a.Bucket, &a.Host, &a.RequestCount, &a.ErrorCount,
			&a.AvgDurationMs, &a.P95DurationMs, &a.MaxDurationMs,
			&a.TotalDBQueries, &a.N1Count); err != nil {
			continue
		}
		results = append(results, a)
	}
	return results, nil
}

type Aggregate1m struct {
	Bucket        int64
	Host          string
	RequestCount  int
	ErrorCount    int
	AvgDurationMs float64
	P95DurationMs float64
	MaxDurationMs float64
	TotalDBQueries int
	N1Count       int
}

// GetTraceCount returns the number of stored traces
func (s *Storage) GetTraceCount() (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM traces").Scan(&count)
	return count, err
}

// GetSlowQueries returns top slow query fingerprints
func (s *Storage) GetSlowQueries(limit int) ([]SlowQuery, error) {
	rows, err := s.db.Query(`
		SELECT fingerprint, sample_sql, host, total_ms, exec_count, avg_ms, max_ms, last_seen
		FROM slow_queries
		ORDER BY avg_ms DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SlowQuery
	for rows.Next() {
		var sq SlowQuery
		if err := rows.Scan(&sq.Fingerprint, &sq.SampleSQL, &sq.Host,
			&sq.TotalMs, &sq.ExecCount, &sq.AvgMs, &sq.MaxMs, &sq.LastSeen); err != nil {
			continue
		}
		results = append(results, sq)
	}
	return results, nil
}

type SlowQuery struct {
	Fingerprint string
	SampleSQL   string
	Host        string
	TotalMs     float64
	ExecCount   int64
	AvgMs       float64
	MaxMs       float64
	LastSeen    int64
}

// UserStats holds per-user statistics
type UserStats struct {
	UID          uint32  `json:"uid"`
	Username     string  `json:"username"`
	Requests     int64   `json:"requests"`
	AvgMs        float64 `json:"avg_duration_ms"`
	MaxMs        float64 `json:"max_duration_ms"`
	ErrorCount   int64   `json:"error_count"`
	N1Count      int64   `json:"n1_count"`
	TotalQueries int64   `json:"total_queries"`
	DomainCount  int64   `json:"domain_count"`
	Domains      string  `json:"domains,omitempty"`
}

// GetUserStats returns per-user aggregate statistics
func (s *Storage) GetUserStats(windowMin int, limit int, sortBy string) ([]UserStats, error) {
	cutoff := time.Now().Unix() - int64(windowMin)*60

	// Validate sort column
	orderCol := "avg_ms"
	switch sortBy {
	case "requests", "reqs":
		orderCol = "reqs"
	case "avg_ms", "avg":
		orderCol = "avg_ms"
	case "max_ms", "max":
		orderCol = "max_ms"
	case "errors":
		orderCol = "errs"
	case "queries":
		orderCol = "total_queries"
	}

	query := fmt.Sprintf(`
		SELECT uid, COALESCE(username, '') as username,
			COUNT(*) as reqs,
			AVG(duration_ms) as avg_ms,
			MAX(duration_ms) as max_ms,
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as errs,
			SUM(n_plus_one) as n1,
			SUM(db_count) as total_queries,
			COUNT(DISTINCT host) as domain_count,
			GROUP_CONCAT(DISTINCT host) as domains
		FROM traces
		WHERE timestamp >= ? AND uid >= 500
		GROUP BY uid
		ORDER BY %s DESC
		LIMIT ?
	`, orderCol)

	rows, err := s.db.Query(query, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []UserStats
	for rows.Next() {
		var u UserStats
		var domains sql.NullString
		if err := rows.Scan(&u.UID, &u.Username, &u.Requests, &u.AvgMs, &u.MaxMs,
			&u.ErrorCount, &u.N1Count, &u.TotalQueries, &u.DomainCount, &domains); err != nil {
			continue
		}
		if domains.Valid {
			u.Domains = domains.String
		}

		// If username is empty, try to resolve from /etc/passwd
		if u.Username == "" && s.users != nil {
			u.Username = s.users.Resolve(u.UID)
		}

		results = append(results, u)
	}
	return results, nil
}

// GetUserDetail returns detailed stats for a specific user
func (s *Storage) GetUserDetail(uid uint32, windowMin int) (*UserStats, []interface{}, error) {
	cutoff := time.Now().Unix() - int64(windowMin)*60

	var u UserStats
	u.UID = uid
	var domains sql.NullString

	err := s.db.QueryRow(`
		SELECT COALESCE(username, '') as username,
			COUNT(*) as reqs,
			AVG(duration_ms) as avg_ms,
			MAX(duration_ms) as max_ms,
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as errs,
			SUM(n_plus_one) as n1,
			SUM(db_count) as total_queries,
			COUNT(DISTINCT host) as domain_count,
			GROUP_CONCAT(DISTINCT host) as domains
		FROM traces
		WHERE timestamp >= ? AND uid = ?
	`, cutoff, uid).Scan(&u.Username, &u.Requests, &u.AvgMs, &u.MaxMs,
		&u.ErrorCount, &u.N1Count, &u.TotalQueries, &u.DomainCount, &domains)
	if err != nil {
		return nil, nil, err
	}
	if domains.Valid {
		u.Domains = domains.String
	}
	if u.Username == "" && s.users != nil {
		u.Username = s.users.Resolve(uid)
	}

	// Top domains for this user
	type DomainStat struct {
		Host     string  `json:"host"`
		Requests int64   `json:"requests"`
		AvgMs    float64 `json:"avg_ms"`
		MaxMs    float64 `json:"max_ms"`
		Errors   int64   `json:"errors"`
	}

	rows, err := s.db.Query(`
		SELECT host, COUNT(*) as reqs, AVG(duration_ms) as avg_ms,
			MAX(duration_ms) as max_ms,
			SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as errs
		FROM traces
		WHERE timestamp >= ? AND uid = ? AND host != ''
		GROUP BY host ORDER BY avg_ms DESC LIMIT 20
	`, cutoff, uid)
	if err != nil {
		return &u, nil, nil
	}
	defer rows.Close()

	var domainStats []interface{}
	for rows.Next() {
		var d DomainStat
		rows.Scan(&d.Host, &d.Requests, &d.AvgMs, &d.MaxMs, &d.Errors)
		domainStats = append(domainStats, d)
	}

	return &u, domainStats, nil
}

// GetFrequentQueries returns top query fingerprints ordered by execution count
func (s *Storage) GetFrequentQueries(limit int, domain string, windowMin int) ([]SlowQuery, error) {
	query := `SELECT fingerprint, sample_sql, host, total_ms, exec_count, avg_ms, max_ms, last_seen
		FROM slow_queries WHERE 1=1`
	var args []interface{}

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

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SlowQuery
	for rows.Next() {
		var sq SlowQuery
		if err := rows.Scan(&sq.Fingerprint, &sq.SampleSQL, &sq.Host,
			&sq.TotalMs, &sq.ExecCount, &sq.AvgMs, &sq.MaxMs, &sq.LastSeen); err != nil {
			continue
		}
		results = append(results, sq)
	}
	return results, nil
}

// ─── Alert Storage Methods ──────────────────────────────────────────────────

// StoreAlert inserts a new alert event and returns its ID
func (s *Storage) StoreAlert(event AlertEvent) (int64, error) {
	result, err := s.db.Exec(`
		INSERT INTO alerts (rule_name, severity, metric, current_value, threshold_value,
			domain, message, created_at, acknowledged)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	`, event.RuleName, event.Severity, event.Metric, event.CurrentValue,
		event.ThresholdValue, event.Domain, event.Message, event.CreatedAt)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetAlerts returns alert events with optional filters
func (s *Storage) GetAlerts(limit int, includeAck bool, severity string) ([]AlertEvent, error) {
	query := `SELECT id, rule_name, severity, metric, current_value, threshold_value,
		domain, message, created_at, acknowledged, ack_at
		FROM alerts WHERE 1=1`
	var args []interface{}

	if !includeAck {
		query += " AND acknowledged = 0"
	}
	if severity != "" {
		query += " AND severity = ?"
		args = append(args, severity)
	}

	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []AlertEvent
	for rows.Next() {
		var a AlertEvent
		var acked int
		var ackAt *int64
		if err := rows.Scan(&a.ID, &a.RuleName, &a.Severity, &a.Metric,
			&a.CurrentValue, &a.ThresholdValue, &a.Domain, &a.Message,
			&a.CreatedAt, &acked, &ackAt); err != nil {
			continue
		}
		a.Acknowledged = acked == 1
		a.AckAt = ackAt
		results = append(results, a)
	}
	return results, nil
}

// AcknowledgeAlert marks an alert as acknowledged
func (s *Storage) AcknowledgeAlert(id int64) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE alerts SET acknowledged = 1, ack_at = ? WHERE id = ?`, now, id)
	return err
}

// GetActiveAlertCount returns the count of unacknowledged alerts
func (s *Storage) GetActiveAlertCount() (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE acknowledged = 0`).Scan(&count)
	return count, err
}

// GetLastAlertTime returns the most recent alert time for a rule+domain combo (for cooldown)
func (s *Storage) GetLastAlertTime(ruleName, domain string) (int64, error) {
	var lastTime int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(created_at), 0) FROM alerts
		WHERE rule_name = ? AND domain = ?`, ruleName, domain).Scan(&lastTime)
	return lastTime, err
}

// appValue normalises the extension's "app" field for storage ("-" and "" mean unknown)
func appValue(app string) string {
	if app == "-" {
		return ""
	}
	return app
}

// ComponentAgg holds aggregated stats for a single component across multiple traces
type ComponentAgg struct {
	Name       string  `json:"name"`
	TotalMs    float64 `json:"total_ms"`
	AvgMs      float64 `json:"avg_ms"`
	MaxMs      float64 `json:"max_ms"`
	TotalCalls int64   `json:"total_calls"`
	TraceCount int64   `json:"trace_count"` // number of requests this component appeared in
	AvgPct     float64 `json:"avg_pct"`     // average % of request time
	Category   string  `json:"category"`    // "plugin", "theme", "vendor", "core", "wp-includes"
}

// GetComponentStats aggregates component data across traces in the time window.
// Only requests with profiled = 1 are considered: the function profile is a
// sampled layer (phpray.profile_mode), so averaging over unprofiled requests
// would silently dilute every number. The second return value is the number of
// profiled requests in the window (the denominator behind the aggregates).
func (s *Storage) GetComponentStats(windowMin int, domain string, domClause string, domArgs []interface{}, limit int, sortBy string) ([]ComponentAgg, int64, error) {
	cutoff := time.Now().Unix() - int64(windowMin)*60

	query := `SELECT components_json, duration_ms FROM traces
		WHERE timestamp >= ? AND profiled = 1` + domClause
	args := append([]interface{}{cutoff}, domArgs...)

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var profiledTraces int64

	// Aggregate components across traces
	type compAccum struct {
		totalMs    float64
		maxMs      float64
		totalCalls int64
		traceCount int64
		totalPct   float64
	}
	accum := make(map[string]*compAccum)

	for rows.Next() {
		var cjson string
		var durationMs float64
		if err := rows.Scan(&cjson, &durationMs); err != nil {
			continue
		}
		profiledTraces++
		if cjson == "" || cjson == "[]" {
			continue // profiled, but no component code ran
		}
		var components []Component
		if err := json.Unmarshal([]byte(cjson), &components); err != nil {
			continue
		}
		for _, c := range components {
			a, ok := accum[c.Name]
			if !ok {
				a = &compAccum{}
				accum[c.Name] = a
			}
			a.totalMs += c.Ms
			if c.Ms > a.maxMs {
				a.maxMs = c.Ms
			}
			a.totalCalls += int64(c.Calls)
			a.traceCount++
			a.totalPct += c.Pct
		}
	}

	// Convert to slice
	result := make([]ComponentAgg, 0, len(accum))
	for name, a := range accum {
		cat := categorizeComponent(name)
		result = append(result, ComponentAgg{
			Name:       name,
			TotalMs:    a.totalMs,
			AvgMs:      a.totalMs / float64(a.traceCount),
			MaxMs:      a.maxMs,
			TotalCalls: a.totalCalls,
			TraceCount: a.traceCount,
			AvgPct:     a.totalPct / float64(a.traceCount),
			Category:   cat,
		})
	}

	// Sort
	sortComponents(result, sortBy)

	// Limit
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}

	return result, profiledTraces, nil
}

// categorizeComponent returns a category for a component name
func categorizeComponent(name string) string {
	switch {
	case len(name) > 8 && name[:8] == "plugins/":
		return "plugin"
	case len(name) > 7 && name[:7] == "themes/":
		return "theme"
	case len(name) > 7 && name[:7] == "vendor/":
		return "vendor"
	case name == "wp-includes":
		return "wp-core"
	case name == "core":
		return "core"
	default:
		return "other"
	}
}

// sortComponents sorts component aggregates by the specified field
func sortComponents(comps []ComponentAgg, sortBy string) {
	switch sortBy {
	case "total_ms":
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.TotalMs > b.TotalMs })
	case "max_ms":
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.MaxMs > b.MaxMs })
	case "calls":
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.TotalCalls > b.TotalCalls })
	case "traces":
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.TraceCount > b.TraceCount })
	case "pct":
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.AvgPct > b.AvgPct })
	default: // avg_ms
		sortComponentsBy(comps, func(a, b ComponentAgg) bool { return a.AvgMs > b.AvgMs })
	}
}

func sortComponentsBy(comps []ComponentAgg, less func(a, b ComponentAgg) bool) {
	// Simple insertion sort (small N, typically <50 components)
	for i := 1; i < len(comps); i++ {
		key := comps[i]
		j := i - 1
		for j >= 0 && less(key, comps[j]) {
			comps[j+1] = comps[j]
			j--
		}
		comps[j+1] = key
	}
}


// DBSizeBytes returns the on-disk size of the database (pages × page size).
func (s *Storage) DBSizeBytes() (int64, error) {
	var pages, pageSize int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}

// PurgeOldestFraction deletes the oldest fraction (0–1) of traces by timestamp;
// the delete cascade drops their queries, http calls, marks and errors.
func (s *Storage) PurgeOldestFraction(fraction float64) (int64, error) {
	var total int64
	if err := s.db.QueryRow("SELECT count(*) FROM traces").Scan(&total); err != nil {
		return 0, err
	}
	n := int64(float64(total) * fraction)
	if n < 1 {
		n = 1
	}
	if total == 0 {
		return 0, nil
	}
	res, err := s.db.Exec("DELETE FROM traces WHERE id IN (SELECT id FROM traces ORDER BY timestamp ASC, id ASC LIMIT ?)", n)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
