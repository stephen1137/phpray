# Quick start

One command installs the extension, the collector and the local dashboard on
a Linux box with root access (Debian/Ubuntu/RHEL-family):

```bash
curl -fsSL https://phpray.dev/install.sh | sudo bash
```

## What the script does

1. Detects PHP version, libc (glibc/musl) and architecture.
2. Downloads the matching prebuilt `phpray.so` from `https://phpray.dev/dl/`
   (checked against `SHA256SUMS`) into the `extension_dir` of every PHP
   version found (system PHP, Debian `php8.x`, Remi, cPanel EA, Plesk,
   LiteSpeed lsphp, DirectAdmin, CloudLinux alt-php).
3. Writes the default INI snippet (enabled, smart mode) as `zz-phpray.ini`
   into the scan directory of every PHP found — on Debian/Ubuntu into every
   SAPI (`/etc/php/<ver>/{cli,fpm,apache2}/conf.d/`).
4. Installs `phpray-collector` (a `.deb`/`.rpm` where possible, otherwise the
   static binary) with the `phpray-collector.service` systemd unit, and the
   `phpray` command alias.
5. Starts the collector with the local dashboard on `127.0.0.1:9191`
   (`--listen 0.0.0.0:9191` binds it to a public address and turns on token
   login; the token is printed once and kept in `/etc/phpray/dashboard-token`).
6. Reloads PHP-FPM gracefully so the extension loads (Apache/LiteSpeed are
   restarted only with `--yes`), then prints the next steps.

## Verify the install

```bash
php -m | grep phpray     # extension loaded
phpray status            # per-PHP extension state, collector, dashboard, cloud
```

`phpray status` lists every PHP binary and whether it loads `phpray.so`, whether
the collector config, ring buffer, JSONL file and database exist, whether the
dashboard API answers, and the state of the Cloud connection.

## Uninstall

```bash
curl -fsSL https://phpray.dev/install.sh | sudo bash -s -- --uninstall          # keeps /etc/phpray and /var/lib/phpray
curl -fsSL https://phpray.dev/install.sh | sudo bash -s -- --uninstall --purge  # deletes config, data and buffers too
```

or manually: stop `phpray-collector`, remove `zz-phpray.ini` from the
`conf.d` directories, delete `phpray.so` from `extension_dir`, remove the
systemd unit and `/var/lib/phpray`. Reload PHP-FPM / restart Apache afterwards.
