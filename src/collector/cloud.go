package main

// Cloud shipper — sends per-site aggregates and sampled traces to a PHPRay console
// (PHPRay Cloud or Host Edition) according to docs/spec/ingest-v1.md.
//
//   - outbound only: HTTPS from the collector to the console, never the other way
//   - aggregates every minute (never dropped), traces in batches of ≤ 500 every 5 s
//     (alert traces immediately), gzip above 8 KB, batch_id for server-side dedup
//   - disk buffer with a byte limit; oldest trace batches are dropped first
//   - error handling per spec §4: 401 stop, 402 traces off, 413 split, 429 Retry-After,
//     5xx exponential backoff 5 s → 5 min
//   - control channel: GET /v1/control long-poll, POST /v1/control/ack; `config`
//     messages change the trace sample rate, `profile` messages are written into the
//     shared-memory control table (control_table.go) that the extension reads at RINIT
//
// Site identity: site_id = sha1(host + "\0" + docroot)[:16]; docroot comes from the
// extension (JSONL "docroot" / ring v5), falling back to "" when unknown.

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cloudTraceBatchMax   = 500
	cloudTraceBatchBytes = 4 << 20
	cloudGzipThreshold   = 8 * 1024
	cloudBackoffMin      = 5 * time.Second
	cloudBackoffMax      = 5 * time.Minute
	cloudControlWait     = 30 * time.Second // long-poll is ≤ 25 s on the server side
	cloudErrMsgMax       = 200
)

// CloudConfig is the [cloud] section of collector.toml.
type CloudConfig struct {
	Enabled         bool
	Endpoint        string
	ServerKey       string
	BufferMaxMB     int
	BufferDir       string
	TraceSampleRate int // percent of eligible traces shipped (0-100)
	MaskHost        bool
	AggInterval     time.Duration
	TraceInterval   time.Duration
	ControlEnabled  bool
}

// DefaultCloudConfig returns the defaults from the spec.
func DefaultCloudConfig() CloudConfig {
	return CloudConfig{
		Enabled:         false,
		Endpoint:        "https://ingest.phpray.dev",
		BufferMaxMB:     32,
		TraceSampleRate: 100,
		AggInterval:     60 * time.Second,
		TraceInterval:   5 * time.Second,
		ControlEnabled:  true,
	}
}

// CloudStatus is what /api/v1/health and `phpray status` report.
type CloudStatus struct {
	Enabled          bool   `json:"enabled"`
	Endpoint         string `json:"endpoint,omitempty"`
	Connected        bool   `json:"connected"`
	AuthFailed       bool   `json:"auth_failed"`
	PlanExhausted    bool   `json:"plan_exhausted"`
	LastSendAt       int64  `json:"last_send_at"`
	LastError        string `json:"last_error,omitempty"`
	LastStatus       int    `json:"last_status"`
	BackoffUntil     int64  `json:"backoff_until,omitempty"`
	TraceSampleRate  int    `json:"trace_sample_rate"`
	ServerSampleRate int    `json:"server_sample_rate,omitempty"`
	SentTraces       int64  `json:"sent_traces"`
	SentAggregates   int64  `json:"sent_aggregates"`
	DroppedTraces    int64  `json:"dropped_traces"`
	// DroppedQuota liczy sie OSOBNO od DroppedTraces, bo znaczy zupelnie co
	// innego i wymaga innego dzialania. Przepelniony bufor to sprawa lokalna,
	// przejdzie sama. Odrzucenie przez limit planu to "Twoje konto nie obejmuje
	// tej strony" — i dopoki tego nie widac, uzytkownik widzi tylko, ze strony
	// NIE MA w konsoli, i uznaje produkt za zepsuty zamiast zmienic plan.
	DroppedQuota    int64 `json:"dropped_quota"`
	BufferedFiles   int   `json:"buffered_files"`
	BufferedBytes   int64 `json:"buffered_bytes"`
	PendingTraces   int   `json:"pending_traces"`
	Sites           int   `json:"sites"`
	ControlMessages int64 `json:"control_messages"`
}

// CloudStatusProvider is implemented by the shipper (or a nil provider in api-only mode).
type CloudStatusProvider interface {
	CloudStatus() CloudStatus
}

// SiteID computes the spec's site identifier: sha1(host + "\0" + docroot)[:16].
func SiteID(host, docroot string) string {
	sum := sha1.Sum([]byte(host + "\x00" + docroot))
	return hex.EncodeToString(sum[:])[:16]
}

