# DirectAdmin

The PHPRay plugin for DirectAdmin gives every account on the server its own
performance view inside the panel, and the administrator a server-wide
overview. It is the panel half of [Host Edition](../cloud/overview.md); the
tracing itself comes from the extension and the collector, which you install
first ([shared hosting](../install/shared-hosting.md)).

**Status: 0.1.2, early.** It has been run on a production CloudLinux server
with DirectAdmin and PHP 7.4 in the panel, against collector 0.15.2, and both
views render real data. The per-account switch was exercised on that server:
off removes the extension from `php -m`, on puts it back, and the script refuses
any account DirectAdmin does not own. It has not yet been through a full
customer rollout, so treat it as a pilot: enable it for your own account first.

## What the customer sees

*Site performance* for each of their domains:

- requests, errors, p95 latency and N+1 requests for the last 24 hours and 7 days,
- the slowest pages with request counts and p95,
- slow SQL fingerprints, plugin and theme breakdown, recent errors,
- a plain-language diagnosis with a health score,
- a switch to turn tracing off for that site, and "profile this URL prefix for
  N minutes" — both written to the account's `.user.ini`, nothing else,
- on CloudLinux with CageFS, a switch that turns **PHPRay itself** on or off for
  the whole account: it writes the extension's ini into the account's own PHP
  scan directory inside its cage and recycles that account's PHP workers. An
  account where the extension is not loaded sees a card saying so with a button,
  instead of pages of zeros.

## What the administrator sees

Collector health and version, a 24-hour overview of the whole server, top
domains by p95 and by request count, top users, active alerts, server-wide
slow queries and the PHP versions in use. The admin view is read-only.

## Requirements

- DirectAdmin with the plugin manager (the panel runs the pages under its own
  `/usr/local/bin/php`, currently PHP 7.4 — the plugin targets that),
- `phpray-collector` **0.15 or newer** running on the same server, with a
  non-empty `[auth] secret` in `/etc/phpray/collector.toml` (the plugin signs
  short-lived JWTs with it and scopes each customer to their own domains),
- the extension installed for the PHP versions your accounts use, with tracing
  off by default and opt-in per account (`install.sh` does this on CloudLinux:
  see [shared hosting](../install/shared-hosting.md)).

## Install

```bash
cd /usr/local/directadmin/plugins
git clone <plugin repository> phpray        # or upload the archive in the plugin manager
/usr/local/directadmin/plugins/phpray/scripts/install.sh
```

The script sets the file modes and owners, creates `conf/phpray.conf` from the
example, copies the collector's `[auth] secret` into it, and installs a
`/etc/sudoers.d/phpray` rule that lets **only** `diradmin` run the one script
that edits a customer's `.user.ini` (validated with `visudo` before it is
installed). `scripts/uninstall.sh` reverses all of it.

Check `conf/phpray.conf` afterwards:

```ini
collector_url=http://127.0.0.1:9191
jwt_secret=<the collector's [auth] secret>
profile_minutes=10
```

Without `jwt_secret` the customer view says so instead of showing empty cards.

## Notes from the first production run

- The panel executes the pages with DirectAdmin's own PHP (7.4 today), not with
  the account's PHP version. Keep that in mind when changing the plugin code.
- The collector API takes its time window in minutes (`window=1440`), not hours.
- Long-lived PHP workers keep the extension they loaded at start: after
  installing or upgrading the extension, recycle that account's workers instead
  of restarting the whole web server.
- Enable the extension per account, never for a whole PHP version. A worker
  that loads `phpray.so` installs its hooks at module startup regardless of
  `phpray.enabled`; use `phpray.master_switch=0` for anything server-wide. See
  [shared hosting](../install/shared-hosting.md#what-went-wrong-on-h1).
- `phpray.enabled` in `.user.ini` is not a way to install or remove the
  extension: PHP reads per-directory values long after module startup. The
  per-account switch writes the ini into
  `/var/cagefs/<n>/<account>/etc/cl.php.d/alt-php<NN>/phpray.ini`, which the
  cage mounts at `/opt/alt/php<NN>/link/conf/`, and then recycles that
  account's `lsphp` workers. PHP Selector does not manage it: the selector only
  knows CloudLinux's own packaged extensions.
