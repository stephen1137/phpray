package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created by an older collector: traces without php_version/app/profiled,
// and no slow_queries/aggregates/alerts tables at all. Opening it must migrate, not fail.
func TestOpenStorageMigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	old := `CREATE TABLE traces (
		id INTEGER PRIMARY KEY AUTOINCREMENT, phpray_id TEXT NOT NULL, timestamp INTEGER NOT NULL,
		uid INTEGER NOT NULL, pid INTEGER NOT NULL, host TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT 'GET',
		uri TEXT NOT NULL DEFAULT '', uri_fingerprint TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 200,
		duration_ms REAL NOT NULL, cpu_user_ms REAL NOT NULL DEFAULT 0, cpu_sys_ms REAL NOT NULL DEFAULT 0,
		memory_peak_mb REAL NOT NULL DEFAULT 0, db_count INTEGER NOT NULL DEFAULT 0, db_ms REAL NOT NULL DEFAULT 0,
		http_count INTEGER NOT NULL DEFAULT 0, http_ms REAL NOT NULL DEFAULT 0, file_count INTEGER NOT NULL DEFAULT 0,
		file_ms REAL NOT NULL DEFAULT 0, redis_count INTEGER NOT NULL DEFAULT 0, redis_ms REAL NOT NULL DEFAULT 0,
		wp INTEGER NOT NULL DEFAULT 0, n_plus_one INTEGER NOT NULL DEFAULT 0, trace_level TEXT NOT NULL DEFAULT 'summary',
		mark_count INTEGER NOT NULL DEFAULT 0, error_count INTEGER NOT NULL DEFAULT 0, components_json TEXT,
		username TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0);
	INSERT INTO traces (phpray_id, timestamp, uid, pid, duration_ms) VALUES ('old-1', 1700000000, 1000, 1, 12.5);`
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenStorage(path)
	if err != nil {
		t.Fatalf("OpenStorage on old schema: %v", err)
	}
	defer s.Close()
	for _, col := range []string{"php_version", "app", "profiled"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('traces') WHERE name = ?", col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column traces.%s missing after migration (err=%v)", col, err)
		}
	}
	var cnt int
	if err := s.db.QueryRow("SELECT count(*) FROM traces WHERE php_version = ''").Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("old row not preserved/defaulted: cnt=%d err=%v", cnt, err)
	}
	// a fresh open must be a no-op
	s2, err := OpenStorage(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}
