#!/usr/bin/env bash
# Czy `phpray-collector update` naprawde sciagnie i podmieni plik binarny.
#
# Testuje przeciwko ZYWEMU serwerowi wydan (phpray.dev/dl), bo to jedyne
# miejsce, gdzie klient bedzie go szukal. Binarium budowane z wersja 0.0.1,
# zeby mialo co aktualizowac — inaczej nie da sie tego sprawdzic, dopoki
# nie wyjdzie nastepne wydanie (ten sam problem zaplonu co przy wtyczce WP).
set -uo pipefail
cd "$(dirname "$0")/.."
ZLE=0
sprawdz() { if [ "$2" = "$3" ]; then printf '  OK   %-46s %s\n' "$1" "$3"
            else printf '  ZLE  %-46s oczekiwano %s, jest %s\n' "$1" "$2" "$3"; ZLE=$((ZLE+1)); fi; }

TAG=$(curl -sS -m 20 https://phpray.dev/dl/LATEST | tr -d '\r\n')
[ -n "$TAG" ] || { echo "PRZERWANE: nie da sie odczytac /dl/LATEST" >&2; exit 2; }
echo "serwer wydan podaje: $TAG"

ROBOCZY=$(mktemp -d); trap 'rm -rf "$ROBOCZY"' EXIT
cp -a src/collector "$ROBOCZY/kod"
sed -i 's/^const version = "[0-9.]*"/const version = "0.0.1"/' "$ROBOCZY/kod/main.go"
grep -q 'const version = "0.0.1"' "$ROBOCZY/kod/main.go" || { echo "PRZERWANE: nie podmienilem wersji" >&2; exit 2; }
mkdir -p "$ROBOCZY/bin"
( cd "$ROBOCZY/kod" && go build -o "$ROBOCZY/bin/phpray-collector" . ) || { echo "PRZERWANE: build padl" >&2; exit 2; }
sprawdz "binarium udaje stara wersje" "phpray-collector v0.0.1" "$("$ROBOCZY/bin/phpray-collector" version)"

echo "== 1. samo sprawdzenie nie rusza pliku"
PRZED=$(sha256sum "$ROBOCZY/bin/phpray-collector" | cut -d' ' -f1)
WY=$("$ROBOCZY/bin/phpray-collector" update --check 2>&1)
echo "$WY" | grep -q "$TAG" && sprawdz "--check widzi nowe wydanie" "1" "1" || { sprawdz "--check widzi nowe wydanie" "1" "0"; echo "    powiedzialo: $WY"; }
sprawdz "--check NIE podmienil pliku" "$PRZED" "$(sha256sum "$ROBOCZY/bin/phpray-collector" | cut -d' ' -f1)"

echo "== 2. brak prawa zapisu mowi, co zrobic"
chmod 555 "$ROBOCZY/bin"
WY=$("$ROBOCZY/bin/phpray-collector" update 2>&1); KOD=$?
chmod 755 "$ROBOCZY/bin"
sprawdz "konczy sie bledem" "1" "$KOD"
echo "$WY" | grep -q "sudo" && sprawdz "podpowiada sudo z pelna sciezka" "1" "1" || sprawdz "podpowiada sudo z pelna sciezka" "1" "0"

echo "== 3. prawdziwa aktualizacja z zywego serwera"
WY=$("$ROBOCZY/bin/phpray-collector" update 2>&1); KOD=$?
echo "$WY" | sed 's/^/    /'
sprawdz "konczy sie powodzeniem" "0" "$KOD"
echo "$WY" | grep -q "suma kontrolna zgodna" && sprawdz "suma kontrolna sprawdzona" "1" "1" || sprawdz "suma kontrolna sprawdzona" "1" "0"
sprawdz "plik faktycznie podmieniony" "0" "$([ "$(sha256sum "$ROBOCZY/bin/phpray-collector" | cut -d' ' -f1)" != "$PRZED" ] && echo 0 || echo 1)"
sprawdz "nowe binarium podaje wersje wydania" "phpray-collector ${TAG/v/v}" "$("$ROBOCZY/bin/phpray-collector" version)"
sprawdz "plik jest wykonywalny" "1" "$([ -x "$ROBOCZY/bin/phpray-collector" ] && echo 1 || echo 0)"
sprawdz "kopia poprzedniej wersji lezy obok" "1" "$([ -f "$ROBOCZY/bin/phpray-collector.poprzedni" ] && echo 1 || echo 0)"
sprawdz "kopia to STARA wersja" "phpray-collector v0.0.1" "$(chmod +x "$ROBOCZY/bin/phpray-collector.poprzedni" 2>/dev/null; "$ROBOCZY/bin/phpray-collector.poprzedni" version 2>/dev/null)"

echo "== 4. pobrane binarium naprawde dziala"
sprawdz "odpowiada na --version" "0" "$("$ROBOCZY/bin/phpray-collector" version >/dev/null 2>&1; echo $?)"
"$ROBOCZY/bin/phpray-collector" update --check 2>&1 | grep -q "masz najnowsze" \
  && sprawdz "po aktualizacji nie proponuje kolejnej" "1" "1" || sprawdz "po aktualizacji nie proponuje kolejnej" "1" "0"

echo
[ "$ZLE" -eq 0 ] && echo "AKTUALIZACJA KOLEKTORA: wszystko przeszlo" || echo "AKTUALIZACJA KOLEKTORA: $ZLE bledow"
exit "$ZLE"
