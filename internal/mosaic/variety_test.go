package mosaic

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"path/filepath"
	"testing"
)

// libraryOfSimilarPhotos crea foto quasi identiche come tonalità: è il caso in
// cui, senza varietà, l'algoritmo sceglie sempre le stesse.
func libraryOfSimilarPhotos(t *testing.T, n int) *Library {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < n; i++ {
		base := color.RGBA{
			R: uint8(90 + rng.Intn(12)),
			G: uint8(70 + rng.Intn(12)),
			B: uint8(120 + rng.Intn(12)),
			A: 255,
		}
		writeSolidJPEG(t, filepath.Join(dir, fmt.Sprintf("p%03d.jpg", i)), base, 32, 32)
	}
	lib := NewLibrary(8, 8)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	return lib
}

func gradientTarget(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(80 + x*40/w), uint8(60 + y*40/h), 120, 255})
		}
	}
	return img
}

func buildWith(t *testing.T, lib *Library, cols, rows int, tune func(*Options)) *Result {
	t.Helper()
	target := gradientTarget(240, 240)
	geo, err := ResolveGeometry(240, 240, GeometrySpec{Grid: Dimension{W: cols, H: rows}, Size: Dimension{W: cols * 8, H: rows * 8}, TilePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Geometry: geo, Fit: FitCover, Blend: 0, MaxReuse: 0, Candidates: 32, Workers: 4}
	tune(&opts)
	res, err := Build(context.Background(), target, lib, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Alzando la varietà devono comparire più foto diverse, senza lasciare celle
// scoperte: il riutilizzo resta illimitato.
func TestVarietySpreadsUsage(t *testing.T) {
	lib := libraryOfSimilarPhotos(t, 120)

	var previous int
	for _, variety := range []float64{0, 0.25, 0.5, 1} {
		res := buildWith(t, lib, 20, 20, func(o *Options) { o.Variety = variety })
		if res.EmptyCells != 0 {
			t.Fatalf("variety %.2f: %d celle nere, il riutilizzo illimitato deve riempire tutto", variety, res.EmptyCells)
		}
		if variety > 0 && res.UniqueUsed < previous {
			t.Errorf("variety %.2f: %d foto diverse, meno delle %d del passo precedente", variety, res.UniqueUsed, previous)
		}
		previous = res.UniqueUsed
	}
	if previous <= 1 {
		t.Fatalf("con foto quasi identiche la varietà non ha prodotto alcun effetto (%d foto usate)", previous)
	}
}

// Con -use-all ogni foto della libreria deve comparire almeno una volta, e
// tutte le celle restano coperte.
func TestUseAllPlacesEveryPhoto(t *testing.T) {
	const photos = 120
	lib := libraryOfSimilarPhotos(t, photos)

	plain := buildWith(t, lib, 20, 20, func(o *Options) {})
	if plain.UniqueUsed >= photos {
		t.Skipf("senza -use-all erano già tutte usate (%d): il caso non è significativo", plain.UniqueUsed)
	}

	res := buildWith(t, lib, 20, 20, func(o *Options) { o.UseAll = true })
	if res.UniqueUsed != photos {
		t.Errorf("usate %d foto su %d nonostante UseAll", res.UniqueUsed, photos)
	}
	if res.EmptyCells != 0 {
		t.Errorf("%d celle nere: UseAll non deve lasciare buchi", res.EmptyCells)
	}
	placed := 0
	for _, u := range res.debugUsage {
		placed += u
	}
	if placed != res.Geometry.Cells() {
		t.Errorf("assegnazioni %d, celle %d", placed, res.Geometry.Cells())
	}
}

// Con meno celle che foto, UseAll piazza quante ne entrano e non va in errore.
func TestUseAllWithFewerCellsThanPhotos(t *testing.T) {
	lib := libraryOfSimilarPhotos(t, 120)
	res := buildWith(t, lib, 5, 5, func(o *Options) { o.UseAll = true })
	if res.UniqueUsed != 25 {
		t.Errorf("25 celle: attese 25 foto diverse, usate %d", res.UniqueUsed)
	}
	if res.EmptyCells != 0 {
		t.Errorf("%d celle nere con riutilizzo illimitato", res.EmptyCells)
	}
}

// Con un limite di riutilizzo alto le foto più affini si prendono tutte le
// celle: UseAll deve far comparire anche le altre, senza superare il limite.
func TestUseAllWithReuseLimit(t *testing.T) {
	const photos = 120
	lib := libraryOfSimilarPhotos(t, photos)

	plain := buildWith(t, lib, 20, 20, func(o *Options) { o.MaxReuse = 4 })
	if plain.UniqueUsed >= photos {
		t.Skipf("senza UseAll erano già tutte usate (%d): il caso non è significativo", plain.UniqueUsed)
	}

	res := buildWith(t, lib, 20, 20, func(o *Options) {
		o.MaxReuse = 4
		o.UseAll = true
	})
	if res.UniqueUsed != photos {
		t.Errorf("usate %d foto su %d con UseAll e MaxReuse 4", res.UniqueUsed, photos)
	}
	for i, used := range res.debugUsage {
		if used > 4 {
			t.Fatalf("foto %d usata %d volte con MaxReuse 4", i, used)
		}
	}
	if res.EmptyCells != 0 {
		t.Errorf("%d celle nere: 120 foto da 4 usi coprono 400 celle", res.EmptyCells)
	}
}

// Con limite uno ogni foto compare già una volta sola: UseAll non deve
// cambiare niente.
func TestUseAllIsImplicitWithReuseOne(t *testing.T) {
	lib := libraryOfSimilarPhotos(t, 120)
	without := buildWith(t, lib, 20, 20, func(o *Options) { o.MaxReuse = 1 })
	with := buildWith(t, lib, 20, 20, func(o *Options) {
		o.MaxReuse = 1
		o.UseAll = true
	})
	if without.UniqueUsed != 120 {
		t.Fatalf("MaxReuse 1 da solo usa %d foto su 120", without.UniqueUsed)
	}
	for i := range without.Image.Pix {
		if without.Image.Pix[i] != with.Image.Pix[i] {
			t.Fatalf("UseAll ha cambiato il mosaico al byte %d", i)
		}
	}
}

// Con -reveal random e un limite le celle scoperte devono restare quelle
// all'inizio dell'ordine casuale, anche con la passata in più di UseAll:
// altrimenti il soggetto comparirebbe subito e le celle tornerebbero nere.
func TestUseAllKeepsTheRandomReveal(t *testing.T) {
	lib := libraryOfSimilarPhotos(t, 60)
	res := buildWith(t, lib, 20, 20, func(o *Options) {
		o.MaxReuse = 3
		o.UseAll = true
		o.Reveal = RevealRandom
	})
	if res.UniqueUsed != 60 {
		t.Errorf("usate %d foto su 60", res.UniqueUsed)
	}
	const capacity = 60 * 3
	if want := res.Geometry.Cells() - capacity; res.EmptyCells != want {
		t.Fatalf("celle nere = %d, attese %d", res.EmptyCells, want)
	}

	order, _ := cellOrder(res.Geometry)
	revealed := make(map[int32]bool, capacity)
	for _, cell := range order[:capacity] {
		revealed[cell] = true
	}
	for cell := 0; cell < res.Geometry.Cells(); cell++ {
		r := res.Geometry.CellRect(cell%res.Geometry.Cols, cell/res.Geometry.Cols)
		c := res.Image.RGBAAt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2)
		black := c.R == 0 && c.G == 0 && c.B == 0
		if revealed[int32(cell)] == black {
			t.Fatalf("cella %d: scoperta nell'ordine=%v ma nera=%v", cell, revealed[int32(cell)], black)
		}
	}
}
