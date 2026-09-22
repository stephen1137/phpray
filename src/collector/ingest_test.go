package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newIngestAPI wires an API to a daemon with a real SQLite store, the way
// `serve` does; traces reach the database after d.flushBatch().
func newIngestAPI(t *testing.T, secret string) (*API, *Daemon, *Storage) {
	t.Helper()
	store, err := OpenStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := DefaultDaemonConfig()
	cfg.BatchSize = 1000
	d := NewDaemon(cfg)
	d.store = store
	api := NewAPI(store, nil, secret)
	api.metrics = &DaemonMetrics{daemon: d}
	api.sink = d
	return api, d, store
}

type ingestOpt func(*http.Request)

func withRemote(addr string) ingestOpt { return func(r *http.Request) { r.RemoteAddr = addr } }
func withBearer(tok string) ingestOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func withGzip() ingestOpt { return func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") } }

func postIngest(api *API, body []byte, opts ...ingestOpt) (*httptest.ResponseRecorder, map[string]interface{}) {
	req := httptest.NewRequest("POST", "/api/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:41000"
	for _, o := range opts {
		o(req)
	}
	rr := httptest.NewRecorder()
	api.router.ServeHTTP(rr, req)
	var out map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

// wpRecord is the record the WordPress plugin ships (build_record + site_id).
func wpRecord(uri string, level string) map[string]interface{} {
	return map[string]interface{}{
		"ts": time.Now().Unix(), "host": "shop.example.com", "method": "GET", "uri": uri,
		"status": 200, "duration_ms": 321.5, "cpu_user_ms": 40.0, "cpu_sys_ms": 5.0, "memory_peak_mb": 64.0,
		"wp": 1, "app": "wordpress", "source": "wp-plugin", "level": level,
		"db_count": 12, "db_ms": 30.25,
		"queries": []map[string]interface{}{
			{"sql": "SELECT * FROM wp_posts WHERE ID = ?", "ms": 12.5, "caller": "wp-content/plugins/woocommerce/includes/x.php"},
		},
		"http_calls": []map[string]interface{}{
			{"url": "https://api.example.com/v1/rates", "method": "POST", "status": 200, "ms": 250.0},
		},
		"errors": []map[string]interface{}{
			{"type": "warning", "msg": "Undefined index: foo", "file": "wp-content/plugins/x/y.php", "line": 12},
		},
		"n1":         0,
		"components": []map[string]interface{}{{"name": "plugins/woocommerce", "incl_ms": 120.5, "calls": 34}},
		"profiled":   1,
		"site_id":    "0123456789abcdef",
	}
}

func envelope(batchID string, recs ...map[string]interface{}) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"batch_id": batchID, "sent_at": time.Now().UnixMilli(), "agent": "wp-plugin/0.1.1", "traces": recs,
	})
	return b
}

func TestIngestEnvelope(t *testing.T) {
	api, d, store := newIngestAPI(t, "")
	rr, out := postIngest(api, envelope("b_1", wpRecord("/checkout/", "full"), wpRecord("/", "summary")))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d body %s", rr.Code, rr.Body)
	}
	if out["accepted"].(float64) != 2 || out["dropped"].(float64) != 0 || out["dropped_quota"].(float64) != 0 {
		t.Fatalf("response: %v", out)
	}
	if d.tracesRead.Load() != 2 {
		t.Fatalf("tracesRead=%d", d.tracesRead.Load())
	}
	d.flushBatch()
	if n, _ := store.GetTraceCount(); n != 2 {
		t.Fatalf("stored %d traces, want 2", n)
	}
	// the record went through StoreTrace: queries, errors, http calls and the
	// plugin-shaped components (incl_ms) are all there
	var host, comps string
	var dbCount, errCount, httpCount int
	var httpMs float64
	err := store.db.QueryRow(`SELECT host, components_json, db_count, error_count, http_count, http_ms FROM traces WHERE uri = '/checkout/'`).
		Scan(&host, &comps, &dbCount, &errCount, &httpCount, &httpMs)
	if err != nil {
		t.Fatal(err)
	}
	// db_count comes from the record; http_count/http_ms are derived from http_calls (the plugin sends the list only)
	if host != "shop.example.com" || dbCount != 12 || errCount != 1 || httpCount != 1 || httpMs != 250 {
		t.Fatalf("stored trace: host=%s db=%d err=%d http=%d http_ms=%v", host, dbCount, errCount, httpCount, httpMs)
	}
	var cs []Component
	json.Unmarshal([]byte(comps), &cs)
	if len(cs) != 1 || cs[0].Name != "plugins/woocommerce" || cs[0].Ms != 120.5 || cs[0].InclNs != 120_500_000 || cs[0].Calls != 34 || cs[0].Pct < 37 || cs[0].Pct > 38 {
		t.Fatalf("components: %s → %+v", comps, cs)
	}
	var q int
	store.db.QueryRow(`SELECT COUNT(*) FROM queries`).Scan(&q)
	if q != 2 { // one query per record, both records stored
		t.Fatalf("queries stored: %d", q)
	}
	st := api.ingest.stats()
	if st.Batches != 1 || st.Traces != 2 || st.Dropped != 0 || st.Rejected != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIngestBareArray(t *testing.T) {
	api, d, _ := newIngestAPI(t, "")
	body, _ := json.Marshal([]map[string]interface{}{wpRecord("/a", "normal"), wpRecord("/b", "normal")})
	rr, out := postIngest(api, body)
	if rr.Code != 202 || out["accepted"].(float64) != 2 {
		t.Fatalf("bare array: %d %v", rr.Code, out)
	}
	if d.tracesRead.Load() != 2 {
		t.Fatalf("tracesRead=%d", d.tracesRead.Load())
	}
}

