package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIngest is a minimal PHPRay console: records batches, deduplicates by batch_id,
// and answers with a configurable status per path.
type fakeIngest struct {
	mu        sync.Mutex
	status    map[string]int // path → status override (0 = 202)
	traces    [][]map[string]interface{}
	batchIDs  []string
	dupes     int
	seen      map[string]bool
	minutes   []map[string]interface{}
	requests  int
	auth      []string
	gzipped   int
	sampleHdr string
	control   []map[string]interface{}
	acks      [][]string
	maxTraces int // 413 above this many traces (0 = unlimited)
}

func newFakeIngest() *fakeIngest {
	return &fakeIngest{status: map[string]int{}, seen: map[string]bool{}}
}

func (f *fakeIngest) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		var body []byte
		if r.Body != nil {
			rd := io.Reader(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(r.Body)
				if err != nil {
					w.WriteHeader(400)
					return
				}
				rd = zr
				f.gzipped++
			}
			body, _ = io.ReadAll(rd)
		}
		if st := f.status[r.URL.Path]; st != 0 {
			if st == 429 {
				w.Header().Set("Retry-After", "2")
			}
			w.WriteHeader(st)
			w.Write([]byte(`{"error":"forced"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/traces":
			var env struct {
				BatchID string                   `json:"batch_id"`
				Agent   string                   `json:"agent"`
				Traces  []map[string]interface{} `json:"traces"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				w.WriteHeader(400)
				return
			}
			if f.maxTraces > 0 && len(env.Traces) > f.maxTraces {
				w.WriteHeader(413)
				return
			}
			f.batchIDs = append(f.batchIDs, env.BatchID)
			if f.seen[env.BatchID] {
				f.dupes++
			} else {
				f.seen[env.BatchID] = true
				f.traces = append(f.traces, env.Traces)
			}
			if f.sampleHdr != "" {
				w.Header().Set("X-PHPRay-Trace-Sample", f.sampleHdr)
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"accepted":` + itoa(len(env.Traces)) + `,"dropped_quota":0}`))
		case "/v1/aggregates":
			var env struct {
				BatchID string                   `json:"batch_id"`
				Minutes []map[string]interface{} `json:"minutes"`
				Server  map[string]interface{}   `json:"server"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				w.WriteHeader(400)
				return
			}
			f.batchIDs = append(f.batchIDs, env.BatchID)
			if f.seen[env.BatchID] {
				f.dupes++
			} else {
				f.seen[env.BatchID] = true
				f.minutes = append(f.minutes, env.Minutes...)
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"accepted":` + itoa(len(env.Minutes)) + `,"duplicates":0}`))
		case "/v1/control":
			msgs := f.control
			f.control = nil
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"server_time": time.Now().UnixMilli(), "messages": msgs})
		case "/v1/control/ack":
			var a struct {
				IDs []string `json:"ids"`
			}
			json.Unmarshal(body, &a)
			f.acks = append(f.acks, a.IDs)
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(404)
		}
	})
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + jsonInt(n)) }
func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newTestShipper(t *testing.T, endpoint string, mod func(*CloudConfig)) *Shipper {
	t.Helper()
	cfg := DefaultCloudConfig()
	cfg.Enabled = true
	cfg.Endpoint = endpoint
	cfg.ServerKey = "prk_0123456789abcdef0123456789abcdef"
	cfg.BufferDir = filepath.Join(t.TempDir(), "buf")
	cfg.BufferMaxMB = 1
	cfg.ControlEnabled = false
	if mod != nil {
		mod(&cfg)
	}
	s, err := NewShipper(cfg)
	if err != nil {
		t.Fatalf("NewShipper: %v", err)
	}
	s.logf = func(format string, args ...interface{}) { t.Logf(format, args...) }
	s.client.Timeout = 2 * time.Second
	return s
}

func sampleTrace(ts int64, host, uri, level string, ms float64) *Trace {
	dbc := uint16(3)
	return &Trace{
		Ts: ts, Host: host, URI: uri, Level: level, DurationMs: ms, Status: 200, Method: "GET",
		Docroot: "/var/www/" + host, PhpVer: "8.3.33", App: "wordpress", Profiled: 1,
		DBCount: &dbc, DBMs: 12.5, CPUUserMs: 40, CPUSysMs: 5, MemoryMB: 32,
		Queries:    []Query{{SQL: "SELECT * FROM wp_posts WHERE ID = 123 AND post_status = 'publish'", Ms: 1.2}},
		Errors:     []Error{{Type: "E_WARNING", Msg: strings.Repeat("x", 300)}},
		Components: []Component{{Name: "plugins/woocommerce", InclNs: 100_000_000, SelfNs: 60_000_000, Calls: 1000}},
	}
}

func TestSiteID(t *testing.T) {
	a := SiteID("shop.example", "/var/www/shop")
	if len(a) != 16 || a != SiteID("shop.example", "/var/www/shop") {
		t.Fatalf("site id unstable or wrong length: %q", a)
	}
	if a == SiteID("shop.example", "/var/www/other") || a == SiteID("other.example", "/var/www/shop") {
		t.Fatalf("site id must depend on host and docroot")
	}
}

func TestCloudTracesBatchPrivacyAndDedup(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)

	now := time.Now().Unix()
	s.Observe(sampleTrace(now, "shop.example", "/sklep/?utm_source=fb&page=2", "normal", 200))
	s.Observe(sampleTrace(now, "shop.example", "/", "summary", 50)) // summary: aggregates only
	s.Observe(sampleTrace(now, "shop.example", "/checkout/", "full", 900))
	s.flushTraces()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.traces) != 1 || len(f.traces[0]) != 2 {
		t.Fatalf("expected 1 batch with 2 traces (summary excluded), got %d batches %v", len(f.traces), f.traces)
	}
	if !strings.HasPrefix(f.batchIDs[0], "b_") {
		t.Fatalf("batch_id must start with b_: %q", f.batchIDs[0])
	}
	if f.auth[0] != "Bearer prk_0123456789abcdef0123456789abcdef" {
		t.Fatalf("authorization header: %q", f.auth[0])
	}
	tr := f.traces[0][0]
	if tr["site_id"] != SiteID("shop.example", "/var/www/shop.example") {
		t.Fatalf("site_id missing or wrong: %v", tr["site_id"])
	}
	if tr["uri"] != "/sklep/?page=*&utm_source=*" {
		t.Fatalf("uri not fingerprinted: %v", tr["uri"])
	}
	q := tr["queries"].([]interface{})[0].(map[string]interface{})
	if strings.Contains(q["sql"].(string), "123") || strings.Contains(q["sql"].(string), "publish") {
		t.Fatalf("sql not fingerprinted: %v", q["sql"])
	}
	e := tr["errors"].([]interface{})[0].(map[string]interface{})
	if len(e["msg"].(string)) != cloudErrMsgMax {
		t.Fatalf("error message not truncated: %d", len(e["msg"].(string)))
	}
	st := s.CloudStatus()
	if !st.Connected || st.SentTraces != 2 || st.LastStatus != 202 {
		t.Fatalf("status after send: %+v", st)
	}
}

func TestCloudRetryKeepsBatchID(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	now := time.Now().Unix()

	f.mu.Lock()
	f.status["/v1/traces"] = 500
	f.mu.Unlock()
	s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	s.flushTraces() // 500 → buffered, backoff 5 s
	files, _ := s.buffer.Stats()
	if files != 1 {
		t.Fatalf("expected 1 buffered batch after 500, got %d", files)
	}
	st := s.CloudStatus()
	if st.Connected || st.BackoffUntil == 0 {
		t.Fatalf("expected backoff after 500: %+v", st)
	}

	// console back, backoff elapsed → replay delivers the SAME payload (same batch_id)
	f.mu.Lock()
	delete(f.status, "/v1/traces")
	f.mu.Unlock()
	s.now = func() time.Time { return time.Now().Add(10 * time.Second) }
	s.replayBuffer()
	files, _ = s.buffer.Stats()
	f.mu.Lock()
	ids := append([]string(nil), f.batchIDs...)
	nTraces := len(f.traces)
	f.mu.Unlock()
	if files != 0 || nTraces != 1 {
		t.Fatalf("replay: files=%d traces=%d", files, nTraces)
	}
	// send the buffered payload once more by hand: the console must see a duplicate, not new data
	payload := s.tracesPayload(ids[0], []json.RawMessage{cloudTraceJSON(sampleTrace(now, "a.example", "/x", "normal", 100), "s", false)})
	if err := s.deliver("/v1/traces", payload); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dupes != 1 || len(f.traces) != 1 {
		t.Fatalf("dedup by batch_id failed: dupes=%d batches=%d", f.dupes, len(f.traces))
	}
}

func TestCloud413SplitsBatch(t *testing.T) {
	f := newFakeIngest()
	f.maxTraces = 2
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	}
	s.flushTraces()
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, b := range f.traces {
		if len(b) > 2 {
			t.Fatalf("batch larger than the limit was accepted: %d", len(b))
		}
		total += len(b)
	}
	if total != 5 {
		t.Fatalf("expected all 5 traces delivered in split batches, got %d in %d batches", total, len(f.traces))
	}
	if s.CloudStatus().SentTraces != 5 {
		t.Fatalf("sent counter: %d", s.CloudStatus().SentTraces)
	}
}

func TestCloud429BackoffAnd401Stop(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	now := time.Now().Unix()

	f.mu.Lock()
	f.status["/v1/traces"] = 429
	f.mu.Unlock()
	s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	s.flushTraces()
	st := s.CloudStatus()
	if st.LastStatus != 429 || st.BackoffUntil < time.Now().Unix()+1 {
		t.Fatalf("expected Retry-After backoff: %+v", st)
	}
	f.mu.Lock()
	before := f.requests
	f.mu.Unlock()
	s.Observe(sampleTrace(now, "a.example", "/y", "normal", 100))
	s.flushTraces() // still backing off: no request, batch buffered
	f.mu.Lock()
	after := f.requests
	f.mu.Unlock()
	if after != before {
		t.Fatalf("request sent during backoff")
	}
	if files, _ := s.buffer.Stats(); files != 2 {
		t.Fatalf("expected 2 buffered batches, got %d", files)
	}

	// 401: everything stops, status surfaces it
	f.mu.Lock()
	f.status["/v1/traces"] = 401
	f.mu.Unlock()
	s.now = func() time.Time { return time.Now().Add(5 * time.Second) }
	s.Observe(sampleTrace(now, "a.example", "/z", "normal", 100))
	s.flushTraces()
	st = s.CloudStatus()
	if !st.AuthFailed || st.Connected {
		t.Fatalf("expected auth_failed after 401: %+v", st)
	}
	f.mu.Lock()
	before = f.requests
	f.mu.Unlock()
	s.flushAggregates(true)
	s.replayBuffer()
	f.mu.Lock()
	after = f.requests
	f.mu.Unlock()
	if after != before {
		t.Fatalf("requests continued after 401")
	}
}

func TestCloudBufferLimitDropsOldestTraces(t *testing.T) {
	dir := t.TempDir()
	buf, err := NewCloudBuffer(dir, 30*1024)
	if err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"batch_id":"b_x","traces":[` + strings.Repeat(`{"a":"`+strings.Repeat("y", 1000)+`"},`, 9) + `{"a":1}]}`) // ~10 KB, 10 traces
	buf.Store("aggregates", "b_agg", []byte(`{"batch_id":"b_agg","minutes":[]}`))
	for i := 0; i < 5; i++ {
		if err := buf.Store("traces", "b_"+jsonInt(i), big); err != nil {
			t.Fatal(err)
		}
	}
	files, bytes := buf.Stats()
	if bytes > 30*1024 || files > 4 {
		t.Fatalf("buffer not trimmed: files=%d bytes=%d", files, bytes)
	}
	list := buf.List()
	if list[0].Kind != "aggregates" {
		t.Fatalf("aggregates must survive trimming, got %+v", list)
	}
	names := []string{}
	for _, f := range list {
		names = append(names, f.Name)
	}
	for _, n := range names {
		if strings.Contains(n, "-traces-0.json") || strings.Contains(n, "-traces-1.json") {
			t.Fatalf("oldest trace batches must be dropped first: %v", names)
		}
	}
	if buf.TakeDropped() != 20 {
		t.Fatalf("dropped counter should count traces in removed batches")
	}

	// shipper with unreachable endpoint: batches land in the buffer, status reports it
	s := newTestShipper(t, "http://127.0.0.1:9", func(c *CloudConfig) { c.BufferDir = filepath.Join(dir, "s") })
	s.client.Timeout = 500 * time.Millisecond
	s.Observe(sampleTrace(time.Now().Unix(), "a.example", "/x", "alert", 5000))
	s.flushTraces()
	st := s.CloudStatus()
	if st.Connected || st.BufferedFiles != 1 || st.LastError == "" {
		t.Fatalf("unreachable endpoint: %+v", st)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "s"))
	if len(entries) != 1 {
		t.Fatalf("expected one buffered file, got %d", len(entries))
	}
}

