# PHPRay — Architecture (v0.12.0)

## Data Flow

```
┌─────────────────────────────────────────────────────────┐
│                    PHP-FPM Worker                        │
│                                                         │
│  RINIT ──→ start clock, capture URI/method/UID/PID      │
│       ──→ generate X-PHPRay-ID, emit response header    │
│       ──→ WordPress detection (path-based)              │
│                                                         │
│  During request:                                        │
│    mysqli_query() ──→ hooked: time + capture SQL        │
│    PDO::query()   ──→ hooked: time + capture SQL        │
│    PDO::exec()    ──→ hooked: time + capture SQL + rows │
│    PDOStmt::execute() ──→ hooked: read queryString prop │
│    curl_exec()    ──→ hooked: time + URL + status       │
│    curl_multi_*() ──→ hooked: async transfer tracking   │
│    file_get_contents() ──→ hooked: URL detect → HTTP    │
│    file_put_contents() ──→ hooked: path + bytes         │
│    zend_execute_ex() ──→ hooked: per-file/component time bucketing │
│    phpray_mark()  ──→ named timing mark                 │
│    PHP errors     ──→ zend_error_cb hook → capture      │
│                                                         │
│  RSHUTDOWN:                                             │
│    ──→ end clock, compute duration/CPU/memory           │
│    ──→ capture HTTP_HOST (sapi_getenv + $_SERVER)       │
│    ──→ smart sampling decision (summary/normal/full/alert) │
│    ──→ N+1 query detection (DJB2 hash fingerprinting)   │
│    ──→ write JSON to /tmp/phpray.jsonl (if output=file/both) │
│    ──→ serialize + write to ring buffer (if output=shm/both) │
│    ──→ free dynamic arrays (queries, http_calls)        │
└─────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
┌─────────────────┐          ┌─────────────────────┐
│  JSONL File      │          │  Ring Buffer         │
│  /tmp/phpray.jsonl│         │  /dev/shm/phpray     │
│                  │          │  32MB MPSC lock-free  │
│  Human-readable  │          │  Binary serialized    │
│  One JSON per line│         │  CAS-based writes     │
│  Easy to parse   │          │  mmap shared memory   │
└────────┬─────────┘          └──────────┬────────────┘
         │                               │
         └───────────┬───────────────────┘
                     ▼
┌─────────────────────────────────────────────────────────┐
│        phpray-collector daemon (Go, ~7400 LOC)          │
│                                                         │
│  ┌─── Reader Worker ──────────────────────────────────┐ │
│  │ Auto mode: ring buffer preferred, JSONL fallback   │ │
│  │ Ring: mmap reader + binary DeserializeTrace         │ │
│  │ JSONL: tail -f with rotation detection (inode)     │ │
│  │ State persistence: resume position after restart   │ │
│  └────────────────────┬───────────────────────────────┘ │
│                       │ batch buffer (50 traces)        │
│                       ▼                                 │
│  ┌─── Flusher Worker ─────────────────────────────────┐ │
│  │ Batch → SQLite (WAL mode, prepared stmts)          │ │
│  │ Periodic flush: every 5s OR when batch full        │ │
│  │ SQL fingerprinting + slow_queries aggregation      │ │
│  └────────────────────┬───────────────────────────────┘ │
│                       ▼                                 │
│  ┌─── Aggregator Worker ──────────────────────────────┐ │
│  │ 1-minute aggregation pipeline:                     │ │
│  │   per-host: request_count, error_count,            │ │
│  │   avg/P95/max_duration, total_db_queries, n1_count │ │
│  │ P95 calculation (sorted percentile in SQLite)      │ │
│  │ Hourly retention sweep (purge old traces + aggs)   │ │
│  └────────────────────────────────────────────────────┘ │
│                                                         │
│  Graceful shutdown: SIGTERM → close(done) →             │
│    workers drain → flushBatch → saveState → exit        │
│                                                         │
│  Also provides JSONL CLI commands:                      │
│    tail   ──→ live stream from JSONL                    │
│    top    ──→ refreshing dashboard from JSONL            │
│    stats  ──→ aggregates from JSONL                     │
│    trace  ──→ per-domain detail from JSONL              │
│                                                         │
│  SQLite CLI commands:                                    │
│    import      ──→ JSONL → SQLite (5400 traces/sec)     │
│    slow-queries ──→ top slow query fingerprints          │
│    agg-query   ──→ query per-minute aggregates           │
│                                                         │
│  WebSocket Hub (in serve/api mode):                      │
│    addToBatch → Hub.Broadcast(trace)                     │
│    Hub.Run() → serialize once → fan-out to all clients   │
│    /ws/traces endpoint → upgrade HTTP → register client  │
└────────────────────┬───────────────┬────────────────────┘
                     │               │
                     ▼               ▼
┌──────────────────────────┐  ┌───────────────────────────┐
│   SQLite Database        │  │   WebSocket Clients       │
│   /var/lib/phpray/       │  │   (dashboard browsers)    │
│   traces.db              │  │                           │
│                          │  │   Real-time TraceSummary  │
│                                                         │
│  traces ────── raw requests   │  │   JSON over WS, lightweight │
│  queries ───── SQL (normal+) │  │   Auto-reconnect + fallback │
│  http_calls ── HTTP calls    │  │   to polling every 5s       │
│  marks ─────── timing points │  └───────────────────────────┘
│  errors ────── PHP errors    │
│  slow_queries ─ aggregated   │
│  aggregates_1m ─ per-minute  │
│  alerts ─────── fired alerts │
└──────────────────────────────┘
```

## Daemon Architecture

### 3-Worker Design

The collector daemon runs 3 concurrent goroutines plus the main signal handler:

