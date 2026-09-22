#!/usr/bin/env bash
# Test ringu per użytkownik: phpray.shm_path z %u / %g (hosting współdzielony, CloudLinux/CageFS).
# W kontenerze php:<wersja>-cli buduje rozszerzenie, zakłada użytkowników alice/bob/carol/dave
# i katalog 1777 (jak /run/phpray), po czym sprawdza:
#   1. ring-%u powstaje przy pierwszym śledzonym żądaniu jako plik 0600 użytkownika,
#      a ślad z RSHUTDOWN trafia do niego (record_count w nagłówku);
#   2. drugi proces tego samego użytkownika podpina istniejący ring (bez kasowania rekordów);
#   3. drugi użytkownik dostaje własny ring i nie odczyta cudzego;
#   4. podstawiony przez innego użytkownika plik pod naszą nazwą nie jest używany (stats=false, plik nietknięty);
#   5. podstawiony symlink też nie;
#   6. %g rozwija się do egid (0600 z %u, 0660 z samym %g);
#   7. ścieżka bez placeholdera działa jak dotąd: ring wspólny, nie 0600, kasowany przez twórcę;
#   8. zmiana euid w jednym procesie (jak mod_ruid2) przepina ring na nowego użytkownika.
# Użycie: scripts/test-shm-peruser.sh [wersja-php=8.3]
set -uo pipefail
cd "$(dirname "$0")/.."
PHPV=${1:-8.3}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/run.sh" <<'RUN'
set -u
cp -r /src /build && cd /build && phpize >/dev/null && ./configure --enable-phpray >/dev/null && make -j"$(nproc)" >/tmp/make.log 2>&1 || { tail -20 /tmp/make.log; exit 1; }
EXT=/build/modules/phpray.so
for u in alice:1001 bob:1002 carol:1003 dave:1004; do useradd -M -u "${u#*:}" "${u%:*}"; done
D=/tmp/phpray-test; install -d -m 1777 "$D"
OPTS="-n -d extension=$EXT -d phpray.trace_cli=1 -d phpray.output_mode=shm -d phpray.shm_size=1048576 -d phpray.profile_mode=off"
php_as() { local u=$1; shift; runuser -u "$u" -- php $OPTS "$@"; }
# record_count z nagłówka ringu (offset 72, u64 LE); "-" gdy pliku nie ma
records() { if [ -f "$1" ]; then php -n -r 'echo unpack("P", substr(file_get_contents($argv[1]), 72, 8))[1];' "$1"; else echo "-"; fi; }
rc=0
ok()  { echo "  OK: $*"; }
bad() { echo "  BŁĄD: $*"; rc=1; }
check() { local msg=$1; shift; if "$@"; then ok "$msg"; else bad "$msg"; fi; }
no_tmp_left() { [ -z "$(ls "$D"/*.tmp 2>/dev/null)" ]; }

echo "PHP $(php -n -r 'echo PHP_VERSION;') ext=$EXT"
echo "== 1. alice: ring-%u powstaje przy pierwszym śledzonym żądaniu, 0600, właściciel alice"
S=$(php_as alice -d "phpray.shm_path=$D/ring-%u" -r 'echo json_encode(phpray_ring_stats(), JSON_UNESCAPED_SLASHES);')
echo "  stats: $S"
check "ścieżka rozwinięta do $D/ring-1001, per_user=true" grep -q "\"path\":\"$D/ring-1001\",\"per_user\":true" <<<"$S"
check "plik istnieje po zakończeniu procesu" test -f "$D/ring-1001"
check "tryb 600, właściciel alice (jest: $(stat -c '%a %U' "$D/ring-1001" 2>/dev/null))" test "$(stat -c '%a %U' "$D/ring-1001" 2>/dev/null)" = "600 alice"
check "ślad zapisany w RSHUTDOWN: record_count=1 (jest: $(records "$D/ring-1001"))" test "$(records "$D/ring-1001")" = 1
check "brak pozostawionych plików tymczasowych" no_tmp_left

echo "== 2. drugi proces alice podpina istniejący ring"
S=$(php_as alice -d "phpray.shm_path=$D/ring-%u" -r 'echo phpray_ring_stats()["records"];')
check "widzi 1 rekord poprzedniego procesu przed własnym zapisem (jest: $S)" test "$S" = 1
check "po drugim procesie record_count=2 (jest: $(records "$D/ring-1001"))" test "$(records "$D/ring-1001")" = 2

