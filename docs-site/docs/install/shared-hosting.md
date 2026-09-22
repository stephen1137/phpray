# Shared hosting

Two things are true at once, and the useful one is usually missed:
`.user.ini` **cannot** load an extension, because `extension=` is
`PHP_INI_SYSTEM` and per-directory files are read far too late — but on most
shared hosts you also get **your own `php.ini`**, and that one *is* system
level for your own PHP processes. Where that is the case you can run PHPRay
yourself, without root and without asking anybody.

## Without root, on your own account

Verified end to end as an ordinary user (uid 1000, no sudo anywhere): the
extension loads from a home directory, writes traces to a file in that home
directory, and the collector reads them as the same user.

```bash
mkdir -p ~/lib ~/phpray-data
# pick the build that matches your PHP: version, glibc or musl, amd64 or arm64
curl -fsSL -o ~/lib/phpray.so \
  "https://phpray.dev/dl/$(curl -fsS https://phpray.dev/dl/LATEST)/phpray-8.3-glibc-amd64.so"
```

Then add this to the `php.ini` your host lets you edit — in cPanel it is
*MultiPHP INI Editor*, in DirectAdmin with CloudLinux it is *Select PHP
Version → Options*, in Plesk *PHP Settings*, and on some hosts it is simply a
`php.ini` in your home directory:

```ini
extension=/home/YOURUSER/lib/phpray.so
phpray.master_switch=1
phpray.enabled=1
phpray.output_mode=file
phpray.output_path=/home/YOURUSER/phpray-data/traces.jsonl
phpray.mode=smart
```

Check it with a `phpinfo()` page, or over SSH if you have it:

```bash
php -r 'echo extension_loaded("phpray") ? phpversion("phpray") : "not loaded", PHP_EOL;'
```

Then read your own traces, again without root:

```bash
curl -fsSL -o ~/phpray "https://phpray.dev/dl/$(curl -fsS https://phpray.dev/dl/LATEST)/phpray-collector-linux-amd64"
chmod +x ~/phpray
~/phpray top -f ~/phpray-data/traces.jsonl
```

### When this will not work

- **`open_basedir` excludes your library directory.** Some hosts confine PHP
  to `public_html`; then PHP cannot read `~/lib/phpray.so`. Putting it inside
  the allowed tree usually solves it.
- **The host does not give you a writable `php.ini`**, or strips `extension=`
  from it. Nothing you can do from the account side — ask them, or use the
  WordPress plugin below.
- **The build does not match your PHP.** Version, thread safety (you need the
  NTS builds we publish) and libc all have to line up. A mismatch makes PHP
  refuse the module at startup; the error names the API number it expected.
- **The host runs each request in a container without your home mounted**
  (rare on classic shared hosting, common on PaaS).

## The other two routes

- **Ask the host to enable PHPRay fleet-wide.** One line in a support request:
  "please install the PHPRay PHP extension for my account (see
  phpray.dev/docs/install/shared-hosting)". If they have it installed already,
  you only need `php_value phpray.enabled=1` in `.user.ini`.
- **Use the WordPress plugin instead.** It works on any shared host with no
  extension at all: it hooks into WordPress (plugins, themes, queries, HTTP
  API, errors) and ships the same trace format to the collector or to PHPRay
  Cloud. See [WordPress plugin](../usage/wordpress-plugin.md).
- **Know what the plugin cannot see.** The plugin is a userland library: it
  cannot see raw request timing around the PHP core, C-extension internals,
  memory peaks outside its sampling, or non-WordPress sites on the account.
  The extension sees all of those; the plugin sees the application layer.

## For hosts

### The one rule

**Load the extension only into the PHP processes of accounts that opted in.**

`phpray.enabled` is `PHP_INI_PERDIR`: it is evaluated per request, so it cannot
stop module startup from touching the process. A worker that loads `phpray.so`
installs the function observer, the error handler and the SQL, cURL, file and
Redis wrappers before any `.user.ini` is read. On a shared server that means a
site which never asked for PHPRay still gets a modified runtime, next to
whatever else that account loads (ionCube, Snuffleupagus, phpiredis, an old
mysqlnd plugin). We learned this the hard way: see
[the h1 incident](#what-went-wrong-on-h1).

Since 0.15.1 the extension also has a process-level guard:

```ini
phpray.master_switch = 0   ; PHP_INI_SYSTEM: the module loads and installs nothing
```

With `master_switch=0` module startup returns immediately — no observer, no
handler replacement, no signal handlers, no shared memory. It is the safe
default for anything server-wide.

### DirectAdmin / CloudLinux with PHP Selector

`install.sh` detects CageFS and puts the ini in the Selector's catalogue
(`/opt/alt/php<NN>/etc/php.d.all/phpray.ini`) **without** linking it into the
server-wide scan directory. Enable it one account at a time:

```bash
selectorctl --enable-extensions=phpray --user=<account> --version=8.2
# the account then turns tracing on itself:
#   php_value phpray.enabled 1     (.htaccess)   or   phpray.enabled=1 (.user.ini)
```

Recycle only that account's workers afterwards (`warp-phpd`/lsphp keep the
extensions they loaded at start). Never restart the whole web server to pick up
an extension change: every account's pool would reload at once.

### cPanel / EA-PHP

The EA-PHP module directory is shared by all users of that PHP version, so a
file in `/opt/cpanel/ea-php<ver>/etc/php.d/` arms everyone. Put
`phpray.master_switch=0` in it and raise the switch per account with a
`PHPRC`/pool ini for that user only.

### Single-tenant servers

One site per server is the case the plain install is built for: the ini goes
into the scan directory, `master_switch` stays at its default of 1 and
`phpray.enabled` decides.

## Per-user control

Users toggle tracing in their `~/.user.ini` (picked up by FPM and lsphp;
mod_php needs `php_value` in `.htaccess`):

```ini
phpray.enabled=1
phpray.mode=smart
```

`phpray.master_switch` is `PHP_INI_SYSTEM` on purpose: a customer can turn
tracing off but cannot arm a process the host did not arm.

## What went wrong on h1

On 2026-09-20 we installed 0.15.0 for PHP 8.2 on a production CloudLinux server
with 659 accounts, with `phpray.enabled=0` server-wide, believing that made it
inert. It did not. As pods recycled, more and more workers came up with the
hooks installed, and one shop — PHP 8.2 with ionCube Loader, phpiredis, APCu,
imagick and opcache — started losing workers mid-request. MariaDB logged 567
aborted connections for that account in one hour, the web server's circuit
breaker opened and the site served errors for about half an hour. No other
account was affected, and rolling the pilot back ended it.

Two things came out of it: the `master_switch` guard above, and the installer no
longer arming a whole PHP version at once.
