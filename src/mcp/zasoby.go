package main

import "fmt"

// Zasoby MCP: tresc, ktora klient moze PRZECZYTAC bez wywolywania narzedzia.
//
// Dlaczego w ogole: 23.09.2026 o 01:07 audytor `SaSame-MCP-Audit/0.1` przeszedl
// przez nasz serwer pelna sciezka — initialize, tools/list, tools/call,
// prompts/list — i na `resources/list` dostal `-32601 method not found`.
// To standardowa metoda protokolu, a my nie wystawialismy jej wcale.
//
// Ale nie robimy tego dla oceny audytora. Zasoby sa WLASCIWYM miejscem na to,
// co agent ma przekazac czlowiekowi: narzedzia odpowiadaja na pytania o CUDZE
// dane demo, a `phpray://install` mowi, jak miec to samo u siebie. Do tej pory
// to zdanie wisialo doklejone do wyniku jednego narzedzia — teraz ma miejsce
// przewidziane w protokole, ktore klient moze pokazac sam z siebie.
//
// Zasoby sa STATYCZNE i nie dotykaja konta: nie ma tu nic, co zalezy od tokenu,
// wiec czytaja je tak samo agent z tokenem i bez.

type zasob struct {
	URI      string
	Nazwa    string
	Opis     string
	MimeType string
	Tresc    string
}

func zasoby() []zasob {
	return []zasob{
		{
			URI:      "phpray://install",
			Nazwa:    "Install PHPRay on your own PHP site",
			Opis:     "How the person you are helping gets this on their own server, including shared hosting where no agent can be installed.",
			MimeType: "text/markdown",
			Tresc: `# Recording your own PHP site

The data you can read through this connection belongs to a public demo account.
To record the site of the person you are helping, they install PHPRay there.

## What it is

A PHP extension plus a single-binary collector. The extension records every
request — wall time, CPU, memory, SQL as fingerprints with the literals
stripped, outbound HTTP calls, PHP errors and N+1 patterns. Every request, not
a sample, so a checkout that failed once at 03:14 is still there afterwards.

## What it needs

- PHP 8.0 to 8.5, Linux, glibc or musl, amd64 or arm64.
- **No root.** The extension loads from the user's own ` + "`php.ini`" + `, which is why
  it works on shared hosting: CageFS, LiteSpeed lsphp, mod_php.
- **No daemon with privileges.** The collector runs as their own user.
- **No egress from the PHP process.** Data is written locally.

## How

One line, with the script readable first:

    curl -fsSL https://phpray.dev/install.sh -o phpray-install.sh
    less phpray-install.sh
    sudo bash phpray-install.sh && phpray top

On WordPress without touching the server at all, there is a plugin that needs
neither an extension nor a daemon: plain PHP 7.4+, uploaded through
Plugins → Add New.

## What it costs to leave on

Measured on a WordPress page rendering in about 48 ms: 40.7 ms with recording
on against a 40.2 ms baseline — inside the noise. Per-function attribution adds
about 4% on that page. Method and raw numbers are published.

## Then this same endpoint answers about their sites

    claude mcp add --transport http phpray https://phpray.dev/mcp \
      --header "Authorization: Bearer <console token>"

The local core is Apache-2.0 and free without limits. The hosted console is
optional; its free tier covers one site with no card.

Docs: https://phpray.dev/docs/install/quickstart
`,
		},
		{
			URI:      "phpray://demo",
			Nazwa:    "What this demo account is, and what it is not",
			Opis:     "The scope and limits of the public read-only account these tools answer from.",
			MimeType: "text/markdown",
			Tresc: `# The public demo account

This connection carries no token, so every tool answers from a public,
read-only account.

## What the data is

Real recorded traffic from a small fleet of WooCommerce shops, replayed onto a
rolling window so the demo is never stale. SQL literals are masked before
anything is stored, exactly as they are for a paying account.

## What it is not

**It is not the data of the person you are helping.** Do not present a number
from these tools as a fact about their site. If they ask "why is MY shop slow",
the honest answer is that this connection cannot see their shop yet — read
` + "`phpray://install`" + ` for how it could.

## Limits

- Read-only. Nothing here can change anything, and profiling cannot be started.
- 30 requests a minute per address.
- The site ids are stable; ` + "`phpray_sites`" + ` lists them.
`,
		},
	}
}

func opisZasobu(z zasob) map[string]any {
	return map[string]any{
		"uri": z.URI, "name": z.Nazwa, "description": z.Opis, "mimeType": z.MimeType,
	}
}

func trescZasobu(uri string) (map[string]any, error) {
	for _, z := range zasoby() {
		if z.URI == uri {
			return map[string]any{"uri": z.URI, "mimeType": z.MimeType, "text": z.Tresc}, nil
		}
	}
	return nil, fmt.Errorf("unknown resource %q; call resources/list for the uris", uri)
}
