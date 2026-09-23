package main

// Raport HTML: jeden plik, który użytkownik wysyła klientowi albo wkleja do
// zgłoszenia. To jest jedyny artefakt PHPRaya, który regularnie ogląda ktoś,
// kto PHPRaya nie zainstalował — agencja pokazuje go właścicielowi sklepu,
// administrator wkleja do ticketu. Dlatego plik jest samowystarczalny (żadnych
// zewnętrznych zasobów, działa z pendrive'a i z załącznika), czytelny bez
// tłumaczenia i podpisany tak, żeby odbiorca wiedział, czym to zrobiono.
//
//	phpray report -domain sklep.pl -format html > raport.html
//	phpray report -domain sklep.pl -format html -anonymize > raport-publiczny.html

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

type widokRaportu struct {
	Domena       string
	Data         string
	Okno         string
	Slady        int
	Ocena        int
	OcenaKlasa   string
	OcenaSlowo   string
	Podsumowanie string
	Ustalenia    []widokUstalenia
	Wersja       string
}

type widokUstalenia struct {
	Waga      string
	WagaKlasa string
	Tytul     string
	Opis      string
	Skutek    string
	Naprawa   string
	Dowody    []string
}

// GenerateHTMLReport buduje samowystarczalny plik HTML. Gdy anonim jest
// prawdziwe, nazwa domeny i dowody zawierające ją są zastąpione — raport da się
// wtedy pokazać publicznie, na forum albo w ofercie, bez ujawniania klienta.
func GenerateHTMLReport(report *DiagReport, anonim bool, wersja string) string {
	domena := report.Domain
	if domena == "" {
		domena = "all domains"
	}
	if anonim {
		domena = "the site"
	}

	klasa, slowo := "dobra", "healthy"
	if report.HealthScore < 80 {
		klasa, slowo = "uwaga", "needs attention"
	}
	if report.HealthScore < 50 {
		klasa, slowo = "zla", "unhealthy"
	}

	bezNazwy := func(t string) string {
		if !anonim {
			return t
		}
		return BezNazwyStrony(t, report.Domain)
	}

	w := widokRaportu{
		Domena:       domena,
		Data:         time.Now().Format("2 January 2006, 15:04 MST"),
		Okno:         oknoSlownie(report.WindowMin),
		Slady:        report.TraceCount,
		Ocena:        report.HealthScore,
		OcenaKlasa:   klasa,
		OcenaSlowo:   slowo,
		Podsumowanie: bezNazwy(report.Summary),
		Wersja:       wersja,
	}

	for _, f := range report.Findings {
		wk := "info"
		switch f.Severity {
		case SevCritical:
			wk = "krytyczne"
		case SevWarning:
			wk = "ostrzezenie"
		}
		var dowody []string
		for _, d := range f.Evidence {
			dowody = append(dowody, bezNazwy(d))
		}
		w.Ustalenia = append(w.Ustalenia, widokUstalenia{
			Waga: string(f.Severity), WagaKlasa: wk,
			Tytul: bezNazwy(f.Title), Opis: bezNazwy(f.Description),
			Skutek: bezNazwy(f.Impact), Naprawa: bezNazwy(f.Fix),
			Dowody: dowody,
		})
	}

	var b strings.Builder
	if err := szablonRaportu.Execute(&b, w); err != nil {
		// Szablon jest stały, więc błąd oznacza pomyłkę programisty, nie danych.
		return fmt.Sprintf("<!doctype html><meta charset=utf-8><p>report template error: %v", err)
	}
	return b.String()
}

func oknoSlownie(min int) string {
	switch {
	case min%1440 == 0 && min >= 1440:
		d := min / 1440
		if d == 1 {
			return "the last 24 hours"
		}
		return fmt.Sprintf("the last %d days", d)
	case min%60 == 0 && min >= 60:
		h := min / 60
		if h == 1 {
			return "the last hour"
		}
		return fmt.Sprintf("the last %d hours", h)
	default:
		return fmt.Sprintf("the last %d minutes", min)
	}
}

