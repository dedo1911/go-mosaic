package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProgressRendersLabelAndCounter(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "Processing images", 10, 80, 0)
	p.Set(0)  // primo fotogramma
	p.Set(10) // il completamento viene sempre disegnato, senza attendere

	out := buf.String()
	for _, want := range []string{"Processing images", "0/10", "10/10", "\r"} {
		if !strings.Contains(out, want) {
			t.Errorf("l'output non contiene %q: %q", want, out)
		}
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("percentuale finale mancante: %q", out)
	}
}

// Ridisegnare a ogni immagine costerebbe più della decodifica: aggiornamenti
// ravvicinati devono essere accorpati.
func TestProgressThrottlesRedraws(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 1000, 80, 0)
	for i := 1; i < 1000; i++ {
		p.Set(i)
	}
	if frames := strings.Count(buf.String(), "\r"); frames > 3 {
		t.Errorf("%d ridisegni per 999 aggiornamenti ravvicinati: troppi", frames)
	}
}

// Passato l'intervallo, il disegno riprende.
func TestProgressRedrawsAfterInterval(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 100, 80, 0)
	p.Set(10)
	time.Sleep(redrawEvery + 20*time.Millisecond)
	p.Set(20)
	if !strings.Contains(buf.String(), "20/100") {
		t.Errorf("l'aggiornamento dopo l'intervallo non è stato disegnato: %q", buf.String())
	}
}

func TestProgressClampsOverflow(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 10, 80, 0)
	p.Set(99)
	if !strings.Contains(buf.String(), "10/10") {
		t.Errorf("valore oltre il totale non limitato: %q", buf.String())
	}
}

func TestProgressDoneClearsLine(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 10, 80, 0)
	p.Set(1)
	buf.Reset()
	p.Done()
	if got := buf.String(); got != "\r\x1b[K" {
		t.Errorf("Done ha scritto %q invece di cancellare la riga", got)
	}
}

// Un'operazione che finisce prima del ritardo non deve lasciare traccia sul
// terminale, nemmeno la sequenza di cancellazione.
func TestProgressStaysSilentForQuickWork(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 100, 80, time.Hour)
	for i := 0; i <= 100; i++ {
		p.Set(i)
	}
	p.Done()
	if buf.Len() != 0 {
		t.Errorf("lavoro breve: scritti %d byte invece di nessuno (%q)", buf.Len(), buf.String())
	}
}

// Passato il ritardo la barra compare anche se il lavoro era iniziato prima.
func TestProgressAppearsAfterDelay(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "x", 100, 80, 50*time.Millisecond)
	p.Set(1)
	if buf.Len() != 0 {
		t.Fatalf("disegnata prima del ritardo: %q", buf.String())
	}
	time.Sleep(80 * time.Millisecond)
	p.Set(2)
	if !strings.Contains(buf.String(), "2/100") {
		t.Errorf("non disegnata dopo il ritardo: %q", buf.String())
	}
}

// Una barra nil è il caso normale quando l'output non è un terminale: non deve
// mai andare in panico.
func TestNilProgressIsSafe(t *testing.T) {
	var p *Progress
	p.Set(5)
	p.Done()
}

// In test l'output è rediretto, quindi New non deve creare nulla.
func TestNewReturnsNilWithoutTerminal(t *testing.T) {
	if p := New("x", 1000); p != nil {
		t.Error("senza terminale la barra non va creata")
	}
	if p := New("x", minWork-1); p != nil {
		t.Error("sotto la soglia di lavoro minimo la barra non va creata")
	}
}

// La barra deve stare nella larghezza del terminale anche quando è stretto.
func TestProgressFitsNarrowTerminals(t *testing.T) {
	for _, width := range []int{20, 40, 80, 200} {
		var buf bytes.Buffer
		newProgress(&buf, "Processing images", 100000, width, 0).Set(0)
		line := strings.TrimPrefix(buf.String(), "\r\x1b[K")
		if visible := len([]rune(stripANSI(line))); width >= 40 && visible > width {
			t.Errorf("larghezza %d: riga di %d caratteri", width, visible)
		}
	}
}

func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' && s[i] != 'K' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
