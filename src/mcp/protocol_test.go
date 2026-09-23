package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// przepusc podaje serwerowi linie żądań i zwraca odpowiedzi.
func przepusc(t *testing.T, s *server, linie ...string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	s.out = bufio.NewWriter(&buf)
	s.run(strings.NewReader(strings.Join(linie, "\n") + "\n"))
	var odp []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("odpowiedź nie jest JSON-em: %q", l)
		}
		odp = append(odp, m)
	}
	return odp
}

func TestUzgodnienieIListaNarzedzi(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x"))}
	odp := przepusc(t,
		s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(odp) != 2 {
		t.Fatalf("powiadomienie nie powinno dostać odpowiedzi; dostałem %d", len(odp))
	}
	wynik := odp[0]["result"].(map[string]any)
	if wynik["protocolVersion"] != protocolVersion {
		t.Fatalf("wersja protokołu: %v", wynik["protocolVersion"])
	}
	narzedzia := odp[1]["result"].(map[string]any)["tools"].([]any)
	if len(narzedzia) < 10 {
		t.Fatalf("za mało narzędzi: %d", len(narzedzia))
	}
	for _, n := range narzedzia {
		m := n.(map[string]any)
		if m["name"] == "" || m["description"] == "" || m["inputSchema"] == nil {
			t.Fatalf("niekompletne narzędzie: %v", m)
		}
	}
}

func TestNieznaneNarzedzieToBlad(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x"))}
	odp := przepusc(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"nie_ma","arguments":{}}}`)
	if odp[0]["error"] == nil {
		t.Fatal("nieznane narzędzie powinno zwrócić błąd protokołu")
	}
}

func TestBrakWymaganegoArgumentuJestCzytelny(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x"))}
	odp := przepusc(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"phpray_overview","arguments":{}}}`)
	w := odp[0]["result"].(map[string]any)
	if w["isError"] != true {
		t.Fatal("brak site_id powinien być zgłoszony jako błąd narzędzia")
	}
	tekst := w["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(tekst, "phpray_sites") {
		t.Fatalf("komunikat powinien kierować do phpray_sites, jest: %q", tekst)
	}
}

func TestSerwerPodajeTokenITnieDuzeOdpowiedzi(t *testing.T) {
	var naglowek string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		naglowek = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"sites":[{"site":{"id":"abc","host":"sklep.pl","app":"wordpress"},"server_name":"h2","stats":{"requests":10,"p95_ms":250.5,"error_rate":0.1,"db_ms_share":0.3}}]}`))
	}))
	defer srv.Close()
	s := &server{tools: zbudujNarzedzia(nowyKlient(srv.URL, "phtk_tajne"))}
	odp := przepusc(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"phpray_sites","arguments":{}}}`)
	if naglowek != "Bearer phtk_tajne" {
		t.Fatalf("token nie trafił do nagłówka: %q", naglowek)
	}
	tekst := odp[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, oczekiwane := range []string{"abc", "sklep.pl", "wordpress", "250.5"} {
		if !strings.Contains(tekst, oczekiwane) {
			t.Fatalf("w tabeli brakuje %q:\n%s", oczekiwane, tekst)
		}
	}
}

