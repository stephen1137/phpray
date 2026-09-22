package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTP ingest — POST /api/v1/ingest on the local collector API.
//
// Accepts the ingest v1 batch envelope (docs/spec/ingest-v1.md, the same one the
// WordPress plugin sends to PHPRay Cloud):
//
//	{"batch_id":"b_…","sent_at":<unix ms>,"agent":"wp-plugin/0.1.1","traces":[ <trace records> ]}
//
// or, for compatibility, a bare JSON array of trace records. Records are the
// JSONL trace record (docs-site/docs/usage/trace-format.md), optionally with a
// site_id. They enter the collector through the same path as ring-buffer and
// JSONL traces (Daemon.addToBatch): live dashboard, SQLite, CLI and — when
// [cloud] is enabled — the Cloud shipper.
//
// Access: with an [auth] secret the endpoint requires the same Bearer JWT as the
// dashboard API (401 otherwise); without a secret it only accepts loopback
// clients (403 for anything else). Limits: 8 MB decoded body (413), 500 records
// per batch (413), malformed JSON (400). batch_id is deduplicated for 24 h.

const (
	maxIngestBodyBytes = 8 << 20       // decoded body limit
	maxIngestRecords   = 500           // records per batch (ingest v1 §4)
	ingestDedupTTL     = 24 * time.Hour // batch_id memory (ingest v1 §1)
)

// TraceSink receives traces from the HTTP ingest endpoint. In serve mode it is
// the Daemon; in api-only mode a storeSink writes straight to SQLite.
type TraceSink interface {
	IngestTrace(t Trace)
}

// storeSink stores ingested traces directly (no daemon in this process).
type storeSink struct {
	store *Storage
	hub   *Hub
}

func (s *storeSink) IngestTrace(t Trace) {
	if s.hub != nil {
		s.hub.Broadcast(t)
	}
	if err := s.store.StoreTrace(&t); err != nil {
		log.Printf("ingest: store trace %q: %v", t.ID, err)
	}
}

// ingestEnvelope is the ingest v1 batch envelope; records stay raw so one bad
// record is dropped without rejecting the batch.
type ingestEnvelope struct {
	BatchID string            `json:"batch_id"`
	SentAt  int64             `json:"sent_at"`
	Agent   string            `json:"agent"`
	Traces  []json.RawMessage `json:"traces"`
}

// IngestStats are the endpoint counters shown by /api/v1/health and `phpray status`.
type IngestStats struct {
	Batches    int64 `json:"batches"`    // accepted batches (202)
	Traces     int64 `json:"traces"`     // records handed to the collector
	Dropped    int64 `json:"dropped"`    // records skipped inside accepted batches
	Duplicates int64 `json:"duplicates"` // batches repeated within 24 h
	Rejected   int64 `json:"rejected"`   // requests answered 4xx/5xx
}

type ingestCounters struct {
	batches, traces, dropped, duplicates, rejected atomic.Int64
}

func (c *ingestCounters) stats() IngestStats {
	return IngestStats{
		Batches:    c.batches.Load(),
		Traces:     c.traces.Load(),
		Dropped:    c.dropped.Load(),
		Duplicates: c.duplicates.Load(),
		Rejected:   c.rejected.Load(),
	}
}

// batchDedup remembers batch ids for ingestDedupTTL so a retried batch
// (same batch_id after a lost response) is not stored twice.
type batchDedup struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastPrune time.Time
	now       func() time.Time
}

func newBatchDedup() *batchDedup {
	return &batchDedup{seen: make(map[string]time.Time), now: time.Now}
}

// Seen records id and reports whether it was already known within the TTL.
func (d *batchDedup) Seen(id string) bool {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.lastPrune) > 10*time.Minute || len(d.seen) > 200000 {
		for k, t := range d.seen {
			if now.Sub(t) > ingestDedupTTL {
				delete(d.seen, k)
			}
		}
		if len(d.seen) > 200000 { // pathological flood of unique ids: start over rather than grow
			d.seen = make(map[string]time.Time)
		}
		d.lastPrune = now
	}
	if t, ok := d.seen[id]; ok && now.Sub(t) <= ingestDedupTTL {
		return true
	}
	d.seen[id] = now
	return false
}

// ingestGuard: with a JWT secret the endpoint takes the dashboard token
// (Authorization: Bearer …, same middleware as /api/v1/*); without a secret it
// is open to loopback clients only.
func ingestGuard(secret string) func(http.Handler) http.Handler {
	auth := AuthMiddleware(secret)
	return func(next http.Handler) http.Handler {
		authed := auth(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" && !isLoopbackAddr(r.RemoteAddr) {
				writeError(w, http.StatusForbidden, "ingest is limited to 127.0.0.1/::1 while [auth] secret is empty")
				return
			}
			authed.ServeHTTP(w, r)
		})
	}
}