var szablonRaportu = template.Must(template.New("raport").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Performance report — {{.Domena}}</title>
<style>
:root{--tlo:#fff;--plyta:#f7f8fa;--tusz:#12181f;--tusz2:#55606d;--tusz3:#8792a1;
  --kreska:#e6e9ee;--ok:#17803d;--uwaga:#b45309;--alarm:#c0332b;--akcent:#a35b00;
  --ok-t:#e9f6ed;--uwaga-t:#fdf3e3;--alarm-t:#fcecea;color-scheme:light}
@media (prefers-color-scheme:dark){:root{--tlo:#171b21;--plyta:#11151a;--tusz:#f2f5f8;
  --tusz2:#b3bcc8;--tusz3:#7d8795;--kreska:#272d35;--ok:#4ade80;--uwaga:#fbbf24;
  --alarm:#f87171;--akcent:#f5a524;--ok-t:#10251a;--uwaga-t:#2a2010;--alarm-t:#2b1515;
  color-scheme:dark}}
*{box-sizing:border-box}
body{margin:0;background:var(--tlo);color:var(--tusz);
  font:16px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
.wrap{max-width:780px;margin:0 auto;padding:40px 22px 60px}
h1{font-size:30px;line-height:1.2;letter-spacing:-.02em;margin:0 0 6px;font-weight:680}
.meta{color:var(--tusz3);font-size:14px;margin:0 0 28px}
.ocena{display:flex;align-items:center;gap:18px;padding:20px 22px;border-radius:12px;
  border:1px solid var(--kreska);background:var(--plyta);margin:0 0 26px}
.ocena .liczba{font-size:44px;font-weight:700;line-height:1;letter-spacing:-.03em}
.ocena.dobra .liczba{color:var(--ok)} .ocena.uwaga .liczba{color:var(--uwaga)}
.ocena.zla .liczba{color:var(--alarm)}
.ocena .opis{font-size:15px;color:var(--tusz2)}
.ocena .opis b{color:var(--tusz);display:block;font-size:17px;margin-bottom:2px}
h2{font-size:20px;margin:34px 0 14px;letter-spacing:-.01em}
.u{border:1px solid var(--kreska);border-radius:12px;padding:18px 20px;margin:0 0 14px;background:var(--tlo)}
.u .naglowek{display:flex;align-items:center;gap:10px;margin:0 0 8px;flex-wrap:wrap}
.u h3{margin:0;font-size:17px}
.waga{font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.06em;
  padding:3px 9px;border-radius:999px;border:1px solid var(--kreska)}
.waga.krytyczne{background:var(--alarm-t);color:var(--alarm)}
.waga.ostrzezenie{background:var(--uwaga-t);color:var(--uwaga)}
.waga.info{background:var(--plyta);color:var(--tusz2)}
.u p{margin:0 0 8px;color:var(--tusz2);font-size:15px}
.u p.skutek{color:var(--tusz)}
.u .naprawa{background:var(--plyta);border-left:3px solid var(--akcent);
  border-radius:0 8px 8px 0;padding:10px 14px;margin:10px 0 0;font-size:15px}
.u .naprawa b{color:var(--tusz)}
.u ul{margin:10px 0 0;padding-left:20px;font-size:14px;color:var(--tusz2)}
.u li{margin:0 0 5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  font-size:13px;word-break:break-all}
.pusto{padding:22px;border:1px dashed var(--kreska);border-radius:12px;color:var(--tusz2)}
footer{margin:44px 0 0;padding-top:20px;border-top:1px solid var(--kreska);
  color:var(--tusz3);font-size:13.5px}
footer a{color:var(--akcent)}
footer b{color:var(--tusz2)}
@media print{body{background:#fff}.wrap{padding:0}}
</style>
</head>
<body><div class="wrap">

<h1>Performance report — {{.Domena}}</h1>
<p class="meta">{{.Data}} · {{.Okno}} · {{.Slady}} requests analysed</p>

<div class="ocena {{.OcenaKlasa}}">
  <span class="liczba">{{.Ocena}}</span>
  <span class="opis"><b>{{.OcenaSlowo}}</b>{{.Podsumowanie}}</span>
</div>

<h2>What we found</h2>
{{if .Ustalenia}}{{range .Ustalenia}}
<div class="u">
  <div class="naglowek"><span class="waga {{.WagaKlasa}}">{{.Waga}}</span><h3>{{.Tytul}}</h3></div>
  {{if .Opis}}<p>{{.Opis}}</p>{{end}}
  {{if .Skutek}}<p class="skutek">{{.Skutek}}</p>{{end}}
  {{if .Naprawa}}<div class="naprawa"><b>What to do:</b> {{.Naprawa}}</div>{{end}}
  {{if .Dowody}}<ul>{{range .Dowody}}<li>{{.}}</li>{{end}}</ul>{{end}}
</div>
{{end}}{{else}}
<p class="pusto">Nothing worth reporting in this window. Every request was recorded and
none of them tripped a rule.</p>
{{end}}

<footer>
Generated by <b>PHPRay {{.Wersja}}</b> — every PHP request recorded: time, SQL, outbound
HTTP, errors and N+1 patterns, on shared hosting too.<br>
<a href="https://phpray.dev/?utm_source=report&amp;utm_medium=html">phpray.dev</a> ·
open source, Apache-2.0 · this report is a single file, safe to forward.<br>
Numbers above come from this site&#39;s own traffic, not from a synthetic page load.
The local core is free and open source —
<a href="https://phpray.dev/en/docs/install/quickstart?utm_source=report&amp;utm_medium=html">how
to run it yourself</a>.
</footer>

</div></body></html>
`))