func TestCloudAggregatesMinute(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	base := int64(1789737540) // a minute boundary
	s.now = func() time.Time { return time.Unix(base+180, 0) }
	durations := []float64{100, 200, 300, 400, 1000}
	for i, d := range durations {
		tr := sampleTrace(base+int64(i), "shop.example", "/checkout/", "normal", d)
		if i == 4 {
			tr.Status = 503
			tr.N1 = 1
		}
		s.Observe(tr)
	}
	s.Observe(sampleTrace(base+5, "shop.example", "/", "summary", 50))
	s.flushAggregates(false)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.minutes) != 1 {
		t.Fatalf("expected 1 minute row, got %d", len(f.minutes))
	}
	m := f.minutes[0]
	if m["ts"].(float64) != float64(base) || m["requests"].(float64) != 6 || m["errors_5xx"].(float64) != 1 || m["profiled"].(float64) != 6 {
		t.Fatalf("minute counters: %v", m)
	}
	if m["site_id"] != SiteID("shop.example", "/var/www/shop.example") || m["app"] != "wordpress" {
		t.Fatalf("site/app: %v %v", m["site_id"], m["app"])
	}
	dur := m["duration_ms"].(map[string]interface{})
	if dur["max"].(float64) != 1000 || dur["p95"].(float64) != 1000 || dur["p50"].(float64) != 300 {
		t.Fatalf("duration stats: %v", dur)
	}
	db := m["db"].(map[string]interface{})
	if db["queries"].(float64) != 18 || db["n1_requests"].(float64) != 1 {
		t.Fatalf("db stats: %v", db)
	}
	lv := m["levels"].(map[string]interface{})
	if lv["normal"].(float64) != 5 || lv["summary"].(float64) != 1 {
		t.Fatalf("levels: %v", lv)
	}
	comps := m["components"].([]interface{})
	c0 := comps[0].(map[string]interface{})
	if c0["name"] != "plugins/woocommerce" || c0["calls"].(float64) != 6000 || c0["incl_ms"].(float64) != 600 {
		t.Fatalf("components: %v", comps)
	}
	uris := m["top_uris"].([]interface{})
	if uris[0].(map[string]interface{})["uri"] != "/checkout/" || uris[0].(map[string]interface{})["requests"].(float64) != 5 {
		t.Fatalf("top_uris: %v", uris)
	}
	if s.CloudStatus().SentAggregates != 1 {
		t.Fatalf("sent aggregates counter: %d", s.CloudStatus().SentAggregates)
	}
	if len(s.minutes) != 0 {
		t.Fatalf("closed minute not released")
	}
}

