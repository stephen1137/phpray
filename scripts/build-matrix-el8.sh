#!/usr/bin/env bash
# Buduje wariant glibc phpray.so na AlmaLinux 8 (glibc 2.28) z PHP z repozytorium Remi,
# żeby binarka działała też na CloudLinux/Alma/Rocky 8, Debian 10+ i Ubuntu 18.10+
# (obrazy php:X-cli są na Debianie z glibc 2.36+, co daje symbole fstat@GLIBC_2.33).
# Użycie: scripts/build-matrix-el8.sh [wersje...]   (domyślnie 8.1 8.2 8.3 8.4 8.5); ARCH=amd64|arm64
set -uo pipefail
cd "$(dirname "$0")/.."
VERSIONS=("$@"); [ ${#VERSIONS[@]} -eq 0 ] && VERSIONS=(8.0 8.1 8.2 8.3 8.4 8.5)
ARCH=${ARCH:-amd64}; case "$ARCH" in amd64) PLATFORM="--platform linux/amd64";; arm64) PLATFORM="--platform linux/arm64";; *) echo "ARCH?"; exit 2;; esac
mkdir -p dist
printf "%-6s %-6s %-6s %-9s %s\n" PHP LIBC ARCH WYNIK SZCZEGÓŁY
for v in "${VERSIONS[@]}"; do
  short="${v/./}"   # 8.2 -> 82
  out="dist/phpray-${v}-glibc-${ARCH}.so"; log="dist/build-${v}-glibc-${ARCH}.log"
  docker run --rm $PLATFORM -v "$PWD/src/extension:/src:ro" -v "$PWD/dist:/dist" almalinux:8 bash -c "
    set -e
    dnf -y -q install epel-release >/dev/null 2>&1
    dnf -y -q install https://rpms.remirepo.net/enterprise/remi-release-8.rpm >/dev/null 2>&1
    dnf -y -q install php${short}-php-cli php${short}-php-devel gcc make autoconf file binutils >/dev/null 2>&1
    export PATH=/opt/remi/php${short}/root/usr/bin:\$PATH
    cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j\$(nproc) >/dev/null 2>/dev/null
    cp modules/phpray.so /dist/$(basename "$out")
    php -n -d extension=/dist/$(basename "$out") -m | grep -qi '^phpray\$'
    php -n -d extension=/dist/$(basename "$out") -r 'echo PHP_VERSION, \" \", phpversion(\"phpray\"), \" glibc-max=\";'
    objdump -T /dist/$(basename "$out") | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1
  " >"$log" 2>&1
  rc=$?
  if [ $rc -eq 0 ]; then printf "%-6s %-6s %-6s %-9s %s\n" "$v" glibc "$ARCH" OK "$(tail -2 "$log" | tr '\n' ' ')"; else printf "%-6s %-6s %-6s %-9s %s\n" "$v" glibc "$ARCH" BŁĄD "$(grep -m1 -iE 'error|fatal|undefined|No match' "$log" | cut -c1-100)"; fi
done
