#!/usr/bin/env bash
# Pomiar narzutu PHPRay na WordPressie. Warianty przeplatane rundami, żeby
# dryf maszyny (termika, inne procesy) rozłożył się równo na wszystkie.
set -u
URL="http://localhost:8098/"
ROUNDS="${ROUNDS:-5}"; WARM="${WARM:-120}"; N="${N:-300}"
OUT="$(dirname "$0")/wyniki.csv"
CONF="$(dirname "$0")/conf/zz-bench.ini"
EXTDIR="/usr/local/lib/php/extensions/no-debug-non-zts-20230831"

ini_for() {
  case "$1" in
    A) : > "$CONF" ;;                                    # bez rozszerzenia
    B) printf 'extension=%s/phpray.so\nphpray.master_switch=0\n' "$EXTDIR" > "$CONF" ;;
    C) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=smart\nphpray.output_mode=file\nphpray.output_path=/tmp/phpray-bench.jsonl\n' "$EXTDIR" > "$CONF" ;;
    D) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=all\nphpray.output_mode=file\nphpray.output_path=/tmp/phpray-bench.jsonl\n' "$EXTDIR" > "$CONF" ;;
    E) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=smart\nphpray.output_mode=shm\nphpray.shm_path=/dev/shm/phpray-ring\n' "$EXTDIR" > "$CONF" ;;
    H) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=smart\nphpray.output_mode=shm\nphpray.shm_path=/dev/shm/phpray-ring\nphpray.profile_functions=1\nphpray.profile_sample_rate=0\n' "$EXTDIR" > "$CONF" ;;
    G) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=smart\nphpray.output_mode=shm\nphpray.shm_path=/dev/shm/phpray-ring\nphpray.profile_functions=0\n' "$EXTDIR" > "$CONF" ;;
    F) printf 'extension=%s/phpray.so\nphpray.master_switch=1\nphpray.enabled=1\nphpray.mode=all\nphpray.output_mode=shm\nphpray.shm_path=/dev/shm/phpray-ring\n' "$EXTDIR" > "$CONF" ;;
  esac
}

echo "runda,wariant,srednia_ms,p50_ms,p95_ms,p99_ms,rps" > "$OUT"
for r in $(seq 1 "$ROUNDS"); do
  for v in ${VARIANTS:-A B C D}; do
    ini_for "$v"
    docker restart bench-wp-1 >/dev/null 2>&1
    for i in $(seq 1 60); do curl -s -o /dev/null --max-time 5 "$URL" >/dev/null 2>&1 && break; sleep 1; done
    # sprawdzenie, że wariant faktycznie obowiązuje
    loaded=$(docker exec bench-wp-1 php -m 2>/dev/null | grep -c '^phpray$')
    taskset -c 12-15 ab -q -n "$WARM" -c 1 "$URL" >/dev/null 2>&1
    res=$(taskset -c 12-15 ab -q -n "$N" -c 1 "$URL" 2>/dev/null)
    mean=$(echo "$res" | awk '/Time per request/ && /mean\)/ {print $4; exit}')
    rps=$(echo "$res"  | awk '/Requests per second/ {print $4; exit}')
    p50=$(echo "$res"  | awk '/^  50%/ {print $2}')
    p95=$(echo "$res"  | awk '/^  95%/ {print $2}')
    p99=$(echo "$res"  | awk '/^  99%/ {print $2}')
    printf '%s,%s,%s,%s,%s,%s,%s\n' "$r" "$v" "${mean:-}" "${p50:-}" "${p95:-}" "${p99:-}" "${rps:-}" >> "$OUT"
    printf '  runda %s wariant %s (phpray w php -m: %s): srednia %s ms, p50 %s ms, p95 %s ms\n' "$r" "$v" "$loaded" "${mean:-?}" "${p50:-?}" "${p95:-?}"
  done
done
echo "gotowe: $OUT"
