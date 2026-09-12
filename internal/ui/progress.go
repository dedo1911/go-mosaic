// Package ui disegna l'avanzamento delle operazioni lunghe sul terminale.
package ui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"golang.org/x/term"
)

// minWork è il numero minimo di elementi sotto al quale la barra non compare:
// per tre file farebbe solo lampeggiare il terminale.
const minWork = 8

// redrawEvery limita i ridisegni: senza, una libreria da migliaia di foto
// passerebbe più tempo a scrivere sul terminale che a decodificare.
const redrawEvery = 60 * time.Millisecond

// showAfter è l'attesa prima di disegnare qualcosa. Un'operazione che finisce
// in un quarto di secondo non deve far lampeggiare una barra: se il lavoro
// finisce prima di questo ritardo, sul terminale non compare nulla.
const showAfter = 250 * time.Millisecond

// Progress è una barra di avanzamento su riga singola, ridisegnata in loco.
//
// Non usa il ciclo di eventi di Bubble Tea perché il programma è anche un
// server e un watcher che scrivono sul terminale: prendersi tutto lo schermo
// creerebbe più problemi di quanti ne risolva. Il disegno della barra è però
// quello del componente progress di Bubbles.
//
// Un Progress nil è valido e non fa nulla: è il caso in cui l'output non è un
// terminale.
type Progress struct {
	mu        sync.Mutex
	out       io.Writer
	bar       progress.Model
	label     string
	total     int
	done      int
	started   time.Time
	showAfter time.Duration
	lastDraw  time.Time
	drawn     bool
}

// New crea una barra sul terminale. Restituisce nil — cioè una barra che non
// disegna niente — se l'output è rediretto su file o se il lavoro è talmente
// breve da non meritarla.
func New(label string, total int) *Progress {
	if total < minWork {
		return nil
	}
	fd := int(os.Stderr.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width < 40 {
		width = 80
	}
	return newProgress(os.Stderr, label, total, width, showAfter)
}

// baseBar viene costruito una volta sola: progress.New interroga il terminale
// per capire che profilo di colore usare, e in modalità watch la barra nasce a
// ogni rigenerazione.
var (
	baseBarOnce sync.Once
	baseBar     progress.Model
)

func barModel(width int) progress.Model {
	baseBarOnce.Do(func() { baseBar = progress.New(progress.WithDefaultGradient()) })
	m := baseBar // copia: il modello è un valore, il ramp di colori è condiviso in sola lettura
	m.Width = width
	return m
}

func newProgress(out io.Writer, label string, total, width int, delay time.Duration) *Progress {
	// Spazio per etichetta, percentuale e contatore "1234/5678".
	barWidth := width - len(label) - 2*len(fmt.Sprint(total)) - 12
	if barWidth < 10 {
		barWidth = 10
	}
	if barWidth > 60 {
		barWidth = 60
	}
	return &Progress{
		out:       out,
		bar:       barModel(barWidth),
		label:     label,
		total:     total,
		started:   time.Now(),
		showAfter: delay,
	}
}

// Set aggiorna il numero di elementi completati.
func (p *Progress) Set(done int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if done > p.total {
		done = p.total
	}
	p.done = done
	if !p.drawn && time.Since(p.started) < p.showAfter {
		return // lavoro breve: meglio non disturbare affatto
	}
	if p.drawn && time.Since(p.lastDraw) < redrawEvery && done < p.total {
		return // troppo presto: al prossimo Set
	}
	p.draw()
}

// Done cancella la barra dalla riga, lasciando il terminale pulito per i
// messaggi che seguono. Se la barra non è mai comparsa non scrive nulla.
func (p *Progress) Done() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.drawn {
		return
	}
	fmt.Fprint(p.out, "\r\x1b[K")
	p.drawn = false
}

// draw va chiamata con il lock già preso.
func (p *Progress) draw() {
	percent := 0.0
	if p.total > 0 {
		percent = float64(p.done) / float64(p.total)
	}
	fmt.Fprintf(p.out, "\r\x1b[K%s %s %d/%d", p.label, p.bar.ViewAs(percent), p.done, p.total)
	p.lastDraw = time.Now()
	p.drawn = true
}