func newBatchID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("b_%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("b_%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ─── Aggregation accumulators ────────────────────────────────────────────────

type cloudCompAgg struct {
	InclMs float64
	SelfMs float64
	Calls  int64
}

type cloudURIAgg struct {
	Requests  int
	Durations []float64
}

type siteMinute struct {
	SiteID    string
	Host      string
	App       string
	Requests  int
	Errors5xx int
	ErrorsPHP int
	Profiled  int
	Durations []float64
	CPUSum    float64
	MemSum    float64
	MemMax    float64
	DBQueries int64
	DBMs      float64
	N1        int
	HTTPCalls int64
	HTTPMs    float64
	HTTPErr   int64
	Levels    map[string]int
	Comps     map[string]*cloudCompAgg
	URIs      map[string]*cloudURIAgg
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * p)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func (m *siteMinute) toJSON(ts int64) map[string]interface{} {
	d := append([]float64(nil), m.Durations...)
	sort.Float64s(d)
	var sum, max float64
	for _, v := range d {
		sum += v
		if v > max {
			max = v
		}
	}
	n := float64(len(d))
	if n == 0 {
		n = 1
	}
	comps := make([]map[string]interface{}, 0, len(m.Comps))
	for name, c := range m.Comps {
		comps = append(comps, map[string]interface{}{
			"name": name, "incl_ms": round1(c.InclMs), "self_ms": round1(c.SelfMs), "calls": c.Calls,
		})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i]["incl_ms"].(float64) > comps[j]["incl_ms"].(float64) })
	if len(comps) > 50 {
		comps = comps[:50]
	}
	uris := make([]map[string]interface{}, 0, len(m.URIs))
	for uri, u := range m.URIs {
		ud := append([]float64(nil), u.Durations...)
		sort.Float64s(ud)
		uris = append(uris, map[string]interface{}{"uri": uri, "requests": u.Requests, "p95_ms": round1(percentile(ud, 0.95))})
	}
	sort.Slice(uris, func(i, j int) bool { return uris[i]["requests"].(int) > uris[j]["requests"].(int) })
	if len(uris) > 10 {
		uris = uris[:10]
	}
	return map[string]interface{}{
		"ts": ts, "site_id": m.SiteID, "host": m.Host, "app": m.App,
		"requests": m.Requests, "errors_5xx": m.Errors5xx, "errors_php": m.ErrorsPHP, "profiled": m.Profiled,
		"duration_ms": map[string]float64{"avg": round1(sum / n), "p50": round1(percentile(d, 0.5)), "p95": round1(percentile(d, 0.95)), "max": round1(max)},
		"cpu_ms":      map[string]float64{"avg": round1(m.CPUSum / n)},
		"memory_mb":   map[string]float64{"avg": round1(m.MemSum / n), "max": round1(m.MemMax)},
		"db":          map[string]interface{}{"queries": m.DBQueries, "ms": round1(m.DBMs), "n1_requests": m.N1},
		"http":        map[string]interface{}{"calls": m.HTTPCalls, "ms": round1(m.HTTPMs), "errors": m.HTTPErr},
		"levels":      m.Levels,
		"components":  comps,
		"top_uris":    uris,
	}
}

// ─── Shipper ─────────────────────────────────────────────────────────────────

var (
	errCloudBackoff   = errors.New("cloud: backing off")
	errCloudTooLarge  = errors.New("cloud: payload too large (413)")
	errCloudPermanent = errors.New("cloud: rejected permanently")
	errCloudAuth      = errors.New("cloud: authentication failed (401)")
	errCloudPlan      = errors.New("cloud: plan exhausted (402)")
)

type Shipper struct {
	cfg          CloudConfig
	client       *http.Client
	agent        string
	hostnameHash string
	osName       string
	now          func() time.Time
	rng          *mrand.Rand
	logf         func(format string, args ...interface{})

	mu             sync.Mutex
	pending        []json.RawMessage
	pendingBytes   int
	minutes        map[int64]map[string]*siteMinute
	phpVersions    map[string]struct{}
	sites          map[string]struct{}
	siteSampleRate map[string]int // control "config" overrides per site ("" = all sites)
	serverSample   int            // X-PHPRay-Trace-Sample (0 = none)
	backoff        time.Duration
	st             CloudStatus

	buffer *CloudBuffer
	wake   chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup

	control     *ControlTable     // shared-memory control table (nil = not available)
	siteDocroot map[string]string // site_id → DOCUMENT_ROOT seen in traces (for profile messages)
}

