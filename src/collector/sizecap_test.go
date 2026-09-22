package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The size cap purges the oldest traces first and reports the database size.
func TestPurgeOldestFractionAndSize(t *testing.T) {
	s, err := OpenStorage(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < 100; i++ {
		if err := s.StoreTrace(&Trace{ID: "t", Ts: base + int64(i), UID: 1, PID: 1, Host: "a", Method: "GET", URI: "/", Status: 200, DurationMs: 1, Level: "summary"}); err != nil {
			t.Fatal(err)
		}
	}
	size, err := s.DBSizeBytes()
	if err != nil || size <= 0 {
		t.Fatalf("DBSizeBytes = %d, %v", size, err)
	}
	n, err := s.PurgeOldestFraction(0.10)
	if err != nil || n != 10 {
		t.Fatalf("purged %d (%v), want 10", n, err)
	}
	var oldest int64
	if err := s.db.QueryRow("SELECT min(timestamp) FROM traces").Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest != base+10 {
		t.Fatalf("oldest remaining = %d, want %d (the 10 oldest must go first)", oldest, base+10)
	}
}
