# WordPress plugin

The PHPRay WordPress plugin gives you the application layer of PHPRay on
any host — including shared hosting — **without the PHP extension**.
Version 0.1.1 is a direct download:
[phpray-wordpress-0.1.1.zip](https://phpray.dev/downloads/phpray-wordpress-0.1.1.zip)
(Plugins → Add New → Upload Plugin). Listing in the WordPress.org directory
is planned. The previous build,
[phpray-wordpress-0.1.0.zip](https://phpray.dev/downloads/phpray-wordpress-0.1.0.zip),
stays available; it only works in `file` mode.

## What it captures

- per-request timing at the application layer (request start → shutdown
  hook; does not see time inside C extensions)
- all SQL via the `$wpdb` query log, including N+1 grouping
- outbound HTTP via the `http_api` filters (WP_Http)
- PHP errors/warnings/notices during the request
- **components**: every active plugin, theme and core area with
  inclusive vs self time — this is what makes "which plugin slows down
  /checkout/" a one-glance answer
- level logic identical to the extension (`smart` thresholds,
  `alert` never sampled)

It produces **the same JSONL trace record** as the extension
([trace format](trace-format.md)) — the collector, dashboard, CLI and
PHPRay Cloud consume both without distinction.

## Settings

Tools → PHPRay:

- **Enabled** — the collector can be switched off without deactivating
  the plugin
- **Thresholds** — normal/full/alert ms (defaults 200/1000/3000)
- **Sample rate** — percent of requests that get the deep instrumentation
  (SQL callers, errors, HTTP calls, per-component breakdown; default 10%)
- **Ignore URIs** — one prefix per line (e.g. `/wp-cron.php`, `/feed`)
- **Exclude logged-in admins** — skip requests by users with
  `manage_options`
- **Output mode** — see below. **Endpoint** and **Server key** apply to
  the `collector` and `cloud` modes.

## Output modes

- **`file`** — the plugin appends JSONL records to
  `wp-content/uploads/phpray/phpray.log` (rotated at 10 MB, 3 files kept).
  A collector on the same box reads that file
  (`[input] jsonl_path = "<path to phpray.log>"` in `collector.toml`) and
  gives you the local dashboard, the CLI and the Cloud upload. Works with
  every plugin and collector version.
- **`cloud`** — **works from plugin 0.1.1.** The plugin talks to PHPRay
  Cloud directly over the [ingest protocol](../cloud/ingest.md): batches
  of trace records go to `https://app.phpray.dev/v1/traces` (envelope with
  `batch_id`, `sent_at`, `agent: wp-plugin/0.1.1`, every record carrying a
  `site_id` derived from the `home_url()` host and `ABSPATH`), and
  per-minute aggregates go to `/v1/aggregates`, which is what the fleet
  view, charts and alerts are built from. Paste the server key (`prk_…`)
  created in the console. Only `normal`/`full`/`alert` records are shipped
  as traces; `summary` requests are counted in the aggregates. Batches are
  sent at most every 5 s (immediately for `alert`) after the response has
  been handed to the web server. The status box in Tools → PHPRay shows the
  site ID, the last accepted batch, a rejected key (HTTP 401 pauses sending
  until you change the key and save), the sampling share requested by the
  console when a site is over quota, and the last error.
- **`collector`** — **works from collector 0.15** (plugin 0.1.1). The same
  envelope is POSTed to the local collector at
  `http://127.0.0.1:9191/api/v1/ingest`
  ([endpoint details](../configuration/collector.md#http-ingest-post-apiv1ingest)).
  The records then take the collector's normal path: live dashboard, SQLite,
  CLI, and the Cloud upload when the collector has `[cloud]` enabled — with
  the plugin's `site_id`, so the site is the same one the plugin would
  register in `cloud` mode. Paste a token in **Server key** only when the
  collector has an `[auth] secret` (mint one with `phpray token -role user`);
  without a secret the endpoint takes loopback clients only, which is what
  a plugin on the same box is. Collector 0.14 has no ingest endpoint: with it,
  keep `file` mode.

## Limits (honest)

- It is a userland library: it cannot see the PHP core, C extensions, or
  time spent outside the request lifecycle hooks. The extension sees all
  of that; the plugin sees the application.
- One plugin per site: it traces the site it is installed in, not other
  sites on the same account or VPS.
- Component time is attributed from SQL and HTTP callers (the plugin or
  theme file that triggered the query or call); it has no per-function
  profiler, so `self_ms` equals `incl_ms` in the aggregates.
- Records are buffered in a WordPress option (200 records, 60 pending
  minutes) between batches; on hosts without `fastcgi_finish_request()` /
  `litespeed_finish_request()` the batch is sent after the page output but
  before the PHP process ends.
- In `smart` mode it only profiles the sampled share; the summary layer is
  always on and cheap (a handful of hooks per request; see
  [Overhead](../index.md#overhead)).