```
main goroutine:
  ├── signal handler (SIGTERM, SIGINT, SIGHUP)
  │   └── close(done) → all workers exit
  │
  ├── Reader Worker (1 of 3 modes):
  │   ├── autoWorker:  try OpenRing() → ringLoop, on fail → jsonlLoop
  │   ├── ringWorker:  OpenRing() with retry, read up to 100 records/cycle
  │   └── jsonlWorker: tailJSONL() with rotation handling
  │
  ├── Flusher Worker:
  │   ├── every FlushInterval: flushBatch() → StoreBatch()
  │   └── every 60s: logStats() (unless quiet mode)
  │
  └── Aggregator Worker:
      ├── every AggInterval: Aggregate1m(previous_bucket)
      └── every 1h: runRetention() → PurgeOld + PurgeOldAggregates
```

### JSONL Rotation Handling

```
1. Open file, stat to get inode
2. Load saved state (offset + inode from collector.state)
3. If same inode AND offset <= file_size → seek to offset (resume)
4. If same inode AND offset > file_size → file truncated, read from 0
5. If different inode → file rotated, read from 0
6. Tail loop: ReadString('\n'), parse JSON, addToBatch
7. On EOF: check inode again → if changed, return (reopen in outer loop)
8. Periodic save: every 10s save current offset + inode to state file
9. On shutdown: save final position
```

### Batch Buffer

```
Trace arrives → mu.Lock → append to batch → mu.Unlock
  if len(batch) >= BatchSize → flushBatch()
    mu.Lock → swap batch with empty → mu.Unlock
    StoreBatch(old_batch) → SQLite transaction
```

Thread-safe: only the flusher and batch-full trigger write to SQLite.

### Configuration

TOML config file (zero-dependency parser):
```
[section]
key = value        # strings, integers, booleans
key = "quoted"     # quoted strings
# comments
```

Supports: `[input]`, `[storage]`, `[collector]`, `[retention]`, `[logging]` sections.
Config → DaemonConfig conversion handles defaults and path derivation.

### Systemd Integration

```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/phpray-collector daemon -c /etc/phpray/collector.toml

# Security hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/phpray /tmp/phpray.jsonl /dev/shm/phpray

# Restart policy
Restart=on-failure
RestartSec=5
```

## WebSocket Live Streaming

### Architecture (Hub/Client pattern)

```
                      ┌─────────────────────┐
                      │     Hub (goroutine)  │
                      │                      │
Daemon.addToBatch() ──→ broadcast channel    │
                      │     │                │
                      │     ▼ serialize once  │
                      │   fan-out to clients │
                      │     │         │      │
                      └─────┼─────────┼──────┘
                            ▼         ▼
                     ┌──────────┐ ┌──────────┐
                     │ Client 1 │ │ Client N │
                     │ writePump│ │ writePump│
                     │ readPump │ │ readPump │
                     └──────────┘ └──────────┘
```

### Design Decisions
- **Channel-based concurrency**: Hub operations via channels, no mutexes for client management
- **Single serialization**: Each trace is serialized to JSON once, same `[]byte` sent to all clients
- **Non-blocking send**: 256-message buffer per client; if full, client is dropped (slow consumer)
- **Keepalive**: ping/pong (54s write deadline, 60s read deadline)
- **TraceSummary**: Lightweight payload — excludes queries/marks/errors to keep WS traffic small
- **Graceful degradation**: Dashboard uses WS when available, falls back to 5s polling

### `serve` Command
The `serve` command runs daemon + API + WebSocket hub in a single process:
```bash
phpray-collector serve -addr :9191 -s /var/lib/phpray/traces.db -mode auto
```
This enables true real-time streaming: trace arrives from ring buffer/JSONL → daemon broadcasts to WS hub → all connected dashboards update instantly.

### Client Reconnection
Dashboard uses exponential backoff: 1s → 2s → 4s → ... → 30s max.
Resets to 1s on successful connection. Polling continues as fallback.

## Dashboard Architecture

The dashboard is a **single embedded HTML file** (go:embed) — zero external dependencies, served from the Go binary itself. This aligns with the single-binary deployment goal.

### Authentication
- **Login overlay**: auto-detects if auth is enabled via `/api/v1/auth/status`
- **Token storage**: JWT stored in `localStorage`, attached to all fetch + WebSocket calls
- **401 handling**: API returns 401 → dashboard shows login overlay automatically
- **Logout**: clears localStorage, re-checks auth status

### Layout
- **Tab navigation**: Overview | Requests | Domains | Slow Queries | Users | Components | Diagnostics | Alerts
- **Global filters**: domain, trace level, time window, min duration, N+1 only
- **Auto-refresh**: 5s polling with WebSocket upgrade for live data

### Time-Series Charts (SVG)
Pure JavaScript SVG chart renderer (`MiniChart` class) — no chart library dependencies:
- **RPS chart**: requests per minute, bar chart
- **Latency chart**: avg/P95/max lines with area fills
- **Errors chart**: 5xx errors (bars) + N+1 detections (line)
- **Database chart**: total queries per bucket (bars)

Features:
- Interactive tooltips on hover (positioned per data point)
- Auto-scaling Y axis with 10% headroom
- Responsive to container width (resize handler)
- Auto-bucket sizing based on time window:
  - ≤15min → 1-minute buckets
  - ≤2h → 5-minute buckets
  - ≤24h → 15-minute buckets
  - >24h → 1-hour buckets

### Time-Series API (`/api/v1/timeseries`)
Computes time-series directly from raw traces (works without daemon aggregation):
```
GET /api/v1/timeseries?window=60&domain=example.com&bucket=300

Response:
{
  "points": [
    {"t": 1774724400, "reqs": 12, "avg_ms": 422.07, "max_ms": 1274.56,
     "p95_ms": 1203.44, "errors": 1, "n1": 2, "queries": 379, "http": 20}
  ],
  "bucket_sec": 300,
  "window_min": 60
}
```
P95 is calculated per bucket by sorting all durations and taking the 95th percentile value.