// NewShipper creates a shipper; it does nothing until Start() is called.
func NewShipper(cfg CloudConfig) (*Shipper, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cloud: endpoint is empty")
	}
	if cfg.ServerKey == "" {
		return nil, errors.New("cloud: server_key is empty")
	}
	if cfg.BufferDir == "" {
		cfg.BufferDir = "/var/lib/phpray/cloud-buffer"
	}
	if cfg.AggInterval <= 0 {
		cfg.AggInterval = 60 * time.Second
	}
	if cfg.TraceInterval <= 0 {
		cfg.TraceInterval = 5 * time.Second
	}
	if cfg.TraceSampleRate < 0 || cfg.TraceSampleRate > 100 {
		cfg.TraceSampleRate = 100
	}
	buf, err := NewCloudBuffer(cfg.BufferDir, int64(cfg.BufferMaxMB)<<20)
	if err != nil {
		return nil, fmt.Errorf("cloud buffer: %w", err)
	}
	hn, _ := os.Hostname()
	hsum := sha1.Sum([]byte(hn))
	s := &Shipper{
		cfg:            cfg,
		client:         &http.Client{Timeout: 30 * time.Second},
		agent:          "collector/" + version,
		hostnameHash:   hex.EncodeToString(hsum[:])[:16],
		osName:         runtime.GOOS + "-" + runtime.GOARCH,
		now:            time.Now,
		rng:            mrand.New(mrand.NewSource(time.Now().UnixNano())),
		logf:           log.Printf,
		minutes:        make(map[int64]map[string]*siteMinute),
		phpVersions:    make(map[string]struct{}),
		sites:          make(map[string]struct{}),
		siteSampleRate: make(map[string]int),
		siteDocroot:    make(map[string]string),
		buffer:         buf,
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
	s.st.Enabled = true
	s.st.Endpoint = cfg.Endpoint
	s.st.TraceSampleRate = cfg.TraceSampleRate
	return s, nil
}

// Start launches the sender and control goroutines.
func (s *Shipper) Start() {
	s.wg.Add(1)
	go s.runSender()
	if s.cfg.ControlEnabled {
		s.wg.Add(1)
		go s.runControl()
	}
}

// Stop flushes what it can and stops the goroutines.
func (s *Shipper) Stop() {
	close(s.done)
	s.wg.Wait()
	s.flushTraces()
	s.flushAggregates(true)
}

// CloudStatus implements CloudStatusProvider.
func (s *Shipper) CloudStatus() CloudStatus {
	s.mu.Lock()
	st := s.st
	st.PendingTraces = len(s.pending)
	st.Sites = len(s.sites)
	st.ServerSampleRate = s.serverSample
	s.mu.Unlock()
	st.BufferedFiles, st.BufferedBytes = s.buffer.Stats()
	return st
}

// ─── Observe: every trace passes through here ─────────────────────────────────

