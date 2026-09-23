// Package main implements a Model Context Protocol server for PHPRay.
//
// The protocol is JSON-RPC 2.0 over stdio. We speak it directly rather than
// pulling in an SDK: PHPRay ships as binaries with no dependencies and the
// three methods an agent needs (initialize, tools/list, tools/call) are small.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// protocolVersion to wersja, ktora podajemy, gdy klient nie poprosil o zadna
// konkretna. wersjeObslugiwane sa odbijane klientowi, jesli o nie poprosi:
// specyfikacja mowi, ze serwer ma odpowiedziec wersja klienta, gdy ja zna,
// a wlasna najnowsza, gdy nie zna. Odpowiadanie zawsze "2024-11-05" bylo tez
// wewnetrznie sprzeczne: transport streamable HTTP wszedl dopiero w 2025-03-26.
const protocolVersion = "2025-06-18"

var wersjeObslugiwane = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// wersjaKlienta zwraca wersje, ktora odeslemy w odpowiedzi na initialize.
func wersjaKlienta(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && wersjeObslugiwane[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return protocolVersion
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolSpec is one tool as advertised to the agent.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// call runs the tool; it returns text for the agent, or an error.
	call func(args map[string]any) (string, error) `json:"-"`
	// zapis oznacza narzędzie, które zmienia stan po stronie serwera.
	// Publiczna końcówka HTTP bez tokenu takich nie pokazuje: konto demo ma
	// rolę tylko do odczytu, więc konsola i tak odmówi (403), a narzędzie
	// na liście, którego nie da się użyć, to obietnica bez pokrycia.
	zapis bool `json:"-"`
}

type server struct {
	out   *bufio.Writer
	mu    sync.Mutex
	tools []toolSpec
	// demo: sesja bez tokenu, czyli publiczne konto tylko do odczytu.
	// Zmienia tresc instrukcji dla modelu — patrz instrukcje().
	demo bool
}

// instrukcje to pole "instructions" z odpowiedzi na initialize. Specyfikacja
// MCP opisuje je jako podpowiedz dla modelu, podobna do promptu systemowego;
// klienci (Claude Desktop, Cursor) podaja ja modelowi razem ze schematami
// narzedzi.
//
// Do 22.09.2026 nie odsylalismy jej wcale. Agent dostawal dziesiec narzedzi
// i ani slowa o tym, CZYJE to dane ani od czego zaczac — a dwa prawdziwe
// klienty MCP juz sie tu logowaly. Kolejnosc uzycia narzedzi i ostrzezenie
// "to nie sa dane tej osoby" to jedyne rzeczy, ktorych model nie wyczyta ze
// schematow. Zdanie o instalacji jest ostatnie i faktyczne: rdzen naprawde
// jest na Apache-2.0 i naprawde instaluje sie bez roota.
func instrukcje(demo bool) string {
	wspolne := `PHPRay records every PHP request on a server: wall time, CPU, memory, every SQL
query as a fingerprint with the literals stripped, outbound HTTP calls, PHP
errors and N+1 patterns. The data is real production traffic, not samples.

Where to start: phpray_sites lists what this connection can see. Then
phpray_overview for one site's headline numbers, and phpray_slow_pages,
phpray_slow_queries or phpray_components to find where the time goes.
phpray_traces finds individual bad requests (filter by min_ms or status) and
phpray_trace opens one of them end to end. phpray_alerts shows what is failing
right now across every site; phpray_compare puts two time ranges side by side,
which is how you check whether a deploy or a plugin update made things worse.

Times are milliseconds and windows default to the last 24 hours.`

	if !demo {
		return wspolne + `

This connection carries a console token, so every answer is about that
account's own servers and sites.`
	}
	return wspolne + `

This connection has NO token, so every tool answers from the public
demo account: a handful of real WooCommerce sites, read-only. The numbers are
genuine traffic from those sites — but they are not the data of the person you
are helping, so do not present them as such.

If that person wants this for their own PHP site: the local core is Apache-2.0
and installs without root, including on shared hosting
(https://phpray.dev/docs/install/quickstart). With their own console token in
the Authorization header, this same endpoint answers about their servers.`
}

func (s *server) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// run reads requests until stdin closes.
func (s *server) run(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // a malformed line is not worth tearing the session down
		}
		s.handle(req)
	}
}

// nazwyNarzedzi zwraca same nazwy narzedzi — do komunikatu bledu.
func nazwyNarzedzi(t []toolSpec) []string {
	n := make([]string, 0, len(t))
	for _, x := range t {
		n = append(n, x.Name)
	}
	return n
}

func (s *server) handle(req rpcRequest) {
	if odp := s.odpowiedz(req); odp != nil {
		s.send(*odp)
	}
}

