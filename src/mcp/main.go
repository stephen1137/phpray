package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

var version = "0.15.9"

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "-v", "--version":
			fmt.Println("phpray-mcp " + version)
			return
		case "-h", "--help":
			pomoc()
			return
		}
	}

	token := strings.TrimSpace(os.Getenv("PHPRAY_TOKEN"))
	sekret := strings.TrimSpace(os.Getenv("PHPRAY_COLLECTOR_SECRET"))
	baza := strings.TrimSpace(os.Getenv("PHPRAY_URL"))

	// Tryb wybiera się sam: token konsoli oznacza chmurę, wszystko inne
	// oznacza kolektor na tej maszynie. PHPRAY_MODE nadpisuje ten wybór.
	tryb := strings.ToLower(strings.TrimSpace(os.Getenv("PHPRAY_MODE")))
	if tryb == "" {
		if token != "" {
			tryb = "cloud"
		} else {
			tryb = "local"
		}
	}

	// Transport HTTP wybiera się osobno od trybu danych: PHPRAY_HTTP_ADDR
	// znaczy „nasłuchuj", brak tej zmiennej znaczy „mów po stdio".
	if adres := strings.TrimSpace(os.Getenv("PHPRAY_HTTP_ADDR")); adres != "" {
		if baza == "" {
			baza = "https://app.phpray.dev"
		}
		demo := strings.TrimSpace(os.Getenv("PHPRAY_DEMO_TOKEN"))
		// Trwaly dziennik wywolan. Bez niego kazde wdrozenie kasowalo historie
		// tego, kto uzywa publicznego serwera — a to jedyny kanal, ktory
		// naprawde przyprowadza nam obcych ludzi. Brak zmiennej = piszemy
		// tylko na stderr, jak dotad.
		if d := strings.TrimSpace(os.Getenv("PHPRAY_DZIENNIK")); d != "" {
			// Nieudany dziennik NIE MOZE zatrzymac serwera. 23.09.2026
			// wdrozylem to z os.Exit(1) i publiczny endpoint wpadl w petle
			// restartow — 502 dla kazdego, bo kontener dziala jako nonroot
			// i nie mial prawa pisac do katalogu Caddy'ego. Dziennik sluzy
			// do MIERZENIA kanalu, a nie do jego dzialania; brak pomiaru
			// jest zawsze mniej kosztowny niz brak uslugi.
			if err := otworzDziennik(d); err != nil {
				fmt.Fprintln(os.Stderr, "phpray-mcp: dziennik wylaczony,", err)
			} else {
				fmt.Fprintln(os.Stderr, "phpray-mcp: dziennik wywolan w", d)
			}
		}
		fmt.Fprintf(os.Stderr, "phpray-mcp %s: HTTP na %s, konsola %s, konto demo: %v\n",
			version, adres, baza, demo != "")
		if err := serwujHTTP(adres, baza, demo); err != nil {
			fmt.Fprintln(os.Stderr, "phpray-mcp:", err)
			os.Exit(1)
		}
		return
	}

	s := &server{out: bufio.NewWriter(os.Stdout)}
	switch tryb {
	case "cloud":
		if token == "" {
			fmt.Fprintln(os.Stderr, "phpray-mcp: cloud mode needs PHPRAY_TOKEN (console token from Settings).")
			os.Exit(2)
		}
		if baza == "" {
			baza = "https://app.phpray.dev"
		}
		s.tools = zbudujNarzedzia(nowyKlient(baza, token))
	case "local":
		if baza == "" {
			baza = "http://127.0.0.1:9191"
		}
		jwt := tokenLokalny{sekret: sekret, rola: "admin", sub: "phpray-mcp"}
		s.tools = narzedziaLokalne(nowyKlientZTokenem(baza, jwt.podpisz))
	default:
		fmt.Fprintf(os.Stderr, "phpray-mcp: unknown PHPRAY_MODE %q, use cloud or local\n", tryb)
		os.Exit(2)
	}
	s.run(os.Stdin)
}

func pomoc() {
	fmt.Println(`phpray-mcp ` + version + ` — Model Context Protocol server for PHPRay

It lets an AI coding agent ask what is slow in a PHP application, using your
own console token, so it sees exactly the sites you see.

Two modes, chosen automatically.

Cloud — your sites across every server, scoped to your console token:
  PHPRAY_TOKEN   console token from PHPRay Cloud (Settings)
  PHPRAY_URL     console URL (default https://app.phpray.dev)

Local — the collector on this machine, no account needed:
  PHPRAY_URL                 collector URL (default http://127.0.0.1:9191)
  PHPRAY_COLLECTOR_SECRET    the [auth] secret from /etc/phpray/collector.toml,
                             only if the collector has one

PHPRAY_MODE=cloud|local overrides the choice.

With Claude Code:
  claude mcp add phpray --env PHPRAY_TOKEN=<console token> -- phpray-mcp
  claude mcp add phpray-local -- phpray-mcp

The server speaks JSON-RPC over stdin and stdout; run it from an agent, not by
hand. Everything is read-only except phpray_profile_url.`)
}