func (s *Shipper) Observe(t *Trace) {
	siteID := t.SiteID // agents that compute their own site_id (WP plugin over /api/v1/ingest)
	if siteID == "" {
		siteID = SiteID(t.Host, t.Docroot)
	}
	ts := t.Ts
	if ts <= 0 {
		ts = s.now().Unix()
	}
	minute := ts / 60 * 60

	s.mu.Lock()
	s.sites[siteID] = struct{}{}
	if t.Docroot != "" && len(s.siteDocroot) < 4096 {
		s.siteDocroot[siteID] = t.Docroot
	}
	if t.PhpVer != "" {
		s.phpVersions[t.PhpVer] = struct{}{}
	}
	sites := s.minutes[minute]
	if sites == nil {
		sites = make(map[string]*siteMinute)
		s.minutes[minute] = sites
	}
	m := sites[siteID]
	if m == nil {
		m = &siteMinute{SiteID: siteID, Host: t.Host, App: appValue(t.App), Levels: make(map[string]int),
			Comps: make(map[string]*cloudCompAgg), URIs: make(map[string]*cloudURIAgg)}
		sites[siteID] = m
	}
	m.Requests++
	if t.Status >= 500 {
		m.Errors5xx++
	}
	m.ErrorsPHP += len(t.Errors)
	if t.Profiled == 1 {
		m.Profiled++
	}
	if len(m.Durations) < 20000 {
		m.Durations = append(m.Durations, t.DurationMs)
	}
	m.CPUSum += t.CPUUserMs + t.CPUSysMs
	m.MemSum += t.MemoryMB
	if t.MemoryMB > m.MemMax {
		m.MemMax = t.MemoryMB
	}
	if t.DBCount != nil {
		m.DBQueries += int64(*t.DBCount)
	}
	m.DBMs += t.DBMs
	if t.N1 == 1 {
		m.N1++
	}
	if t.HTTPCount != nil {
		m.HTTPCalls += int64(*t.HTTPCount)
	}
	m.HTTPMs += t.HTTPMs
	for _, h := range t.HTTPCalls {
		if h.Status >= 400 || h.Status == 0 {
			m.HTTPErr++
		}
	}
	lvl := t.Level
	if lvl == "" {
		lvl = "summary"
	}
	m.Levels[lvl]++
	if t.Profiled == 1 {
		for _, c := range t.Components {
			ca := m.Comps[c.Name]
			if ca == nil {
				ca = &cloudCompAgg{}
				m.Comps[c.Name] = ca
			}
			incl := c.Ms
			if c.InclNs > 0 {
				incl = float64(c.InclNs) / 1e6
			}
			ca.InclMs += incl
			ca.SelfMs += float64(c.SelfNs) / 1e6
			ca.Calls += int64(c.Calls)
		}
	}
	uri := cloudURI(t)
	if len(m.URIs) < 500 || m.URIs[uri] != nil {
		ua := m.URIs[uri]
		if ua == nil {
			ua = &cloudURIAgg{}
			m.URIs[uri] = ua
		}
		ua.Requests++
		if len(ua.Durations) < 5000 {
			ua.Durations = append(ua.Durations, t.DurationMs)
		}
	}

	// Traces: only normal/full/alert, sampled, privacy-transformed.
	//
	// Żądanie, które się wywróciło, NIE podlega losowaniu. Cała obietnica
	// produktu brzmi "ten jeden nieudany checkout nadal tu będzie, kiedy
	// zajrzysz", a przy próbkowaniu 3 % jedyny błąd doby ginął w 97 %
	// przypadków: agregat pokazywał 0,003 % błędów, a zakładka Errors była
	// pusta, bo do chmury nie pojechał żaden ślad.
	//
	// Wyjątek jest wąski celowo. Zwykłe E_WARNING i E_NOTICE NIE omijają
	// próbkowania: na starszym WordPressie potrafi ich być dwadzieścia na
	// żądanie i taka reguła wysyłałaby do chmury wszystko, przepalając limit
	// planu. Omijają je tylko odpowiedzi 5xx, poziom "alert" (kolektor nadaje
	// go żądaniom wolnym i zepsutym) oraz błędy klasy fatalnej.
	ship := lvl == "normal" || lvl == "full" || lvl == "alert"
	zawsze := lvl == "alert" || t.Status >= 500 || maFatalny(t.Errors)
	if ship && !zawsze {
		rate := s.effectiveSampleRateLocked(siteID)
		if rate < 100 && s.rng.Intn(100) >= rate {
			ship = false
		}
	}
	if ship && !s.st.PlanExhausted {
		rec := cloudTraceJSON(t, siteID, s.cfg.MaskHost)
		if rec != nil {
			s.pending = append(s.pending, rec)
			s.pendingBytes += len(rec)
		}
	}
	isAlert := lvl == "alert"
	s.mu.Unlock()

	if isAlert && ship {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// maFatalny mówi, czy wśród błędów żądania jest taki, po którym PHP nie
// mogło dokończyć pracy. Ostrzeżenia i noty nie liczą się — są zbyt częste,
// żeby traktować je jako zdarzenie warte pominięcia próbkowania.
func maFatalny(errs []Error) bool {
	for _, e := range errs {
		switch e.Type {
		case "E_ERROR", "E_PARSE", "E_CORE_ERROR", "E_COMPILE_ERROR", "E_USER_ERROR", "E_RECOVERABLE_ERROR":
			return true
		}
	}
	return false
}

func (s *Shipper) effectiveSampleRateLocked(siteID string) int {
	rate := s.cfg.TraceSampleRate
	if r, ok := s.siteSampleRate[""]; ok {
		rate = r
	}
	if r, ok := s.siteSampleRate[siteID]; ok {
		rate = r
	}
	if s.serverSample > 0 && s.serverSample < rate {
		rate = s.serverSample
	}
	return rate
}

// cloudURI returns path + fingerprinted query (values replaced by *), never ignore-list URIs.
func cloudURI(t *Trace) string {
	if t.URIFp != "" {
		return t.URIFp
	}
	uri := t.URI
	q := strings.IndexByte(uri, '?')
	if q < 0 {
		return uri
	}
	path := uri[:q]
	var keys []string
	for _, part := range strings.Split(uri[q+1:], "&") {
		if part == "" {
			continue
		}
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			part = part[:eq]
		}
		keys = append(keys, part+"=*")
	}
	sort.Strings(keys)
	return path + "?" + strings.Join(keys, "&")
}

// cloudTraceRecord is the JSONL record plus site_id (embedding flattens the fields).
type cloudTraceRecord struct {
	Trace
	SiteID string `json:"site_id"`
}

// cloudTraceJSON applies the privacy rules of spec §5 and serialises one trace.
func cloudTraceJSON(t *Trace, siteID string, maskHost bool) json.RawMessage {
	c := cloudTraceRecord{Trace: *t, SiteID: siteID}
	c.Trace.SiteID = "" // the outer field carries it
	c.URI = cloudURI(t)
	c.URIFp = ""
	if maskHost {
		c.Host = siteID
	}
	if len(t.Queries) > 0 {
		c.Queries = make([]Query, len(t.Queries))
		for i, q := range t.Queries {
			q.SQL = FingerprintSQL(q.SQL)
			c.Queries[i] = q
		}
	}
	if len(t.Errors) > 0 {
		c.Errors = make([]Error, len(t.Errors))
		for i, e := range t.Errors {
			if len(e.Msg) > cloudErrMsgMax {
				e.Msg = e.Msg[:cloudErrMsgMax]
			}
			c.Errors[i] = e
		}
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return nil
	}
	return b
}

// ─── Sending ─────────────────────────────────────────────────────────────────

func (s *Shipper) runSender() {
	defer s.wg.Done()
	traceTicker := time.NewTicker(s.cfg.TraceInterval)
	defer traceTicker.Stop()
	aggTicker := time.NewTicker(s.cfg.AggInterval)
	defer aggTicker.Stop()
	replayTicker := time.NewTicker(15 * time.Second)
	defer replayTicker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-traceTicker.C:
			s.flushTraces()
		case <-s.wake:
			s.flushTraces()
		case <-aggTicker.C:
			s.flushAggregates(false)
		case <-replayTicker.C:
			s.replayBuffer()
			if s.control != nil {
				if n := s.control.Expire(s.now().Unix()); n > 0 {
					s.logf("cloud: control table: %d expired entr%s cleared", n, map[bool]string{true: "y", false: "ies"}[n == 1])
				}
			}
		}
	}
}

