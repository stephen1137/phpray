#!/usr/bin/env bash
# Test funkcjonalny profilu komponentów (zend_observer) — docker/test/components.php.
# Buduje rozszerzenie w kontenerze php:<wersja>-cli, uruchamia wbudowany serwer PHP
# z rozszerzeniem i sprawdza w JSONL spójność incl_ns / self_ns / calls / profiled
# dla trybów: all, off, sample (50 %), url.
#
# Użycie: scripts/test-components.sh [wersja-php=8.3] [n=30]
# Zmienne: EXT_SO=/ścieżka/phpray.so lub "installed" (pomija build), PHP_BIN=php, IMAGE=...
#          CASES="all off sample url" — które tryby uruchomić
#          SERVER_PRELOAD=auto|/ścieżka/libasan.so — serwer pod ASan (+ USE_ZEND_ALLOC=0)
set -uo pipefail
cd "$(dirname "$0")/.."
PHPV=${1:-8.3}; N=${2:-30}
IMAGE=${IMAGE:-php:${PHPV}-cli}

docker run --rm -v "$PWD/src/extension:/src:ro" -v "$PWD/docker/test:/t:ro" \
  -e N="$N" -e EXT_SO="${EXT_SO:-}" -e PHP_BIN="${PHP_BIN:-php}" -e SERVER_PRELOAD="${SERVER_PRELOAD:-}" -e CASES="${CASES:-all off sample url}" \
  -e ASAN_OPTIONS="${ASAN_OPTIONS:-detect_leaks=0:halt_on_error=1:abort_on_error=0:verify_asan_link_order=0}" \
  "$IMAGE" bash -c '
set -u
[ "$EXT_SO" = installed ] && EXT_SO=$(php-config --extension-dir)/phpray.so
if [ -z "$EXT_SO" ]; then
  cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j"$(nproc)" >/tmp/make.log 2>&1 || { tail -30 /tmp/make.log; exit 1; }
  EXT_SO=/build/modules/phpray.so
fi
SERVER_ENV=()
if [ -n "$SERVER_PRELOAD" ]; then
  # auto = shim zdejmujący RTLD_DEEPBIND (docker/asan/nodeepbind.c, obraz phpray-asan) + libasan
  [ "$SERVER_PRELOAD" = auto ] && SERVER_PRELOAD=/usr/local/lib/nodeepbind.so:$(gcc -print-file-name=libasan.so)
  SERVER_ENV=(env LD_PRELOAD="$SERVER_PRELOAD" USE_ZEND_ALLOC=0 ZEND_DONT_UNLOAD_MODULES=1 ASAN_OPTIONS="$ASAN_OPTIONS")
  echo "serwer pod ASan: $SERVER_PRELOAD (USE_ZEND_ALLOC=0)"
fi
echo "PHP $($PHP_BIN -n -r "echo PHP_VERSION;") ext=$EXT_SO"
rc=0
run_case() { # nazwa tryb [dodatkowe -d ...]
  local name=$1 mode=$2; shift 2
  local out=/tmp/phpray-$name.jsonl log=/tmp/server-$name.log client=/tmp/client-$name.json
  rm -f $out /dev/shm/phpray-test-$name
  "${SERVER_ENV[@]}" $PHP_BIN -n -S 127.0.0.1:8080 -t /t -d extension=$EXT_SO -d phpray.output_path=$out \
     -d phpray.output_mode=both -d phpray.shm_path=/dev/shm/phpray-test-$name -d phpray.shm_size=1048576 \
     -d phpray.profile_mode=$mode "$@" >$log 2>&1 &
  local pid=$!
  for i in $(seq 1 50); do $PHP_BIN -n -r "exit(@fsockopen(\"127.0.0.1\", 8080) ? 0 : 1);" && break; sleep 0.2; done
  local variant=plain; [ "$mode" = url ] && variant=url
  $PHP_BIN -n /t/components_client.php http://127.0.0.1:8080 "$N" $variant >$client 2>/tmp/client-$name.err
  local crc=$?
  kill $pid 2>/dev/null; wait $pid 2>/dev/null
  echo "=== $name (profile_mode=$mode $*) client_rc=$crc ==="
  [ $crc -ne 0 ] && { echo "klient: błędy żądań"; head -5 /tmp/client-$name.err; echo "--- log serwera (ostatnie 40 linii):"; tail -40 $log; rc=1; }
  if grep -qi "AddressSanitizer\|ERROR: \|Segmentation\|core dumped" $log; then echo "!!! SERWER: raport ASan/crash"; grep -i -A20 "AddressSanitizer\|Segmentation" $log | head -60; rc=1; fi
  $PHP_BIN -n /t/components_check.php $mode $out $client || rc=1
}
for c in $CASES; do
  case $c in
    all)    run_case all all ;;
    off)    run_case off off ;;
    sample) run_case sample sample -d phpray.profile_sample_rate=50 ;;
    url)    run_case url url -d phpray.profile_url="/other,/components.php?prof" ;;
  esac
done
echo "=== wynik: $([ $rc -eq 0 ] && echo OK || echo BŁĄD) ==="
exit $rc
'
