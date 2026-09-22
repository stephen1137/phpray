package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// klient rozmawia z API konsoli PHPRay tokenem użytkownika.
// Token wyznacza zakres: agent widzi dokładnie te serwisy, co jego właściciel.
type klient struct {
	baza string
	// token zwraca nagłówek autoryzacji na każde żądanie. W trybie chmurowym
	// jest to stały token konsoli; lokalnie krótko żyjący JWT podpisany
	// sekretem kolektora, więc nie da się go trzymać w stałej.
	token func() string
	http  *http.Client
}

func nowyKlient(baza, token string) *klient {
	return nowyKlientZTokenem(baza, func() string { return token })
}

func nowyKlientZTokenem(baza string, token func() string) *klient {
	return &klient{
		baza:  strings.TrimRight(baza, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (k *klient) zapytaj(metoda, sciezka string, q url.Values, body io.Reader) (json.RawMessage, error) {
	adres := k.baza + sciezka
	if len(q) > 0 {
		adres += "?" + q.Encode()
	}
	req, err := http.NewRequest(metoda, adres, body)
	if err != nil {
		return nil, err
	}
	if k.token != nil {
		if t := k.token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", k.baza, err)
	}
	defer resp.Body.Close()
	dane, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case 200, 201, 202:
		return dane, nil
	case 401:
		return nil, fmt.Errorf("authentication was rejected (401). In cloud mode set PHPRAY_TOKEN to a console token from Settings; " +
			"locally set PHPRAY_COLLECTOR_SECRET to the [auth] secret from /etc/phpray/collector.toml")
	case 403:
		return nil, fmt.Errorf("this token is not allowed to do that (403); a read-only token cannot start profiling")
	case 404:
		return nil, fmt.Errorf("not found (404): check the site id, %s", sciezka)
	default:
		krotko := strings.TrimSpace(string(dane))
		if len(krotko) > 300 {
			krotko = krotko[:300]
		}
		return nil, fmt.Errorf("console returned %d: %s", resp.StatusCode, krotko)
	}
}

// ladnie formatuje odpowiedź tak, żeby agent mógł ją przeczytać bez parsowania.
func ladnie(surowe json.RawMessage) string {
	var v any
	if err := json.Unmarshal(surowe, &v); err != nil {
		return string(surowe)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(surowe)
	}
	const limit = 60000 // nie zalewamy okna kontekstu agenta
	if len(b) > limit {
		// Przycięty JSON nie jest JSON-em: agent musi wiedzieć, że to urywek,
		// zamiast próbować go sparsować i dostać błąd składni.
		return "UWAGA: odpowiedź za duża i została urwana. Zawęź window_minutes albo limit.\n\n" +
			string(b[:limit]) + "\n… (urwane)"
	}
	return string(b)
}

// tekst pobiera argument tekstowy, z wartością domyślną.
func tekst(args map[string]any, klucz, domyslna string) string {
	if v, ok := args[klucz]; ok {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return domyslna
}

// liczba pobiera argument liczbowy, z wartością domyślną.
func liczba(args map[string]any, klucz string, domyslna int) int {
	if v, ok := args[klucz]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return i
			}
		}
	}
	return domyslna
}