echo "== 3. bob: własny ring, ring alice niedostępny"
php_as bob -d "phpray.shm_path=$D/ring-%u" -r 'usleep(1000);'
check "ring-1002: 600 bob (jest: $(stat -c '%a %U' "$D/ring-1002" 2>/dev/null))" test "$(stat -c '%a %U' "$D/ring-1002" 2>/dev/null)" = "600 bob"
check "ring-1002 ma 1 rekord (jest: $(records "$D/ring-1002"))" test "$(records "$D/ring-1002")" = 1
check "ring-1001 bez zmian: 600 alice, 2 rekordy" test "$(stat -c '%a %U' "$D/ring-1001")" = "600 alice" -a "$(records "$D/ring-1001")" = 2
if runuser -u bob -- cat "$D/ring-1001" >/dev/null 2>&1; then bad "bob odczytał ring alice"; else ok "bob nie może odczytać ringu alice"; fi

echo "== 4. plik podstawiony przez boba pod nazwą ring-1003 (0666, poprawny rozmiar): carol go nie używa"
runuser -u bob -- php -n -r 'file_put_contents($argv[1], str_repeat("\0", 1048576)); chmod($argv[1], 0666);' "$D/ring-1003"
H1=$(sha256sum "$D/ring-1003" | cut -d' ' -f1)
S=$(php_as carol -d "phpray.shm_path=$D/ring-%u" -r 'var_export(phpray_ring_stats());')
check "carol: phpray_ring_stats() === false (jest: $S)" test "$S" = false
check "plik boba nietknięty (właściciel bob, ta sama suma)" test "$(stat -c %U "$D/ring-1003")" = bob -a "$(sha256sum "$D/ring-1003" | cut -d' ' -f1)" = "$H1"
check "brak pozostawionych plików tymczasowych" no_tmp_left

echo "== 5. symlink podstawiony przez boba pod nazwą ring-1004: dave go nie używa"
runuser -u bob -- ln -s /etc/hostname "$D/ring-1004"
SZ=$(stat -c %s /etc/hostname)
S=$(php_as dave -d "phpray.shm_path=$D/ring-%u" -r 'var_export(phpray_ring_stats());')
check "dave: phpray_ring_stats() === false (jest: $S)" test "$S" = false
check "symlink i jego cel nietknięte" test -L "$D/ring-1004" -a "$(stat -c %s /etc/hostname)" = "$SZ"

echo "== 6. %g: egid; 0600 z %u, 0660 z samym %g"
php_as alice -d "phpray.shm_path=$D/ring-%u-%g" -r 'usleep(1000);'
check "ring-1001-1001: 600 alice (jest: $(stat -c '%a %U' "$D/ring-1001-1001" 2>/dev/null))" test "$(stat -c '%a %U' "$D/ring-1001-1001" 2>/dev/null)" = "600 alice"
php_as alice -d "phpray.shm_path=$D/grp-%g" -r 'usleep(1000);'
check "grp-1001: 660 alice:alice (jest: $(stat -c '%a %U:%G' "$D/grp-1001" 2>/dev/null))" test "$(stat -c '%a %U:%G' "$D/grp-1001" 2>/dev/null)" = "660 alice:alice"

echo "== 7. ścieżka bez placeholdera: jak dotąd (wspólny ring, nie 0600, kasowany przez twórcę)"
S=$(php_as alice -d "phpray.shm_path=$D/classic" -r '$s = phpray_ring_stats(); echo $s["path"], " ", substr(sprintf("%o", fileperms($s["path"])), -3), " ", var_export($s["per_user"], true);')
echo "  w trakcie: $S"
check "per_user=false, tryb inny niż 600" bash -c "[[ '$S' == '$D/classic '[0-7][0-7][0-7]' false' && '$S' != *' 600 '* ]]"
check "plik usunięty po zakończeniu procesu-twórcy" test ! -e "$D/classic"

echo "== 8. zmiana euid w jednym procesie (jak mod_ruid2) przepina ring"
S=$(php $OPTS -d "phpray.shm_path=$D/ring-%u" -r '
  if (!function_exists("posix_seteuid")) { echo "SKIP"; exit; }
  posix_seteuid(1001); $a = phpray_ring_stats()["path"];
  posix_seteuid(0); posix_seteuid(1002); $b = phpray_ring_stats()["path"];
  echo "$a $b";')
if [ "$S" = SKIP ]; then echo "  (pominięte: brak ext/posix)"; else
  check "euid 1001 → ring-1001, potem euid 1002 → ring-1002 (jest: $S)" test "$S" = "$D/ring-1001 $D/ring-1002"
  check "ślad procesu trafił do ringu ostatniego użytkownika (bob: 2 rekordy, jest: $(records "$D/ring-1002"))" test "$(records "$D/ring-1002")" = 2
fi

echo "=== wynik: $([ $rc -eq 0 ] && echo OK || echo BŁĄD) ==="; exit $rc
RUN
docker run --rm -v "$PWD/src/extension:/src:ro" -v "$TMP/run.sh:/run.sh:ro" "php:${PHPV}-cli" bash /run.sh
