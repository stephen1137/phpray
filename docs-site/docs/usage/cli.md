# CLI

`phpray` is a command alias for `phpray-collector`, installed by `install.sh`
and by the `.deb`/`.rpm` packages. It reads the same JSONL file, ring buffer and
SQLite database as the collector service, so it works with or without the
dashboard.

## Health

### `phpray status`

One screen: every PHP binary found and whether it loads `phpray.so`, the
collector config, ring buffer, JSONL file and database, whether the dashboard
API answers on `127.0.0.1:9191` (`-addr` to change) and the Cloud connection
state (off / waiting / connected / key rejected).

When `[input] shm_path` is a glob (one ring per PHP-FPM pool, see
[collector configuration](../configuration/collector.md#several-ring-buffers-shm_path-glob))
it lists the ring files found and, if the service runs, every ring the
collector has open with its record and overflow counters. The
`Dashboard / API` section also shows the state of `POST /api/v1/ingest`
(batches and traces accepted, records dropped, requests rejected, and whether
the endpoint takes a Bearer token or loopback clients only).

```
Collector
  config /etc/phpray/collector.toml        ok
  ring buffers /run/phpray/ring-*          2 found
    /run/phpray/ring-1001                  ok
    /run/phpray/ring-1002                  ok
  ...
Dashboard / API
  http://127.0.0.1:9191/  ok   (HTTP 200, 48213 traces stored)
  ring buffers open in the collector: 2
    /run/phpray/ring-1001                  v5  31207 records, 0 dropped, 3% full
    /run/phpray/ring-1002                  v5  17006 records, 0 dropped, 1% full
  ingest http://127.0.0.1:9191/api/v1/ingest  ok   (120 batches, 4180 traces, 0 records dropped, 0 requests rejected; loopback clients only)
```

## Live views (JSONL, no database needed)

### `phpray tail`

Live trace stream, like `tail -f` with colours. `-f <path>` (default
`/tmp/phpray.jsonl`), `-d <domain>` to filter one site.

### `phpray top`

Auto-refreshing top slow requests, an `htop` for PHP. `-n 20` entries,
`-r 2` refresh seconds, `-w 60` window in seconds, `-d <domain>`.

### `phpray stats`

Aggregate statistics from the JSONL file: request counts, latency percentiles,
DB/HTTP share. `-w 60` window in minutes.

### `phpray trace`

Per-request detail with the waterfall breakdown (PHP, SQL, HTTP, files, Redis).

```bash
phpray trace -d shop.example.com -n 20            # last 20 requests of a site
phpray trace -d shop.example.com -level alert     # only alert-level traces
phpray trace -d shop.example.com -n1              # only requests with an N+1 pattern
phpray trace -d shop.example.com -json            # raw JSON lines for scripting
```

## Database views (SQLite)

### `phpray import`

Import a JSONL file into the SQLite database: `-f <jsonl> -s <db>`.

### `phpray slow-queries`

Top slow SQL fingerprints, deduplicated and aggregated: `-s <db> -n 10`.

### `phpray agg-query`

Per-minute aggregates from the database: `-s <db> -w 60 [-d <domain>]`.

### `phpray report`

Diagnostic report for one site, markdown or text, to stdout:

```bash
phpray report -domain shop.example.com -w 60 -format markdown > report.md
```

## Service

- `phpray serve -c /etc/phpray/collector.toml -addr 127.0.0.1:9191` — collector,
  REST API and dashboard in one process (what the systemd unit runs).
- `phpray daemon -c /etc/phpray/collector.toml` — collector only, no API.
- `phpray token -secret <key> -sub <user> -role admin|user [-domains "a.com,b.com"] [-exp 720h]`
  — mint a dashboard/API login token.
- `phpray control list | set -docroot <dir> [-prefix /p] [-rate 100] [-ttl 600s] | clear -docroot <dir> | expire`
  — the on-demand profiling table read by the extension (the Cloud console
  writes the same table through the collector).
- `phpray version`.