// isLoopbackAddr reports whether a RemoteAddr ("ip:port") is 127.0.0.0/8 or ::1.
// An empty address means a unix socket / in-process listener and counts as local.
func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "@" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var errIngestTooLarge = errors.New("body exceeds the 8 MB limit")

// readIngestBody returns the (gunzipped) request body, capped at maxIngestBodyBytes.
func readIngestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(w, r.Body, maxIngestBodyBytes)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, fmt.Errorf("gzip: %v", err)
		}
		defer gz.Close()
		rd = gz
	}
	body, err := io.ReadAll(io.LimitReader(rd, maxIngestBodyBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large") {
			return nil, errIngestTooLarge
		}
		return nil, fmt.Errorf("read body: %v", err)
	}
	if len(body) > maxIngestBodyBytes {
		return nil, errIngestTooLarge
	}
	return body, nil
}

// parseIngestBody accepts the envelope (JSON object) or a bare array of records.
func parseIngestBody(body []byte) (*ingestEnvelope, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, errors.New("empty body")
	}
	env := &ingestEnvelope{}
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &env.Traces); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %v", err)
		}
	case '{':
		if err := json.Unmarshal(trimmed, env); err != nil {
			return nil, fmt.Errorf("invalid JSON envelope: %v", err)
		}
	default:
		return nil, errors.New("expected an ingest v1 envelope (JSON object) or a JSON array of trace records")
	}
	return env, nil
}

// normalizeIngestedTrace fills what the record producers leave out: missing or
// millisecond timestamps, http_count/http_ms next to http_calls (the WP plugin
// sends the list only), component percentages.
func normalizeIngestedTrace(t *Trace, now int64) {
	if t.Ts > 1_000_000_000_000 { // unix ms (trace-format.md) → seconds, as stored everywhere else
		t.Ts /= 1000
	}
	if t.Ts <= 0 {
		t.Ts = now
	}
	if t.HTTPCount == nil && len(t.HTTPCalls) > 0 {
		n := uint16(len(t.HTTPCalls))
		t.HTTPCount = &n
		if t.HTTPMs == 0 {
			for _, h := range t.HTTPCalls {
				t.HTTPMs += h.Ms
			}
		}
	}
	if t.DBCount == nil && len(t.Queries) > 0 {
		n := uint16(len(t.Queries))
		t.DBCount = &n
	}
	if t.DurationMs > 0 {
		for i := range t.Components {
			if t.Components[i].Pct == 0 && t.Components[i].Ms > 0 {
				t.Components[i].Pct = t.Components[i].Ms / t.DurationMs * 100
			}
		}
	}
}

// POST /api/v1/ingest
func (a *API) handleIngest(w http.ResponseWriter, r *http.Request) {
	if a.sink == nil {
		a.ingest.rejected.Add(1)
		writeError(w, http.StatusServiceUnavailable, "ingest unavailable: no trace sink in this process")
		return
	}
	body, err := readIngestBody(w, r)
	if err != nil {
		a.ingest.rejected.Add(1)
		if errors.Is(err, errIngestTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	env, err := parseIngestBody(body)
	if err != nil {
		a.ingest.rejected.Add(1)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(env.Traces) > maxIngestRecords {
		a.ingest.rejected.Add(1)
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("batch exceeds %d records, split in half and retry", maxIngestRecords))
		return
	}
	if env.BatchID != "" && a.dedup.Seen(env.BatchID) {
		a.ingest.duplicates.Add(1)
		writeIngestResponse(w, 0, 0, true)
		return
	}

	accepted, dropped := 0, 0
	now := time.Now().Unix()
	for _, raw := range env.Traces {
		var t Trace
		if err := json.Unmarshal(raw, &t); err != nil || (t.Host == "" && t.URI == "") {
			dropped++
			continue
		}
		normalizeIngestedTrace(&t, now)
		a.sink.IngestTrace(t)
		accepted++
	}
	a.ingest.batches.Add(1)
	a.ingest.traces.Add(int64(accepted))
	a.ingest.dropped.Add(int64(dropped))
	writeIngestResponse(w, accepted, dropped, false)
}

// writeIngestResponse answers 202 like the Cloud does; dropped_quota is always 0
// locally (the WordPress plugin reads it to clear its sampling state).
func writeIngestResponse(w http.ResponseWriter, accepted, dropped int, duplicate bool) {
	out := map[string]interface{}{
		"accepted":      accepted,
		"dropped":       dropped,
		"dropped_quota": 0,
		"server_time":   time.Now().UnixMilli(),
	}
	if duplicate {
		out["duplicate"] = true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(out)
}
