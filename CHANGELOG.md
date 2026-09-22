# Changelog

## 0.15.5 — 2026-09-22

**The extension changes in this release** (0.15.3 → 0.15.5) and every variant was
rebuilt: 24 `.so` files, PHP 8.0–8.5 × glibc/musl × amd64/arm64. The glibc builds
still cap at `GLIBC_2.17` so they load on CloudLinux 8 and AlmaLinux 8.

### Fixed — data integrity in the ring buffer

The writer reserved space with a CAS on `write_pos` and only then copied the
record. From the moment the CAS landed the reader considered the slot ready,
while the bytes in it were still the tail of a record from the previous lap of
the buffer — so it could read that as a new trace. On a production server this
put a row into the database whose `host` column held a fragment of SQL, with
status `28521` and a duration of 8 529 657 644 052 ms.

The publication order is now `RESERVED → length → payload → real type`, with the
last store carrying a release barrier. The reader only accepts a record with a
non-zero length and a type other than `RESERVED`, and it zeroes the header after
consuming, so a freshly reserved slot always reads as length zero. If a PHP
process dies between reserving and writing, the reader abandons that slot after
two seconds instead of stalling the ring for good.

The byte layout does not change, so the ring version stays 5: bumping it would
stop collectors that customers already have from reading the ring at all.

A second line of defence rejects traces that cannot come from a real request —
an impossible HTTP status, a duration over a day, a timestamp from before 2020.

### Fixed — first five minutes after install

- `phpray top` asks the API for a window two minutes wider than the one it
  displays and trims to the second itself. The API counts whole minutes, so
  `window=1` could exclude the current, incomplete minute — which is exactly
  where the newest requests are.
- The installer's "Next steps" said `phpray-collector top` while the README and
  the site say `phpray top`. Both work; now they read the same.
- The local dashboard's "Top Components" card rendered a bare table header when
  the component list was empty, so the flagship feature looked broken. It now
  explains why there is no data and what to switch on.
- The dashboard no longer requests a missing `/favicon.ico`, and the token field
  sits in a form with a hidden username field, so the browser console is clean.

### Changed — MCP output for agents

`phpray_components` reported sums as raw milliseconds (`2528110`). That is
correct and unreadable: the answer goes to a language model, which will read it
out as "two and a half million milliseconds". Sums are now `42.1 min`, `1.1 h`,
`3.5 s`, and the column names say which figure is a total and which is per
request.

## 0.15.4 — 2026-09-21

The extension is unchanged in this release and still reports **0.15.3**; only the
collector, the CLI and the panel plugin moved. Components are versioned
independently when their code does not change, so nobody has to rebuild PHP
workers for a CLI feature.

### Added

- **`phpray report -format html`** — one self-contained file you can send to
  someone else. No external requests, no fonts, no scripts: it opens from an
  email attachment on a machine with no network. Light and dark, and a print
  stylesheet. It carries the health score, what was found, what each finding
  costs and what to do about it.
- **`-anonymize`** replaces the domain name everywhere — in the title, the
  summary, the findings and the evidence — so the same report can go on a forum
  or into a proposal without naming the customer. Verified on a production
  account: zero occurrences left.
- **`-o <file>`** writes the report to a file instead of stdout.
- **DirectAdmin plugin 0.1.3**: a *Download report* button on the customer page.
  The report is generated as root, because the collector reads a database the
  customer cannot open, and the domain is checked against the account's
  `domains.list` first — asking for someone else's domain is refused.

## 0.15.3 — 2026-09-21

### Fixed

- **A failed request is no longer sampled away.** Traces were shipped to the
  cloud on a uniform sample, so with the default 3 % a site's only error of the
  day survived one time in thirty. One of our own sites showed 29,561 requests,
  an error rate of 0.003 % in the aggregate, and an empty Errors tab, because no
  trace of that one request had ever left the machine. Responses of 5xx, traces
  the collector already marked `alert`, and fatal-class PHP errors (`E_ERROR`,
  `E_PARSE`, `E_CORE_ERROR`, `E_COMPILE_ERROR`, `E_USER_ERROR`,
  `E_RECOVERABLE_ERROR`) now bypass sampling. Ordinary `E_WARNING` and
  `E_NOTICE` deliberately do not: an older WordPress emits twenty per request,
  and treating those as exceptional would ship everything and burn the plan.
- **`phpray top` read a file that production installs never write.** Its default
  source was `/tmp/phpray.jsonl` while every real install writes to the shared
  memory ring, so the command showed an empty list on any normal server and said
  nothing about why. It now queries the collector's API, signing a one-minute
  admin token from the config, and falls back to a file with `-f`. When the
  collector is unreachable it says so with three concrete next steps instead of
  clearing the screen; when the window is genuinely quiet it says that too.
  Column `LEVEL` and the `N+1` flag were also always blank — the API returns
  `level` and `n1`, not `trace_level` and `n_plus_one`.

### Changed

- Documentation now describes how to run PHPRay on shared hosting **without
  root**, verified end to end as an unprivileged user: `.user.ini` cannot load
  an extension, but the `php.ini` most hosts let you edit is system level for
  your own processes and can point `extension=` at a `.so` in your home
  directory. The four cases where this fails are listed with it.

