package mosaic

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// signTarget è un cartello: fondo nero con una fascia bianca centrale, come
// una scritta. La fascia occupa il 20% delle celle.
func signTarget(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{0, 0, 0, 255}
			if y >= h*2/5 && y < h*3/5 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// partyLibrary crea foto scure e chiare, come quelle di una festa.
func partyLibrary(t *testing.T, n int) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < n; i++ {
		lum := uint8(15 + rng.Intn(40))
		if i%2 == 0 {
			lum = uint8(200 + rng.Intn(50))
		}
		writeSolidJPEG(t, filepath.Join(dir, fmt.Sprintf("p%03d.jpg", i)), color.RGBA{lum, lum, lum, 255}, 24, 24)
	}
	lib := NewLibrary(8, 8)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	return lib, dir
}

func buildSign(t *testing.T, lib *Library, reveal Reveal) (*Result, []int32) {
	t.Helper()
	const cols, rows = 40, 20
	geo, err := ResolveGeometry(320, 160, GeometrySpec{
		Grid: Dimension{W: cols, H: rows}, Size: Dimension{W: cols * 8, H: rows * 8}, TilePx: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), signTarget(320, 160), lib, Options{
		Geometry: geo, Fit: FitCover, MaxReuse: 1, Candidates: 32, Workers: 4, Reveal: reveal,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ricostruisce quali celle hanno una foto guardando i pixel: le celle
	// scoperte sono nero pieno, quelle con una foto no.
	filled := make([]int32, 0, geo.Cells())
	for cell := 0; cell < geo.Cells(); cell++ {
		r := geo.CellRect(cell%geo.Cols, cell/geo.Cols)
		c := res.Image.RGBAAt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2)
		if c.R != 0 || c.G != 0 || c.B != 0 {
			filled = append(filled, int32(cell))
		}
	}
	return res, filled
}

// Su un fondo uniforme le foto non devono accumularsi nelle prime righe.
func TestFitDoesNotFillInReadingOrder(t *testing.T) {
	lib, _ := partyLibrary(t, 120) // 120 foto per 800 celle
	res, _ := buildSign(t, lib, RevealFit)

	const cols, rows = 40, 20
	rowsWithDarkPhotos := 0
	for row := 0; row < rows; row++ {
		if row >= rows*2/5 && row < rows*3/5 {
			continue // la fascia bianca è a parte
		}
		for col := 0; col < cols; col++ {
			r := res.Geometry.CellRect(col, row)
			if c := res.Image.RGBAAt(r.Min.X+4, r.Min.Y+4); c.R != 0 {
				rowsWithDarkPhotos++
				break
			}
		}
	}
	// 16 righe di fondo nero: in ordine di lettura le foto scure ne
	// occuperebbero solo le prime due.
	if rowsWithDarkPhotos < 8 {
		t.Errorf("foto sullo sfondo in sole %d righe su 16: riempimento in ordine di lettura", rowsWithDarkPhotos)
	}
}

// Con "fit" le foto chiare finiscono sulla scritta, che si legge subito; con
// "random" le celle scoperte sono distribuite su tutto il target.
func TestRevealRandomHidesTheSubject(t *testing.T) {
	lib, _ := partyLibrary(t, 120)
	inBand := func(filled []int32) float64 {
		n := 0
		for _, cell := range filled {
			if row := int(cell) / 40; row >= 8 && row < 12 {
				n++
			}
		}
		return float64(n) / float64(len(filled))
	}

	_, fit := buildSign(t, lib, RevealFit)
	_, random := buildSign(t, lib, RevealRandom)

	if len(fit) != 120 || len(random) != 120 {
		t.Fatalf("celle con foto: fit %d, random %d, attese 120", len(fit), len(random))
	}
	// La fascia è il 20% della griglia.
	if share := inBand(fit); share < 0.4 {
		t.Errorf("fit: solo il %.0f%% delle foto sulla scritta, doveva concentrarsi lì", share*100)
	}
	if share := inBand(random); share > 0.35 {
		t.Errorf("random: il %.0f%% delle foto sulla scritta, doveva essere vicino al 20%%", share*100)
	}
}

// Con "random" una cella scoperta resta scoperta quando arrivano altre foto:
// sul muro dal vivo non si devono vedere tessere tornare nere.
func TestRevealRandomIsMonotonic(t *testing.T) {
	_, dir := partyLibrary(t, 150)

	build := func(photos int) map[int32]bool {
		sub := t.TempDir()
		for i := 0; i < photos; i++ {
			name := fmt.Sprintf("p%03d.jpg", i)
			copyFile(t, filepath.Join(dir, name), filepath.Join(sub, name))
		}
		lib := NewLibrary(8, 8)
		if _, err := lib.Sync(context.Background(), sub, SyncOptions{Recursive: true, Workers: 4}); err != nil {
			t.Fatal(err)
		}
		_, filled := buildSign(t, lib, RevealRandom)
		set := make(map[int32]bool, len(filled))
		for _, cell := range filled {
			set[cell] = true
		}
		return set
	}

	before := build(80)
	after := build(150)
	for cell := range before {
		if !after[cell] {
			t.Fatalf("la cella %d aveva una foto con 80 foto e l'ha persa con 150", cell)
		}
	}
}

// A griglia completa le due modalità devono dare lo stesso identico mosaico.
func TestRevealModesAgreeWhenGridIsFull(t *testing.T) {
	lib, _ := partyLibrary(t, 120)
	geo, err := ResolveGeometry(320, 160, GeometrySpec{
		Grid: Dimension{W: 10, H: 10}, Size: Dimension{W: 80, H: 80}, TilePx: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	build := func(reveal Reveal) *image.RGBA {
		res, err := Build(context.Background(), signTarget(320, 160), lib, Options{
			Geometry: geo, Fit: FitCover, MaxReuse: 1, Candidates: 32, Workers: 4, Reveal: reveal,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.EmptyCells != 0 {
			t.Fatalf("%s: %d celle nere con 120 foto per 100 celle", reveal, res.EmptyCells)
		}
		return res.Image
	}
	fit, random := build(RevealFit), build(RevealRandom)
	for i := range fit.Pix {
		if fit.Pix[i] != random.Pix[i] {
			t.Fatalf("griglia piena: le due modalità differiscono al byte %d", i)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
