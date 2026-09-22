# Extension configuration (INI)

All directives live in PHP's INI system. Scope decides where you can set it:

- **PERDIR** — settable in `.htaccess` (`php_value`, mod_php), `.user.ini`
  (FPM / lsphp) and per-vhost ini files, in addition to `php.ini`.
- **SYSTEM** — only meaningful in a global `php.ini` (or the ini include of
  the whole PHP binary). PERDIR directives can also be set in `php.ini` as
  the fleet-wide default.

## Directives

| Directive | Scope | Default | Meaning |
|---|---|---|---|
| `phpray.enabled` | PERDIR | `1` | Master switch. `0` = extension is inert (no tracing, no output). |
| `phpray.mode` | PERDIR | `smart` | `smart` = automatic level by request duration, `all` = full trace of every request, `manual` = only when triggered (console/CLI). |
| `phpray.smart_threshold_normal` | PERDIR | `200` | ms. Requests slower than this get a `normal` trace. |
| `phpray.smart_threshold_full` | PERDIR | `1000` | ms. Slower than this → `full` trace. |
| `phpray.smart_threshold_alert` | PERDIR | `3000` | ms. Slower than this → `alert` trace (never sampled). |
| `phpray.manual_sample_rate` | PERDIR | `10` | percent. In `manual` mode, share of requests traced when a trigger is active. |
| `phpray.always_trace_errors` | PERDIR | `1` | `1` = even fast requests that hit a PHP error get at least a `normal` trace. |
| `phpray.ignore_uris` | PERDIR | `""` | comma-separated URI prefixes (or `*`) that are never traced, e.g. `/wp-cron.php,/assets/*`. |
| `phpray.trace_cli` | SYSTEM | `0` | `1` = also trace CLI/SOAP/FPM workers, not just web SAPIs. |
| `phpray.emit_header` | PERDIR | `1` | Add the `X-PHPRay: <level>` response header so proxies/log collectors can see the level. |
| `phpray.capture_errors` | PERDIR | `1` | Capture PHP errors/warnings/notices into the trace record. |
| `phpray.output_mode` | SYSTEM | `file` | `file` = write JSONL to `output_path`, `shm` = write into shared memory for the collector, `both`. |
| `phpray.output_path` | SYSTEM | `/tmp/phpray.jsonl` | JSONL trace file, one record per line (when `file` or `both`). |
| `phpray.shm_path` | SYSTEM | `/dev/shm/phpray` | Path of the shared-memory ring buffer. May contain `%u` (effective uid) and `%g` (effective gid): then every user gets a private ring, see [Per-user rings](#per-user-rings-u-g). |
| `phpray.shm_size` | SYSTEM | `33554432` | Ring size in bytes (32 MB). With per-user rings this is the size of *each* user's ring — use 1–2 MB there. |
| `phpray.health_path` | SYSTEM | `/tmp/phpray_health` | Crash-recovery health file (crash counters, auto-disable flag). Accepts the same `%u` / `%g` placeholders; with `%u` it is created `0600` and only used when owned by that uid. |
| `phpray.profile_functions` | SYSTEM | `1` | `1` = register the function-call observer at startup (required for profiling; costs nothing unless profiling is on). |
| `phpray.profile_mode` | PERDIR | `sample` | `off` = no profiling, `sample` = random sample at `profile_sample_rate`, `url` = only URIs matching `profile_url`, `all` = every request. |
| `phpray.profile_sample_rate` | PERDIR | `3` | percent of requests that get a per-function profile. |
| `phpray.profile_url` | PERDIR | `""` | comma-separated URI prefixes to profile when `profile_mode=url`, e.g. `/checkout/,/cart/`. |
| `phpray.profile_max_components` | SYSTEM | `64` | cap on distinct components (plugins/functions) stored per trace. |
| `phpray.crash_threshold` | SYSTEM | `3` | crash recovery: this many PHP crashes (SIGSEGV/SIGBUS/SIGABRT) within `crash_window` disable the extension until the health file is removed or `phpray_health_reset()` is called. |
| `phpray.crash_window` | SYSTEM | `60` | seconds, window for the crash threshold. |

## Recipes

### 1. Disable for one site

In that site's `~/.user.ini` (FPM/lsphp) or `.htaccess` (mod_php):

```ini
php_value phpray.enabled=0
```

Everything else on the server keeps tracing; this docroot is inert.

### 2. Profile one URL for a debugging session

Temporarily, for one site:

```ini
php_value phpray.profile_mode=url
php_value phpray.profile_url=/checkout/
php_value phpray.profile_sample_rate=100
```

Visit `/checkout/` a few times, read the per-function breakdown in the
dashboard (`phpray trace <domain>` / dashboard → site → traces), then remove
the three lines. Alternatively do this from the cloud console without
touching ini: the collector pushes a time-boxed `profile` control message
(see [Ingest protocol](../cloud/ingest.md), `GET /v1/control`).

### 3. Run with zero profiling

Fleet-wide in `php.ini`:

```ini
phpray.profile_mode=off
```

You keep always-on summaries, `normal`/`full`/`alert` traces and N+1/error
detection, but the function-level observer never samples a request —
minimal overhead (see [Overhead](../index.md#overhead)).

## Per-user rings (`%u` / `%g`)

By default one ring buffer serves the whole server. It is created at module
start (`0666` minus umask) and every PHP worker writes into it — fine on a
single-tenant box, but on shared hosting a world-readable ring lets one
customer's PHP read the traces (URIs, SQL, hosts) of every other customer.

Put `%u` (effective uid) and/or `%g` (effective gid) into `phpray.shm_path`
and the extension switches to one private ring per user:

```ini
phpray.output_mode = shm
phpray.shm_path    = /run/phpray/ring-%u
phpray.shm_size    = 2097152
```

- Each worker attaches its ring **lazily, at its first traced request** — after
  PHP-FPM, lsphp or CGI has already switched to the site's user (module start
  still runs as root in the FPM master, so expanding `%u` there would be wrong).
  If the effective uid/gid changes later in the same process (mod_ruid2 style
  SAPIs), the worker re-attaches to the new user's ring.
- `%u` → the file is created `0600`; `%g` alone → `0660`. The collector runs as
  root and reads all of them: `[input] shm_path = "/run/phpray/ring-*"` (glob,
  collector 0.15+).
- The ring is opened if it already exists (workers of one user share it) and
  built only when missing or stale (different size/version). A new ring is
  prepared under a temporary name and published atomically, so a reader never
  sees a half-initialised header, and nothing is ever truncated in place.
- The directory is **not** created by the extension. Use a root-owned `1777`
  directory (`/run/phpray`, see the installer's CageFS mode); the extension
  refuses a file it does not own and never follows symlinks, so a file another
  user plants under your name is simply ignored.
- Per-user rings are never removed by PHP; they live on tmpfs and disappear at
  reboot. Memory: touched pages only, at most `shm_size` per active user
  (1 MB × 1000 active accounts = 1 GB worst case) — keep `shm_size` small here.
- `phpray_ring_stats()` returns `path` (the expanded, attached ring) and
  `per_user`; `phpinfo()` shows the same under "Ring Buffer".

`%%` is a literal `%`; any other `%x` is copied unchanged. On CloudLinux the
ring must live in a directory shared with the cages — `/dev/shm` is private per
cage, see [Troubleshooting → CloudLinux / CageFS](../troubleshooting.md#cloudlinux-cagefs).

## On-demand profiling table

`phpray.control_path = /dev/shm/phpray-control` — the shared-memory table the
collector writes (from `phpray control set …` or from the Cloud console) and the
extension reads at request start. Entries match by document root and URI prefix
and switch full profiling on for a limited time, without touching php.ini. The
table is shared by all users (read-only for PHP), so it takes no `%u`; on CageFS
hosts point it at the shared directory too: `phpray.control_path = /run/phpray/control`.
