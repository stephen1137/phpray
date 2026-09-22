# PHPRay

PHPRay is always-on request tracing for PHP: every request gets a lightweight
record of its timing, SQL queries, outbound HTTP calls, PHP errors and N+1
query patterns, at an overhead low enough to leave on in production
(see [Overhead](#overhead) for what has been measured so far).

On top of the always-on summary, PHPRay runs sampled per-plugin and
per-component breakdowns, so you can see which plugin, theme or function
eats the request time without profiling every request.

PHPRay works inside PHP itself — there is no root-level agent, sidecar or
JVM. It is a PHP extension plus a small on-box collector and a local
dashboard, and it runs on shared hosting (CageFS / lsphp), dedicated VPS
boxes and containers alike.

The core is free: extension, collector and local dashboard under an open
license. The optional PHPRay Cloud fleet console adds cross-server dashboards,
plans and alerts, starting at 19 USD per month.

## Overhead

The always-on layer hooks request-level events only (request start/end,
SQL, outbound HTTP, file I/O, PHP errors) and never sees individual function
calls. Per-function profiling uses PHP's observer API and runs only for
sampled requests (default 3 %); the other 97 % take no function-level
samples at all.

Our internal benchmark (WooCommerce store, 22 plugins, PHP 8.3, OPcache) puts
the always-on layer within the measurement noise of the bare-PHP baseline,
and profiling of *every* request measurably above it, yet far below what a
classic `execute_ex` hook costs.
Those runs were made on a laptop, so we do not quote them as product
figures: certified numbers, the methodology and the raw data will be
published once the suite has run on a dedicated, quiet host. Until then
treat any precise overhead figure you see quoted for PHPRay as unverified.
