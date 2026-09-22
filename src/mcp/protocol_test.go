package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
