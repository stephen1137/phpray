#!/usr/bin/env bash
# Statyczna binarka kolektora (CGO + go-sqlite3, musl static) w dockerze golang:1.24-alpine.
# go-sqlite3 wymaga cgo — build z CGO_ENABLED=0 daje stub, który nie otwiera bazy (wpadka 18.09.2026).
# Użycie: scripts/build-collector-static.sh [amd64|arm64]   → dist/phpray-collector-linux-<arch>
set -euo pipefail
cd "$(dirname "$0")/.."
ARCH=${1:-amd64}
case "$ARCH" in amd64) PLATFORM=linux/amd64;; arm64) PLATFORM=linux/arm64;; *) echo "arch?"; exit 2;; esac
mkdir -p dist
docker run --rm --platform "$PLATFORM" -v "$PWD/src/collector:/src:ro" -v "$PWD/dist:/out" -w /build golang:1.24-alpine sh -c '
  set -e; apk add -q gcc musl-dev sqlite-dev >/dev/null
  cp -r /src/. /build/ && rm -f /build/phpray-collector-linux-*
  CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags "-s -w -linkmode external -extldflags \"-static\"" -o /out/phpray-collector-linux-'"$ARCH"' .
  /out/phpray-collector-linux-'"$ARCH"' version'
ls -la "dist/phpray-collector-linux-$ARCH" | awk '{print $5" B  "$9}'
