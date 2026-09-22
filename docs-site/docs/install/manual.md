# Manual install

For boxes where the installer script is not wanted: everything is available
as plain artifacts at `https://phpray.dev/dl/`. The current tag is in
`https://phpray.dev/dl/LATEST`; every file is listed with its checksum in
`SHA256SUMS` next to it. GitHub Releases will mirror the same files once the
repository is public.

## 1. Prebuilt extension

Every release ships a prebuilt `phpray.so` per combination of:

- PHP version (8.1, 8.2, 8.3, 8.4 or 8.5; NTS builds only)
- libc (glibc, musl)
- architecture (amd64, arm64)

Example: `phpray-8.3-glibc-amd64.so` from `https://phpray.dev/dl/v0.14.0/`.

Place the `.so` into your `extension_dir` (see `php -i | grep extension_dir`)
and add an ini snippet, e.g. `/etc/php/8.3/fpm/conf.d/zz-phpray.ini` (on
Debian/Ubuntu repeat it for `cli/` and `apache2/`):

```ini
extension=phpray.so
phpray.enabled=1
phpray.mode=smart
```

Then reload the PHP workers (`systemctl reload php8.3-fpm`).

## 2. Collector

The collector is in the same release as a `.deb`, an `.rpm` (both amd64) and
a static binary (`phpray-collector-linux-amd64`). The packages install
`/usr/bin/phpray-collector`, the `phpray` alias, `/etc/phpray/collector.toml`
and the `phpray-collector.service` unit:

```bash
sudo dpkg -i phpray-collector_0.14.0-5_amd64.deb      # Debian / Ubuntu
sudo rpm -U phpray-collector-0.14.0-5.x86_64.rpm      # RHEL family
sudo systemctl enable --now phpray-collector
```

With the static binary, copy it to `/usr/bin/phpray-collector`, create
`/etc/phpray/collector.toml` (see [Collector](../configuration/collector.md))
and this unit:

```ini
[Unit]
Description=PHPRay Collector
After=network.target

[Service]
ExecStart=/usr/bin/phpray-collector serve -c /etc/phpray/collector.toml -addr 127.0.0.1:9191
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5
StateDirectory=phpray
LogsDirectory=phpray
PrivateTmp=no
ReadWritePaths=/var/lib/phpray /dev/shm -/tmp/phpray.jsonl

[Install]
WantedBy=multi-user.target
```

`serve` runs the collector, the REST API and the local dashboard in one
process; `-addr` chooses the listen address (token login on a public address:
[Dashboard](../usage/dashboard.md)).

## 3. Containers

No collector image is published yet. In your own images:

- the PHP image gets the `.so` matching its PHP version and libc (`musl` for
  `php:*-alpine`) plus the ini snippet above;
- the collector container runs the static binary:
  `phpray-collector serve -c /etc/phpray/collector.toml -addr 0.0.0.0:9191`;
- both containers share the shared-memory path (`phpray.shm_path`, default
  `/dev/shm/phpray`), e.g. one volume mounted at `/dev/shm/phpray` in both,
  and the collector keeps its database on a persistent volume at
  `/var/lib/phpray`.

## 4. Build from source

The source repository is not public yet, so there is no build from source
today; use the prebuilt artifacts above. The extension is a standard `phpize`
module (`phpize && ./configure && make && sudo make install`) and will build
that way once the repository is published.
