#!/usr/bin/env node
// Pobiera binarkę phpray-mcp dla bieżącej platformy do bin/.
//
// Pakiet npm jest tu tylko opakowaniem: sam serwer to jeden statyczny plik
// w Go, publikowany razem z wydaniem i sumami kontrolnymi. Wersja pakietu
// wyznacza wydanie, z którego bierzemy binarkę — żadnego "latest", żeby
// npm install dwa razy z rzędu dawał ten sam plik.
'use strict';
const fs = require('fs');
const path = require('path');
const https = require('https');
const crypto = require('crypto');

const wersja = require('./package.json').version;
const baza = `https://phpray.dev/dl/v${wersja}`;

const plat = { linux: 'linux', darwin: 'darwin' }[process.platform];
const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
if (!plat || !arch) {
  console.error(`phpray-mcp: no prebuilt binary for ${process.platform}/${process.arch}.`);
  console.error('Build it from source: https://github.com/stephen1137/phpray/tree/main/src/mcp');
  process.exit(0); // nie wywracamy instalacji całego projektu użytkownika
}

const nazwa = `phpray-mcp-${plat}-${arch}`;
const cel = path.join(__dirname, 'bin', 'phpray-mcp-bin');

function pobierz(url, cb, zostalo = 5) {
  https.get(url, (res) => {
    if ([301, 302, 307, 308].includes(res.statusCode) && res.headers.location && zostalo > 0) {
      res.resume();
      return pobierz(new URL(res.headers.location, url).href, cb, zostalo - 1);
    }
    if (res.statusCode !== 200) {
      res.resume();
      return cb(new Error(`${url} → HTTP ${res.statusCode}`));
    }
    const kawalki = [];
    res.on('data', (c) => kawalki.push(c));
    res.on('end', () => cb(null, Buffer.concat(kawalki)));
  }).on('error', cb);
}

pobierz(`${baza}/SHA256SUMS`, (err, sumy) => {
  if (err) return zakoncz(err);
  const linia = sumy.toString().split('\n').find((l) => l.trim().endsWith(' ' + nazwa) || l.trim().endsWith('*' + nazwa));
  if (!linia) return zakoncz(new Error(`${nazwa} not listed in SHA256SUMS`));
  const oczekiwana = linia.trim().split(/\s+/)[0];

  pobierz(`${baza}/${nazwa}`, (err2, bin) => {
    if (err2) return zakoncz(err2);
    const suma = crypto.createHash('sha256').update(bin).digest('hex');
    if (suma !== oczekiwana) return zakoncz(new Error(`checksum mismatch for ${nazwa}`));
    fs.mkdirSync(path.dirname(cel), { recursive: true });
    fs.writeFileSync(cel, bin, { mode: 0o755 });
    console.log(`phpray-mcp ${wersja} (${plat}/${arch}) installed, checksum verified.`);
  });
});

function zakoncz(e) {
  console.error('phpray-mcp: could not fetch the binary:', e.message);
  console.error(`Download it by hand from ${baza}/ and put it in ${cel}`);
  process.exit(0);
}
