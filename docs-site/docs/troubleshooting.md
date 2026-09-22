# Troubleshooting

Start with `phpray status` (see [CLI](usage/cli.md)) — it names the first
thing that is wrong in every case below.

## Extension not loading

`php -m | grep phpray` prints nothing, or `php -v` fails:

- **Wrong PHP version** — the `.so` in the release is per version
  (`php8.3-glibc-x86_64` will not load under 8.2). Match `php -v` to the
  artifact name.
- **ZTS vs NTS** — build with `--enable-shared` (ZTS) needs the ZTS
  variant; most distro PHP is NTS. `php -i | grep Thread` tells you.
- **Wrong libc** — musl (Alpine) vs glibc (everything else). Alpine
  containers need the `musl` artifact.
- Check `error_log` / `dmesg` for the loader message (missing symbols →
  wrong version or build).

## No traces for a site

Run `phpray status` and `phpray trace -d <domain> -n 5`; common causes in order:

- the URI is in `phpray.ignore_uris` (or the site's `.user.ini` sets it)
- `phpray.mode=manual` and no trigger is active (console/CLI) — switch to
  `smart` or fire a profile
- `phpray.enabled=0` in a `.user.ini`/`.htaccess` closer to the docroot
  (PERDIR directives override `php.ini`; the closest file wins)
- the request was fast — `smart` only stores `normal` at 200 ms and up;
  lower `phpray.smart_threshold_normal` or use `all`
- `phpray.profile_mode=off` — traces still appear, just without the
  components table (expected, not a fault)

## Collector not reading shm

`phpray status` shows "collector ok" but no data:

- the extension's `phpray.shm_path`/`phpray.shm_size` and the collector's
  `[input] shm_path` must match exactly (different PHP versions with
  different ini includes are a classic split); with per-user rings
  (`shm_path = /run/phpray/ring-%u`) the collector needs the glob form
  `shm_path = "/run/phpray/ring-*"`
- `output_mode=file` on one side, `shm` on the other
- shm on a full `/dev/shm` — check `df -h /dev/shm`
- per-user rings appear only after the first traced request of that user
  (`ls -la /run/phpray/` shows `ring-<uid>` files, mode `0600`)

## Permission issues

- `/var/lib/phpray` must be owned by the `phpray` collector user; if the
  extension writes files (`output_mode=file|both`), the PHP user (e.g.
  `nobody`, `www-data`, the per-user lsphp uid) must be able to write it —
  on multi-user boxes use `shm` mode so PHP never writes to the storage dir
- a per-user ring (`%u` in `shm_path`) is used only when the file is a regular
  file owned by that uid: a file or symlink another user planted under the
  name is ignored (`phpray_ring_stats()` returns `false`); delete it as root

## CloudLinux / CageFS

Symptom: the extension loads, `phpray status` says "collector ok", but no
traces arrive — and `/dev/shm/phpray` on the host stays empty (or the cage
creates its own copy). Cause: every CageFS user has a **private, empty
`/dev/shm`** (own mount namespace), so a ring under `/dev/shm` is invisible to
the collector, and a single world-writable ring would let customers read each
other's traces anyway. Use a shared directory and one ring per user:

```bash
# 1. shared runtime dir, sticky and world-writable (survives reboots via tmpfiles)
install -d -m 1777 -o root -g root /run/phpray
echo 'd /run/phpray 1777 root root -' > /etc/tmpfiles.d/phpray.conf

# 2. mount it into every cage: one line without prefix in cagefs.mp
grep -qx '/run/phpray' /etc/cagefs/cagefs.mp || echo '/run/phpray' >> /etc/cagefs/cagefs.mp
cagefsctl --force-update        # server-wide: rebuilds the skeleton and remounts all users
                                # (cagefsctl --remount-all only remounts; CloudLinux documents it
                                # as the command to run after a cagefs.mp change — both are global)
```

Extension ini (every PHP version, including `/opt/alt/php*`):

```ini
extension=phpray.so
phpray.enabled      = 0                       ; opt-in: phpray.enabled=1 in the account's .user.ini
phpray.output_mode  = shm
phpray.shm_path     = /run/phpray/ring-%u     ; one private 0600 ring per uid
phpray.shm_size     = 2097152                 ; per user — keep it small
phpray.control_path = /run/phpray/control     ; shared, read-only for PHP
```

Collector (`/etc/phpray/collector.toml`, collector 0.15+ reads the glob):

```toml
[input]
shm_path     = "/run/phpray/ring-*"
control_path = "/run/phpray/control"
```

`systemctl restart phpray-collector`.

Verify from inside a cage, as the customer:

```bash
su -s /bin/bash <user> -c 'ls -ld /run/phpray && php -r "var_dump(phpray_ring_stats());"'
# after the first traced request of that site:
ls -la /run/phpray/            # ring-<uid>  -rw------- <user>
phpray status                  # collector: rings found
```

The installer does all of the above when it detects `cagefsctl`
(`install.sh --cagefs` forces it, `--no-cagefs` skips it), see
[Shared hosting](install/shared-hosting.md#cloudlinux-cagefs-what-the-installer-does).

### Two failure modes worth knowing

Both come from the systemd unit and both were seen on a live CloudLinux server.

- **`Ring buffer … not usable yet: read-only file system` in the collector log.**
  The unit runs with `ProtectSystem=strict`, which makes `/run` read-only, so
  `/run/phpray` has to be listed in `ReadWritePaths`. Units from PHPRay 0.15 on
  include it.
- **PHP stops writing its ring after the collector restarts** — `phpray_ring_stats()`
  returns `false` inside the cage while the directory looks perfectly fine from the
  host. `RuntimeDirectory=phpray` **deletes** `/run/phpray` when the service stops;
  the cages keep a mount of the removed inode. Compare them:

```bash
stat -c %i /run/phpray                                        # host
su -l <account> -s /bin/bash -c 'stat -c %i /run/phpray'      # inside the cage
```

  Different numbers mean a stale mount: `cagefsctl --remount-all` fixes it. Units
  from PHPRay 0.15 on set `RuntimeDirectoryPreserve=yes` and `RuntimeDirectoryMode=1777`,
  so the directory survives restarts; an older unit needs a drop-in:

```ini
# /etc/systemd/system/phpray-collector.service.d/cagefs.conf
[Service]
RuntimeDirectoryMode=1777
RuntimeDirectoryPreserve=yes
ReadWritePaths=-/run/phpray
```

Long-lived PHP workers keep the `.so` they loaded at start: after replacing the
extension, recycle that account's workers (`pkill -u <account> -f lsphp`) instead
of restarting the whole web server.

## Collecting a bug report

```bash
phpray report -o phpray-report.txt
journalctl -u phpray-collector --since "10 min ago" >> phpray-report.txt
```

Send the report, your PHP version and distro, and the exact
`php -i | grep -E "phpray|extension_dir"` output.
