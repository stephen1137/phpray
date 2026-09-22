set -eu
cp -r /src /build && cd /build && phpize >/dev/null 2>&1 && ./configure --enable-phpray >/dev/null 2>&1
make -j"$(nproc)" >/tmp/m.log 2>&1 || { echo "BŁĄD"; grep -E " error" /tmp/m.log | head -5; exit 1; }
cp modules/phpray.so /out/phpray.so
php -v | head -1
php --ini | grep -i 'extension_dir' || php -i | grep '^extension_dir'
