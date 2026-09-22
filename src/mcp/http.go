package main

// Transport HTTP dla serwera MCP (streamable HTTP): ten sam dispatch, co po
// stdio, tylko wynik wraca w odpowiedzi HTTP zamiast na standardowe wyjście.
//
// Po co: klient MCP uruchamiany lokalnie musi mieć zainstalowaną binarkę.
// Końcówka pod adresem publicznym pozwala dodać PHPRaya jedną linijką, bez
// instalowania czegokolwiek — i dopiero wtedy narzędzie jest widoczne dla
// katalogów serwerów MCP, które skanują domeny po /mcp i /sse.
//
//	PHPRAY_HTTP_ADDR=0.0.0.0:9200 PHPRAY_URL=https://app.phpray.dev \
//	PHPRAY_DEMO_TOKEN=<token viewer konta demo> phpray-mcp
//
// Token bierzemy z nagłówka Authorization każdego żądania, więc jeden proces
// obsługuje wielu użytkowników i sam nie przechowuje niczyich poświadczeń.
// Żądanie bez tokenu dostaje konto demo (tylko odczyt) — po to, żeby dało się
// spróbować bez zakładania konta.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maksCiala      = 1 << 20 // 1 MiB — żądania MCP są małe, reszta to nadużycie
	limitAnonNaMin = 30      // żądań na minutę z jednego adresu bez tokenu
)

type uslugaHTTP struct {
	baza      string
	demoToken string
	limit     *licznikMinutowy
}

// licznikMinutowy to najprostsze możliwe ograniczenie tempa: kubełek na adres,
// zerowany co minutę. Chroni konto demo przed zalaniem, a nie udaje, że jest
// systemem antyabuse'owym.
type licznikMinutowy struct {
	mu    sync.Mutex
	okno  time.Time
	licz  map[string]int
	naMin int
}

func nowyLicznik(naMin int) *licznikMinutowy {
	return &licznikMinutowy{okno: time.Now().Truncate(time.Minute), licz: map[string]int{}, naMin: naMin}
}

func (l *licznikMinutowy) wolno(adres string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	teraz := time.Now().Truncate(time.Minute)
	if teraz.After(l.okno) {
		l.okno = teraz
		l.licz = map[string]int{}
	}
	l.licz[adres]++
	return l.licz[adres] <= l.naMin
}

func adresKlienta(r *http.Request) string {
	// Za Caddym prawdziwy adres jest w X-Forwarded-For; bierzemy pierwszy wpis.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (u *uslugaHTTP) tokenZZadania(r *http.Request) (string, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return u.demoToken, true // tryb demo
	}
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(auth[len("bearer "):])
	if tok == "" {
		return "", false
	}
	return tok, false
}

func pisz(w http.ResponseWriter, kod int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(kod)
	_ = json.NewEncoder(w).Encode(v)
}

func bladRPC(kod int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: kod, Message: msg}}
}

func (u *uslugaHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		// Nie prowadzimy strumienia SSE. Zamiast gołego 405 od serwera WWW
		// oddajemy opis, bo pod ten adres trafiają i ludzie, i katalogi MCP.
		pisz(w, http.StatusOK, map[string]any{
			"name":            "phpray",
			"version":         version,
			"protocolVersion": protocolVersion,
			// Katalogi skanują /mcp, /sse i /api/mcp; wszystkie trzy trafiają
			// tutaj, więc każda z nich musi powiedzieć, gdzie jest ta właściwa.
			"endpoint":      "https://phpray.dev/mcp",
			"transport":     "streamable-http (POST only, no SSE stream)",
			"authorization": "Bearer <console token>; without it you get the read-only demo account",
			"docs":          "https://phpray.dev/mcp-server/",
			"usage":         `claude mcp add --transport http phpray https://phpray.dev/mcp`,
		})
		return
	case http.MethodPost:
	default:
		pisz(w, http.StatusMethodNotAllowed, bladRPC(-32600, "use POST with a JSON-RPC body"))
		return
	}

	token, demo := u.tokenZZadania(r)
	if token == "" {
		pisz(w, http.StatusUnauthorized, bladRPC(-32001,
			"console token required: send Authorization: Bearer <token>, or omit it to use the demo account"))
		return
	}
	if demo && !u.limit.wolno(adresKlienta(r)) {
		pisz(w, http.StatusTooManyRequests, bladRPC(-32002,
			"too many requests to the demo account from this address; use your own console token"))
		return
	}

	cialo, err := io.ReadAll(io.LimitReader(r.Body, maksCiala+1))
	if err != nil || len(cialo) > maksCiala {
		pisz(w, http.StatusRequestEntityTooLarge, bladRPC(-32600, "request body too large"))
		return
	}
	cialo = []byte(strings.TrimSpace(string(cialo)))
	if len(cialo) == 0 {
		pisz(w, http.StatusBadRequest, bladRPC(-32700, "empty body"))
		return
	}

	narzedzia := zbudujNarzedzia(nowyKlient(u.baza, token))
	if demo {
		tylkoOdczyt := narzedzia[:0:0]
		for _, n := range narzedzia {
			if !n.zapis {
				tylkoOdczyt = append(tylkoOdczyt, n)
			}
		}
		narzedzia = tylkoOdczyt
	}
	s := &server{tools: narzedzia}

	// JSON-RPC dopuszcza paczkę żądań; klienci MCP rzadko jej używają, ale
	// odrzucanie tablicy wyglądałoby jak awaria serwera, a nie jak wybór.
	if cialo[0] == '[' {
		var paczka []rpcRequest
		if err := json.Unmarshal(cialo, &paczka); err != nil {
			pisz(w, http.StatusBadRequest, bladRPC(-32700, "parse error"))
			return
		}
		odp := make([]rpcResponse, 0, len(paczka))
		for _, req := range paczka {
			if o := s.odpowiedz(req); o != nil {
				odp = append(odp, *o)
			}
		}
		if len(odp) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		pisz(w, http.StatusOK, odp)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(cialo, &req); err != nil {
		pisz(w, http.StatusBadRequest, bladRPC(-32700, "parse error"))
		return
	}
	odp := s.odpowiedz(req)
	if odp == nil {
		// Powiadomienie: brak treści, ale odpowiedź musi być, żeby klient
		// nie czekał na zamknięcie połączenia.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	pisz(w, http.StatusOK, *odp)
}

// serwujHTTP uruchamia transport HTTP i nie wraca, dopóki serwer żyje.
func serwujHTTP(adres, baza, demoToken string) error {
	u := &uslugaHTTP{baza: baza, demoToken: demoToken, limit: nowyLicznik(limitAnonNaMin)}
	mux := http.NewServeMux()
	mux.Handle("/", u)
	// Zdrowie osobno, żeby monitoring nie musiał mówić po JSON-RPC.
	mux.HandleFunc("/zdrowie", func(w http.ResponseWriter, r *http.Request) {
		pisz(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	})
	srv := &http.Server{
		Addr:              adres,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}
