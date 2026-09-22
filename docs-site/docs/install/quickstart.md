# Quick start

One script installs the extension, the collector and the local dashboard on
a Linux box with root access (Debian/Ubuntu/RHEL-family). Download it, read it,
then run it — it asks for root, so it deserves a look first:

```bash
curl -fsSL https://phpray.dev/install.sh -o phpray-install.sh
less phpray-install.sh
sudo bash phpray-install.sh
```

Piping a remote script straight into `sudo bash` also works and you will see it
in other people's instructions, but then you are running code you have not
seen, fetched over a connection you cannot inspect afterwards. The three lines
above cost a few seconds and leave the script on disk, so you can check later
what actually ran.

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
sudo bash phpray-install.sh --uninstall          # keeps /etc/phpray and /var/lib/phpray
sudo bash phpray-install.sh --uninstall --purge  # deletes config, data and buffers too
```

If you no longer have the script, download it the same way as above.

or manually: stop `phpray-collector`, remove `zz-phpray.ini` from the
`conf.d` directories, delete `phpray.so` from `extension_dir`, remove the
systemd unit and `/var/lib/phpray`. Reload PHP-FPM / restart Apache afterwards.
