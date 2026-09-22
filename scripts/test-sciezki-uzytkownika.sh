#!/usr/bin/env bash
# Przechodzi ścieżkę nowego użytkownika od zera w czystym kontenerze i mówi,
# czy każdy jej krok naprawdę działa. Nie testuje kodu — testuje to, co
# zobaczy ktoś, kto nas nie zna.
#
#   scripts/test-sciezki-uzytkownika.sh            # instalacja z phpray.dev
#   ZRODLO=./packaging/install.sh  scripts/…       # instalacja z lokalnego pliku
#
# Powstało po serii rund, w których KAŻDE takie przejście znajdowało usterkę
# niewidoczną w testach jednostkowych: „start the collector manually" bez
# podania polecenia, pięć błędów JSONL na świeżej instalacji, wezwanie do
# płatnego planu prowadzące na polską stronę z angielskiego panelu. Żadnej
# z nich nie złapałby test kodu, bo kod działał poprawnie.
set -uo pipefail
cd "$(dirname "$0")/.."

OBRAZ="${OBRAZ:-debian:12}"
ZRODLO="${ZRODLO:-}"
ROBOCZY="$(mktemp -d)"
trap 'rm -rf "$ROBOCZY"' EXIT

cat > "$ROBOCZY/w-srodku.sh" <<'SCENARIUSZ'
set -u
zle=0
sprawdz() { # sprawdz "opis" "oczekiwane" "otrzymane"
  if [ "$2" = "$3" ]; then printf '  OK   %-46s %s\n' "$1" "$3"
  else printf '  ZLE  %-46s oczekiwano %s, jest %s\n' "$1" "$2" "$3"; zle=$((zle+1)); fi
}
zawiera() { # zawiera "opis" "wzorzec" "plik"
  if grep -q "$2" "$3" 2>/dev/null; then printf '  OK   %-46s\n' "$1"
  else printf '  ZLE  %-46s brak wzorca: %s\n' "$1" "$2"; zle=$((zle+1)); fi
}

apt-get update -qq >/dev/null 2>&1
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates php-cli procps >/dev/null 2>&1

echo "== 1. instalacja"
if [ -f /install.sh ]; then cp /install.sh i.sh; else curl -fsSL https://phpray.dev/install.sh -o i.sh; fi
bash i.sh --yes > /tmp/i.log 2>&1
sprawdz "instalator kończy się bez błędu" "0" "$?"
sprawdz "rozszerzenie załadowane" "1" "$(php -m | grep -c '^phpray$')"

echo "== 2. instrukcje, które da się wykonać dosłownie"
zawiera "bez systemd podaje pełne polecenie" "phpray-collector serve -c" /tmp/i.log
zawiera "nie każe dopisywać drugiej sekcji [cloud]" "ALREADY in /etc/phpray/collector.toml" /tmp/i.log

echo "== 3. kolektor startuje poleceniem z instrukcji"
POLECENIE="$(grep -oP '(?<=\[WARN\]   )phpray-collector serve.*' /tmp/i.log | head -1)"
[ -n "$POLECENIE" ] || POLECENIE="phpray-collector serve -c /etc/phpray/collector.toml -addr 127.0.0.1:9191"
rm -f /tmp/phpray.jsonl /dev/shm/phpray
nohup $POLECENIE > /tmp/col.log 2>&1 &
sleep 5
sprawdz "panel odpowiada" "200" "$(curl -sS -o /dev/null -m 6 -w '%{http_code}' http://127.0.0.1:9191/)"
sprawdz "dziennik startu bez slowa error" "0" "$(grep -ci 'JSONL error' /tmp/col.log)"

echo "== 4. ruch trafia do kolektora"
mkdir -p /srv/www && printf '<?php usleep(random_int(5000,60000)); echo "ok";' > /srv/www/index.php
php -S 127.0.0.1:8080 -t /srv/www >/dev/null 2>&1 &
sleep 2
for i in $(seq 1 20); do curl -sS -o /dev/null "http://127.0.0.1:8080/?i=$i"; done
sleep 10
WIDZI="$(timeout 9 phpray top 2>&1 | grep -oE '[0-9]+ requests in last 60s' | tail -1 | grep -oE '^[0-9]+')"
sprawdz "phpray top widzi 20 zadan" "20" "${WIDZI:-0}"

echo "== 5. panel lokalny kieruje na wlasciwa wersje jezykowa"
curl -sS -m 8 http://127.0.0.1:9191/ -o /tmp/p.html
sprawdz "wezwanie do Cloud na /en/" "1" "$(grep -c 'phpray.dev/en/cloud' /tmp/p.html)"

echo
if [ "$zle" -eq 0 ]; then echo "SCIEZKA UZYTKOWNIKA: wszystko przeszlo"; else echo "SCIEZKA UZYTKOWNIKA: $zle bledow"; fi
exit "$zle"
SCENARIUSZ

MONT=(-v "$ROBOCZY/w-srodku.sh:/t.sh:ro")
if [ -n "$ZRODLO" ]; then
  [ -f "$ZRODLO" ] || { echo "nie ma pliku $ZRODLO" >&2; exit 2; }
  MONT+=(-v "$(readlink -f "$ZRODLO"):/install.sh:ro")
  echo "instalator: $ZRODLO (lokalny)"
else
  echo "instalator: https://phpray.dev/install.sh (opublikowany)"
fi

docker run --rm "${MONT[@]}" "$OBRAZ" bash /t.sh