## JWT Authentication

### Design Principles
- **Zero external dependencies** — minimal JWT (HS256) in ~60 lines of Go
- **Backward compatible** — no secret configured = auth completely disabled
- **Role-based access** — `admin` (full access) and `user` (domain-restricted)

### Token Flow
```
1. Admin generates token:
   phpray-collector token -secret <key> -sub user123 -role user -domains "a.com,b.com"

2. Dashboard stores in localStorage:
   Authorization: Bearer <token>  →  all API requests
   ?token=<token>                 →  WebSocket connection

3. API middleware validates:
   extractToken(r) → VerifyToken(secret, token) → JWTClaims → context

4. Domain filtering (user role):
   DomainFilter(r, "host") → " AND (host LIKE ? OR host LIKE ?)", ["%a.com%", "%b.com%"]
   Injected into every SQL query automatically
```

### Route Protection
```
Public (no auth):
  GET /                        ← dashboard HTML
  GET /api/v1/health           ← health check
  GET /api/v1/auth/status      ← tells dashboard if auth is enabled
  GET /metrics                 ← Prometheus metrics exposition

WebSocket auth (query param):
  WS /ws/traces?token=<jwt>    ← WSAuthMiddleware

Protected (Bearer token):
  GET /api/v1/*                ← AuthMiddleware
  Notable endpoints:
    GET /api/v1/queries/frequent       ← most common queries by exec count
    GET /api/v1/components             ← cross-request component/plugin breakdown
    GET /api/v1/diagnostics/{d}/report ← exportable markdown/text report
```

### Token Structure (HS256)
```json
Header:  {"alg":"HS256","typ":"JWT"}
Payload: {"sub":"user123","role":"user","domains":["a.com","b.com"],"exp":1774799999,"iat":1774713600}
Signature: HMAC-SHA256(header.payload, secret)
```

### Secret Resolution Priority
1. `-secret` CLI flag
2. `[auth].secret` in collector.toml
3. `PHPRAY_JWT_SECRET` environment variable
4. Empty = auth disabled

## Prometheus Metrics

### Design
- **Zero dependencies** — Prometheus text exposition format is plain text, no client library needed
- **MetricsProvider interface** — decouples daemon counters from the API; `DaemonMetrics` wraps a live daemon, `NilMetrics` returns zeros in standalone API mode
- **10-second cache** — DB-sourced metrics (gauges) are cached to prevent excessive SQLite queries during Prometheus scrapes
- **Public endpoint** — `/metrics` requires no authentication, like `/health`

### Metric Categories

```
Daemon counters (from atomic fields, always current):
  phpray_traces_read_total      counter   traces read from ring buffer / JSONL
  phpray_traces_stored_total    counter   traces successfully written to SQLite
  phpray_traces_failed_total    counter   parse/store failures
  phpray_bytes_read_total       counter   raw input bytes

Process metrics:
  phpray_uptime_seconds         gauge     time since API start
  phpray_info{version=...}      gauge     always 1, carries version label

DB metrics (queried at scrape, cached 10s):
  phpray_traces_total{level=...}  gauge   trace count per level
  phpray_requests_total           gauge   total traces in DB
  phpray_domains_active           gauge   distinct hosts (last hour)
  phpray_avg_duration_ms          gauge   avg request duration (last hour)
  phpray_max_duration_ms          gauge   max request duration (last hour)
  phpray_p95_duration_ms          gauge   P95 from latest 1m aggregation
  phpray_errors_total             gauge   HTTP 5xx traces (last hour)
  phpray_n1_detections_total      gauge   N+1 detections (last hour)
  phpray_slow_queries_total       gauge   slow query fingerprints tracked
```

### Caching Strategy
```
Scrape request → check cache age
  if < 10s → return cached string
  if >= 10s → run 9 SQLite queries (5s timeout) → cache result → return
```

## User Enrichment (v0.7.0)

### Architecture

The collector resolves numeric UIDs to usernames via `/etc/passwd`:

```
UserResolver
  ├── Read /etc/passwd → build UID→username map
  ├── 5-minute TTL cache (thread-safe, RWMutex)
  ├── Lazy refresh on first stale access
  └── Fallback: "uid:XXXX" for unknown UIDs
```

### Integration Points

1. **Storage (write time)**: When storing a trace, username is resolved and written to `traces.username` column
2. **API (read time)**: If stored username is empty (old traces), resolver enriches on the fly
3. **WebSocket**: Hub resolves username before broadcasting to connected clients
4. **Dashboard**: Users tab shows sortable per-user stats with drill-down

### Schema Migration

Existing databases are auto-migrated: `ALTER TABLE traces ADD COLUMN username TEXT NOT NULL DEFAULT ''`.
The migration runs on startup via `runMigrations()` and is idempotent (checks `pragma_table_info`).

### API Endpoints

```
GET /api/v1/users                 ← Per-user stats (sortable: avg_ms, requests, errors, queries)
GET /api/v1/users/{uid}           ← User detail with domain breakdown
GET /api/v1/traces                ← username field in each trace
GET /api/v1/traces/{id}           ← username field in trace detail
WS  /ws/traces                    ← username field in live stream
```

### Hosting User Detection

UIDs >= 500 are considered hosting users (standard Linux convention for regular users).
System users (root, www-data, etc.) are excluded from the Users dashboard tab.

## Domain → User Mapping (v0.11.0)

### Architecture

PHPRay maps domain names to their hosting user owners by scanning DirectAdmin's user data directory structure. This enables the dashboard to show who owns each domain and enriches domain-level reporting.

