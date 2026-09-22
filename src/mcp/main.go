package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

var version = "0.15.4"

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
