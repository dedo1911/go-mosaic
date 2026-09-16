package server

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIndexRendersState(t *testing.T) {
	s := newTestServer(t)
	s.Update(func(st *State) {
		st.Status = "ready"
		st.Version = 7
		st.Cols, st.Rows = 80, 60
		st.OutW, st.OutH = 3840, 2160
		st.TileW, st.TileH = 48, 36
		st.Photos = 1234
		st.UniqueUsed = 900
		st.Duration = 1500 * time.Millisecond
		st.UpdatedAt = time.Now()
		st.Image = []byte{0xff, 0xd8, 0xff}
		st.ImageType = "image/jpeg"
		st.ImageName = "mosaico.jpg"
		st.SourceDir = "/foto/vacanze"
		st.TargetPath = "/foto/ritratto.jpg"
		st.OutputPath = "mosaico.jpg"
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"go-mosaic", "ready", "80 × 60", "3840 × 2160 px", "1,234", "/image?v=" + s.eventKey(7), "1.5 s"} {
		if !strings.Contains(body, want) {
			t.Errorf("la pagina non contiene %q", want)
		}
	}
}

// Il pannello deve riportare i valori dello stato, compresi i link e il numero
// di celle ancora scoperte.
func TestFragmentShowsCountersAndLinks(t *testing.T) {
	s := newTestServer(t)
	s.Update(func(st *State) {
		st.Status = "ready"
		st.Version = 3
		st.Cols, st.Rows = 20, 15
		st.Photos = 7
		st.UniqueUsed = 7
		st.EmptyCells = 293
		st.MaxReuse = 1
		st.Image = []byte("jpeg")
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fragment", nil))
	body := rec.Body.String()
	for _, want := range []string{"Empty cells", "293", `href="/mosaic"`, `src="/image?v=` + s.eventKey(3) + `"`, "1×"} {
		if !strings.Contains(body, want) {
			t.Errorf("il pannello non contiene %q", want)
		}
	}
}

func TestFragmentIsRenderedServerSide(t *testing.T) {
	s := newTestServer(t)
	s.Update(func(st *State) {
		st.Status = "building"
		st.Cols, st.Rows = 10, 10
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fragment", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("il frammento non deve contenere la pagina intera")
	}
	if !strings.Contains(body, "building") {
		t.Errorf("stato mancante nel frammento: %s", body)
	}
}

// /mosaic è una pagina HTML renderizzata lato server che si aggiorna via SSE,
// non i byte dell'immagine.
func TestPreviewPageIsServerRendered(t *testing.T) {
	s := newTestServer(t)
	s.Update(func(st *State) {
		st.Status = "ready"
		st.Version = 12
		st.Image = []byte("jpeg")
		st.ImageType = "image/jpeg"
		st.ImageName = "mosaico.jpg"
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mosaic", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`src="/image?v=` + s.eventKey(12) + `"`, "EventSource(\"/events\")", "background: #000"} {
		if !strings.Contains(body, want) {
			t.Errorf("l'anteprima non contiene %q", want)
		}
	}
}

func TestPreviewPageWithoutImage(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mosaic", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Waiting for the first build") {
		t.Errorf("anteprima senza immagine inattesa: %s", body)
	}
}

func TestImageEndpoint(t *testing.T) {
	s := newTestServer(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("senza immagine ci si aspetta 404, ottenuto %d", rec.Code)
	}

	s.Update(func(st *State) {
		st.Image = []byte("finti-byte-jpeg")
		st.ImageType = "image/jpeg"
		st.ImageName = "mosaico.jpg"
	})

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image?v=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("l'immagine non deve essere messa in cache dal browser")
	}
	if rec.Body.String() != "finti-byte-jpeg" {
		t.Error("contenuto dell'immagine inatteso")
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image?v=1&download=1", nil))
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "mosaico.jpg") {
		t.Errorf("content-disposition = %q", cd)
	}
}

// Il browser deve ricevere un evento SSE a ogni nuova versione del mosaico.
func TestEventsNotifyNewVersion(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("lettura SSE: %v", err)
			}
			if line == "\n" {
				return sb.String()
			}
			sb.WriteString(line)
		}
	}

	if hello := readEvent(); !strings.Contains(hello, "event: hello") {
		t.Fatalf("primo evento inatteso: %q", hello)
	}

	done := make(chan string, 1)
	go func() { done <- readEvent() }()

	s.Update(func(st *State) { st.Version = 42; st.Status = "ready" })

	select {
	case ev := <-done:
		if !strings.Contains(ev, "event: mosaic") || !strings.Contains(ev, "data: "+s.eventKey(42)) {
			t.Fatalf("evento inatteso: %q", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nessun evento SSE ricevuto dopo l'aggiornamento")
	}
}

// Il cuore del bug sul riavvio: due avvii con la stessa versione devono
// produrre chiavi diverse, altrimenti il browser riconnesso non si accorge che
// il mosaico è cambiato.
func TestEventKeyDiffersAcrossRestarts(t *testing.T) {
	first, second := newTestServer(t), newTestServer(t)
	if first.instance == second.instance {
		t.Fatal("due avvii hanno lo stesso identificativo di istanza")
	}
	if first.eventKey(1) == second.eventKey(1) {
		t.Fatalf("stessa versione, stessa chiave dopo il riavvio: %q", first.eventKey(1))
	}
	if first.eventKey(1) == first.eventKey(2) {
		t.Fatal("versioni diverse della stessa istanza con la stessa chiave")
	}
}

// Il primo evento dello stream deve portare la chiave completa, ed entrambe le
// pagine devono conoscere la chiave con cui sono state renderizzate.
func TestHelloAndPagesShareTheKey(t *testing.T) {
	s := newTestServer(t)
	s.Update(func(st *State) {
		st.Status = "ready"
		st.Version = 5
		st.Image = []byte("jpeg")
	})
	key := s.eventKey(5)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var hello strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("lettura SSE: %v", err)
		}
		if line == "\n" {
			break
		}
		hello.WriteString(line)
	}
	if !strings.Contains(hello.String(), "data: "+key) {
		t.Errorf("hello senza la chiave %q: %q", key, hello.String())
	}

	for _, path := range []string{"/", "/mosaic"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// html/template rende la chiave come stringa JavaScript tra virgolette.
		if !strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Errorf("%s non incorpora la chiave %q nello script", path, key)
		}
	}
}

func TestShortPath(t *testing.T) {
	cases := map[string]string{
		"":                      "—",
		"/a/b/c/d/foto":         "…/d/foto",
		"foto":                  "foto",
		"/tmp/foto":             "…/tmp/foto",
		"./foto/vacanze/estate": "…/vacanze/estate",
	}
	for in, want := range cases {
		if got := shortPath(in); got != want {
			t.Errorf("shortPath(%q) = %q, atteso %q", in, got, want)
		}
	}
}

func TestFormatInt(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 1234: "1,234", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := formatInt(in); got != want {
			t.Errorf("formatInt(%d) = %q, atteso %q", in, got, want)
		}
	}
}
