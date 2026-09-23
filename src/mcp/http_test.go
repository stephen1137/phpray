package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pod /mcp trafiaja trzy rozne rodzaje gosci i kazdy potrzebuje czego innego.
// 22.09.2026 czlowiek z Binga dostal 371 bajtow surowego JSON-a zamiast strony
// z instrukcja instalacji — znalazl nas sam i zgubilismy go na ostatnim kroku.
// Ten test pilnuje calej trojki naraz, bo naprawienie jednego przypadku bardzo
// latwo psuje pozostale: katalogi MCP zyja z tego JSON-a, a oficjalny SDK
// wymaga 405 na prosbe o strumien SSE.
func TestGETnaMcpRozrozniaGoscia(t *testing.T) {
	u := &uslugaHTTP{baza: "http://127.0.0.1:1", limit: nowyLicznik(1000)}
	for _, p := range []struct {
		nazwa  string
		accept string
		kod    int
		gdzie  string
	}{
		{"przegladarka", "text/html,application/xhtml+xml,*/*;q=0.8",
			http.StatusSeeOther, "https://phpray.dev/mcp-server/?via=mcp"},
		{"klient MCP proszacy o strumien", "text/event-stream",
			http.StatusMethodNotAllowed, ""},
		{"katalog MCP", "application/json", http.StatusOK, ""},
		{"narzedzie bez Accept", "", http.StatusOK, ""},
	} {
		t.Run(p.nazwa, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			if p.accept != "" {
				r.Header.Set("Accept", p.accept)
			}
			w := httptest.NewRecorder()
			u.ServeHTTP(w, r)
			if w.Code != p.kod {
				t.Fatalf("kod %d, oczekiwano %d; cialo: %s", w.Code, p.kod, w.Body.String())
			}
			if p.gdzie != "" && w.Header().Get("Location") != p.gdzie {
				t.Fatalf("przekierowanie na %q, oczekiwano %q", w.Header().Get("Location"), p.gdzie)
			}
			// Maszyna MUSI dalej dostawac to, z czego zyja katalogi.
			if p.kod == http.StatusOK {
				for _, m := range []string{"streamable-http", "phpray.dev/mcp", "mcp-server"} {
					if !strings.Contains(w.Body.String(), m) {
						t.Errorf("w opisie dla maszyny brakuje %q", m)
					}
				}
			}
		})
	}
}

// Katalogi serwerow MCP buduja swoj wpis o nas z deskryptora pod GET /mcp.
// 23.09.2026 pytalo o niego dziesiec roznych robotow (mcp-drift-monitor,
// mcphub-probe, ProofBench, io.verifymcp, GlideMcpIndex, exaforce-mcprep,
// MCPMeter i inne), a deskryptor nie niosl ani slowa opisu.
//
// Najwazniejsza czesc tego testu to NIE obecnosc pol, tylko ostatnia asercja:
// opis musi byc DOSLOWNIE ten sam, co w server.json wyslanym do oficjalnego
// rejestru MCP. Jedno zdanie w dwoch miejscach rozjezdza sie po cichu, a wtedy
// ten sam produkt jest w katalogach opisany na dwa sposoby.
func TestDeskryptorNiesieToCzegoPotrzebujeKatalog(t *testing.T) {
	u := &uslugaHTTP{baza: "http://127.0.0.1:1", limit: nowyLicznik(1000)}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("kod %d, oczekiwano 200", w.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("deskryptor nie jest JSON-em: %v", err)
	}
	for _, k := range []string{"name", "description", "icon", "website", "license",
		"endpoint", "transport", "usage", "docs", "authRequired", "version"} {
		if _, ok := d[k]; !ok {
			t.Errorf("w deskryptorze brakuje pola %q — katalog zostawi puste miejsce", k)
		}
	}
	if d["authRequired"] != false {
		t.Error("authRequired musi byc false: publiczny endpoint dziala BEZ tokena " +
			"i to jest rzecz, ktora odroznia nas od wiekszosci wpisow")
	}

	// Ta sama nazwa i ten sam opis, co w zgloszeniu do oficjalnego rejestru.
	b, err := os.ReadFile(filepath.Join("..", "..", "server.json"))
	if err != nil {
		t.Skipf("nie znalazlem server.json: %v", err)
	}
	var rej struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(b, &rej); err != nil {
		t.Fatalf("server.json nie jest JSON-em: %v", err)
	}
	if d["description"] != rej.Description {
		t.Errorf("opis rozjechal sie z rejestrem MCP — katalogi opisza nas dwojako:\n"+
			"  deskryptor: %v\n  server.json: %v", d["description"], rej.Description)
	}
}
