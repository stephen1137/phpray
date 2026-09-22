package main

// Kanal aktualizacji kolektora.
//
// Po co: do 22.09.2026 kolektor nie mial ZADNEGO sposobu, zeby dowiedziec sie
// o nowym wydaniu ani zeby sie zaktualizowac. Rozszerzenie i kolektor
// instaluja sie skryptem z rootem, wiec poprawka — takze poprawka
// bezpieczenstwa — nie miala jak dotrzec do zadnej istniejacej instalacji.
// Wtyczka WordPress dostala swoj kanal dzien wczesniej; to jest jej
// odpowiednik dla czesci serwerowej.
//
// Czego NIE robi: nie aktualizuje sie sama w tle i nie wysyla niczego o
// serwerze. Pobiera dwa statyczne pliki (LATEST i SHA256SUMS) metoda GET i
// robi cokolwiek dopiero, gdy operator wpisze polecenie. Narzedzie, ktore
// podmienia sobie plik binarny bez pytania, nie ma czego szukac na cudzej
// produkcji.

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const bazaWydan = "https://phpray.dev/dl"

var klientAktualizacji = &http.Client{Timeout: 60 * time.Second}

// pobierzTekst sciaga maly plik tekstowy.
func pobierzTekst(url string) (string, error) {
	o, err := klientAktualizacji.Get(url)
	if err != nil {
		return "", err
	}
	defer o.Body.Close()
	if o.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s oddal %d", url, o.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(o.Body, 1<<20))
	return strings.TrimSpace(string(b)), err
}

var reWydanie = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// nowszy mowi, czy tag wydania jest nowszy niz wersja wbudowana.
func nowszy(tag, obecna string) (bool, error) {
	m := reWydanie.FindStringSubmatch(tag)
	if m == nil {
		return false, fmt.Errorf("nieoczekiwany znacznik wydania: %q", tag)
	}
	o := strings.SplitN(obecna, "-", 2)[0]
	n := strings.Split(o, ".")
	if len(n) != 3 {
		return false, fmt.Errorf("nieoczekiwana wersja wbudowana: %q", obecna)
	}
	for i := 0; i < 3; i++ {
		a, _ := strconv.Atoi(m[i+1])
		b, _ := strconv.Atoi(n[i])
		if a != b {
			return a > b, nil
		}
	}
	return false, nil
}

// nazwaPliku zwraca nazwe pliku binarnego dla tej maszyny.
func nazwaPliku() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return "phpray-collector-linux-" + runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("brak wydania dla architektury %s", runtime.GOARCH)
	}
}

// sumaZManifestu wyciaga oczekiwana sume SHA-256 dla nazwy pliku.
func sumaZManifestu(manifest, nazwa string) (string, error) {
	for _, ln := range strings.Split(manifest, "\n") {
		p := strings.Fields(ln)
		if len(p) == 2 && p[1] == nazwa {
			return p[0], nil
		}
	}
	return "", fmt.Errorf("w SHA256SUMS nie ma wpisu dla %s", nazwa)
}