### Data Source

DirectAdmin stores each user's domains in a simple text file:
```
/usr/local/directadmin/data/users/<username>/domains.list
```
Each file contains one domain per line. The resolver scans all user directories to build a complete domain→owner map.

### DomainResolver

```
DomainResolver
  ├── Scan /usr/local/directadmin/data/users/*/domains.list
  ├── Build domain → DomainOwner{Username, UID} map
  ├── UID lookup via UserResolver (cross-reference /etc/passwd)
  ├── 10-minute TTL cache (domain ownership changes rarely)
  ├── Thread-safe (RWMutex)
  └── Graceful degradation: no DA directory = no mapping (no error)
```

### Domain Resolution Logic

1. **Exact match**: `alice.com` → alice
2. **Port stripping**: `alice.com:8080` → `alice.com` → alice
3. **Subdomain matching**: `www.shop.alice.com` → `shop.alice.com` → `alice.com` → alice
4. **Unknown**: returns nil (domain not in any user's list)

### Configuration

```toml
[server]
da_data_path = "/usr/local/directadmin/data/users"   # default, optional
```

If omitted, the default DirectAdmin path is used. On non-DA servers, the resolver silently initializes with zero domains.

### Integration Points

1. **API: GET /api/v1/domains** — each domain row includes `owner` and `owner_uid` fields
2. **API: GET /api/v1/domains/{d}/stats** — domain detail includes owner info
3. **API: GET /api/v1/domain-owners** — dedicated endpoint listing all domain→user mappings
   - Supports `?user=alice` filter
   - Returns source type (directadmin/none)
4. **Dashboard: Domains tab** — "Owner" column shows the DA username for each domain

### Batch Resolution

`ResolveMulti(domains)` resolves multiple domains in a single lock acquisition, used for efficient list rendering.

## PHP Version Capture (v0.10.0)

### Architecture

PHPRay captures the PHP version at compile time using the `PHP_VERSION` C macro (available from php.h). This is a zero-cost operation — it's a string literal embedded in the compiled extension, no runtime syscalls needed.

### Data Flow
```
Extension (C):
  phpray.c → phpray_write_trace() → "php_ver":"8.3.12" in JSONL output
  ringbuffer.c → phpray_serialize_request() → php_version bytes in binary record

Collector (Go):
  tail.go → Trace.PhpVer field (JSON: "php_ver")
  ringbuffer.go → DeserializeTrace reads php_version_len + string (v2 format)
  storage.go → traces.php_version column in SQLite
  api.go → included in trace list, trace detail, GET /api/v1/php-versions

Dashboard:
  Overview tab → "PHP Versions" info box with EOL status
  Trace detail → PHP version badge in request metadata
```

### Ring Buffer Version History (v1 → v2 → v3)

**v1 → v2**: Added `php_version_len` (uint8) after `phpray_id_len` in the fixed header.

**v2 → v3** (v0.12.0): Added `redis_call_count` (uint16) + `redis_total_ns` (uint64) fields after `file_total_ns`, and `serialized_component_count` (uint16) after `serialized_error_count`. Component records (function profiling buckets) are appended after error records. This enables Redis tracking and component/plugin breakdown via ring buffer (previously only available through JSONL).

The Go reader (`DeserializeTrace`) handles all three formats:
- **v1**: No `php_version_len` field, no php version string
- **v2**: Read `php_version_len` (uint8) after phpray_id_len, read string after phpray_id
- **v3**: Additionally reads redis counts and component records

The ring buffer file version is checked in `OpenRing()` — v1, v2, and v3 are all accepted.

### Diagnostics Rule: `php_version_check`

Scans all traces for unique PHP versions and produces findings based on support status:
- **Critical**: PHP < 8.1 (EOL — no security patches)
- **Warning**: PHP 8.1 (security-only support, no bug fixes)
- **Info**: Multiple actively-supported versions detected

Evidence includes: which domains run which versions, request counts per version.

As of 2026: PHP 8.4 is latest stable, PHP 8.3 and 8.2 are actively supported.

## Ring Buffer Design

### Why ring buffer?
- PHP extension **cannot block** — no locks, no waiting syscalls
- Write must be < 1μs per request
- Data loss is acceptable (buffer full → drop oldest, increment counter)
- Multiple writers (FPM workers), single reader (collector) = **MPSC**

### Memory Layout
```
/dev/shm/phpray (32MB default, mmap MAP_SHARED):

┌─────────────────────────────────────────────────────┐
│ Header (192 bytes, 3 cache lines):                  │
│   Line 0: magic(4) version(4) capacity(8) total(8)  │
│   Line 1: write_pos(8) record_count(8) drop_count(8)│ ← writers
│   Line 2: read_pos(8)                                │ ← reader only
├─────────────────────────────────────────────────────┤
│ Data Region (~32MB):                                 │
│   [record_hdr(5)][payload(N)][padding]               │
│   [record_hdr(5)][payload(N)][padding]               │
│   [PADDING_SENTINEL]  ← wraparound marker            │
│   [record_hdr(5)][payload(N)][padding]               │
│   ...                                                │
└─────────────────────────────────────────────────────┘
```

### Write Algorithm (lock-free CAS)
1. Read `write_pos` and `read_pos` atomically
2. Check free space: `capacity - (write_pos - read_pos)`
3. If record doesn't fit contiguously at tail → write padding sentinel, wrap to 0
4. `CAS(&write_pos, old, new)` — if fails, retry (another worker claimed the space)
5. Copy record header + payload to claimed region
6. Memory fence + increment `record_count`

### Binary Record Format
```
Ring record:
  [record_len: u32][record_type: u8][payload...]

Trace payload (phpray_trace_record_t, v2):
  Fixed header (packed):
    request_id(8) uid(4) pid(4) timestamp(8) duration_ns(8)
    cpu_user_ns(8) cpu_sys_ns(8) response_code(2) memory_peak(8)
    query_count(2) db_total_ns(8) http_call_count(2) http_total_ns(8)
    file_op_count(2) file_total_ns(8) wp_detected(1) trace_level(1)
    mark_count(2) error_count(2) n_plus_one(1)
    server_name_len(2) request_uri_len(2) request_method_len(2) phpray_id_len(2)
    php_version_len(1)   ← NEW in v2
    serialized_query_count(2) serialized_http_count(2)
    serialized_mark_count(2) serialized_error_count(2)
  
  Variable strings:
    [server_name][request_uri][request_method][phpray_id][php_version]
  
  Queries (for normal+):
    [sql_len(2) duration_ns(8) offset_ns(8) affected_rows(4) source(1)][sql_text...]
    × serialized_query_count
  
  HTTP calls (for normal+):
    [url_len(2) duration_ns(8) offset_ns(8) response_code(2)][url_text...]
    × serialized_http_count
  
  Marks (always):
    [name_len(1) offset_ns(8)][name_text...]
    × serialized_mark_count
  
  Errors (always):
    [msg_len(2) type(4) offset_ns(8)][msg_text...]
    × serialized_error_count
```

## Slow Request Backtrace Capture (v0.4.0)

### Why Backtraces?
When a query is slow (>50ms) or an HTTP call takes too long (>500ms), knowing *where in the code* it was called from is essential for debugging. Simply seeing "SELECT * FROM wp_options took 120ms" doesn't help — knowing it was called from `plugins/woocommerce/class-wc-product.php:234` tells you exactly what to fix.

### Architecture

```
Query/HTTP hook completes → check duration threshold
  if query > 50ms OR HTTP call > 500ms:
    zend_fetch_debug_backtrace() → PHP call stack
    Extract top 5 frames (skip internal phpray frames)
    Abbreviate file paths (strip /home/user/public_html/)
    Store in phpray_frame_t array on the query/HTTP record
```

### Stack Frame Capture
- Uses `zend_fetch_debug_backtrace()` with `DEBUG_BACKTRACE_IGNORE_ARGS` (lightweight — no argument serialization)
- Cost: ~1-5μs per backtrace capture — only triggered for genuinely slow operations
- Max 5 frames per query/HTTP call (configurable via `PHPRAY_MAX_BACKTRACE_FRAMES`)
- Frames skip internal phpray hook frames
- File paths are abbreviated for readability:
  - `/home/user/public_html/wp-content/plugins/woo/class.php` → `wp-content/plugins/woo/class.php`
  - `/home/user/public_html/vendor/package/src/file.php` → `vendor/package/src/file.php`

### Thresholds
- SQL queries: >50ms (`PHPRAY_SLOW_QUERY_BACKTRACE_NS`)
- HTTP calls: >500ms (`PHPRAY_SLOW_HTTP_BACKTRACE_NS`)

### Storage
- **JSONL**: `"bt"` array in query/HTTP objects: `[{"f":"file.php","l":42}, ...]`
- **Ring buffer**: `phpray_serial_frame_t` records follow each query/HTTP call data
- **SQLite**: `backtrace` TEXT column (JSON array) in `queries` and `http_calls` tables
- **API**: `backtrace` field in trace detail endpoint responses

### Wire Format (Ring Buffer)
```
After each query's sql_text:
  × bt_count frames:
    [file_len(1) line(4)][file_text...]

After each HTTP call's url_text:
  × bt_count frames:
    [file_len(1) line(4)][file_text...]
```

## Hook Strategy

### Function Hooking (mysqli, curl, file_*)
Replace `internal_function.handler` (zif_handler) in the function table:
```c
func = zend_hash_str_find_ptr(CG(function_table), "mysqli_query", ...);
orig_handler = func->internal_function.handler;
func->internal_function.handler = our_wrapper;
```
Wrapper: start timer → call original → stop timer → record data.

### curl_multi Hooking (Async HTTP Tracking)

`curl_multi_*` requires state tracking across multiple calls since transfers are asynchronous:

```
curl_multi_add_handle($mh, $ch)   → hooked: register easy handle with URL + start time
curl_multi_exec($mh, &$running)   → NOT hooked (called in loop, no 1:1 transfer mapping)
curl_multi_info_read($mh)         → hooked: detect completed transfers, record HTTP call
curl_multi_remove_handle($mh, $ch) → hooked: fallback recording if info_read was skipped
```

**Tracking Design:**
- Fixed array of 32 tracking slots (`phpray_multi_track_t`) embedded in request struct
- Each slot stores: easy handle pointer (for identity matching), URL, start timestamp
- On `add_handle`: allocate slot, capture URL via `curl_getinfo(CURLINFO_EFFECTIVE_URL)`, start timer
- On `info_read`: match returned handle to tracked slot, record HTTP call with duration + response code
- On `remove_handle`: fallback — if slot still active (info_read missed), record and free
- On RSHUTDOWN: flush any still-active slots as incomplete transfers (safety net)

**Identity:** Easy handles are identified by their `zend_object*` pointer, which is stable within a request.

**URL Resolution:** URL is captured at `add_handle` time (already set via `CURLOPT_URL`), then re-checked at completion for redirect following.

### Method Hooking (PDO, PDOStatement)
Same approach but via class function table:
```c
ce = zend_hash_find_ptr(CG(class_table), "pdo");  // lowercase!
func = zend_hash_str_find_ptr(&ce->function_table, "query", ...);
```
**Important:** Use `CG(class_table)` with lowercase names, NOT `zend_lookup_class()` which triggers autoloading and crashes in MINIT.

### Error Hooking
Replace `zend_error_cb` global function pointer in MINIT, restore in MSHUTDOWN.

### Function Profiler (`hooks_profiler.c`)

Hooks `zend_execute_ex` — PHP's internal function call dispatcher — to measure per-component execution time:

- **Bucket by file path**: Each PHP function call's `op_array->filename` is mapped to a component category:
  - `plugins/<name>` — WordPress plugins (detected via `/wp-content/plugins/` path)
  - `themes/<name>` — WordPress themes (detected via `/wp-content/themes/` path)
  - `wp-includes` — WordPress core includes
  - `vendor/<vendor>/<package>` — Composer packages
  - `core` — everything else
- **Exclusive time tracking**: Each call measures wall time, but subtracts child call time to avoid double-counting. This means if plugin A calls function B (50ms) which calls function C (30ms), plugin A gets credited 20ms (exclusive), not 50ms (inclusive).
- **64 buckets max**: Fixed-size array per request, ~100-200ns overhead per function call
- **Stored as `components_json`**: JSON object in SQLite traces table, exposed via API and dashboard

## N+1 Query Detection

Simple but effective heuristic:
1. In RSHUTDOWN, if `query_count > 5`:
2. For each query, compute DJB2 hash of first 64 non-digit, non-quoted-string chars
3. Use 64-slot hash table to count fingerprint collisions
4. If any slot has >5 hits → `n_plus_one = 1`

This catches the classic WordPress pattern:
```sql
SELECT * FROM wp_posts WHERE ID = 1    ← hash A
SELECT * FROM wp_posts WHERE ID = 2    ← hash A (same! digits skipped)
SELECT * FROM wp_posts WHERE ID = 3    ← hash A
... × 20 → slot A has 20 hits → N+1 detected!
```

Cost: ~3μs for 50 queries. Only runs when there are >5 queries.

## Smart Sampling Decision Tree

```
RSHUTDOWN:
  if (5xx error && always_trace_errors) → FULL
  if (duration > alert_threshold)       → ALERT
  if (duration > full_threshold)        → FULL
  if (duration > normal_threshold)      → NORMAL
  else                                  → SUMMARY
  
  // Promotion rules:
  if (error_count > 0 && level < NORMAL) → promote to NORMAL
```

## SQLite Schema

```sql
-- Raw request data (main table)
traces (id, phpray_id, timestamp, uid, pid, host, method, uri, uri_fingerprint,
        status, duration_ms, cpu_user_ms, cpu_sys_ms, memory_peak_mb,
        db_count, db_ms, http_count, http_ms, file_count, file_ms,
        wp, n_plus_one, trace_level, mark_count, error_count,
        php_version, username)

-- Sub-records (CASCADE delete with parent trace)
queries (trace_id, sql_text, sql_fingerprint, duration_ms, offset_ms, affected_rows, source)
http_calls (trace_id, url, duration_ms, offset_ms, status)
marks (trace_id, name, offset_ms)
errors (trace_id, error_type, message, offset_ms)

-- Aggregated slow queries (UPSERT on fingerprint+host)
slow_queries (fingerprint, sample_sql, host, total_ms, exec_count, avg_ms, max_ms, last_seen)

-- Per-minute aggregates (populated by daemon aggregation worker)
aggregates_1m (bucket, host, request_count, error_count,
               avg_duration_ms, p95_duration_ms, max_duration_ms,
               total_db_queries, n1_count)

-- Fired alerts (threshold-based anomaly detection)
alerts (id, rule_name, severity, metric, current_value, threshold_value,
        domain, message, created_at, acknowledged, ack_at)
```

**Indexes:** timestamp, host, host+timestamp, duration DESC, trace_level, query fingerprint, aggregate bucket, alert created_at, alert acknowledged.
**WAL mode** for concurrent reads during daemon writes.

## SQL Fingerprinting

```
Input:  SELECT * FROM users WHERE id = 1234 AND name = 'John'
Output: SELECT * FROM users WHERE id = ? AND name = ?

Rules:
  - Single-quoted strings → ?
  - Numbers (int, float, hex) → ? (unless part of identifier like wp_options)
  - IN (1, 2, 3, 'a', 'b') → IN (?)
  - Backtick/double-quote identifiers preserved
  - Whitespace normalized
  - Escaped quotes handled ('it''s', 'O\'Brien')
```

## AI Diagnostics Engine

### Architecture

```
Storage.LoadTracesForDiag(domain, window)
   │
   ▼
DiagContext (traces + queries + HTTP calls + errors + pre-computed stats)
   │
   ▼
DiagEngine.Analyze(ctx)
   │
   ├── RuleN1Queries.Evaluate()       → N+1 pattern analysis
   ├── RuleSlowQueries.Evaluate()     → Query fingerprint timing analysis
   ├── RuleExternalAPICalls.Evaluate() → HTTP bottleneck detection
   ├── RuleHighMemory.Evaluate()      → Memory peak analysis
   ├── RuleHighErrorRate.Evaluate()   → 5xx rate analysis
   ├── RuleSlowResponses.Evaluate()   → >3s response detection
   ├── RuleWPAutoload.Evaluate()      → wp_options autoload timing
   ├── RuleMissingOpcache.Evaluate()  → PHP bottleneck heuristic
   └── RulePhpVersion.Evaluate()     → PHP version EOL/upgrade check
   │
   ▼
DiagReport
   ├── findings[]  (sorted by score, worst first)
   ├── health_score (0-100, diminishing returns)
   └── summary (human-readable)
   │
   ▼
GenerateMarkdownReport(report) → markdown string (with emoji indicators)
GeneratePlainTextReport(report) → plain text string
   │
   ├── API: GET /api/v1/diagnostics/{domain}/report?format=markdown|text
   └── CLI: phpray-collector report -domain <domain> -s <db> -w <minutes>
```

### Rule Interface
```go
type DiagRule interface {
    Name() string
    Evaluate(ctx *DiagContext) []DiagFinding
}
```

Each rule returns 0 or more findings with:
- **Severity**: critical / warning / info
- **Score**: 0-100 impact weight (used for health score calculation)
- **Evidence**: specific URIs, queries, or metrics that triggered the finding
- **Fix**: actionable recommendation

### Health Score Calculation
Uses diminishing returns to prevent score from always being 0 when multiple issues exist:
```
penalty = Σ(finding.score × weight[i])
  where weight[0] = 1.0, weight[i] = 1.0 / (1.0 + i × 0.5)
health_score = max(0, 100 - penalty)
```

### Data Loading
`LoadTracesForDiag` loads up to 1000 traces with their sub-records (queries, HTTP calls, errors) for analysis. For normal+ traces, sub-records are loaded individually to provide evidence.

## Component/Plugin Breakdown (v0.9.0)

### Architecture

The component breakdown provides cross-request aggregation of PHP function-level profiling data, answering "which plugins/packages consume the most time across all requests?"

```
GET /api/v1/components?window=60&sort=total_ms&category=plugin&domain=example.com

Storage.GetComponentStats()
  ├── Query traces with non-empty components_json in time window
  ├── Parse JSON arrays from each trace
  ├── Aggregate per component name:
  │   ├── total_ms (sum of all time across requests)
  │   ├── avg_ms (average time per appearance)
  │   ├── max_ms (worst single request)
  │   ├── total_calls (sum of function calls)
  │   ├── trace_count (number of requests containing this component)
  │   └── avg_pct (average % of request time)
  ├── Categorize: plugin, theme, vendor, wp-core, core, other
  ├── Sort by requested field
  └── Return top N results
```

### Categories

Component names from the profiler are categorized by path prefix:
- `plugins/<name>` → "plugin" (WordPress plugins)
- `themes/<name>` → "theme" (WordPress themes)
- `vendor/<package>` → "vendor" (Composer packages)
- `wp-includes` → "wp-core" (WordPress core)
- `core` → "core" (application code)

### Dashboard

The Components tab includes:
- **Sortable table**: All components with time, calls, request count, avg % impact
- **Horizontal bar chart**: Top 10 components by total time, color-coded by category
- **Top Plugins panel**: WP plugins ranked by average % of request time with impact rating
- **Category filter**: Filter to show only plugins, themes, vendor packages, etc.

### Impact Rating (for plugins)

Based on average percentage of request time:
- 🔴 High: >20%
- 🟡 Medium: >10%
- 🟢 Low: >3%
- ⚪ Minimal: ≤3%

## Alerting System (v0.8.0)

### Architecture

The alerting system runs as part of the daemon's aggregation cycle (every 60s), evaluating threshold-based rules against recent trace data:

```
Daemon aggWorker
  └── runAggregation() → Aggregate1m()
      └── checkAlerts()
          ├── DefaultAlertRules() → 6 built-in rules
          ├── EvaluateAlerts(store, rules)
          │   ├── computeAlertMetrics() → per-domain stats (avg, P95, error rate, N+1 rate)
          │   ├── For each rule × domain:
          │   │   ├── Check threshold (gt/lt operator)
          │   │   ├── Check cooldown (GetLastAlertTime)
          │   │   └── If triggered → create AlertEvent
          │   └── Return new events
          └── StoreAlert() for each event + log at severity level
```

### Default Rules (always active)

| Rule | Metric | Threshold | Severity | Cooldown |
|------|--------|-----------|----------|----------|
| p95_warning | P95 duration | >3000ms | warning | 15min |
| p95_critical | P95 duration | >10000ms | critical | 15min |
| error_rate_warning | Error rate | >5% | warning | 15min |
| error_rate_critical | Error rate | >20% | critical | 15min |
| n1_rate_warning | N+1 rate | >30% | warning | 30min |
| avg_duration_warning | Avg duration | >5000ms | warning | 15min |

### Deduplication & Cooldown

Each rule+domain combination has a cooldown period. `GetLastAlertTime(ruleName, domain)` returns the most recent alert timestamp for that combo. If within cooldown window, the alert is suppressed.

Minimum traffic threshold: domains with <5 requests in the window are skipped to avoid false positives.

### Storage Schema

```sql
alerts (id, rule_name, severity, metric, current_value, threshold_value,
        domain, message, created_at, acknowledged, ack_at)
```

### API Endpoints

```
GET  /api/v1/alerts         ← list alerts (params: limit, ack=0|1, severity)
GET  /api/v1/alerts/count   ← unacknowledged count (lightweight, for badge polling)
POST /api/v1/alerts/{id}/ack ← acknowledge an alert
```

Domain-based RBAC applies: user-role tokens only see alerts for their domains.

### Dashboard Integration

- Alerts tab with red badge showing unacknowledged count
- Badge polls `/api/v1/alerts/count` every 30s
- Severity-coded rows (red=critical, yellow=warning, blue=info)
- Per-alert acknowledge button → POST, then refresh
- Severity filter dropdown + "show acknowledged" checkbox

## Crash Recovery (v0.12.0)

### Architecture

The crash recovery system auto-disables PHPRay if it crashes repeatedly, preventing customer sites from being impacted on shared hosting. It uses a shared mmap'd health file for zero-overhead status checks.

```
┌─────────────────────────────────────────────────────────┐
│                    PHP-FPM Worker                        │
│                                                         │
│  MINIT:                                                 │
│    phpray_health_init()                                 │
│      ├── Open/create /tmp/phpray_health (mmap)          │
│      ├── Count crashes within window (configurable)     │
│      ├── If crashes >= threshold → return -1 (disable)  │
│      └── Install signal handlers (SIGSEGV/SIGBUS/SIGABRT)│
│                                                         │
│  RINIT (fast path):                                     │
│    phpray_health_check()                                │
│      └── Single atomic read of mmap disabled flag       │
│                                                         │
│  On crash (signal handler — async-signal-safe):         │
│    phpray_crash_handler()                               │
│      ├── Atomic increment crash_count                   │
│      ├── Record timestamp in crash_times ring           │
│      ├── write() to stderr (async-signal-safe)          │
│      └── raise(sig) — reraise for default action        │
│                                                         │
│  MSHUTDOWN:                                             │
│    phpray_health_shutdown()                             │
│      ├── Restore original signal handlers               │
│      └── munmap + close health file                     │
└─────────────────────────────────────────────────────────┘
```

### Health File Structure

```
/tmp/phpray_health (mmap'd, shared across FPM workers):

  magic(4)         — 0x50485279 ("PHRy")
  version(4)       — 1
  crash_count(4)   — total crashes recorded
  disabled(4)      — 1 if auto-disabled
  crash_times[8]   — ring of recent crash timestamps (int64)
  crash_slot(4)    — next write slot in ring
  last_reset(8)    — timestamp of last manual reset
```

### Signal Handler Safety

The crash handler is strictly async-signal-safe:
- Uses only `write()`, `clock_gettime()`, `raise()` — all POSIX async-signal-safe
- No malloc, printf, PHP/Zend functions, stdio, syslog
- Uses `__atomic_*` builtins for concurrent mmap access
- `SA_RESETHAND` flag ensures handler doesn't loop on repeated signals

### Configuration

```ini
phpray.crash_threshold = 3     ; crashes within window to trigger disable
phpray.crash_window = 60       ; time window in seconds
```

### Userland Functions

- `phpray_health_status(): array` — returns crashes, threshold, window_sec, disabled, last_crash_time, last_reset
- `phpray_health_reset(): void` — resets crash counter and re-enables the extension

### Recovery Flow

1. Extension crashes → signal handler records timestamp in mmap'd health file
2. PHP-FPM spawns new worker → MINIT runs → `phpray_health_init()` checks crash count
3. If >= threshold crashes in window → auto-disable, emit E_WARNING
4. After window expires (crashes age out) → next FPM restart re-enables automatically
5. Manual re-enable: call `phpray_health_reset()` or delete `/tmp/phpray_health`

## Install System

### Architecture

The install script (`scripts/install.sh`) handles multi-PHP-version deployments common in shared hosting environments (e.g., DirectAdmin with PHP 8.1, 8.2, 8.3 side-by-side).

```
install.sh
  ├── detect_php_versions()
  │   ├── /usr/bin/phpize8.*           (Debian/Ubuntu)
  │   ├── /usr/bin/phpize-8.*          (some distros)
  │   ├── /usr/local/php*/bin/phpize   (DirectAdmin/custom)
  │   └── PHPRAY_PHP_VERSIONS env var  (override)
  │
  ├── For each PHP version:
  │   ├── get_phpize(ver) → build in temp dir
  │   ├── phpize + configure + make → phpray.so
  │   ├── Install to extension_dir (from php-config)
  │   └── configure_ini(ver) → detect INI dir:
  │       ├── mods-available + phpenmod (Debian)
  │       └── conf.d/99-phpray.ini (direct)
  │
  ├── build_collector() → Go build → bin/phpray-collector
  │
  ├── install_collector()
  │   ├── Binary → /usr/local/bin/phpray-collector
  │   ├── Config → /etc/phpray/collector.toml
  │   └── Data dir → /var/lib/phpray/
  │
  ├── install_service() → systemd unit + enable + start
  │
  └── Reload PHP-FPM for installed versions
```

### Modes
- `--ext-only`: Build and install extension only (no Go, no service)
- `--collector-only`: Build and install collector + systemd only
- `--uninstall`: Clean removal (preserves config/data by default)
- `--status`: Check installation health

### Uninstall Flow
1. Stop + disable systemd service
2. Remove collector binary
3. For each PHP version: remove phpray.so + INI files
4. Clean shared memory
5. Preserve `/etc/phpray` and `/var/lib/phpray` (data safety)

## CLI Tool (`phpray`)

Unified CLI wrapper that delegates to `phpray-collector` for most operations and adds:
- `phpray status` — composite status check (extension in all PHP versions, service, API health, ring buffer, JSONL, database sizes)
- `phpray diag <domain>` — pretty-printed diagnostics with colored health score, severity icons, and fix recommendations (calls `/api/v1/diagnostics/{domain}` API)
- Pass-through for all `phpray-collector` commands (top, tail, trace, slow-queries, serve, etc.)

Binary lookup order: `/usr/local/bin/phpray-collector` → `/usr/bin/phpray-collector` → `../bin/phpray-collector` (relative to script).

## Data Volume Estimates (production, 200 req/s)

```
~90% requests (<200ms) → summary 200B  →  ~1.4 MB/h
~8%  requests (200ms-1s) → normal 2KB  →  ~11.5 MB/h
~2%  requests (>1s)     → full 5KB     →  ~7.2 MB/h
──────────────────────────────────────────
Total: ~20 MB/h = ~480 MB/day raw
With compression: ~150 MB/day
7 day retention: ~1 GB
30 day retention: ~4.5 GB
```

Ring buffer: 32MB = stores ~5 minutes of full-rate traffic before collector must consume.

SQLite aggregates_1m: ~200 domains × 1440 buckets/day × 30 days = ~8.6M rows ≈ ~500MB
With retention, stays bounded.