func TestCloudGzipAboveThreshold(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	now := time.Now().Unix()
	for i := 0; i < 40; i++ { // 40 × ~600 B > 8 KB
		s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	}
	s.flushTraces()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gzipped != 1 || len(f.traces) != 1 || len(f.traces[0]) != 40 {
		t.Fatalf("expected one gzipped batch of 40, got gz=%d batches=%d", f.gzipped, len(f.traces))
	}
}

func TestCloudSampleRateAndServerHeader(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, func(c *CloudConfig) { c.TraceSampleRate = 0 })
	now := time.Now().Unix()
	for i := 0; i < 20; i++ {
		s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	}
	s.flushTraces()
	f.mu.Lock()
	n := len(f.traces)
	f.sampleHdr = "50"
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("sample_rate 0 must ship no traces, got %d batches", n)
	}
	// server-imposed sampling arrives in the response header of an accepted batch
	s.cfg.TraceSampleRate = 100
	s.Observe(sampleTrace(now, "a.example", "/x", "normal", 100))
	s.flushTraces()
	if st := s.CloudStatus(); st.ServerSampleRate != 50 {
		t.Fatalf("server sample rate not applied: %+v", st)
	}
}

func TestCloudControlMessages(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)
	rate := 10
	f.mu.Lock()
	f.control = []map[string]interface{}{
		{"id": "m_1", "type": "profile", "site_id": "abc", "url_prefix": "/checkout/", "sample_rate": 100, "until": time.Now().Unix() + 600},
		{"id": "m_2", "type": "config", "site_id": "", "traces": map[string]interface{}{"sample_rate": rate}},
	}
	f.mu.Unlock()
	n, err := s.pollControl()
	if err != nil || n != 2 {
		t.Fatalf("pollControl: n=%d err=%v", n, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acks) != 1 || len(f.acks[0]) != 2 || f.acks[0][0] != "m_1" || f.acks[0][1] != "m_2" {
		t.Fatalf("ack: %v", f.acks)
	}
	st := s.CloudStatus()
	if st.TraceSampleRate != 10 || st.ControlMessages != 2 {
		t.Fatalf("config message not applied: %+v", st)
	}
	s.mu.Lock()
	eff := s.effectiveSampleRateLocked("any")
	s.mu.Unlock()
	if eff != 10 {
		t.Fatalf("effective sample rate: %d", eff)
	}
}

func TestCloudConfigParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.toml")
	os.WriteFile(path, []byte(`
[input]
jsonl_path = "/tmp/x.jsonl"

[cloud]
enabled = true
endpoint = "https://ingest.example.test"
server_key = "prk_abc"
buffer.max_mb = 8
traces.sample_rate = 25

[cloud.privacy]
mask_host = true
`), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	c := cfg.Cloud
	if !c.Enabled || c.Endpoint != "https://ingest.example.test" || c.ServerKey != "prk_abc" || c.BufferMaxMB != 8 || c.TraceSampleRate != 25 || !c.MaskHost {
		t.Fatalf("cloud config: %+v", c)
	}
	dc := cfg.ToDaemonConfig()
	if !strings.HasSuffix(dc.Cloud.BufferDir, "/cloud-buffer") {
		t.Fatalf("buffer dir default: %q", dc.Cloud.BufferDir)
	}
}

// Backlog draining: the console counts requests per minute, so a backlog of
// small buffered batches has to go out as few, full requests. Regression for
// a production collector that sat on 122 buffered files because it replayed
// one file per request against a 12-request budget it was already using up.
func TestReplayBufferCoalescesTraceBatches(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)

	// 40 stored batches of 3 traces: 120 records, well under one batch limit.
	for i := 0; i < 40; i++ {
		var recs []json.RawMessage
		for j := 0; j < 3; j++ {
			recs = append(recs, json.RawMessage(`{"id":"t`+jsonInt(i*3+j)+`","ts":1789737540,"host":"a.example"}`))
		}
		if err := s.buffer.Store("traces", newBatchID(), s.tracesPayload(newBatchID(), recs)); err != nil {
			t.Fatal(err)
		}
	}
	s.replayBuffer()

	if files, _ := s.buffer.Stats(); files != 0 {
		t.Fatalf("buffer not drained: %d files left", files)
	}
	f.mu.Lock()
	requests, batches := f.requests, len(f.traces)
	total := 0
	for _, b := range f.traces {
		total += len(b)
	}
	f.mu.Unlock()
	if total != 120 {
		t.Fatalf("expected all 120 records delivered, got %d in %d batches", total, batches)
	}
	if requests != 1 {
		t.Fatalf("expected the backlog to go out as one request, got %d", requests)
	}
	if st := s.CloudStatus(); st.SentTraces != 120 {
		t.Fatalf("sent_traces = %d, want 120", st.SentTraces)
	}
}

