package mosaic

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"sync"
	"testing"
	"time"
)

// La riduzione a blocchi si attiva solo quando la sorgente è molto più grande
// della tessera, e non deve alterare il ritaglio centrale.
func TestPreShrinkTriggersOnlyForLargeSources(t *testing.T) {
	big := image.Rect(0, 0, 1200, 400)
	if _, _, ok := preShrink(image.NewRGBA(big), big, 32, 32); !ok {
		t.Error("sorgente 1200x400 verso tessera 32x32: la riduzione doveva attivarsi")
	}

	small := image.Rect(0, 0, 100, 100)
	if _, _, ok := preShrink(image.NewRGBA(small), small, 64, 64); ok {
		t.Error("sorgente 100x100 verso tessera 64x64: la riduzione non doveva attivarsi")
	}
}

// Una foto grande ritagliata al centro deve conservare i colori del centro,
// anche passando dalla riduzione a blocchi.
func TestLargeSourceKeepsCenterColours(t *testing.T) {
	const w, h = 1200, 400
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			switch {
			case x < 400:
				src.Set(x, y, color.RGBA{255, 0, 0, 255}) // da scartare
			case x < 800:
				src.Set(x, y, color.RGBA{0, 255, 0, 255}) // il centro
			default:
				src.Set(x, y, color.RGBA{0, 0, 255, 255}) // da scartare
			}
		}
	}

	tile := NewTileFromImage("grande", 0, 0, src, 32, 32)
	for i := 0; i < len(tile.Pix); i += 4 {
		r, g, b := tile.Pix[i], tile.Pix[i+1], tile.Pix[i+2]
		if g < 200 || r > 60 || b > 60 {
			t.Fatalf("pixel %d = (%d,%d,%d): atteso verde", i/4, r, g, b)
		}
	}
}

// genericImage nasconde il tipo concreto, così preShrink cade sul percorso
// generico invece che su quello veloce per i JPEG.
type genericImage struct{ image.Image }

// Il percorso veloce per i JPEG (piani Y/Cb/Cr letti direttamente) deve dare
// esattamente lo stesso risultato di quello generico.
func TestPreShrinkYCbCrMatchesGeneric(t *testing.T) {
	const w, h = 600, 400
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			rgba.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(*image.YCbCr); !ok {
		t.Skipf("il decoder ha restituito %T invece di *image.YCbCr", decoded)
	}

	fast, _, okFast := preShrink(decoded, decoded.Bounds(), 32, 32)
	slow, _, okSlow := preShrink(genericImage{decoded}, decoded.Bounds(), 32, 32)
	if !okFast || !okSlow {
		t.Fatalf("la riduzione non si è attivata (veloce=%v, generico=%v)", okFast, okSlow)
	}

	a, b := fast.(*image.RGBA), slow.(*image.RGBA)
	if !a.Bounds().Eq(b.Bounds()) {
		t.Fatalf("dimensioni diverse: %v e %v", a.Bounds(), b.Bounds())
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			t.Fatalf("byte %d: percorso veloce %d, generico %d", i, a.Pix[i], b.Pix[i])
		}
	}
}

func TestMemGateSerialisesLargeJobs(t *testing.T) {
	g := newMemGate(100)

	g.acquire(60)
	done := make(chan struct{})
	go func() {
		g.acquire(60) // non c'è spazio: deve attendere il rilascio
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("la seconda prenotazione doveva attendere")
	case <-time.After(120 * time.Millisecond):
	}

	g.release(60)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("la seconda prenotazione non è mai partita")
	}
	g.release(60)
}

// Una sola immagine più grande dell'intero budget non deve bloccare tutto.
func TestMemGateLetsOversizedJobThrough(t *testing.T) {
	g := newMemGate(50)
	done := make(chan struct{})
	go func() {
		g.acquire(500)
		g.release(500)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("una prenotazione più grande del budget ha bloccato il gate")
	}
}

// Senza budget il gate è disattivato e non deve mai bloccare.
func TestMemGateDisabled(t *testing.T) {
	g := newMemGate(0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.acquire(1 << 30)
			g.release(1 << 30)
		}()
	}
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("il gate disattivato ha bloccato")
	}
}

// Il ridimensionamento a bande deve dare esattamente lo stesso risultato di una
// passata sola: ogni banda è una sotto-immagine, ma le coordinate si calcolano
// sul rettangolo intero.
func TestRenderFitBandsMatchSinglePass(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			src.Set(x, y, color.RGBA{uint8(x * 255 / 320), uint8(y * 255 / 200), uint8((x * y) % 256), 255})
		}
	}

	for _, fit := range []Fit{FitCover, FitContain, FitStretch} {
		one, err := renderFitInto(context.Background(), src, 900, 700, fit, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		many, err := renderFitInto(context.Background(), src, 900, 700, fit, 8, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range one.Pix {
			if one.Pix[i] != many.Pix[i] {
				t.Fatalf("fit %s: byte %d differisce fra 4 bande (%d) e 32 bande (%d)",
					fit, i, one.Pix[i], many.Pix[i])
			}
		}
	}
}

// Riducendo, il filtro di qualità resta quello di prima.
func TestRenderFitDownscaleStillWorks(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 800, 800))
	for i := range src.Pix {
		src.Pix[i] = 255
	}
	out, err := renderFitInto(context.Background(), src, 100, 100, FitCover, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := out.RGBAAt(50, 50); c.R < 250 || c.G < 250 || c.B < 250 {
		t.Fatalf("riduzione: pixel centrale = %+v, atteso bianco", c)
	}
}
