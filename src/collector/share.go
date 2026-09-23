package main

// `phpray-collector report --share` — wejscie do petli wzrostu.
//
// Raport jest plikiem. Ludzie dziela sie linkami: link wkleja sie klientowi
// w rozmowie, zalacznik trzeba zapisac, znalezc i doslac. Publikowanie
// dostala najpierw konsola Cloud, ale kont zewnetrznych jest zero, a petla
// wirusowa mnozy baze uzytkownikow — mnozenie zera daje zero. Wejscie musi
// byc TUTAJ, w otwartym kolektorze, ktory kazdy uruchamia jedna linia, bez
// konta i bez karty.
//
// Co wysylamy: WYLACZNIE dane ustalen, bez nazwy domeny. Serwer renderuje
// je wlasnym szablonem — nie przyjmuje gotowego HTML-a, wiec nic stad nie
// moze stac sie znacznikiem na cudzym ekranie.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const adresPublikacji = "https://app.phpray.dev/r"

// ustaleniePubliczne to KONTRAKT z serwerem, celowo osobny od DiagFinding.
//
// Pierwsza wersja wysylala DiagFinding wprost i serwer odrzucil ladunek
// (400), bo DiagFinding ma pole `score`, ktorego kontrakt nie przewidywal.
// Typ wewnetrzny bedzie sie zmienial; kontrakt na drucie ma sie zmieniac
// tylko wtedy, gdy tak zdecyduje ktos po obu stronach.
type ustaleniePubliczne struct {
	Rule        string   `json:"rule"`
	Severity    string   `json:"severity"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Impact      string   `json:"impact"`
	Fix         string   `json:"fix"`
	Evidence    []string `json:"evidence,omitempty"`
}

// ladunekPublikacji to dokladnie tyle, ile serwer potrzebuje. Domeny NIE MA
// w tej strukturze — nie da sie jej wyslac przez pomylke.
type ladunekPublikacji struct {
	WindowMin   int                  `json:"window_minutes"`
	TraceCount  int                  `json:"trace_count"`
	HealthScore int                  `json:"health_score"`
	Summary     string               `json:"summary"`
	Findings    []ustaleniePubliczne `json:"findings"`
	Agent       string               `json:"agent"`
}

// opublikujRaport wysyla ustalenia i zwraca publiczny adres.
func opublikujRaport(report *DiagReport, adres string) (string, string, error) {
	if adres == "" {
		adres = adresPublikacji
	}
	// Nazwa strony wycinana z KAZDEGO pola tekstowego, nie tylko z pola
	// "domain" — ktorego w kontrakcie w ogole nie ma. Pierwsza wersja usuwala
	// samo pole i opublikowala dokument z "sklep-tvsat.com" w podsumowaniu.
	bez := func(t string) string { return BezNazwyStrony(t, report.Domain) }
	l := ladunekPublikacji{
		WindowMin:   report.WindowMin,
		TraceCount:  report.TraceCount,
		HealthScore: report.HealthScore,
		Summary:     bez(report.Summary),
		Agent:       "phpray-collector/" + version,
	}
	for _, f := range report.Findings {
		dowody := make([]string, 0, len(f.Evidence))
		for _, d := range f.Evidence {
			dowody = append(dowody, bez(d))
		}
		l.Findings = append(l.Findings, ustaleniePubliczne{
			Rule:        f.Rule,
			Severity:    string(f.Severity),
			Title:       bez(f.Title),
			Description: bez(f.Description),
			Impact:      bez(f.Impact),
			Fix:         bez(f.Fix),
			Evidence:    dowody,
		})
	}
	// Dowody moga zawierac adresy URL z cudzej strony. Przycinamy liczbe,
	// ale nie tresc — to programista decyduje o publikacji i widzi, co
	// zawiera raport, zanim go opublikuje.
	body, err := json.Marshal(l)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, adres, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "phpray-collector/"+version)

	o, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer o.Body.Close()
	tresc, _ := io.ReadAll(io.LimitReader(o.Body, 1<<16))
	if o.StatusCode == http.StatusTooManyRequests {
		return "", "", fmt.Errorf("too many published reports from this address — try again later")
	}
	if o.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("%s returned %d: %s", adres, o.StatusCode, przytnijOdpowiedz(string(tresc)))
	}
	var w struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(tresc, &w); err != nil || w.URL == "" {
		return "", "", fmt.Errorf("unexpected response from the server")
	}
	return w.URL, w.ExpiresAt, nil
}

func przytnijOdpowiedz(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// wypiszAdres pokazuje wynik publikacji tak, zeby dalo sie go od razu wkleic.
//
// Po angielsku, jak reszta wyjscia: raport, ktory ten link otwiera, jest
// angielski, a kupujacy sa poza Polska. Do 0.15.9 kolektor mowil tu i w
// `update` po polsku — czytal to kazdy, kto probowal produktu.
//
// Ostatnia linia jest jedynym miejscem, gdzie mowimy o platnej konsoli:
// czyta ja ten, kto WLASNIE podzielil sie raportem, czyli ktos opiekujacy
// sie cudza strona. Jedno zdanie, po udanej publikacji, bez powtarzania.
func wypiszAdres(url, wygasa string) {
	fmt.Println(url)
	fmt.Fprintf(os.Stderr, "\nLink ready to send. The site name has been removed;\n")
	if t, err := time.Parse(time.RFC3339, wygasa); wygasa != "" && err == nil {
		fmt.Fprintf(os.Stderr, "the address expires on %s and is not indexed by search engines.\n",
			t.Local().Format("2006-01-02"))
	} else {
		fmt.Fprintf(os.Stderr, "the address expires and is not indexed by search engines.\n")
	}
	fmt.Fprintf(os.Stderr, "\nLooking after more than one site? Every one of them in a single list:\n")
	fmt.Fprintf(os.Stderr, "https://phpray.dev/en/pricing?utm_source=cli&utm_medium=share\n")
}
