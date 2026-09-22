package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Gdy konto Free dostaje drugi serwis, konsola odrzuca go W CISZY: zlicza do
// dropped_quota i nic nie odpowiada. Kolektor te liczbe zapisywal i nie
// pokazywal nigdzie. Uzytkownik widzial tylko, ze strony w konsoli NIE MA.
func TestTopWidziOdrzuceniePrzezLimitPlanu(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"cloud":  map[string]any{"dropped_quota": 1234, "dropped_traces": 7},
		})
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	if n, wy := odrzuconePrzezPlan(addr); n != 1234 || wy {
		t.Errorf("odrzuconych przez plan: %d, chcialem 1234", n)
	}
}

// Brak kolektora albo brak pola nie moze wywracac `phpray top` ani wypisywac
// ostrzezenia, ktorego nikt nie potrzebuje.
func TestTopMilczyGdyNicNieOdrzucono(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()
	if n, _ := odrzuconePrzezPlan(strings.TrimPrefix(srv.URL, "http://")); n != 0 {
		t.Errorf("bez pola ma byc 0, jest %d", n)
	}
	if n, _ := odrzuconePrzezPlan("127.0.0.1:1"); n != 0 {
		t.Errorf("bez kolektora ma byc 0, jest %d", n)
	}
}

// DroppedQuota MUSI byc osobne od DroppedTraces: przepelniony bufor przejdzie
// sam, a limit planu wymaga decyzji czlowieka. Zlane w jedna liczbe nie daja
// sie odroznic, a wymagaja przeciwnych dzialan.
func TestLimitPlanuNieMieszaSieZPrzepelnieniemBufora(t *testing.T) {
	var st CloudStatus
	st.DroppedTraces = 5
	st.DroppedQuota = 9
	b, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["dropped_traces"] != float64(5) || m["dropped_quota"] != float64(9) {
		t.Errorf("liczniki sie zlewaja: %v", m)
	}
}

// 402 znaczy "plan wyczerpany albo nieoplacony" — slady zatrzymane w calosci.
// Stalo w CloudStatus od dawna i nie bylo pokazywane nigdzie.
func TestTopWidziWyczerpanyPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cloud": map[string]any{"plan_exhausted": true, "dropped_quota": 0},
		})
	}))
	defer srv.Close()
	n, wyczerpany := odrzuconePrzezPlan(strings.TrimPrefix(srv.URL, "http://"))
	if !wyczerpany {
		t.Error("402 z konsoli ma dotrzec do phpray top")
	}
	if n != 0 {
		t.Errorf("bez odrzucen ma byc 0, jest %d", n)
	}
}
