#!/usr/bin/env bash
# Buduje phpray.so dla macierzy wersji PHP × libc w dockerze i robi test dymny (php -m).
# Użycie: scripts/build-matrix.sh [wersje...]   np. scripts/build-matrix.sh 8.3 8.4
# Zmienne: LIBCS (domyślnie tylko musl — patrz niżej), ARCH=amd64|arm64 (domyślnie uname -m; arm64 na amd64 = emulacja qemu)
#
# WARIANTY GLIBC BUDUJE scripts/build-matrix-el8.sh, NIE TEN SKRYPT.
# Obrazy php:X-cli stoją na Debianie i produkują .so wymagające GLIBC_2.33,
# które nie ładuje się na CloudLinux 8 ani AlmaLinux 8, czyli na naszym
# głównym rynku. 20.09.2026 takie pliki trafiły do wydania; 21.09.2026 ten
# skrypt po raz drugi nadpisał poprawne binarki z el8. Dlatego domyślnie
# buduje już tylko musl, a glibc wymaga świadomego ALLOW_GLIBC=1.
set -uo pipefail
cd "$(dirname "$0")/.."
VERSIONS=("$@"); [ ${#VERSIONS[@]} -eq 0 ] && VERSIONS=(8.0 8.1 8.2 8.3 8.4 8.5)
LIBCS=${LIBCS:-"musl"}
if [ "${ALLOW_GLIBC:-0}" != 1 ]; then
  case " $LIBCS " in
    *" glibc "*)
      echo "glibc pomijam: te warianty buduje scripts/build-matrix-el8.sh (GLIBC_2.17)." >&2
      echo "Świadomie i tylko do testów lokalnych: ALLOW_GLIBC=1 $0 ..." >&2
      LIBCS=$(echo "$LIBCS" | tr ' ' '\n' | grep -v '^glibc$' | tr '\n' ' ')
      [ -n "${LIBCS// /}" ] || exit 0
      ;;
  esac
fi
ARCH=${ARCH:-$(uname -m)}; case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; esac
# Cross-build przez emulację (binfmt/qemu): ARCH=arm64 scripts/build-matrix.sh — dodaje --platform do docker run.
PLATFORM=""; case "$ARCH" in amd64) PLATFORM="--platform linux/amd64";; arm64) PLATFORM="--platform linux/arm64";; esac
mkdir -p dist
printf "%-6s %-6s %-6s %-9s %s\n" PHP LIBC ARCH WYNIK SZCZEGÓŁY
for v in "${VERSIONS[@]}"; do
  for libc in $LIBCS; do
    if [ "$libc" = musl ]; then img="php:${v}-cli-alpine"; prep='apk update >/dev/null 2>&1; apk add --no-cache $PHPIZE_DEPS >/dev/null'; else img="php:${v}-cli"; prep='true'; fi
    out="dist/phpray-${v}-${libc}-${ARCH}.so"; log="dist/build-${v}-${libc}-${ARCH}.log"
    docker run --rm $PLATFORM -v "$PWD/src/extension:/src:ro" -v "$PWD/dist:/dist" "$img" sh -c "
      set -e; $prep; cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j\$(nproc) >/dev/null 2>/dev/null
      cp modules/phpray.so /dist/$(basename "$out")
      php -n -d extension=/dist/$(basename "$out") -m | grep -qi '^phpray$'
      php -n -d extension=/dist/$(basename "$out") -r 'echo PHP_VERSION, \" \", phpversion(\"phpray\");'
    " >"$log" 2>&1
    rc=$?
    if [ $rc -eq 0 ]; then printf "%-6s %-6s %-6s %-9s %s\n" "$v" "$libc" "$ARCH" OK "$(tail -1 "$log")"; else printf "%-6s %-6s %-6s %-9s %s\n" "$v" "$libc" "$ARCH" BŁĄD "$(grep -m1 -iE 'error|fatal|undefined' "$log" | cut -c1-90)"; fi
  done
done
ls -la dist/*.so 2>/dev/null | awk '{print $5" B  "$9}'