func TestIngestGzip(t *testing.T) {
	api, _, _ := newIngestAPI(t, "")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(envelope("b_gz", wpRecord("/gz", "full")))
	gz.Close()
	rr, out := postIngest(api, buf.Bytes(), withGzip())
	if rr.Code != 202 || out["accepted"].(float64) != 1 {
		t.Fatalf("gzip: %d %s", rr.Code, rr.Body)
	}
	// broken gzip → 400
	rr, _ = postIngest(api, []byte("not gzip at all"), withGzip())
	if rr.Code != 400 {
		t.Fatalf("broken gzip: %d", rr.Code)
	}
}

func TestIngestDedupBatchID(t *testing.T) {
	api, d, store := newIngestAPI(t, "")
	body := envelope("b_same", wpRecord("/x", "full"))
	if rr, _ := postIngest(api, body); rr.Code != 202 {
		t.Fatalf("first: %d", rr.Code)
	}
	rr, out := postIngest(api, body)
	if rr.Code != 202 || out["accepted"].(float64) != 0 || out["duplicate"] != true {
		t.Fatalf("duplicate: %d %v", rr.Code, out)
	}
	d.flushBatch()
	if n, _ := store.GetTraceCount(); n != 1 {
		t.Fatalf("stored %d, want 1 (batch replayed)", n)
	}
	if api.ingest.stats().Duplicates != 1 {
		t.Fatalf("duplicates counter: %+v", api.ingest.stats())
	}
	// after the TTL the id is forgotten
	api.dedup.now = func() time.Time { return time.Now().Add(ingestDedupTTL + time.Minute) }
	if rr, out := postIngest(api, body); rr.Code != 202 || out["accepted"].(float64) != 1 {
		t.Fatalf("after TTL: %d %v", rr.Code, out)
	}
	// records without batch_id are never deduplicated
	arr, _ := json.Marshal([]map[string]interface{}{wpRecord("/y", "full")})
	for i := 0; i < 2; i++ {
		if _, out := postIngest(api, arr); out["accepted"].(float64) != 1 {
			t.Fatalf("bare array #%d: %v", i, out)
		}
	}
}

