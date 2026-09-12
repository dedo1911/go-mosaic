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

// Nessuna foto deve comparire più volte di quanto consentito da MaxReuse, né
// quando la libreria abbonda né quando servono tessere di riempimento.
func TestMaxReuseIsRespected(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))
	const nPhotos = 220
	for i := 0; i < nPhotos; i++ {
		c := color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255}
		writeSolidJPEG(t, filepath.Join(dir, fmt.Sprintf("p%03d.jpg", i)), c, 60, 60)
	}

	lib := NewLibrary(16, 16)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if lib.Len() != nPhotos {
		t.Fatalf("libreria con %d foto invece di %d", lib.Len(), nPhotos)
	}

	target := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 320; x++ {
			target.Set(x, y, color.RGBA{uint8(x * 255 / 320), uint8(y * 255 / 320), 128, 255})
		}
	}

	cases := []struct {
		cols, rows, maxReuse int
		wantUnique           int
	}{
		{cols: 10, rows: 10, maxReuse: 1, wantUnique: 100}, // 100 celle, 220 foto
		{cols: 14, rows: 14, maxReuse: 1, wantUnique: 196}, // 196 celle
		{cols: 20, rows: 20, maxReuse: 1, wantUnique: 220}, // 400 celle: servono i riempimenti
		{cols: 10, rows: 10, maxReuse: 3},                  // riutilizzo fino a 3
	}

	for _, tc := range cases {
		name := fmt.Sprintf("%dx%d_reuse%d", tc.cols, tc.rows, tc.maxReuse)
		t.Run(name, func(t *testing.T) {
			geo, err := ResolveGeometry(320, 320, GeometrySpec{
				Grid:   Dimension{W: tc.cols, H: tc.rows},
				Size:   Dimension{W: tc.cols * 16, H: tc.rows * 16},
				TilePx: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := Build(context.Background(), target, lib, Options{
				Geometry: geo, Fit: FitCover, Blend: 0.25,
				MaxReuse: tc.maxReuse, Candidates: 32, Workers: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			for i, used := range res.debugUsage {
				if used > tc.maxReuse {
					t.Fatalf("tessera %d usata %d volte, il limite è %d", i, used, tc.maxReuse)
				}
			}
			if tc.wantUnique > 0 && res.UniqueUsed != tc.wantUnique {
				t.Errorf("foto distinte usate = %d, attese %d", res.UniqueUsed, tc.wantUnique)
			}
			// Ogni cella deve avere ricevuto o una foto o la tessera nera.
			placed := 0
			for _, used := range res.debugUsage {
				placed += used
			}
			if placed+res.EmptyCells != geo.Cells() {
				t.Errorf("%d celle con foto + %d nere ≠ %d celle", placed, res.EmptyCells, geo.Cells())
			}
		})
	}
}

// Le celle senza una foto vera restano nere anche con -blend alto: il mosaico
// si compone man mano che i file arrivano, senza mostrare il target sotto.
func TestFillerCellsStayBlack(t *testing.T) {
	dir := t.TempDir()
	writeSolidJPEG(t, filepath.Join(dir, "rosso.jpg"), color.RGBA{230, 20, 20, 255}, 40, 40)

	lib := NewLibrary(16, 16)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 1}); err != nil {
		t.Fatal(err)
	}

	// Target: metà sinistra rossa, metà destra bianca.
	target := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 32 {
				target.Set(x, y, color.RGBA{230, 20, 20, 255})
			} else {
				target.Set(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}

	geo, err := ResolveGeometry(64, 32, GeometrySpec{Grid: Dimension{W: 2, H: 1}, Size: Dimension{W: 32, H: 16}, TilePx: 16})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), target, lib, Options{
		Geometry: geo, Fit: FitCover, Blend: 0.8, MaxReuse: 1, Candidates: 4, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EmptyCells != 1 {
		t.Fatalf("attesa 1 cella nera, ottenute %d", res.EmptyCells)
	}

	// La cella con la foto usa la fusione, quella scoperta resta nera pura.
	filled := res.Image.RGBAAt(8, 8)
	if filled.R < 120 {
		t.Errorf("cella con foto = %+v, attesa rossa", filled)
	}
	empty := res.Image.RGBAAt(24, 8)
	if empty.R != 0 || empty.G != 0 || empty.B != 0 {
		t.Fatalf("cella scoperta = %+v, atteso nero pieno (il target non deve trasparire)", empty)
	}
}

// hopefulCells scarta le celle che non potranno mai ricevere una foto, ma solo
// quando la libreria è troppo piccola per coprire la griglia.
func TestHopefulCells(t *testing.T) {
	const k = 2
	// Quattro celle, la migliore affinità in ordine inverso: 3, 2, 1, 0.
	cands := make([]candidate, 4*k)
	for cell := 0; cell < 4; cell++ {
		cands[cell*k] = candidate{tile: 0, dist: float32(4 - cell)}
		cands[cell*k+1] = candidate{tile: 1, dist: float32(10 + cell)}
	}

	// Capienza sufficiente: si considerano tutte le celle, in ordine.
	got := hopefulCells(cands, k, 4, 4)
	if len(got) != 4 {
		t.Fatalf("capienza sufficiente: %d celle invece di 4", len(got))
	}
	for i, cell := range got {
		if int(cell) != i {
			t.Errorf("ordine alterato: posizione %d contiene la cella %d", i, cell)
		}
	}

	// Riutilizzo illimitato (capienza 0 per convenzione): tutte le celle.
	if got := hopefulCells(cands, k, 4, 0); len(got) != 4 {
		t.Errorf("riuso illimitato: %d celle invece di 4", len(got))
	}
}

// Con molte più celle che foto la selezione deve tenere le celle più affini e
// non deve far perdere nessuna foto.
func TestHugeGridPlacesEveryPhoto(t *testing.T) {
	dir := t.TempDir()
	const photos = 40
	for i := 0; i < photos; i++ {
		c := color.RGBA{uint8(i * 6), uint8(255 - i*6), uint8(i * 3), 255}
		writeSolidJPEG(t, filepath.Join(dir, fmt.Sprintf("p%02d.jpg", i)), c, 40, 40)
	}

	lib := NewLibrary(4, 4)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 2}); err != nil {
		t.Fatal(err)
	}

	target := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			target.Set(x, y, color.RGBA{uint8(x * 255 / 400), uint8(255 - y*255/400), uint8(x / 4), 255})
		}
	}

	// 10000 celle per 40 foto: quasi tutto nero, ma ogni foto va piazzata.
	geo, err := ResolveGeometry(400, 400, GeometrySpec{Grid: Dimension{W: 100, H: 100}, Size: Dimension{W: 400, H: 400}, TilePx: 4})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), target, lib, Options{
		Geometry: geo, Fit: FitCover, Blend: 0.25, MaxReuse: 1, Candidates: 16, Workers: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.UniqueUsed != photos {
		t.Errorf("usate %d foto su %d: la selezione delle celle ne ha perse per strada", res.UniqueUsed, photos)
	}
	if want := geo.Cells() - photos; res.EmptyCells != want {
		t.Errorf("celle nere = %d, attese %d", res.EmptyCells, want)
	}
	for i, used := range res.debugUsage {
		if used > 1 {
			t.Fatalf("foto %d usata %d volte con -max-reuse 1", i, used)
		}
	}
}
