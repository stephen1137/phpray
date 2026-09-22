# Collector configuration

`phpray-collector` is a small Go daemon that reads the trace stream from
shared memory (or the file path), stores it, applies retention/alerts and
serves the local API + dashboard. Config file: `/etc/phpray/collector.toml`
(override with `--config`).

## `collector.toml`

The installer writes `/etc/phpray/collector.toml`; every key below is optional
and shown with its default.

```toml
[input]
jsonl_path   = "/tmp/phpray.jsonl"          # where the extension appends JSONL records (output_mode = file)
shm_path     = "/dev/shm/phpray"            # ring buffer (output_mode = ring); a glob such as "/run/phpray/ring-*" reads one ring per PHP-FPM pool
control_path = "/dev/shm/phpray-control"    # on-demand profiling table, read by the extension at request start

[storage]
db_path = "/var/lib/phpray/traces.db"       # SQLite: traces, queries, aggregates, alerts

[collector]
mode             = "auto"                   # auto | ring | jsonl  (auto prefers the ring buffer when it exists)
poll_interval_ms = 100
flush_interval_s = 5
batch_size       = 50
agg_interval_s   = 60                       # per-minute aggregates

[retention]
days   = 30                                 # purge traces and aggregates older than this
max_mb = 4096                               # hard cap on the database file: the oldest traces are purged beyond it (0 = off)

[logging]
level = "info"
quiet = false

[auth]
secret = ""                                 # JWT secret; empty = dashboard/API without login (keep it on 127.0.0.1)

[server]
da_data_path = ""                           # DirectAdmin: /usr/local/directadmin/data, maps users to domains

[cloud]
enabled          = false
endpoint         = "https://app.phpray.dev"
server_key       = ""                       # prk_… from your PHPRay Cloud account (one per server)
buffer.max_mb    = 32                       # disk buffer while the console is unreachable; oldest batches dropped first
buffer.dir       = ""                       # default: <db dir>/cloud-buffer
traces.sample_rate = 100                    # % of normal/full/alert traces shipped (the console may lower it)
control          = true                     # long-poll /v1/control for "profile this URL" messages

[cloud.privacy]
mask_host = false                           # send the site id instead of the hostname
```

The listen address of the dashboard/API is not in the file: it is the `-addr`
flag of `phpray-collector serve` in the systemd unit (the installer's
`--listen` option). Inline comments after a value are allowed.

## Several ring buffers (`shm_path` glob)

From collector 0.15 `shm_path` may be a glob pattern. Use it when the
extension writes one ring per identity — `phpray.shm_path = "/run/phpray/ring-%u"`
in php.ini (`%u` = uid, `%g` = gid, extension 0.15+), which keeps every
PHP-FPM pool's traces in a file only that user (and root) can open:

```toml
[input]
shm_path = "/run/phpray/ring-*"
```

- The collector rescans the pattern every 5 s: a ring that appears (a pool
  served its first request) is opened and logged, a ring whose file went away
  is closed and logged, a file recreated with a new inode is reopened.
- All matching rings are read in one loop; overflow is reported per ring.
- `mode = "auto"` and `mode = "ring"` both use the set; a pattern never falls
  back to JSONL. `mode = "jsonl"` ignores `shm_path`.
- A pattern with no match yet is normal on a fresh server; `phpray status`
  lists the files found and, when the service runs, the rings it has open.

Without a pattern the collector reads the single file as before.

## HTTP ingest (`POST /api/v1/ingest`)

From collector 0.15 the local API also accepts trace batches over HTTP. It is
what the [WordPress plugin](../usage/wordpress-plugin.md) uses in `collector`
mode and what any third-party agent can use; the records land on the same
path as ring-buffer and JSONL traces: live dashboard, SQLite, CLI and — when
`[cloud]` is enabled — the Cloud upload, with the `site_id` from the record.

- **Body**: the [ingest protocol](../cloud/ingest.md) batch envelope
  `{"batch_id":"b_…","sent_at":<unix ms>,"agent":"wp-plugin/0.1.1","traces":[…]}`,
  or a bare JSON array of trace records. `Content-Type: application/json`,
  optional `Content-Encoding: gzip`.
- **Records**: the [trace record](../usage/trace-format.md), optionally with
  `site_id`. `ts` in seconds (milliseconds are converted). A record without
  `host` and `uri`, or that is not an object, is skipped and counted in
  `dropped`; the rest of the batch is still accepted.
- **Access**: with an `[auth] secret` the request needs the dashboard token
  (`Authorization: Bearer <jwt>`, minted with `phpray token`); without a secret
  only clients from `127.0.0.1` / `::1` may post — anything else gets `403`.
- **Limits**: 8 MB per body (after decompression) and 500 records per batch,
  both answered with `413`; malformed JSON with `400`.
- **Idempotent**: a `batch_id` seen in the last 24 h is answered `202` with
  `accepted: 0` and `duplicate: true`, nothing is stored twice.
- **Response**: `202 {"accepted": N, "dropped": M, "dropped_quota": 0, "server_time": <unix ms>}`.

```bash
curl -s -X POST http://127.0.0.1:9191/api/v1/ingest \
  -H 'Content-Type: application/json' \
  -d '{"batch_id":"b_test","sent_at":1789737600000,"agent":"curl/1",
       "traces":[{"ts":1789737600,"host":"shop.example","uri":"/checkout/","method":"GET",
                  "status":200,"duration_ms":312.5,"level":"normal"}]}'
# {"accepted":1,"dropped":0,"dropped_quota":0,"server_time":1789737600123}
```

Counters: `GET /api/v1/health` → `ingest` (`batches`, `traces`, `dropped`,
`duplicates`, `rejected`), `/metrics` → `phpray_ingest_*`, and `phpray status`.

## Running it

```bash
systemctl enable --now phpray-collector
systemctl status phpray-collector
phpray status          # one-line view: collector, shm, dashboard, cloud
```

## Logs

- journald: `journalctl -u phpray-collector`
- file fallback (only if journald unavailable): `/var/log/phpray/collector.log`
- trace storage: `/var/lib/phpray/phpray.db` (SQLite, one file)
- raw file-mode traces (if `output_mode=file`): `/var/lib/phpray/traces/*.jsonl`

## Disk limits

The collector enforces `[retention].max_mb` (default 4 GB) as a hard cap on
the SQLite file: when the file exceeds it, oldest rows are pruned until it
fits again. Retention windows (`traces_days`, `aggregates_days`) prune
independently, whichever hits first. The cloud spool is separately capped by
`[cloud].buffer.max_mb` (default 32 MB) and drops oldest traces, never
aggregates.
