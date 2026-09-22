#!/usr/bin/env bash
# Test przeplotu JSONL: 4 procesy PHP (php -S na 4 portach) piszą duże rekordy (~8-10 KB,
# docker/test/bigtrace.php) do JEDNEGO pliku; 4 klientów × N żądań równolegle.
# Oczekiwanie: 0 nieparsowalnych linii i liczba rekordów == liczba żądań.
# Użycie: scripts/test-jsonl-concurrency.sh [wersja-php=8.3] [n-na-proces=2000]
# Zmienne: EXT_SO_HOST=/ścieżka/phpray.so — użyj gotowego .so z hosta (np. starego binarium do porównania)
set -uo pipefail
cd "$(dirname "$0")/.."
PHPV=${1:-8.3}; N=${2:-2000}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/run.sh" <<'RUN'
set -u
if [ -z "${EXT_SO:-}" ]; then
  cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j"$(nproc)" >/tmp/make.log 2>&1 || { tail -20 /tmp/make.log; exit 1; }
  EXT_SO=/build/modules/phpray.so
fi
OUT=/tmp/phpray-concurrent.jsonl; rm -f $OUT
for port in 8081 8082 8083 8084; do
  php -n -S 127.0.0.1:$port -t /t -d extension=$EXT_SO -d phpray.output_path=$OUT -d phpray.profile_mode=all -d phpray.capture_errors=1 >/tmp/srv-$port.log 2>&1 &
done
sleep 1
cat > /tmp/client.php <<'PHP'
<?php
$port = $argv[1]; $n = (int)$argv[2]; $fail = 0;
for ($i = 0; $i < $n; $i++) { $r = @file_get_contents("http://127.0.0.1:$port/bigtrace.php"); if ($r === false) $fail++; }
echo "port $port: $n żądań, $fail błędów\n"; exit($fail ? 1 : 0);
PHP
cat > /tmp/check.php <<'PHP'
<?php
$lines = 0; $bad = 0; $bytes = 0; $sizes = [];
$fh = fopen($argv[1], "r");
while (($l = fgets($fh)) !== false) { $lines++; $bytes += strlen($l); $sizes[] = strlen($l); if (json_decode($l) === null) $bad++; }
sort($sizes);
printf("rekordów: %d (oczekiwano %d), nieparsowalnych: %d, rozmiar rekordu: min %d / mediana %d / max %d B, plik %.1f MB\n",
  $lines, (int)$argv[2], $bad, $sizes[0] ?? 0, $sizes[intdiv(count($sizes), 2)] ?? 0, end($sizes) ?: 0, $bytes / 1048576);
exit(($bad === 0 && $lines === (int)$argv[2]) ? 0 : 1);
PHP
start=$(date +%s)
CLIENTS=""
for port in 8081 8082 8083 8084; do php -n /tmp/client.php $port "$N" & CLIENTS="$CLIENTS $!"; done
wait $CLIENTS
echo "czas: $(( $(date +%s) - start )) s"
kill $(jobs -p) 2>/dev/null; wait 2>/dev/null
if php -n /tmp/check.php $OUT $((N * 4)); then echo "=== wynik: OK ==="; else echo "=== wynik: BŁĄD ==="; exit 1; fi
RUN
EXT_SO=${EXT_SO:-}
EXTRA_MOUNT=()
if [ -n "${EXT_SO_HOST:-}" ]; then EXTRA_MOUNT=(-v "$EXT_SO_HOST:/ext/phpray.so:ro"); EXT_SO=/ext/phpray.so; fi
docker run --rm -v "$PWD/src/extension:/src:ro" -v "$PWD/docker/test:/t:ro" -v "$TMP/run.sh:/run.sh:ro" "${EXTRA_MOUNT[@]}" \
  -e N="$N" -e EXT_SO="$EXT_SO" "php:${PHPV}-cli" bash /run.sh