## 0.15.2 — 2026-09-21

### Fixed

- **A busy server could never catch up after a break in cloud connectivity.**
  The console counts requests per minute; the collector replayed its buffer one
  stored file per request, and the default 5-second flush already used the whole
  budget, so a backlog only grew. Our own production host sat on 122 buffered
  files and a permanent 429. Trace batches are now merged on replay, up to the
  console's 500 records / 4 MB per request, and the console's per-minute budget
  leaves headroom above the steady-state flush rate. On the same host the
  backlog of 130 files drained to zero in about 90 seconds with nothing dropped.
- The public console demo landed on an almost empty fleet view; it now opens the
  site it is meant to show.

### Added

- Prebuilt extension binaries for **PHP 8.0** on all four platform variants
  (glibc/musl × amd64/arm64), so the matrix now covers PHP 8.0 through 8.5.

## 0.15.1 — 2026-09-21

**PHP 8.0 is supported again.** The function profiler carried a guard saying it
needed PHP 8.1 or newer. That was wrong: PHP 8.0 has the same
`zend_observer_fcall_register` and the same
`zend_get_op_array_extension_handle(const char *)`, and the extension builds,
loads and traces on it. The guard now sits at 8.0, where the observer genuinely
begins. On one of our own shared servers that is 17 more accounts.

**New: an MCP server.** `phpray-mcp` lets a coding agent ask what is slow.
It speaks the Model Context Protocol over stdio and reads the Cloud console
with your own console token, so the agent sees exactly the sites that token is
scoped to. Eleven tools: sites, overview, slowest pages, slow SQL, component
breakdown, errors, traces, one trace in full, before/after comparison, alerts,
and turning profiling on for a URL prefix. Only the last one changes anything.

```bash
curl -fsSL -o phpray-mcp https://phpray.dev/dl/v0.15.1/phpray-mcp-linux-amd64
chmod +x phpray-mcp && sudo mv phpray-mcp /usr/local/bin/
claude mcp add phpray --env PHPRAY_TOKEN=<console token> -- phpray-mcp
```

It runs in two modes. **Cloud** reads the console with your token and sees every
server you own. **Local** reads the collector on the same machine and needs no
account, which makes it useful for anyone running only the open-source half.
Local mode adds `phpray_diagnose`, the collector's own plain-language read of
what is wrong with a domain, findings and suggested fixes included.

Responses are summarised rather than passed through: the console answers with
48 KB for a site list and 136 KB for one overview, which would eat an agent's
context window, and a JSON blob cut in half is no longer JSON. The server turns
those into 10 KB and 2 KB of readable tables. Builds for linux and macOS, amd64
and arm64, are in the release directory with checksums.


**Fixes a real incident.** On a production shared-hosting server, installing the
extension for one PHP version with `phpray.enabled=0` took a customer's shop
offline for 31 minutes. `phpray.enabled` is `PHP_INI_PERDIR` and is only read
per request, so module startup still installed the function observer, replaced
the error handler and wrapped SQL, cURL and file calls in **every** worker of
that PHP version, including accounts that never asked for PHPRay.

- **New: `phpray.master_switch`** (`PHP_INI_SYSTEM`, default `1`). With `0`,
  module startup returns immediately: no observer, no handler replacement, no
  signal handlers, no shared memory. It is the only setting that makes the
  extension genuinely inert in a process.
- Redis hooks are no longer installed on requests where the switch is off; they
  used to be installed before `phpray.enabled` was even checked.
- `phpinfo()` shows the switch state.
- **Installer:** on CloudLinux with CageFS the ini is catalogued for the PHP
  Selector and is **not** linked into the server-wide scan directory. Enable it
  per account with
  `selectorctl --enable-extensions=phpray --user=<account> --version=<v>`.
- Two `.phpt` tests pin the behaviour in both directions.
- Shared-hosting documentation rewritten, including what went wrong.

**Measured overhead.** On a WordPress page rendering in 48 ms, the always-on
layer could not be separated from a build without the extension across five
interleaved rounds. With the function observer on — today's default — the page
took 50 ms. The cost comes from the observer alone and does not fall when you
lower the sample rate, because PHP switches every user function call onto the
observed path as soon as any observer is registered. Method and raw numbers:
`docs/BENCHMARK-WORDPRESS-2026-09-20.md`.

**Build.** Alpine images for arm64 ship without a package index, so every musl
arm64 build failed with "no such package" for the compiler. The build script now
refreshes the index first.

## 0.15.0 — 2026-09-20

- Per-user ring buffer (`%u` / `%g` in `phpray.shm_path`, mode 0600) so accounts
  on a shared host cannot read each other's traces.
- CloudLinux / CageFS support in the installer: one shared directory mounted
  into every cage.
- Collector: p95 for a domain and per URI (previously always 0).
- systemd unit revision 3: `/run/phpray` in `ReadWritePaths`, and
  `RuntimeDirectoryPreserve=yes` so stopping the service no longer leaves cages
  holding a deleted inode.
