# Trace format

PHPRay traces are **JSONL** (one JSON object per line), produced by the
extension and consumed by the collector, the dashboard, the CLI and —
unchanged — by PHPRay Cloud and the WordPress plugin.

> **Note**
> The WordPress plugin ships **the exact same record format**
> ([plugin docs](wordpress-plugin.md)). Anything that consumes PHPRay
> traces from the extension also works with plugin data, and vice versa.

## Record

```json
{
  "v": 1,
  "ts": 1789737541123,
  "host": "shop.example",
  "docroot": "/home/shop/public_html",
  "uri": "/checkout/",
  "level": "full",
  "mode": "smart",
  "php": "8.3.33",
  "request": {
    "duration_ms": 3120.5,
    "cpu_ms": 912.3,
    "memory_peak_mb": 128.0,
    "status": 500
  },
  "db": {
    "queries": 184,
    "ms": 1820.4,
    "rows": 41200,
    "n1": [
      {"sql": "SELECT * FROM wp_posts WHERE ID IN (?)", "count": 47, "ms": 612.0}
    ]
  },
  "http": [
    {"target": "api.stripe.com", "method": "POST", "status": 200, "ms": 210.0}
  ],
  "errors": [
    {"type": "Error", "msg": "Call to undefined method ...", "file": "wp-content/plugins/x/inc/order.php", "line": 118}
  ],
  "components": [
    {"name": "plugins/woocommerce", "incl_ms": 1201.5, "self_ms": 640.1, "calls": 18800},
    {"name": "theme/storefront", "incl_ms": 210.0, "self_ms": 98.4, "calls": 310}
  ],
  "backtrace": [
    {"fn": "WC_Checkout::process_order", "file": "wp-content/plugins/woocommerce/includes/class-wc-checkout.php", "line": 1430, "self_ms": 640.1}
  ]
}
```

## Fields

| Field | Meaning |
|---|---|
| `v` | format version (currently `1`) |
| `ts` | unix ms, request start |
| `host`, `docroot`, `uri` | site identity; `uri` has no query string (see privacy, [ingest](../cloud/ingest.md) §5) |
| `level` | `summary` / `normal` / `full` / `alert` |
| `mode` | `smart` / `all` / `manual` that produced it |
| `php` | SAPI + version |
| `request` | wall/cpu/memory/HTTP status |
| `db` | totals + `n1` grouped N+1 query fingerprints (SQL as fingerprint, never literals) |
| `http[]` | outbound calls: target, method, status, time |
| `errors[]` | PHP errors captured in-request; messages truncated, paths abbreviated |
| `components[]` | per-plugin/component incl vs self ms, calls — only present when this request was profiled |
| `backtrace[]` | per-function samples — only in profiled (`full`/`alert` with sampling on) requests |

## Privacy

SQL is stored as a fingerprint (numbers and quoted strings replaced),
backtrace paths are abbreviated from `wp-content/` or `vendor/`, no
absolute home paths, no visitor IPs, no request/response bodies. The same
defaults apply to what gets shipped to PHPRay Cloud
([ingest protocol §5](../cloud/ingest.md)).
