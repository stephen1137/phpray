#!/usr/bin/env bash
# Test spójności tabeli sterującej w shm: kolektor (Go, host) pisze wpis {docroot, url_prefix,
# sample_rate, until}; rozszerzenie (php -S w dockerze, profile_mode=off) czyta go w RINIT i profiluje
# tylko żądania z pasującym prefiksem; clear/expire wyłącza profil bez restartu PHP.
# Użycie: scripts/test-control.sh [wersja-php=8.3]
set -uo pipefail
cd "$(dirname "$0")/.."
PHPV=${1:-8.3}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
chmod 755 "$TMP"
( cd src/collector && go build -o "$TMP/phpray-collector" . ) || { echo "go build BŁĄD"; exit 1; }
CTL="$TMP/phpray-control"
# docroot pod php -S = katalog -t (/t w kontenerze); wpis: tylko /control_status.php?prof, 100 %
"$TMP/phpray-collector" control set -path "$CTL" -docroot /t -prefix "/control_status.php?prof" -rate 100 -ttl 300s
"$TMP/phpray-collector" control list -path "$CTL"
chmod 644 "$CTL"
cat > "$TMP/run.sh" <<'RUN'
set -u
cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j"$(nproc)" >/tmp/make.log 2>&1 || { tail -20 /tmp/make.log; exit 1; }
php -n -S 127.0.0.1:8080 -t /t -d extension=/build/modules/phpray.so -d phpray.output_path=/tmp/o.jsonl \
   -d phpray.profile_mode=off -d phpray.control_path=/ctl/phpray-control >/tmp/srv.log 2>&1 &
sleep 1
get() { php -n -r 'echo file_get_contents("http://127.0.0.1:8080" . $argv[1]);' "$1"; }
echo "== wpis aktywny: /control_status.php?prof=1 → profil, /control_status.php → bez"
A=$(get "/control_status.php?prof=1"); B=$(get "/control_status.php"); sleep 0.3
echo "$A" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); printf("  mapped=%s entries=%d hits=%d prefix=%s\n", var_export($d["control"]["mapped"],true), count($d["control"]["entries"]), $d["control"]["hits"], $d["control"]["entries"][0]["url_prefix"] ?? "-");'
IDA=$(echo "$A" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); echo $d["id"];'); IDB=$(echo "$B" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); echo $d["id"];')
PA=$(grep "\"id\":\"$IDA\"" /tmp/o.jsonl | grep -o '"profiled":[01]' | cut -d: -f2); PB=$(grep "\"id\":\"$IDB\"" /tmp/o.jsonl | grep -o '"profiled":[01]' | cut -d: -f2)
CA=$(grep "\"id\":\"$IDA\"" /tmp/o.jsonl | grep -c '"components"')
echo "  ?prof=1 → profiled=$PA components=$CA ; bez prefiksu → profiled=$PB"
rc=0; [ "$PA" = 1 ] && [ "$CA" = 1 ] && [ "$PB" = 0 ] || rc=1
echo "== czekam na clear (host)"; while [ ! -f /ctl/cleared ]; do sleep 0.2; done
C=$(get "/control_status.php?prof=1"); sleep 0.3
IDC=$(echo "$C" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); echo $d["id"];'); PC=$(grep "\"id\":\"$IDC\"" /tmp/o.jsonl | grep -o '"profiled":[01]' | cut -d: -f2)
echo "  po clear: ?prof=1 → profiled=$PC (oczekiwane 0)"; [ "$PC" = 0 ] || rc=1
echo "== czekam na ponowny set z rate=0 (host)"; while [ ! -f /ctl/reset ]; do sleep 0.2; done
D=$(get "/control_status.php?prof=1"); sleep 0.3
IDD=$(echo "$D" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); echo $d["id"];'); PD=$(grep "\"id\":\"$IDD\"" /tmp/o.jsonl | grep -o '"profiled":[01]' | cut -d: -f2)
echo "  rate=0: ?prof=1 → profiled=$PD (oczekiwane 0)"; [ "$PD" = 0 ] || rc=1
echo "== plik tabeli usunięty i utworzony na nowo (inny inode) — po ≤5 s worker mapuje nowy"
while [ ! -f /ctl/recreated ]; do sleep 0.2; done; sleep 6
E=$(get "/control_status.php?prof=1"); sleep 0.3
IDE=$(echo "$E" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); echo $d["id"];'); PE=$(grep "\"id\":\"$IDE\"" /tmp/o.jsonl | grep -o '"profiled":[01]' | cut -d: -f2)
echo "  nowa tabela (rate=100): ?prof=1 → profiled=$PE (oczekiwane 1)"; [ "$PE" = 1 ] || rc=1
echo "$E" | php -n -r '$d=json_decode(stream_get_contents(STDIN),true); printf("  status: mapped=%s entries=%d hits=%d seq=%d\n", var_export($d["control"]["mapped"],true), count($d["control"]["entries"]), $d["control"]["hits"], $d["control"]["seq"]);'
kill %1 2>/dev/null; wait 2>/dev/null
echo "=== wynik: $([ $rc -eq 0 ] && echo OK || echo BŁĄD) ==="; exit $rc
RUN
docker run --rm -v "$PWD/src/extension:/src:ro" -v "$PWD/docker/test:/t:ro" -v "$TMP/run.sh:/run.sh:ro" -v "$TMP:/ctl" "php:${PHPV}-cli" bash /run.sh &
DOCKER_PID=$!
sleep 25   # build + pierwsze żądania
"$TMP/phpray-collector" control clear -path "$CTL" -docroot /t >/dev/null; touch "$TMP/cleared"
sleep 3
"$TMP/phpray-collector" control set -path "$CTL" -docroot /t -prefix "/control_status.php?prof" -rate 0 -ttl 300s >/dev/null; touch "$TMP/reset"
sleep 3
rm -f "$CTL"; "$TMP/phpray-collector" control set -path "$CTL" -docroot /t -prefix "/control_status.php?prof" -rate 100 -ttl 300s >/dev/null; chmod 644 "$CTL"; touch "$TMP/recreated"
wait $DOCKER_PID
