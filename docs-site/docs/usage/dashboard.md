# Local dashboard

The collector serves a local dashboard at `http://127.0.0.1:9191` (the
address is the `-addr` flag of `phpray-collector serve`; the installer sets it
with `--listen`). It is bound to loopback by default. When it listens on a
public address, login is protected by a JWT signed with the `[auth]` secret:
the installer prints an admin token once and keeps it in
`/etc/phpray/dashboard-token`; `phpray token -secret <secret> -sub <name> -role admin`
mints more. In the browser you paste the token string.

## Pages

- **Overview** — per PHP version: requests/min, error rate, avg/p95/max
  duration, CPU time, memory, DB queries/min, HTTP calls/min, N+1 count.
  This is the "always-on" summary layer; it never samples.
- **Sites** — list of docroots/hostnames with the same metrics per site;
  click through for the per-site drill-down.
- **Traces** — the `normal`/`full`/`alert` records, filterable by level,
  duration, URI prefix, site. Each trace shows:
  - the timeline: request duration split into PHP, DB, HTTP, errors
  - the SQL list with fingerprints, durations and N+1 grouping
  - outbound HTTP calls (target, status, time)
  - PHP errors captured during the request
  - **components** (per-plugin / per-function breakdown) when the request
    was profiled — inclusive vs self time and call counts
- **Alerts** — `alert`-level traces, crash alerts (crash_threshold/window)
  and 4xx/5xx spikes; each alert links to the traces around it.

## Reading a profile

In the components table: `incl` is time including children, `self` is time
in the function/plugin itself. Sorted by `self` by default — that is where
the wall time actually goes. WooCommerce, theme, plugin names appear as
their canonical component path (e.g. `plugins/woocommerce`).

## What the dashboard does not do

No cross-server view, no long-term storage beyond `retention`, no alert
channels beyond local — that is what PHPRay Cloud is for
([overview](../cloud/overview.md)).