// flushTraces sends all pending traces in batches of ≤ 500 records / ≤ 4 MB.
func (s *Shipper) flushTraces() {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.mu.Unlock()
			return
		}
		n, bytes := 0, 0
		for n < len(s.pending) && n < cloudTraceBatchMax && (n == 0 || bytes+len(s.pending[n]) <= cloudTraceBatchBytes) {
			bytes += len(s.pending[n])
			n++
		}
		batch := s.pending[:n]
		s.pending = append([]json.RawMessage(nil), s.pending[n:]...)
		s.pendingBytes -= bytes
		s.mu.Unlock()
		s.sendTraces(batch, newBatchID())
	}
}

func (s *Shipper) tracesPayload(batchID string, traces []json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteString(`{"batch_id":`)
	b.WriteString(strconv.Quote(batchID))
	b.WriteString(`,"sent_at":`)
	b.WriteString(strconv.FormatInt(s.now().UnixMilli(), 10))
	b.WriteString(`,"agent":`)
	b.WriteString(strconv.Quote(s.agent))
	b.WriteString(`,"traces":[`)
	for i, t := range traces {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(t)
	}
	b.WriteString("]}")
	return b.Bytes()
}

// sendTraces delivers one batch, splitting on 413 and buffering on transient errors.
func (s *Shipper) sendTraces(traces []json.RawMessage, batchID string) {
	if len(traces) == 0 {
		return
	}
	payload := s.tracesPayload(batchID, traces)
	err := s.deliver("/v1/traces", payload)
	switch {
	case err == nil:
		s.mu.Lock()
		s.st.SentTraces += int64(len(traces))
		s.mu.Unlock()
	case errors.Is(err, errCloudTooLarge):
		if len(traces) == 1 {
			s.dropTraces(1, "record larger than the console accepts")
			return
		}
		half := len(traces) / 2
		s.sendTraces(traces[:half], newBatchID())
		s.sendTraces(traces[half:], newBatchID())
	case errors.Is(err, errCloudPlan):
		s.dropTraces(len(traces), "plan exhausted (402)")
	case errors.Is(err, errCloudPermanent):
		s.dropTraces(len(traces), err.Error())
	default: // backoff / 401 / network: keep for later, same batch_id (server dedups)
		if berr := s.buffer.Store("traces", batchID, payload); berr != nil {
			s.dropTraces(len(traces), "buffer: "+berr.Error())
		} else {
			s.mu.Lock()
			s.st.DroppedTraces += s.buffer.TakeDropped()
			s.mu.Unlock()
		}
	}
}

func (s *Shipper) dropTraces(n int, why string) {
	s.mu.Lock()
	s.st.DroppedTraces += int64(n)
	s.mu.Unlock()
	s.logf("cloud: dropped %d trace(s): %s", n, why)
}

