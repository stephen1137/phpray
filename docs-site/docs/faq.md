# FAQ

### Does it slow down my site?

The always-on summary layer (plus the `normal`/`full`/`alert` traces) hooks
request-level events only; in our WooCommerce benchmark it stays within the
measurement noise of the baseline — see [Overhead](index.md#overhead) for what
has been measured and what still awaits certification. To be clear about the rest:
per-function profiling is **sampled** (default 3% of requests, and only for
profiled requests) — the remaining 97% run without a single function-level
sample. You can also run with `phpray.profile_mode=off` for the minimum
footprint (the observer is then never registered for a request).

### Do I need root access?

On a VPS/container, yes (to install the extension and the collector). On
shared hosting the host installs the extension; as a customer you can use
the WordPress plugin with no root at all.

### Does it work with opcache / REND / lsphp / CageFS?

Yes. It is a standard PHP extension and works under lsphp, mod_php,
FPM and REND; opcache is unaffected (PHPRay reads no files at request
time). CageFS: the shm path must be mounted into the cage (`/etc/cagefs/cagefs.mp`, see [Troubleshooting](troubleshooting.md)).

### What does it store? Where?

JSONL trace records (one line per request, a few KB each; [trace format](usage/trace-format.md))
in shared memory or a local directory, then into a local SQLite file with
a hard size cap (default 4 GB) and retention windows. Nothing leaves the
box unless you configure PHPRay Cloud.

### Does it send any of my data anywhere?

Not by default — collector and dashboard are local and bound to
127.0.0.1. With PHPRay Cloud, only aggregates and sampled traces go out,
and with privacy defaults applied: SQL as fingerprints, URIs without query
values, no bodies, no cookies, no visitor IPs
([ingest protocol §5](cloud/ingest.md)).

### Is my SQL data / secrets safe?

SQL is stored as a fingerprint — numbers and quoted strings are replaced
before storage. Request/response bodies, cookies and headers are never
captured. Error messages are truncated and paths abbreviated.

### It works for PHP — what about the PHP that runs my cron jobs?

`phpray.trace_cli=1` (SYSTEM scope) includes CLI workers. Cron and queue
workers are traced like web requests; set it only if you want it.

### Multiple PHP versions on one box?

Install one `.so` per version (the installer script does this for you) and
the collector reads them all; traces carry the PHP version.

### Can I exclude a site / a URI pattern?

Yes: `phpray.enabled=0` or `phpray.ignore_uris=/assets/*,/wp-cron.php` in
that site's `.user.ini` (PERDIR scope, closest file wins).

### What is the difference between the extension and the WordPress plugin?

The extension sees the whole request (core, C extensions, memory, every
query). The plugin sees the application layer (WP core, plugins, themes,
`$wpdb`, WP HTTP) and needs no extension at all — so it is the option for
shared hosting. Both emit the same trace record.