func TestBladKonsoliJestWyjasniony(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	s := &server{tools: zbudujNarzedzia(nowyKlient(srv.URL, "zly"))}
	odp := przepusc(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"phpray_sites","arguments":{}}}`)
	w := odp[0]["result"].(map[string]any)
	tekst := w["content"].([]any)[0].(map[string]any)["text"].(string)
	if w["isError"] != true || !strings.Contains(tekst, "PHPRAY_TOKEN") {
		t.Fatalf("401 powinien tłumaczyć, co zrobić; jest: %q", tekst)
	}
}

// Do 22.09.2026 initialize nie odsylalo pola "instructions" wcale: agent
// dostawal dziesiec narzedzi i ani slowa o tym, czyje to dane ani od czego
// zaczac. Dwa prawdziwe klienty MCP juz sie tu wtedy logowaly.
func TestInitializeNiesieInstrukcjeDlaModelu(t *testing.T) {
	for _, p := range []struct {
		nazwa string
		demo  bool
		musi  []string
		niemo []string
	}{
		{
			nazwa: "bez tokenu",
			demo:  true,
			musi: []string{"phpray_sites", "phpray_trace", "NO token", "demo account",
				"not the data of the person", "Apache-2.0", "phpray.dev/docs/install/quickstart"},
			niemo: []string{"carries a console token"},
		},
		{
			nazwa: "z tokenem",
			demo:  false,
			musi:  []string{"phpray_sites", "carries a console token"},
			niemo: []string{"NO token", "demo account", "quickstart"},
		},
	} {
		t.Run(p.nazwa, func(t *testing.T) {
			s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x")), demo: p.demo}
			odp := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize"})
			if odp == nil || odp.Error != nil {
				t.Fatalf("initialize nie odpowiedzialo: %+v", odp)
			}
			wynik, ok := odp.Result.(map[string]any)
			if !ok {
				t.Fatalf("wynik nie jest obiektem: %T", odp.Result)
			}
			ins, ok := wynik["instructions"].(string)
			if !ok || ins == "" {
				t.Fatal("brak pola instructions w odpowiedzi na initialize")
			}
			for _, m := range p.musi {
				if !strings.Contains(ins, m) {
					t.Errorf("w instrukcjach brakuje %q", m)
				}
			}
			for _, n := range p.niemo {
				if strings.Contains(ins, n) {
					t.Errorf("instrukcje nie powinny zawierac %q", n)
				}
			}
		})
	}
}

// Pierwszy prawdziwy klient MCP spoza naszego kregu podlaczyl sie 22.09.2026
// o 10:41 i jedyne, co o nim wiedzialem, to dwa POST-y z kodem 200. Ten test
// pilnuje, ze linia dziennika niesie to, po co powstala — i ze NIE niesie
// argumentow wywolania, bo w nich stoja cudze domeny.
func TestDziennikWywolanNiesieMetodeINarzedzieBezArgumentow(t *testing.T) {
	stare := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	zapiszWywolanie(rpcRequest{
		Method: "tools/call",
		Params: json.RawMessage(`{"name":"phpray_slow_pages","arguments":{"site":"sklep-klienta.pl"}}`),
	}, true, "203.0.113.7", "claude-code/2.1", 1500*time.Millisecond, nil)
	w.Close()
	os.Stderr = stare
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	linia := b.String()

	for _, musi := range []string{"metoda=tools/call", "narzedzie=phpray_slow_pages",
		"klient=claude-code/2.1", "demo=true", "adres=203.0.113.7", "ms=1500", "ok"} {
		if !strings.Contains(linia, musi) {
			t.Errorf("w linii brakuje %q, jest: %s", musi, linia)
		}
	}
	if strings.Contains(linia, "sklep-klienta.pl") {
		t.Errorf("argumenty wywolania NIE moga trafiac do dziennika, jest: %s", linia)
	}
}

// Blad ma byc widoczny w dzienniku razem z kodem — inaczej nie odroznimy
// "wywolal i dostal odpowiedz" od "wywolal i sie wywalilo".
func TestDziennikWywolanPokazujeKodBledu(t *testing.T) {
	stare := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	zapiszWywolanie(rpcRequest{Method: "tools/list"}, false, "203.0.113.8", "cursor/1.0", 5*time.Millisecond,
		&rpcError{Code: -32601, Message: "method not found"})
	w.Close()
	os.Stderr = stare
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	if linia := b.String(); !strings.Contains(linia, "blad=-32601") {
		t.Errorf("brak kodu bledu w linii: %s", linia)
	}
}

// Nazwa klienta rozstrzyga, czy to czlowiek, czy robot indeksujacy — a przy
// initialize prawdziwa nazwe niesie clientInfo, nie naglowek HTTP.
// 22.09.2026 SentinelOracle i BrickBlueBot wygladaly w dzienniku dokladnie
// jak zainteresowany deweloper.
func TestDziennikBierzeNazweKlientaZInitialize(t *testing.T) {
	stare := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	zapiszWywolanie(rpcRequest{
		Method: "initialize",
		Params: json.RawMessage(`{"clientInfo":{"name":"claude-code","version":"2.1.0"}}`),
	}, true, "203.0.113.9", "node", 0, nil)
	w.Close()
	os.Stderr = stare
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	linia := b.String()
	if !strings.Contains(linia, "klient=claude-code/2.1.0") {
		t.Errorf("przy initialize nazwa ma pochodzic z clientInfo, jest: %s", linia)
	}
	if strings.Contains(linia, "klient=node") {
		t.Errorf("naglowek HTTP nie moze przebic clientInfo, jest: %s", linia)
	}
}

// Bez clientInfo (kazda metoda poza initialize) zostaje nazwa klienta HTTP —
// wlasnie tam widac SentinelOracle i BrickBlueBot.
func TestDziennikSpadaNaNazweKlientaHTTP(t *testing.T) {
	stare := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	zapiszWywolanie(rpcRequest{Method: "tools/list"}, true, "203.0.113.10",
		"SentinelOracle/0.1 (+https://glimind.com/opt-out)", 0, nil)
	w.Close()
	os.Stderr = stare
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	linia := b.String()
	// Sama nazwa, bez reszty naglowka — inaczej spacje rozwala uklad pol.
	if !strings.Contains(linia, "klient=SentinelOracle/0.1 ") {
		t.Errorf("brak nazwy robota w linii: %s", linia)
	}
	if strings.Contains(linia, "glimind.com") {
		t.Errorf("do dziennika ma trafic sama nazwa, nie caly naglowek: %s", linia)
	}
}

// Przez cala dobe od wpisania nas do rejestru MCP kazdy klient — szesc
// katalogow i dwoch ludzi z lacz domowych — konczyl na tools/list. Ani
// jednego tools/call. Klient laczy sie przy starcie i pobiera liste, a
// czlowiek zostaje z dziesiecioma nazwami typu phpray_slow_queries i zadna
// podpowiedzia. Prompty sa w specyfikacji wlasnie na to.
func TestSerwerOglaszaIOddajePrompty(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x")), demo: true}

	odp := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize"})
	caps := odp.Result.(map[string]any)["capabilities"].(map[string]any)
	if _, jest := caps["prompts"]; !jest {
		t.Fatal("initialize nie oglasza zdolnosci prompts — klient nie zapyta o liste")
	}

	odp = s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "prompts/list"})
	if odp.Error != nil {
		t.Fatalf("prompts/list: %+v", odp.Error)
	}
	lista := odp.Result.(map[string]any)["prompts"].([]map[string]any)
	if len(lista) < 3 {
		t.Fatalf("za malo promptow: %d", len(lista))
	}
	for _, p := range lista {
		for _, pole := range []string{"name", "title", "description"} {
			if s, _ := p[pole].(string); s == "" {
				t.Errorf("prompt %v bez pola %q", p["name"], pole)
			}
		}
	}

	// Kazdy prompt MUSI dzialac na koncie demo, bez tokenu — bo dokladnie tam
	// trafia ktos, kto nas wlasnie dodal. Czyli nie moze wolac narzedzia
	// zapisu, ktorego sesja bez tokenu w ogole nie widzi.
	// UWAGA: liste widocznych budujemy TAK SAMO jak warstwa HTTP, czyli
	// odsiewajac narzedzia zapisu. Pierwsza wersja brala po prostu s.tools
	// — a filtrowanie dzieje sie w http.go, nie w zbudujNarzedzia — wiec
	// phpray_profile_url byl "widoczny" i asercja nie mogla nigdy paść.
	widoczne := map[string]bool{}
	for _, n := range s.tools {
		if s.demo && n.zapis {
			continue
		}
		widoczne[n.Name] = true
	}
	for _, p := range prompty() {
		odp = s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "prompts/get",
			Params: json.RawMessage(`{"name":"` + p.Name + `"}`)})
		if odp.Error != nil {
			t.Fatalf("prompts/get %s: %+v", p.Name, odp.Error)
		}
		tresc := odp.Result.(map[string]any)["messages"].([]map[string]any)[0]["content"].(map[string]any)["text"].(string)
		if !strings.Contains(tresc, "phpray_") {
			t.Errorf("prompt %s nie kieruje do zadnego narzedzia", p.Name)
		}
		for _, slowo := range strings.Fields(strings.ReplaceAll(tresc, ".", " ")) {
			if strings.HasPrefix(slowo, "phpray_") && !widoczne[strings.Trim(slowo, ",;:")] {
				t.Errorf("prompt %s wola %s, ktorego sesja demo NIE WIDZI", p.Name, slowo)
			}
		}
	}

	odp = s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("4"), Method: "prompts/get",
		Params: json.RawMessage(`{"name":"nie-ma-takiego"}`)})
	if odp.Error == nil {
		t.Error("nieznany prompt ma dawac blad, nie pustke")
	}
}

// Nazwy promptow sa IDENTYFIKATORAMI pokazywanymi uzytkownikowi — czesc
// klientow robi z nich polecenia (/what_is_slow). Pierwsza wersja miala
// nazwy polskie, a produkt jest angielski i tego samego dnia dodal nas
// deweloper z Francji.
func TestNazwyPromptowSaPoAngielsku(t *testing.T) {
	// Litery spoza ASCII i polskie dwuznaki w identyfikatorze to blad.
	for _, p := range prompty() {
		for _, r := range p.Name {
			if r > 127 {
				t.Errorf("nazwa promptu %q ma znak spoza ASCII: %q", p.Name, r)
			}
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				t.Errorf("nazwa promptu %q ma niedozwolony znak %q", p.Name, r)
			}
		}
		for _, polskie := range []string{"co_", "ktora", "sie_", "jest_wolne", "psuje", "zmienilo", "wtyczka"} {
			if strings.Contains(p.Name, polskie) {
				t.Errorf("nazwa promptu %q wyglada na polska (%q) — klient pokazuje ja uzytkownikowi",
					p.Name, polskie)
			}
		}
	}
}

// Goly komunikat "unknown tool X" nie pomaga ani czlowiekowi, ani walidatorowi
// katalogu. Odpowiedz na bledne wywolanie jest przy okazji wizytowka jakosci.
func TestBledyMowiaCoJestDostepne(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x")), demo: true}

	odp := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: json.RawMessage(`{"name":"__nie_ma__"}`)})
	if odp.Error == nil {
		t.Fatal("nieznane narzedzie ma dawac blad")
	}
	if !strings.Contains(odp.Error.Message, "phpray_sites") {
		t.Errorf("blad nie wymienia dostepnych narzedzi: %s", odp.Error.Message)
	}

	odp = s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "server/discover"})
	if odp.Error == nil {
		t.Fatal("nieznana metoda ma dawac blad")
	}
	for _, musi := range []string{"tools/list", "prompts/list"} {
		if !strings.Contains(odp.Error.Message, musi) {
			t.Errorf("blad metody nie wymienia %q: %s", musi, odp.Error.Message)
		}
	}
}

// Stopka pod wynikiem phpray_sites to jedyne miejsce, w ktorym czlowiek po
// drugiej stronie agenta dowiaduje sie, CZYJE sa te liczby i jak miec je
// u siebie. Test pilnuje trzech rzeczy naraz, bo kazda z nich zepsuta osobno
// zamienia te stopke w halas albo w reklame we wlasnym produkcie:
// jest na narzedziu wejsciowym, NIE MA jej na pozostalych, i nie ma jej wcale,
// gdy klient laczy sie wlasnym tokenem.
func TestStopkaTylkoNaWejsciuITylkoWDemo(t *testing.T) {
	const slad = "https://phpray.dev"
	for _, p := range []struct {
		nazwa     string
		demo      bool
		narzedzie string
		chce      bool
	}{
		{"demo, narzedzie wejsciowe", true, "phpray_sites", true},
		{"demo, kolejne narzedzie", true, "phpray_overview", false},
		{"wlasny token, narzedzie wejsciowe", false, "phpray_sites", false},
		{"wlasny token, kolejne narzedzie", false, "phpray_overview", false},
	} {
		t.Run(p.nazwa, func(t *testing.T) {
			got := stopka(p.demo, p.narzedzie)
			if p.chce && !strings.Contains(got, slad) {
				t.Fatalf("brak odnosnika do produktu w stopce: %q", got)
			}
			if !p.chce && got != "" {
				t.Fatalf("stopka nie powinna tu wystapic, jest: %q", got)
			}
		})
	}
	// Tresc, nie sam odnosnik: ostrzezenie „to nie sa dane tej osoby" chroni
	// czlowieka przed wzieciem cudzego sklepu za swoj, a licencja i brak roota
	// sa jedynym powodem, dla ktorego ktos moze to u siebie odpalic od reki.
	s := stopka(true, "phpray_sites")
	for _, m := range []string{"NOT the data of the person", "Apache-2.0",
		"without root", "shared hosting",
		// bez tego znacznika ruch z MCP zleje sie z bezposrednim i kanalu
		// nie da sie zmierzyc; lejek.py liczy go osobnym wierszem
		"?via=mcp"} {
		if !strings.Contains(s, m) {
			t.Errorf("w stopce brakuje %q", m)
		}
	}
}

// 23.09.2026 o 01:07 audytor SaSame-MCP-Audit przeszedl przez serwer pelna
// sciezka i na `resources/list` dostal -32601. Zasoby to wlasciwe miejsce na
// tresc, ktora agent ma PRZEKAZAC czlowiekowi — narzedzia odpowiadaja o cudzych
// danych demo, a phpray://install mowi, jak miec to samo u siebie.
func TestZasobyOdpowiadajaINiosaSciezkeDoNas(t *testing.T) {
	s := &server{tools: zbudujNarzedzia(nowyKlient("http://127.0.0.1:1", "x")), demo: true}

	odp := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "resources/list"})
	if odp == nil || odp.Error != nil {
		t.Fatalf("resources/list nie odpowiedzialo: %+v", odp)
	}
	lista := odp.Result.(map[string]any)["resources"].([]map[string]any)
	if len(lista) < 2 {
		t.Fatalf("zasobow %d, oczekiwano co najmniej 2", len(lista))
	}
	for _, z := range lista {
		for _, k := range []string{"uri", "name", "description", "mimeType"} {
			if z[k] == nil || z[k] == "" {
				t.Errorf("zasob %v bez pola %q — katalog pokaze puste miejsce", z["uri"], k)
			}
		}
	}

	// Sama lista nic nie daje: tresc musi dac sie PRZECZYTAC.
	o2 := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "resources/read",
		Params: json.RawMessage(`{"uri":"phpray://install"}`)})
	if o2 == nil || o2.Error != nil {
		t.Fatalf("resources/read nie odpowiedzialo: %+v", o2)
	}
	tekst := o2.Result.(map[string]any)["contents"].([]map[string]any)[0]["text"].(string)
	// To sa rzeczy, dla ktorych ten zasob w ogole istnieje.
	for _, m := range []string{"phpray.dev/install.sh", "No root", "shared hosting",
		"Apache-2.0", "8.0 to 8.5"} {
		if !strings.Contains(tekst, m) {
			t.Errorf("w phpray://install brakuje %q", m)
		}
	}

	// Ostrzezenie, ze to NIE sa dane tej osoby — to samo, co w instrukcjach.
	o3 := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "resources/read",
		Params: json.RawMessage(`{"uri":"phpray://demo"}`)})
	demo := o3.Result.(map[string]any)["contents"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(demo, "not the data of the person") {
		t.Error("phpray://demo nie ostrzega, ze to nie sa dane tej osoby")
	}

	// Nieznany uri: blad z podpowiedzia, nie gole 'not found'.
	o4 := s.odpowiedz(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("4"), Method: "resources/read",
		Params: json.RawMessage(`{"uri":"phpray://nie-ma"}`)})
	if o4 == nil || o4.Error == nil || !strings.Contains(o4.Error.Message, "resources/list") {
		t.Errorf("blad dla nieznanego zasobu nie mowi, gdzie szukac nazw: %+v", o4)
	}
}