// The per-batch limits still hold: 600 records cannot travel as one request.
func TestReplayBufferRespectsBatchRecordLimit(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)

	for i := 0; i < 6; i++ {
		var recs []json.RawMessage
		for j := 0; j < 100; j++ {
			recs = append(recs, json.RawMessage(`{"id":"t`+jsonInt(i*100+j)+`","ts":1789737540,"host":"a.example"}`))
		}
		if err := s.buffer.Store("traces", newBatchID(), s.tracesPayload(newBatchID(), recs)); err != nil {
			t.Fatal(err)
		}
	}
	s.replayBuffer()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.traces) != 2 {
		t.Fatalf("600 records must not travel as one batch of 500: got %d batches", len(f.traces))
	}
	if len(f.traces[0]) != 500 || len(f.traces[1]) != 100 {
		t.Fatalf("batches should fill to the limit: %d then %d", len(f.traces[0]), len(f.traces[1]))
	}
}

// Błąd nie podlega próbkowaniu: przy 0 % do chmury i tak jedzie ślad żądania
// z błędem PHP albo odpowiedzią 5xx. Regresja po tym, jak agregat pokazywał
// jeden błąd na dobę, a zakładka Errors w konsoli była pusta.
func TestBledyOmijajaProbkowanie(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, func(c *CloudConfig) { c.TraceSampleRate = 0 })

	now := time.Now().Unix()
	// sampleTrace wkłada E_WARNING do każdego śladu — tu sprawdzamy właśnie,
	// że samo ostrzeżenie NIE wystarcza, żeby ominąć próbkowanie.
	zwykly := sampleTrace(now, "a.example", "/fast", "normal", 20)
	piecset := sampleTrace(now, "a.example", "/boom", "normal", 30)
	piecset.Status = 503
	fatalny := sampleTrace(now, "a.example", "/fatal", "normal", 40)
	fatalny.Errors = []Error{{Type: "E_ERROR", Msg: "call to undefined function"}}
	alert := sampleTrace(now, "a.example", "/slow", "alert", 9000)

	for _, tr := range []*Trace{zwykly, piecset, fatalny, alert} {
		s.Observe(tr)
	}
	s.flushTraces()

	f.mu.Lock()
	defer f.mu.Unlock()
	wyslane := map[string]bool{}
	for _, batch := range f.traces {
		for _, rec := range batch {
			if u, ok := rec["uri"].(string); ok {
				wyslane[u] = true
			}
		}
	}
	for _, uri := range []string{"/boom", "/fatal", "/slow"} {
		if !wyslane[uri] {
			t.Fatalf("przy próbkowaniu 0%% %s musi pojechać do chmury, wysłane: %v", uri, wyslane)
		}
	}
	if wyslane["/fast"] {
		t.Fatalf("zwykłe żądanie przy próbkowaniu 0%% nie powinno jechać, wysłane: %v", wyslane)
	}
}
