# PHPRay ingest protocol v1 (draft, 2026-09-18)

How data gets from a server to a console. Applies to the Go collector (`phpray-collector`),
the WordPress plugin and any third-party agent. Consoles: PHPRay Cloud (`https://ingest.phpray.dev`)
and Host Edition (self-hosted, same API). Status: **draft** — freeze together with collector 0.14.

## 1. Principles

- **Outbound only.** Agents open HTTPS connections to the console; the console never connects
  to a server. No inbound ports, no SSH, no shell.
- **Aggregates always, traces selectively.** Every minute an agent ships per-site aggregates
  (cheap, bounded). Individual traces are shipped only for levels `normal`/`full`/`alert`
  and are sampled server-side when the site exceeds its plan quota.
- **No secrets, no payloads.** SQL is shipped as a fingerprint (numbers and quoted strings
  replaced), URIs without query values (query keys kept, values `*`), no request/response bodies,
  no cookies, no headers except `Host`. Hostnames can be pseudonymised per site (`privacy.mask_host`).
- **Idempotent and lossy-safe.** Batches carry a `batch_id`; the console deduplicates for 24 h.
  An agent that cannot deliver keeps at most `buffer.max_mb` (default 32 MB) on disk and drops
  the oldest traces first, never aggregates.

## 2. Identity

| Thing | Identifier | Where it comes from |
|---|---|---|
| Account (agency / host) | `account_id` | console |
| Server | `server_key` = `prk_<32 hex>` | created in console → pasted into `collector.toml` (`[cloud] server_key`) or WP plugin settings |
| Site | `site_id` = sha1(`host` + `\0` + `docroot`)[:16] | computed by the agent; the WP plugin uses `home_url()` host + `ABSPATH` |
| Agent | `agent` = `collector/0.14.0` or `wp-plugin/0.1.0` | constant per build |

A `server_key` belongs to exactly one account and one server; rotating it in the console
invalidates the old key after 24 h grace.

## 3. Endpoints

All requests: `Authorization: Bearer <server_key>`, `Content-Type: application/json`,
optional `Content-Encoding: gzip` (recommended above 8 KB). Responses are JSON.
Clock skew: the agent sends `sent_at` (unix ms); the console uses its own receive time for
ordering and returns `server_time` so the agent can log drift.

### `POST /v1/aggregates`

One request per minute per server (may batch several minutes after an outage).

```json
{
  "batch_id": "b_<uuid>", "sent_at": 1789737600000, "agent": "collector/0.14.0",
  "server": {"hostname_hash": "<sha1[:16]>", "php_versions": ["8.3.33"], "os": "linux-amd64"},
  "minutes": [
    {"ts": 1789737540, "site_id": "…", "host": "shop.example", "app": "wordpress",
     "requests": 412, "errors_5xx": 3, "errors_php": 7, "profiled": 12,
     "duration_ms": {"avg": 231.4, "p50": 198.0, "p95": 640.2, "max": 3120.5},
     "cpu_ms": {"avg": 91.2}, "memory_mb": {"avg": 42.1, "max": 128.0},
     "db": {"queries": 40120, "ms": 18200.4, "n1_requests": 5},
     "http": {"calls": 63, "ms": 9120.0, "errors": 2},
     "levels": {"summary": 380, "normal": 27, "full": 4, "alert": 1},
     "components": [{"name": "plugins/woocommerce", "incl_ms": 1201.5, "self_ms": 640.1, "calls": 18800}],
     "top_uris": [{"uri": "/checkout/", "requests": 31, "p95_ms": 1120.0}]}
  ]
}
```

Response `202 {"accepted": 3, "duplicates": 0, "server_time": …}`.

### `POST /v1/traces`

Batches of up to 500 trace records (the JSONL record produced by the extension or the WP
plugin, unchanged, plus `site_id`). Sent at most every 5 s, or immediately for `alert`.

```json
{"batch_id": "b_<uuid>", "sent_at": …, "agent": "…",
 "traces": [ { "site_id": "…", "ts": …, "host": "…", "uri": "…", "level": "full", … } ]}
```

Response `202 {"accepted": 120, "dropped_quota": 0}`. If `dropped_quota > 0` the console is
sampling; the agent should lower its own `traces.sample_rate` to the value returned in
`X-PHPRay-Trace-Sample` (percent) until the next successful full batch.

### `GET /v1/control`

Long-poll (up to 25 s) for control messages addressed to this server — this is how
"profile URL X for 10 minutes" from the console or the DirectAdmin plugin reaches the
extension without touching php.ini:

```json
{"server_time": …, "messages": [
  {"id": "m_1", "type": "profile", "site_id": "…", "url_prefix": "/checkout/",
   "sample_rate": 100, "until": 1789738200},
  {"id": "m_2", "type": "config", "site_id": "…", "traces": {"sample_rate": 10}}
]}
```

The collector writes `profile` messages into the shared-memory control table read by the
extension at RINIT (docroot → {url_prefix, sample_rate, until}); the WP plugin applies them
in-process. Agents acknowledge with `POST /v1/control/ack {"ids": ["m_1"]}`.

### `GET /v1/health`

`200 {"ok": true, "account": "…", "server": "…", "plan": "studio", "quota": {"traces_per_day": 200000, "used": 84121}}`.
Used by `phpray status` and by the WP plugin settings page ("connected as …").

## 4. Errors and limits

| Code | Meaning | Agent behaviour |
|---|---|---|
| 401 | bad or rotated key | stop sending, surface in `phpray status` and dashboard |
| 402 | plan exhausted / unpaid | keep aggregates, stop traces, surface |
| 413 | batch too large | split batch in half and retry |
| 429 | rate limited (`Retry-After`) | back off, keep buffering |
| 5xx | console problem | exponential backoff 5 s → 5 min, keep buffering |

Hard limits: aggregates 1 request/min/server (burst 10 after outage), traces 500 records
or 4 MB per batch, 12 batches/min/server. Site limit per plan enforced on `site_id`.

## 5. Privacy defaults (agent side, before sending)

- SQL → fingerprint; `queries[].sql` never contains literals. Backtrace frames: file path
  abbreviated from `wp-content/` or `vendor/`, no absolute home paths.
- URI: path + fingerprinted query (`?utm_source=*&page=*`); `ignore_uris` never sent.
- `host` may be replaced with `site_id` when `privacy.mask_host = true` (per site).
- No IP addresses of visitors. No user identifiers. No cookies, headers, bodies.
- Errors: message truncated to 200 chars; file paths abbreviated as above.

## 6. Versioning

`/v1` is stable once frozen; additive fields only. Breaking changes → `/v2` with 12 months
of parallel support. Agents send `agent` and consoles may refuse EOL agents with `426`.