// flushAggregates ships every closed minute (or everything when final).
func (s *Shipper) flushAggregates(final bool) {
	closed := s.now().Unix()/60*60 - 60 // minutes strictly before the previous minute boundary are complete
	s.mu.Lock()
	var minutes []int64
	for ts := range s.minutes {
		if final || ts <= closed {
			minutes = append(minutes, ts)
		}
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	var items []map[string]interface{}
	for _, ts := range minutes {
		for _, m := range s.minutes[ts] {
			items = append(items, m.toJSON(ts))
		}
		delete(s.minutes, ts)
	}
	versions := make([]string, 0, len(s.phpVersions))
	for v := range s.phpVersions {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	s.mu.Unlock()
	if len(items) == 0 {
		return
	}
	s.sendAggregates(items, versions, newBatchID())
}

func (s *Shipper) aggregatesPayload(batchID string, items []map[string]interface{}, versions []string) []byte {
	body := map[string]interface{}{
		"batch_id": batchID,
		"sent_at":  s.now().UnixMilli(),
		"agent":    s.agent,
		"server":   map[string]interface{}{"hostname_hash": s.hostnameHash, "php_versions": versions, "os": s.osName},
		"minutes":  items,
	}
	b, _ := json.Marshal(body)
	return b
}

func (s *Shipper) sendAggregates(items []map[string]interface{}, versions []string, batchID string) {
	payload := s.aggregatesPayload(batchID, items, versions)
	err := s.deliver("/v1/aggregates", payload)
	switch {
	case err == nil:
		s.mu.Lock()
		s.st.SentAggregates += int64(len(items))
		s.mu.Unlock()
	case errors.Is(err, errCloudTooLarge) && len(items) > 1:
		half := len(items) / 2
		s.sendAggregates(items[:half], versions, newBatchID())
		s.sendAggregates(items[half:], versions, newBatchID())
	case errors.Is(err, errCloudPermanent):
		s.logf("cloud: aggregates rejected permanently: %v", err)
	default: // never drop aggregates: buffer them (401/402/backoff/network)
		if berr := s.buffer.Store("aggregates", batchID, payload); berr != nil {
			s.logf("cloud: cannot buffer aggregates: %v", berr)
		}
	}
}

// replayBuffer resends buffered batches, oldest first, while the console accepts them.
//
// Consecutive trace batches are merged into as few requests as the console's
// per-batch limits allow. The console counts requests per minute, not records,
// and the default flush interval already uses that budget up, so replaying one
// stored file per request can never drain a backlog: it would need a free
// request slot in the same minute the shipper is filling.
func (s *Shipper) replayBuffer() {
	s.mu.Lock()
	blocked := s.st.AuthFailed || s.now().Before(time.Unix(s.st.BackoffUntil, 0))
	s.mu.Unlock()
	if blocked {
		return
	}
	files := s.buffer.List()
	for i := 0; i < len(files); i++ {
		f := files[i]
		payload, err := s.buffer.Read(f.Name)
		if err != nil {
			s.buffer.Remove(f.Name)
			continue
		}
		merged := []string{f.Name}
		path := "/v1/aggregates"
		if f.Kind == "traces" {
			path = "/v1/traces"
			s.mu.Lock()
			plan := s.st.PlanExhausted
			s.mu.Unlock()
			if plan {
				s.buffer.Remove(f.Name)
				s.dropTraces(1, "plan exhausted (402), buffered batch discarded")
				continue
			}
			if p2, names, n := s.coalesceTraces(payload, files[i+1:]); n > 0 {
				payload = p2
				merged = append(merged, names...)
				i += n
			}
		}
		err = s.deliver(path, payload)
		switch {
		case err == nil:
			s.removeAll(merged)
			s.mu.Lock()
			if f.Kind == "traces" {
				s.st.SentTraces += int64(countTraces(payload))
			} else {
				s.st.SentAggregates++
			}
			s.mu.Unlock()
		case errors.Is(err, errCloudTooLarge):
			// split the stored batch into halves and re-send them as new batches
			s.removeAll(merged)
			if f.Kind == "traces" {
				recs := splitTracesPayload(payload)
				if len(recs) <= 1 {
					s.dropTraces(len(recs), "record larger than the console accepts")
					continue
				}
				half := len(recs) / 2
				s.sendTraces(recs[:half], newBatchID())
				s.sendTraces(recs[half:], newBatchID())
			} else {
				s.logf("cloud: buffered aggregates batch too large, discarded (%s)", f.Name)
			}
		case errors.Is(err, errCloudPermanent), errors.Is(err, errCloudPlan):
			s.removeAll(merged)
			if f.Kind == "traces" {
				s.dropTraces(countTraces(payload), err.Error())
			}
		default:
			return // backoff / auth / network: stop replaying for now
		}
	}
}

// removeAll drops every stored file a single request stood for.
func (s *Shipper) removeAll(names []string) {
	for _, n := range names {
		s.buffer.Remove(n)
	}
}

// coalesceTraces merges the records of following buffered trace files into the
// first one's payload, while the console's per-batch limits allow. It returns
// the merged payload, the names of the files folded in and how many entries of
// next were consumed; n == 0 means nothing was merged and payload is unchanged.
func (s *Shipper) coalesceTraces(first []byte, next []bufferedFile) ([]byte, []string, int) {
	recs := splitTracesPayload(first)
	if len(recs) == 0 {
		return first, nil, 0
	}
	bytes := len(first)
	var names []string
	n := 0
	for _, f := range next {
		if f.Kind != "traces" {
			break
		}
		payload, err := s.buffer.Read(f.Name)
		if err != nil {
			break
		}
		more := splitTracesPayload(payload)
		if len(more) == 0 {
			break
		}
		if len(recs)+len(more) > cloudTraceBatchMax || bytes+len(payload) > cloudTraceBatchBytes {
			break
		}
		recs = append(recs, more...)
		bytes += len(payload)
		names = append(names, f.Name)
		n++
	}
	if n == 0 {
		return first, nil, 0
	}
	// A fresh batch_id is correct here: the console deduplicates traces by
	// record id, and reusing one of the merged ids would make the console
	// discard the whole request as already delivered.
	return s.tracesPayload(newBatchID(), recs), names, n
}

// countTraces counts records in a stored traces payload (cheap parse of the array).
func countTraces(payload []byte) int {
	return len(splitTracesPayload(payload))
}

func splitTracesPayload(payload []byte) []json.RawMessage {
	var env struct {
		Traces []json.RawMessage `json:"traces"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil
	}
	return env.Traces
}

// deliver POSTs a payload and maps the response to the spec's agent behaviour.
func (s *Shipper) deliver(path string, payload []byte) error {
	s.mu.Lock()
	if s.st.AuthFailed {
		s.mu.Unlock()
		return errCloudAuth
	}
	if s.now().Before(time.Unix(s.st.BackoffUntil, 0)) {
		s.mu.Unlock()
		return errCloudBackoff
	}
	s.mu.Unlock()

	body := payload
	gzipped := false
	if len(payload) > cloudGzipThreshold {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(payload)
		zw.Close()
		body = b.Bytes()
		gzipped = true
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.cfg.Endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return s.fail(0, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.ServerKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "phpray-"+s.agent)
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return s.fail(0, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == 202 || resp.StatusCode == 200:
		s.mu.Lock()
		s.st.Connected = true
		s.st.AuthFailed = false
		s.st.LastError = ""
		s.st.LastStatus = resp.StatusCode
		s.st.LastSendAt = s.now().Unix()
		s.st.BackoffUntil = 0
		s.backoff = 0
		if path == "/v1/traces" {
			if v := resp.Header.Get("X-PHPRay-Trace-Sample"); v != "" {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 100 {
					s.serverSample = n
				}
			} else {
				s.serverSample = 0 // a full batch accepted without sampling: back to our own rate
			}
			var r struct {
				DroppedQuota int `json:"dropped_quota"`
			}
			if json.Unmarshal(respBody, &r) == nil && r.DroppedQuota > 0 {
				s.st.DroppedQuota += int64(r.DroppedQuota)
				s.logf("cloud: %d trace(s) not accepted — the account's plan does not "+
					"cover this site; plans: https://phpray.dev/en/pricing", r.DroppedQuota)
			}
		}
		s.mu.Unlock()
		return nil
	case resp.StatusCode == 401:
		s.mu.Lock()
		s.st.AuthFailed = true
		s.st.Connected = false
		s.st.LastStatus = 401
		s.st.LastError = "401 unauthorized: server_key rejected or rotated"
		s.mu.Unlock()
		s.logf("cloud: %s", "401 unauthorized — sending stopped; check [cloud] server_key")
		return errCloudAuth
	case resp.StatusCode == 402:
		s.mu.Lock()
		s.st.PlanExhausted = true
		s.st.Connected = true
		s.st.LastStatus = 402
		s.st.LastError = "402 plan exhausted or unpaid: traces stopped, aggregates continue"
		s.mu.Unlock()
		if path == "/v1/traces" {
			return errCloudPlan
		}
		return errCloudBackoff
	case resp.StatusCode == 413:
		s.mu.Lock()
		s.st.LastStatus = 413
		s.mu.Unlock()
		return errCloudTooLarge
	case resp.StatusCode == 429:
		wait := 60 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		s.mu.Lock()
		s.st.LastStatus = 429
		s.st.LastError = fmt.Sprintf("429 rate limited, retry after %s", wait)
		s.st.BackoffUntil = s.now().Add(wait).Unix()
		s.mu.Unlock()
		return errCloudBackoff
	case resp.StatusCode >= 500:
		return s.fail(resp.StatusCode, fmt.Errorf("console error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
	case resp.StatusCode == 426:
		s.mu.Lock()
		s.st.LastStatus = 426
		s.st.LastError = "426 agent version no longer accepted — upgrade phpray-collector"
		s.mu.Unlock()
		return errCloudPermanent
	default:
		s.mu.Lock()
		s.st.LastStatus = resp.StatusCode
		s.st.LastError = fmt.Sprintf("%d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		s.mu.Unlock()
		return fmt.Errorf("%w: %d %s", errCloudPermanent, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// fail records a transient failure and doubles the backoff (5 s → 5 min).
func (s *Shipper) fail(status int, err error) error {
	s.mu.Lock()
	if s.backoff == 0 {
		s.backoff = cloudBackoffMin
	} else {
		s.backoff *= 2
		if s.backoff > cloudBackoffMax {
			s.backoff = cloudBackoffMax
		}
	}
	s.st.Connected = false
	s.st.LastStatus = status
	s.st.LastError = err.Error()
	s.st.BackoffUntil = s.now().Add(s.backoff).Unix()
	wait := s.backoff
	s.mu.Unlock()
	s.logf("cloud: %v — next attempt in %s", err, wait)
	return errCloudBackoff
}

// ─── Control channel ─────────────────────────────────────────────────────────

type cloudControlMessage struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	SiteID     string `json:"site_id"`
	URLPrefix  string `json:"url_prefix"`
	SampleRate int    `json:"sample_rate"`
	Until      int64  `json:"until"`
	Traces     *struct {
		SampleRate *int `json:"sample_rate"`
	} `json:"traces"`
}

func (s *Shipper) runControl() {
	defer s.wg.Done()
	wait := time.Duration(0)
	for {
		if wait > 0 {
			select {
			case <-s.done:
				return
			case <-time.After(wait):
			}
		}
		select {
		case <-s.done:
			return
		default:
		}
		s.mu.Lock()
		blocked := s.st.AuthFailed || !s.st.Connected
		s.mu.Unlock()
		if blocked {
			wait = 30 * time.Second // nothing to poll until the first successful send
			continue
		}
		n, err := s.pollControl()
		if err != nil {
			if wait == 0 {
				wait = cloudBackoffMin
			} else {
				wait *= 2
				if wait > cloudBackoffMax {
					wait = cloudBackoffMax
				}
			}
			continue
		}
		wait = 0
		if n == 0 {
			wait = 500 * time.Millisecond // the server long-polls up to 25 s; be gentle if it answers instantly
		}
	}
}

// pollControl does one GET /v1/control, applies the messages and acknowledges them.
func (s *Shipper) pollControl() (int, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(s.cfg.Endpoint, "/")+"/v1/control", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.ServerKey)
	req.Header.Set("User-Agent", "phpray-"+s.agent)
	client := &http.Client{Timeout: cloudControlWait}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		s.mu.Lock()
		s.st.AuthFailed = true
		s.st.Connected = false
		s.mu.Unlock()
		return 0, errCloudAuth
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("control: status %d", resp.StatusCode)
	}
	var body struct {
		Messages []cloudControlMessage `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return 0, err
	}
	if len(body.Messages) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(body.Messages))
	for _, m := range body.Messages {
		s.applyControl(m)
		ids = append(ids, m.ID)
	}
	ack, _ := json.Marshal(map[string]interface{}{"ids": ids})
	areq, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.cfg.Endpoint, "/")+"/v1/control/ack", bytes.NewReader(ack))
	if err != nil {
		return len(ids), err
	}
	areq.Header.Set("Authorization", "Bearer "+s.cfg.ServerKey)
	areq.Header.Set("Content-Type", "application/json")
	aresp, err := s.client.Do(areq)
	if err != nil {
		return len(ids), err
	}
	aresp.Body.Close()
	return len(ids), nil
}

