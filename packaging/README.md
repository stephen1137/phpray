# PHPRay packaging

This directory holds the packaging assets for the **collector daemon** half of
PHPRay. The PHP extension (`.so`) is not packaged; it is distributed as raw
release artifacts and installed by the one-line installer. Only the collector
gets a proper system package (`.deb` / `.rpm`) plus a curl-able installer.

## Files

| File | Purpose |
|------|---------|
| `nfpm.yaml` | nfpm package spec for `phpray-collector`. Version/arch come from the `${VERSION}` / `${ARCH}` env vars. |
| `phpray-collector.service` | Systemd unit bundled into the package (ExecStart → `/usr/bin/phpray-collector`). |
| `postinstall.sh` | `daemon-reload` + enable (does **not** start). |
| `preremove.sh` | stop + disable. |
| `install.sh` | The one-line server installer (see below). |

## How releases are built

Release builds live in `.github/workflows/release.yml` and are triggered by a
`v*` tag. The flow:

1. **Build** (reused from `build.yml` via `workflow_call`):
   - the Go collector is cross-compiled for `linux/amd64` and `linux/arm64`,
   - the C extension is built for the PHP 8.1–8.5 × glibc/musl × amd64/arm64
     matrix via `scripts/build-matrix.sh`.
2. **Package**: all artifacts are downloaded, a `SHA256SUMS` is generated, and
   nfpm is run (through the `goreleaser/nfpm` docker image) to produce the
   `.deb` and `.rpm` for each architecture.
3. **Release**: everything (`.so` files, collector binaries, packages,
   `SHA256SUMS`) is uploaded to a GitHub Release with auto-generated notes via
   `softprops/action-gh-release`.

Artifacts are named:

- `phpray-<php>-<libc>-<arch>.so`  (e.g. `phpray-8.3-glibc-amd64.so`)
- `phpray-collector-linux-<arch>`
- `SHA256SUMS`
- `phpray-collector_<ver>_<arch>.deb` / `phpray-collector-<ver>-<arch>.rpm`

## Testing the installer locally in docker

The installer is intentionally self-contained, so you can smoke-test it in an
official PHP image without a real server. A dry run performs no writes, which
makes it safe to run without `sudo`-equivalent privileges inside the container.

```sh
# Start a throwaway PHP 8.3 CLI container.
docker run --rm -it php:8.3-cli bash

# Inside the container, fetch the installer and do a dry run:
curl -fsSL https://phpray.dev/install.sh -o /tmp/install.sh
bash /tmp/install.sh --dry-run
```

`--dry-run` prints every action (downloads, checksums, file installs,
`systemctl` calls) without performing them, so you can verify detection and
paths on any distro by swapping the image tag (e.g. `php:8.2-cli-alpine` to
exercise the musl path, or an alpine/cpanel-style image for the alternate PHP
locations).

To test a real (non-dry-run) install inside a container, remember the container
must be run as root (`-it` above already is root) and systemd is typically not
available in a container — the installer detects that and skips the
`systemctl` enable/step with a warning rather than failing.
