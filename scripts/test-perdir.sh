#!/usr/bin/env bash
# Test PHP_INI_PERDIR: globalne phpray.enabled=0, katalog docker/test/perdir/ włącza phpray przez .htaccess
# (Apache + mod_php). Sprawdza: hooki i observer zainstalowane w MINIT niezależnie od enabled
# (ślad z /perdir/ ma file_count i components), a żądanie poza katalogiem nie jest śledzone.
# Użycie: scripts/test-perdir.sh [wersja-php=8.3]
set -uo pipefail
cd "$(dirname "$0")/.."
PHPV=${1:-8.3}
IMG=phpray-apache-test:$PHPV
docker build -q -f docker/Dockerfile.apache-test --build-arg PHP_VERSION=$PHPV -t $IMG . >/dev/null || { echo "build BŁĄD"; exit 1; }
NAME=phpray-perdir-test-$$
docker run -d --name $NAME -v "$PWD/docker/test:/var/www/html:ro" \
  -e PHPRAY_INI="phpray.enabled=0
phpray.profile_mode=off
phpray.output_path=/tmp/phpray.jsonl" $IMG sh -c 'printf "%s\n" "$PHPRAY_INI" > /usr/local/etc/php/conf.d/zz-test.ini && exec apache2-foreground' >/dev/null
trap 'docker rm -f $NAME >/dev/null 2>&1' EXIT
for i in $(seq 1 50); do docker exec $NAME sh -c 'curl -sf -o /dev/null http://127.0.0.1/health.php' 2>/dev/null && break; sleep 0.3; done
rc=0
echo "== php.ini: enabled=0 → /perdir_off.php nie śledzone"
OFF=$(docker exec $NAME sh -c 'curl -s -D /tmp/h.txt http://127.0.0.1/perdir_off.php; echo; grep -ci x-phpray-id /tmp/h.txt')
echo "$OFF" | head -1
echo "$OFF" | tail -1 | grep -q '^0$' && echo "  OK: brak nagłówka X-PHPRay-ID" || { echo "  BŁĄD: nagłówek obecny"; rc=1; }
echo "$OFF" | head -1 | grep -q '"tracing":false' && echo "  OK: phpray_is_tracing()=false" || { echo "  BŁĄD: tracing=true"; rc=1; }
echo "== .htaccess: php_value phpray.enabled 1 → /perdir/ śledzone z profilem"
ON=$(docker exec $NAME sh -c 'curl -s -D /tmp/h.txt http://127.0.0.1/perdir/; echo; grep -ci x-phpray-id /tmp/h.txt')
echo "$ON" | head -1
echo "$ON" | head -1 > /tmp/perdir-on-$$.json
echo "$ON" | tail -1 | grep -q '^1$' && echo "  OK: nagłówek X-PHPRay-ID" || { echo "  BŁĄD: brak nagłówka"; rc=1; }
echo "$ON" | head -1 | grep -q '"tracing":true' && echo "  OK: phpray_is_tracing()=true" || { echo "  BŁĄD: tracing=false"; rc=1; }
sleep 0.5
docker exec $NAME sh -c 'cat /tmp/phpray.jsonl 2>/dev/null' > /tmp/perdir-$$.jsonl
N=$(grep -c '' /tmp/perdir-$$.jsonl)
echo "== JSONL: $N rekordów (oczekiwany 1 — tylko /perdir/)"
[ "$N" = 1 ] || { echo "  BŁĄD: liczba rekordów $N"; rc=1; }
python3 - /tmp/perdir-$$.jsonl /tmp/perdir-on-$$.json <<'PY' || rc=1
import json, sys
ok = True
expected = json.load(open(sys.argv[2])).get('expected', {})
for line in open(sys.argv[1]):
    t = json.loads(line)
    comps = {c['name']: c for c in t.get('components', [])}
    checks = {
        'uri=/perdir/': t.get('uri') == '/perdir/',
        'file hook (file_count>0)': t.get('file_count', 0) > 0,
        'profiled=1': t.get('profiled') == 1,
        'component plugins/a': 'plugins/a' in comps,
        'calls plugins/a == counted in PHP (%s)' % expected.get('plugins/a'): comps.get('plugins/a', {}).get('calls') == expected.get('plugins/a'),
        'no wp-includes component': 'wp-includes' not in comps,
    }
    for k, v in checks.items():
        print(('  OK: ' if v else '  BŁĄD: ') + k)
        ok = ok and v
sys.exit(0 if ok else 1)
PY
rm -f /tmp/perdir-$$.jsonl /tmp/perdir-on-$$.json
echo "=== wynik: $([ $rc -eq 0 ] && echo OK || echo BŁĄD) ==="
exit $rc