func TestIngestAuthRequiredWithSecret(t *testing.T) {
	secret := "ingest-secret"
	api, _, _ := newIngestAPI(t, secret)
	body := envelope("b_auth", wpRecord("/", "full"))

	if rr, _ := postIngest(api, body); rr.Code != 401 {
		t.Fatalf("no token: %d, want 401", rr.Code)
	}
	if rr, _ := postIngest(api, body, withBearer("garbage.token.here")); rr.Code != 401 {
		t.Fatalf("bad token: %d, want 401", rr.Code)
	}
	if api.ingest.stats().Rejected != 0 { // rejected by the middleware, never reached the handler
		t.Fatalf("rejected counter: %+v", api.ingest.stats())
	}
	tok, err := GenerateToken(secret, JWTClaims{Sub: "wp", Role: "user", Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if rr, out := postIngest(api, body, withBearer(tok)); rr.Code != 202 || out["accepted"].(float64) != 1 {
		t.Fatalf("valid token: %d %v", rr.Code, out)
	}
	// with a secret, remote agents are allowed when they carry the token
	if rr, _ := postIngest(api, envelope("b_remote", wpRecord("/", "full")), withBearer(tok), withRemote("203.0.113.9:5555")); rr.Code != 202 {
		t.Fatalf("remote with token: %d", rr.Code)
	}
}

func TestIngestLoopbackOnlyWithoutSecret(t *testing.T) {
	api, _, _ := newIngestAPI(t, "")
	body := envelope("b_lo", wpRecord("/", "full"))
	if rr, _ := postIngest(api, body, withRemote("203.0.113.9:5555")); rr.Code != 403 {
		t.Fatalf("remote without secret: %d, want 403", rr.Code)
	}
	if rr, _ := postIngest(api, body, withRemote("10.0.0.7:5555")); rr.Code != 403 {
		t.Fatalf("private remote without secret: %d, want 403", rr.Code)
	}
	for _, addr := range []string{"127.0.0.1:1", "127.0.0.9:2", "[::1]:3", "[::ffff:127.0.0.1]:4"} {
		if rr, _ := postIngest(api, envelope("b_"+addr, wpRecord("/", "full")), withRemote(addr)); rr.Code != 202 {
			t.Fatalf("loopback %s: %d, want 202", addr, rr.Code)
		}
	}
	if isLoopbackAddr("192.168.1.1:80") || !isLoopbackAddr("") {
		t.Fatalf("isLoopbackAddr edge cases")
	}
}

func TestIngestLimits(t *testing.T) {
	api, _, _ := newIngestAPI(t, "")

	// 501 records → 413
	recs := make([]map[string]interface{}, maxIngestRecords+1)
	for i := range recs {
		recs[i] = map[string]interface{}{"ts": 1, "host": "h", "uri": fmt.Sprintf("/%d", i), "level": "summary", "duration_ms": 1}
	}
	if rr, _ := postIngest(api, envelope("b_many", recs...)); rr.Code != 413 {
		t.Fatalf("501 records: %d, want 413", rr.Code)
	}
	// exactly 500 is fine
	if rr, out := postIngest(api, envelope("b_500", recs[:maxIngestRecords]...)); rr.Code != 202 || out["accepted"].(float64) != 500 {
		t.Fatalf("500 records: %d %v", rr.Code, out)
	}

	// body over 8 MB → 413 (plain and gzip-encoded)
	big := []byte(`[{"host":"h","uri":"/","pad":"` + strings.Repeat("x", maxIngestBodyBytes) + `"}]`)
	if rr, _ := postIngest(api, big); rr.Code != 413 {
		t.Fatalf("8 MB+ body: %d, want 413", rr.Code)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(big)
	gz.Close()
	if rr, _ := postIngest(api, buf.Bytes(), withGzip()); rr.Code != 413 {
		t.Fatalf("8 MB+ gzip body: %d, want 413", rr.Code)
	}

	// malformed → 400
	for _, body := range []string{"", "   ", "{", "[1,", `"text"`, "42"} {
		if rr, _ := postIngest(api, []byte(body)); rr.Code != 400 {
			t.Fatalf("body %q: %d, want 400", body, rr.Code)
		}
	}
	if st := api.ingest.stats(); st.Rejected != 9 || st.Batches != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIngestDropsBadRecordsKeepsGood(t *testing.T) {
	api, d, _ := newIngestAPI(t, "")
	body := []byte(`[null, 123, "str", {"foo": 1}, {"host":"ok.local","uri":"/fine","level":"normal","duration_ms":5,"ts":` +
		fmt.Sprint(time.Now().UnixMilli()) + `}]`)
	rr, out := postIngest(api, body)
	if rr.Code != 202 || out["accepted"].(float64) != 1 || out["dropped"].(float64) != 4 {
		t.Fatalf("mixed batch: %d %v", rr.Code, out)
	}
	d.mu.Lock()
	tr := d.batch[0]
	d.mu.Unlock()
	if tr.Host != "ok.local" || tr.Ts > time.Now().Unix()+1 || tr.Ts < time.Now().Unix()-60 {
		t.Fatalf("ms timestamp not normalised: %+v", tr)
	}
	if st := api.ingest.stats(); st.Traces != 1 || st.Dropped != 4 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestIngestWithoutSink(t *testing.T) {
	store, err := OpenStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewAPI(store, nil, "")
	if rr, _ := postIngest(api, envelope("b_nosink", wpRecord("/", "full"))); rr.Code != 503 {
		t.Fatalf("no sink: %d, want 503", rr.Code)
	}
	// api-only mode: storeSink writes straight to SQLite
	api.sink = &storeSink{store: store}
	if rr, _ := postIngest(api, envelope("b_direct", wpRecord("/", "full"))); rr.Code != 202 {
		t.Fatalf("storeSink: %d", rr.Code)
	}
	if n, _ := store.GetTraceCount(); n != 1 {
		t.Fatalf("storeSink stored %d", n)
	}
}

func TestIngestHealthAndMetrics(t *testing.T) {
	api, _, _ := newIngestAPI(t, "")
	postIngest(api, envelope("b_h1", wpRecord("/", "full"), wpRecord("/2", "full")))
	postIngest(api, []byte("{"))

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	rr := httptest.NewRecorder()
	api.router.ServeHTTP(rr, req)
	var h struct {
		AuthEnabled bool        `json:"auth_enabled"`
		Ingest      IngestStats `json:"ingest"`
		Input       InputStatus `json:"input"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.AuthEnabled || h.Ingest.Batches != 1 || h.Ingest.Traces != 2 || h.Ingest.Rejected != 1 {
		t.Fatalf("health: %+v (%s)", h, rr.Body)
	}
	if h.Input.SHMPath != "/dev/shm/phpray" || h.Input.Glob || len(h.Input.Rings) != 0 {
		t.Fatalf("health input: %+v", h.Input)
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	rr = httptest.NewRecorder()
	api.router.ServeHTTP(rr, req)
	for _, want := range []string{"phpray_ingest_batches_total 1", "phpray_ingest_traces_total 2", "phpray_ingest_rejected_total 1", "phpray_ring_buffers_open 0"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, rr.Body.String())
		}
	}
}

// The site_id an agent computed itself (WP plugin: home_url host + ABSPATH) is
// what the Cloud shipper uses, not sha1(host + "\0" + docroot).
func TestIngestedSiteIDReachesCloud(t *testing.T) {
	f := newFakeIngest()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := newTestShipper(t, srv.URL, nil)

	var tr Trace
	b, _ := json.Marshal(wpRecord("/checkout/", "full"))
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	s.Observe(&tr)
	s.Observe(sampleTrace(time.Now().Unix(), "other.example", "/", "normal", 100))
	s.flushTraces()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.traces) != 1 || len(f.traces[0]) != 2 {
		t.Fatalf("batches: %v", f.traces)
	}
	got := map[string]bool{}
	for _, rec := range f.traces[0] {
		got[rec["site_id"].(string)] = true
	}
	if !got["0123456789abcdef"] || !got[SiteID("other.example", "/var/www/other.example")] {
		t.Fatalf("site ids shipped: %v", got)
	}
}

func TestComponentUnmarshalBothShapes(t *testing.T) {
	var c Component
	if err := json.Unmarshal([]byte(`{"name":"plugins/x","incl_ms":12.5,"self_ms":4,"calls":3}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "plugins/x" || c.Ms != 12.5 || c.InclNs != 12_500_000 || c.SelfNs != 4_000_000 || c.Calls != 3 {
		t.Fatalf("plugin shape: %+v", c)
	}
	c = Component{}
	if err := json.Unmarshal([]byte(`{"name":"vendor/a/b","ms":1.5,"incl_ns":1500000,"self_ns":1000000,"calls":2,"pct":10}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Ms != 1.5 || c.InclNs != 1_500_000 || c.SelfNs != 1_000_000 || c.Calls != 2 || c.Pct != 10 {
		t.Fatalf("extension shape: %+v", c)
	}
	// round trip through Marshal keeps the canonical fields
	out, _ := json.Marshal(c)
	var back Component
	json.Unmarshal(out, &back)
	if back != c {
		t.Fatalf("round trip: %+v vs %+v", back, c)
	}
}