func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	tylkoSprawdz := fs.Bool("check", false, "tylko sprawdź, czy jest nowsze wydanie; nic nie pobieraj")
	fs.Parse(args)

	tag, err := pobierzTekst(bazaWydan + "/LATEST")
	if err != nil {
		fmt.Fprintf(os.Stderr, "nie udało się sprawdzić wydań: %v\n", err)
		os.Exit(1)
	}
	jest, err := nowszy(tag, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !jest {
		fmt.Printf("masz najnowsze wydanie: v%s\n", version)
		return
	}
	fmt.Printf("jest nowsze wydanie: %s (masz v%s)\n", tag, version)
	fmt.Printf("  opis zmian: https://phpray.dev/dl/\n")
	if *tylkoSprawdz {
		return
	}

	nazwa, err := nazwaPliku()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	url := fmt.Sprintf("%s/%s/%s", bazaWydan, tag, nazwa)

	// Zanim cokolwiek pobierzemy: czy w ogole mamy gdzie to zapisac.
	// Lepiej powiedziec to teraz niz po sciagnieciu dwunastu megabajtow.
	sciezka, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nie wiem, gdzie leżę: %v\n", err)
		os.Exit(1)
	}
	sciezka, _ = filepath.EvalSymlinks(sciezka)
	katalog := filepath.Dir(sciezka)
	probny, err := os.CreateTemp(katalog, ".phpray-update-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "brak prawa zapisu w %s.\n", katalog)
		fmt.Fprintf(os.Stderr, "Uruchom ponownie z podniesionymi prawami:\n  sudo %s update\n", sciezka)
		os.Exit(1)
	}
	tymczasowy := probny.Name()
	sprzataj := func() { probny.Close(); os.Remove(tymczasowy) }

	manifest, err := pobierzTekst(fmt.Sprintf("%s/%s/SHA256SUMS", bazaWydan, tag))
	if err != nil {
		sprzataj()
		fmt.Fprintf(os.Stderr, "nie udało się pobrać SHA256SUMS: %v\n", err)
		os.Exit(1)
	}
	oczekiwana, err := sumaZManifestu(manifest, nazwa)
	if err != nil {
		sprzataj()
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("pobieram %s …\n", url)
	o, err := klientAktualizacji.Get(url)
	if err != nil {
		sprzataj()
		fmt.Fprintf(os.Stderr, "pobieranie nie powiodło się: %v\n", err)
		os.Exit(1)
	}
	defer o.Body.Close()
	if o.StatusCode != http.StatusOK {
		sprzataj()
		fmt.Fprintf(os.Stderr, "%s oddał %d\n", url, o.StatusCode)
		os.Exit(1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(probny, h), o.Body)
	if err != nil {
		sprzataj()
		fmt.Fprintf(os.Stderr, "zapis nie powiódł się: %v\n", err)
		os.Exit(1)
	}
	probny.Close()

	suma := hex.EncodeToString(h.Sum(nil))
	if suma != oczekiwana {
		os.Remove(tymczasowy)
		fmt.Fprintf(os.Stderr, "SUMA KONTROLNA SIĘ NIE ZGADZA — nie podmieniam pliku.\n")
		fmt.Fprintf(os.Stderr, "  oczekiwana: %s\n  pobrana:    %s\n", oczekiwana, suma)
		os.Exit(1)
	}
	fmt.Printf("pobrane %.1f MB, suma kontrolna zgodna\n", float64(n)/1048576)

	// Zachowaj prawa dotychczasowego pliku, zeby usluga dalej dzialala.
	if st, err := os.Stat(sciezka); err == nil {
		os.Chmod(tymczasowy, st.Mode().Perm())
	} else {
		os.Chmod(tymczasowy, 0o755)
	}
	// Kopia zapasowa OBOK, nie w /tmp: gdyby podmiana wyszla zle, operator
	// ma czym wrocic bez sieci.
	kopia := sciezka + ".poprzedni"
	os.Remove(kopia)
	if err := os.Link(sciezka, kopia); err != nil {
		_ = err // brak kopii nie jest powodem, zeby nie aktualizowac
	}
	if err := os.Rename(tymczasowy, sciezka); err != nil {
		os.Remove(tymczasowy)
		fmt.Fprintf(os.Stderr, "podmiana pliku nie powiodła się: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("zaktualizowane: %s → %s\n", sciezka, tag)
	if _, err := os.Stat(kopia); err == nil {
		fmt.Printf("  poprzednia wersja: %s\n", kopia)
	}
	fmt.Printf("\nCo jeszcze trzeba zrobić ręcznie:\n")
	fmt.Printf("  1. Uruchom ponownie usługę, żeby wziąć nowy plik:\n")
	fmt.Printf("       systemctl restart phpray-collector   # albo Twój sposób uruchamiania\n")
	fmt.Printf("  2. Rozszerzenie PHP aktualizuje się OSOBNO (to inny plik, .so na każdą\n")
	fmt.Printf("     wersję PHP) i wymaga przeładowania PHP-FPM:\n")
	fmt.Printf("       curl -fsSL https://phpray.dev/install.sh -o phpray-install.sh\n")
	fmt.Printf("       less phpray-install.sh && sudo bash phpray-install.sh\n")
}

// dopiskoWydania zwraca krotka informacje o nowszym wydaniu albo pusty tekst.
//
// Wolane z `status`, zeby kanal aktualizacji dalo sie ZAUWAZYC bez wiedzy o
// jego istnieniu. Kanal, o ktorym nikt nie wie, nie jest kanalem — ale
// jednoekranowe sprawdzenie stanu nie moze przez to wisiec ani sypac bledami,
// wiec przy braku sieci milczy.
func dopiskoWydania() string {
	k := &http.Client{Timeout: 3 * time.Second}
	o, err := k.Get(bazaWydan + "/LATEST")
	if err != nil {
		return ""
	}
	defer o.Body.Close()
	if o.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(o.Body, 64))
	if err != nil {
		return ""
	}
	tag := strings.TrimSpace(string(b))
	jest, err := nowszy(tag, version)
	if err != nil || !jest {
		return ""
	}
	return fmt.Sprintf("  —  jest nowsze wydanie %s, zainstaluj: phpray-collector update", tag)
}
