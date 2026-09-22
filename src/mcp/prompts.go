package main

// Gotowe pytania, ktore klient MCP pokazuje czlowiekowi.
//
// Dlaczego to powstalo: przez cala dobe od wpisania nas do oficjalnego
// rejestru KAZDY klient — szesc katalogow i dwoch ludzi z lacz domowych
// (Wroclaw, Francja) — konczyl na tools/list. Ani jednego tools/call.
//
// Przy katalogach to normalne. Przy czlowieku znaczy tyle, ze klient MCP
// laczy sie przy starcie i pobiera liste narzedzi, a potem uzytkownik
// zostaje z dziesiecioma nazwami typu phpray_slow_queries i zadna
// podpowiedzia, o co w ogole zapytac. Prompty sa w specyfikacji wlasnie na
// to: Claude Desktop pokazuje je jako polecenia, inne klienty jako sugestie.
//
// Kazdy z nich MUSI dzialac na koncie demo, bez tokenu — bo dokladnie tam
// trafia ktos, kto nas wlasnie dodal.

type prompt struct {
	Name        string
	Title       string
	Description string
	Tresc       string
}

func prompty() []prompt {
	return []prompt{
		{
			Name:        "co_jest_wolne",
			Title:       "What is slow here?",
			Description: "Walk the fleet, find the site that hurts most and say why — in plain words.",
			Tresc: "Use the PHPRay tools to find what is slow.\n\n" +
				"1. Call phpray_sites to see every site with its p95 and error rate.\n" +
				"2. Pick the one that looks worst and call phpray_overview on it.\n" +
				"3. Follow the biggest cost: phpray_slow_pages for the URLs, " +
				"phpray_slow_queries if the database dominates, phpray_components " +
				"if PHP time does.\n" +
				"4. Open one bad request with phpray_traces and phpray_trace.\n\n" +
				"Answer in plain words: which site, which page, what is eating the time, " +
				"and what you would look at first. Name the numbers you used.",
		},
		{
			Name:        "ktora_wtyczka",
			Title:       "Which plugin costs the most?",
			Description: "Attribute request time to WordPress plugins and themes, with the numbers.",
			Tresc: "Find which plugin or theme costs the most time.\n\n" +
				"Call phpray_sites, pick a WordPress site, then phpray_components on it. " +
				"Self time is the one that says which component to look at; incl includes " +
				"what it calls.\n\n" +
				"Report the top few with their per-request cost, and say plainly whether " +
				"the numbers justify doing anything about them. If the breakdown is " +
				"missing, say that per-function profiling is off rather than guessing.",
		},
		{
			Name:        "co_sie_psuje",
			Title:       "Is anything failing right now?",
			Description: "Check alerts and failing requests, and separate real breakage from scanner noise.",
			Tresc: "Check whether anything is failing.\n\n" +
				"Call phpray_alerts for everything currently over threshold, then " +
				"phpray_errors on the worst site.\n\n" +
				"Be careful with one thing: requests returning 5xx on paths that do not " +
				"exist on the site — /.env, /.git/config, credential files — are " +
				"vulnerability scanners, not the site breaking. Say which is which " +
				"instead of reporting the raw count.",
		},
		{
			Name:        "co_sie_zmienilo",
			Title:       "Did anything get worse?",
			Description: "Compare two time windows to see whether a deploy or an update made things slower.",
			Tresc: "Check whether anything got worse recently.\n\n" +
				"Call phpray_sites, pick a site, then phpray_compare with two windows — " +
				"the last 24 hours against the 24 before that.\n\n" +
				"Report only the differences that matter: which URLs and which components " +
				"moved, per request, and by how much. If nothing moved beyond noise, say " +
				"so — that is a useful answer too.",
		},
	}
}