func (s *Shipper) applyControl(m cloudControlMessage) {
	s.mu.Lock()
	s.st.ControlMessages++
	s.mu.Unlock()
	switch m.Type {
	case "config":
		if m.Traces != nil && m.Traces.SampleRate != nil {
			r := *m.Traces.SampleRate
			if r < 0 {
				r = 0
			}
			if r > 100 {
				r = 100
			}
			s.mu.Lock()
			s.siteSampleRate[m.SiteID] = r
			if m.SiteID == "" {
				s.st.TraceSampleRate = r
			}
			s.mu.Unlock()
			s.logf("cloud: control %s: traces.sample_rate=%d for site %q", m.ID, r, m.SiteID)
		} else {
			s.logf("cloud: control %s: config message without known fields", m.ID)
		}
	case "profile":
		// Written into the shared-memory control table (docroot → {url_prefix, sample_rate, until})
		// that the extension reads at RINIT; the docroot comes from traces seen for that site_id.
		s.mu.Lock()
		docroot := s.siteDocroot[m.SiteID]
		s.mu.Unlock()
		switch {
		case s.control == nil:
			s.logf("cloud: control %s: profile site=%s url_prefix=%q sample_rate=%d until=%s — no control table (input.control_path)",
				m.ID, m.SiteID, m.URLPrefix, m.SampleRate, time.Unix(m.Until, 0).Format(time.RFC3339))
		case docroot == "":
			s.logf("cloud: control %s: profile site=%s — docroot unknown (no trace with docroot seen for this site yet), ignored", m.ID, m.SiteID)
		default:
			if err := s.control.Set(docroot, m.URLPrefix, m.SampleRate, m.Until); err != nil {
				s.logf("cloud: control %s: profile site=%s: %v", m.ID, m.SiteID, err)
			} else {
				s.logf("cloud: control %s: profile site=%s docroot=%s url_prefix=%q sample_rate=%d until=%s → control table",
					m.ID, m.SiteID, docroot, m.URLPrefix, m.SampleRate, time.Unix(m.Until, 0).Format(time.RFC3339))
			}
		}
	default:
		s.logf("cloud: control %s: unknown message type %q ignored", m.ID, m.Type)
	}
}

// NilCloud is used when the shipper is disabled.
type NilCloud struct{}

func (NilCloud) CloudStatus() CloudStatus { return CloudStatus{Enabled: false} }
