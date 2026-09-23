# Changelog

## 0.15.10 — 2026-09-23

**A published report said `the site/checkout/`.** The scrubber replaces the
domain with the words "the site", which reads correctly in a sentence and
wrongly in an address: evidence lines quote URLs, so the slowest path came
out as `the site/checkout/: 104 N+1 requests`. Found on a real published
report, not in a test. A name followed by a path is now dropped entirely,
with or without scheme and `www.`, leaving `/checkout/`. The behaviour has
its own test, which the function did not have before.

**The collector spoke Polish.** `phpray-collector update` printed its whole
conversation — what it downloaded, whether the checksum matched, what to
restart afterwards — in Polish, and so did the message after `report
--share`. Both are on the path a first-time user walks, and buyers are
outside Poland. All of it is English now; the code comments stay Polish.

After a report is published, the last line says that sites can be kept in
one list, with a link to the plans. It is the one place where the tool
mentions the paid console, it appears once, after something worked, and it
is read by the person who has just sent someone else's report on.

## 0.15.9 — 2026-09-22

**A report you can send as a link, not a file.** People share links; an
attachment has to be saved, found and forwarded. Every published report is a
page carrying our name, read by someone who has just learned their site is
slow — and none of it costs anything per reader.

- `phpray-collector report --domain example.com --share` publishes the
  findings and prints a link. No account, no card: the local core is
  Apache-2.0 and this works from it.
- The local dashboard has a **Share a link** button next to Export Report,
  which copies the URL straight to the clipboard.
- What gets sent is **data**, not a document: the server renders it with its
  own template, so nothing in a report can become markup on someone else's
  screen. The published page is anonymised, expires after 30 days, is not
  indexed, and its address carries 128 bits of randomness.
- The site name is stripped from **every text field**, not just the one named
  after it. The first version removed the field and published
  `example.com: 1 critical issue(s) found` in the summary — found on
  production, the document was deleted within minutes and the scrubbing is
  now a package-level function used by both report paths, with a test that
  walks every field.

**The extension no longer wedges its own ring buffer.** 0.15.8 fixed the
reader; this fixes the writer. The reader's repair needs an updated
collector, so a ring could still be dead where only the extension was new.
Free space is computed through one guarded helper now, at all three places
that used to subtract positions directly — the third was found only by
listing every occurrence, after fixing the first two.

The extension version moves to 0.15.9 because its source changed; it had
stayed at 0.15.5 through three releases that did not touch it.

## 0.15.8 — 2026-09-22

**A wedged ring buffer silently discarded every trace, forever.** This is the
most damaging bug we have shipped: on our own shared host, 24,654 of 80,501
traces were lost — **23.4%** — and one account was losing **90%** of its
traffic. The product's central claim is "every request, not a sample", and on
that machine it was false.

The mechanism: the extension decides whether a record fits by computing
`write_pos - read_pos` on a `uint64`. If the read position ever gets *ahead*
of the write position, that subtraction wraps to roughly 10^19, the writer
concludes the buffer is full and drops **every** record from then on. Nothing
recovers it — the ring stays dead until the file is removed.

The reader could move ahead of the writer at five separate places: skipping a
gap at the end of the buffer, abandoning a slot whose writer died between
reserving and filling it, skipping a record whose length was corrupt, and
stepping over a padding record. None of them checked that the new position
stayed behind the writer.

- Every advance is now clamped so the read position can never pass the write
  position.
- If a ring is found already inverted, the reader **repairs it** by levelling
  the positions and logs it. That matters: it means existing wedged rings
  recover as soon as the collector is updated, without touching the extension
  or restarting anyone's PHP.
- `Stats()` no longer reports fill percentages like `109952421083179.7%`,
  which is what made this look cosmetic for a day.

Found by updating our own server and actually reading its log — the first time
we had looked at what was running there.

The extension is unchanged in this release. Hardening the writer's own
arithmetic against the same inversion is still to do; the reader-side repair
is what makes the fix deployable today.

## 0.15.7 — 2026-09-22

**The collector can now update itself, on demand.** Until this release there
was no update channel for the server side at all: the collector had no way to
learn that a newer release existed and no way to fetch it. The proof that this
mattered was on our own infrastructure — the shared host we run PHPRay on was
still on 0.15.4 while 0.15.6 had been out for a day, and nothing anywhere
would have said so.

- `phpray-collector update` downloads the release for this architecture,
  **verifies its SHA-256 against the published SHA256SUMS before replacing
  anything**, keeps the previous binary next to the new one as
  `phpray-collector.poprzedni`, and then tells you to restart the service.
- `phpray-collector update --check` only reports, downloads nothing.
- No background self-update and no telemetry. It fetches two static files by
  GET and does nothing until you type the command. A tool that watches other
  people's production has no business replacing its own binary unasked.
- If the binary's directory is not writable, it says so **before** downloading
  twelve megabytes, and prints the exact `sudo` command for your install path.

The PHP extension still updates separately — it is a different file per PHP
version and needs PHP-FPM reloaded — and `update` says so explicitly rather
than leaving you to find out.

**The extension is unchanged in this release**, so the `.so` files still
report `0.15.5`, deliberately: a version number describes the artifact, and
this artifact did not change.

## 0.15.6 — 2026-09-22

**The extension is unchanged in this release.** Its source has not moved since
0.15.5, so the `.so` files still report `0.15.5` and that is deliberate: a
version number describes the artifact, and this artifact did not change. The
collector and the MCP server are at 0.15.6.

Everything here came from walking the new-user path from zero in a clean
container, the way a stranger does it. Real people started downloading the
installer this week, so these are the first things they meet.

### Fixed — "start the collector manually" was a dead end

Without systemd (containers, WSL, macOS, some shared hosts) the installer said
`systemctl not available - start the collector manually` and stopped there.
`phpray-collector` prints the usage screen; `phpray-collector daemon` starts but
serves no dashboard. The command that actually works —
`phpray-collector serve -c /etc/phpray/collector.toml -addr 127.0.0.1:9191` —
lived only inside the systemd unit, where someone without systemd never looks.

The installer now prints that command, and so does `phpray top` when the
collector is not answering.

### Fixed — a fresh install greeted you with five errors

The collector logged `JSONL error: no such file or directory` every five
seconds on a new machine. The file was missing because PHP had not served a
request yet — a state that cannot be avoided and is not a failure. Someone who
had just installed an unfamiliar tool saw five errors in the first twenty-five
seconds.

It now says once, calmly, that it is waiting and what to do about it. `error`
is reserved for things that are actually wrong.

### Fixed — the only upgrade link in the free product pointed at Polish

The local dashboard is entirely in English, and its "Connect to PHPRay Cloud"
button linked to the Polish page. It now links to `/en/cloud`.

### Fixed — the MCP binary reported the wrong version

`phpray-mcp` in the 0.15.5 downloads identified itself as 0.15.4: its version
constant was bumped after the release artifacts were built.

### Added — the MCP server also speaks HTTP

`phpray-mcp` now serves the Model Context Protocol over HTTP as well as stdio,
which is what runs the hosted endpoint at `https://phpray.dev/mcp`. The token
comes from the `Authorization` header of each request, so one process serves
many users and stores no credentials.

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