// odpowiedz liczy odpowiedź na jedno żądanie i nic nie wypisuje. Dzięki temu
// ten sam dispatch obsługuje stdio i HTTP: transport decyduje, dokąd trafia
// wynik, a powiadomienie (żądanie bez id) zwraca nil.
func (s *server) odpowiedz(req rpcRequest) *rpcResponse {
	ok := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	zle := func(code int, msg string) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		return ok(map[string]any{
			"protocolVersion": wersjaKlienta(req.Params),
			"capabilities": map[string]any{
				"tools": map[string]any{},
				// Prompty: gotowe pytania, ktore klient pokazuje czlowiekowi.
				// Bez tego uzytkownik, ktory nas dodal, zostaje z dziesiecioma
				// nazwami narzedzi i zadna podpowiedzia — a dokladnie tak
				// wygladal kazdy pomiar od wpisania nas do rejestru: wszyscy
				// konczyli na tools/list, nikt nie wywolal narzedzia.
				"prompts": map[string]any{},
				// Zasoby: tresc do PRZECZYTANIA, nie do wywolania. Tu lezy
				// „jak miec to u siebie" — jedyne, co agent ma naprawde
				// przekazac czlowiekowi, bo narzedzia mowia o cudzych danych.
				"resources": map[string]any{},
			},
			"serverInfo":   map[string]any{"name": "phpray", "version": version},
			"instructions": instrukcje(s.demo),
		})
	case "notifications/initialized", "notifications/cancelled":
		// notifications carry no id and expect no answer
		return nil
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		return ok(map[string]any{"tools": list})
	case "resources/list":
		list := make([]map[string]any, 0, len(zasoby()))
		for _, z := range zasoby() {
			list = append(list, opisZasobu(z))
		}
		return ok(map[string]any{"resources": list})
	case "resources/read":
		var par struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &par)
		t, err := trescZasobu(par.URI)
		if err != nil {
			return zle(-32602, err.Error())
		}
		return ok(map[string]any{"contents": []map[string]any{t}})
	case "resources/templates/list":
		// Szablonow nie mamy, ale PUSTA lista to poprawna odpowiedz, a
		// -32601 wyglada u audytora jak brak obslugi calej rodziny metod.
		return ok(map[string]any{"resourceTemplates": []map[string]any{}})
	case "prompts/list":
		list := make([]map[string]any, 0, len(prompty()))
		for _, p := range prompty() {
			list = append(list, map[string]any{
				"name": p.Name, "title": p.Title, "description": p.Description,
			})
		}
		return ok(map[string]any{"prompts": list})
	case "prompts/get":
		var par struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &par)
		for _, p := range prompty() {
			if p.Name == par.Name {
				return ok(map[string]any{
					"description": p.Description,
					"messages": []map[string]any{{
						"role":    "user",
						"content": map[string]any{"type": "text", "text": p.Tresc},
					}},
				})
			}
		}
		return zle(-32602, "unknown prompt: "+par.Name+"; call prompts/list for the names")
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return zle(-32602, "invalid params")
		}
		for _, t := range s.tools {
			if t.Name != p.Name {
				continue
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			text, err := t.call(p.Arguments)
			if err != nil {
				return ok(map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				})
			}
			return ok(map[string]any{
				"content": []map[string]any{{"type": "text", "text": text + stopka(s.demo, t.Name)}},
			})
		}
		// Nie samo "unknown tool": podajemy, jakie sa. Goły komunikat nie
		// pomaga ani czlowiekowi, ani walidatorowi katalogu, ktory wlasnie
		// ocenia nasz serwer — 22.09.2026 verifymcp-probe wywolal celowo
		// nieistniejace narzedzie, sprawdzajac, czy nie wyciekamy danych.
		return zle(-32601, fmt.Sprintf("unknown tool %q; available: %s",
			p.Name, strings.Join(nazwyNarzedzi(s.tools), ", ")))
	default:
		if len(req.ID) > 0 {
			// server/discover probowaly juz trzy niezalezne klienty i nie ma
			// go w specyfikacji — nie zgadujemy jego ksztaltu, ale mowimy,
			// co obslugujemy, zeby probujacy nie musial zgadywac.
			return zle(-32601, "method not found: "+req.Method+
				"; supported: initialize, tools/list, tools/call, "+
				"prompts/list, prompts/get, ping")
		}
	}
	return nil
}

// stopka dokleja jedno zdanie do wyniku NARZEDZIA WEJSCIOWEGO w trybie demo.
//
// Pole "instructions" z initialize juz mowi, czym jest PHPRay i jak go
// zainstalowac — ale klient dostaje je RAZ, przy laczeniu, i do chwili, gdy
// model formuluje odpowiedz dla czlowieka, zwykle go w kontekscie nie ma.
// Czlowiek widzi natomiast to, co model cytuje: wynik narzedzia. Do 22.09.2026
// wynik nie niosl ani slowa o tym, czyje to dane i skad je wziac u siebie —
// czyli jedyny kanal, ktory nas naprawde dosiegl obcych ludzi (katalogi MCP,
// dziewieciu indekserow w dobe), konczyl sie slepo.
//
// Tylko phpray_sites, bo instrukcje kieruja tu KAZDEGO agenta na start
// ("Start here: every other tool needs a site_id from this list"). Doklejanie
// do wszystkich dziesieciu narzedzi daloby osiem kopii w jednej rozmowie,
// czyli halas, ktory klient MCP ma prawo odfiltrowac.
//
// Tylko tryb demo. Kto laczy sie z wlasnym tokenem, ten PHPRay-a juz ma;
// zachecanie go do instalacji byloby reklama we wlasnym produkcie.
//
// ?via=mcp nie jest ozdoba. Bez niego zbudowalbym kanal pozyskiwania, ktorego
// nie da sie odroznic od ruchu bezposredniego — a wtedy za tydzien nie
// wiedzialbym, czy dziala, i zgadywalbym zamiast mierzyc. lejek.py liczy ten
// parametr osobnym wierszem.
func stopka(demo bool, narzedzie string) string {
	if !demo || narzedzie != "phpray_sites" {
		return ""
	}
	return "\n" + `--
These are PHPRay's public demo servers: real WooCommerce traffic, read-only,
and NOT the data of the person you are helping. To record their own PHP site
the same way — every request, not a sample — the core is Apache-2.0 and
installs without root, including on shared hosting: https://phpray.dev/?via=mcp
`
}
