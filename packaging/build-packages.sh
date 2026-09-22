#!/usr/bin/env bash
# Builds .deb and .rpm for phpray-collector for the given architectures using nfpm (docker).
# Usage: packaging/build-packages.sh <version> [amd64 arm64]
# Expects dist/phpray-collector-linux-<arch> to exist (cross-compiled collector binaries).
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:?version required, e.g. 0.14.0}"; shift || true
ARCHES=("$@"); [ ${#ARCHES[@]} -eq 0 ] && ARCHES=(amd64 arm64)
NFPM_IMAGE="${NFPM_IMAGE:-goreleaser/nfpm:latest}"
mkdir -p dist/.build
for ARCH in "${ARCHES[@]}"; do
  bin="dist/phpray-collector-linux-${ARCH}"
  [ -f "$bin" ] || { echo "missing $bin" >&2; exit 1; }
  cp "$bin" dist/.build/phpray-collector
  # nfpm does not expand env vars inside contents.src, so substitute the whole file first.
  VERSION="$VERSION" ARCH="$ARCH" envsubst '${VERSION} ${ARCH}' < packaging/nfpm.yaml > dist/.build/nfpm.yaml
  for PK in deb rpm; do
    docker run --rm -v "$PWD:/src" -w /src "$NFPM_IMAGE" package --config dist/.build/nfpm.yaml --packager "$PK" --target dist/
  done
done
rm -rf dist/.build
ls -1 dist/*.deb dist/*.rpm
