package mosaic

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func TestParseDimension(t *testing.T) {
	cases := []struct {
		in      string
		want    Dimension
		wantErr bool
	}{
		{in: "", want: Dimension{}},
		{in: "auto", want: Dimension{}},
		{in: "64", want: Dimension{W: 64}},
		{in: "80x60", want: Dimension{W: 80, H: 60}},
		{in: " 1920 X 1080 ", want: Dimension{W: 1920, H: 1080}},
		{in: "1920×1080", want: Dimension{W: 1920, H: 1080}},
		{in: "0x10", wantErr: true},
		{in: "10x0", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "-4", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseDimension(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDimension(%q): atteso errore, ottenuto %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDimension(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDimension(%q) = %+v, atteso %+v", c.in, got, c.want)
		}
	}
}

func TestResolveGeometry(t *testing.T) {
	// Griglia e risoluzione entrambe esplicite: vanno rispettate alla lettera.
	g, err := ResolveGeometry(900, 600, GeometrySpec{Grid: Dimension{W: 80, H: 60}, Size: Dimension{W: 1920, H: 1080}, TilePx: 64})
	if err != nil {
		t.Fatal(err)
	}
	if g.Cols != 80 || g.Rows != 60 || g.OutW != 1920 || g.OutH != 1080 {
		t.Fatalf("geometria inattesa: %+v", g)
	}

	// Righe automatiche dedotte dalle proporzioni del target.
	g, err = ResolveGeometry(900, 600, GeometrySpec{Grid: Dimension{W: 60}, Size: Dimension{}, TilePx: 32})
	if err != nil {
		t.Fatal(err)
	}
	if g.Rows != 40 {
		t.Fatalf("righe automatiche = %d, attese 40", g.Rows)
	}
	if g.OutW != 60*32 || g.OutH != 40*32 {
		t.Fatalf("risoluzione dedotta inattesa: %dx%d", g.OutW, g.OutH)
	}

	// Grigia più fitta della risoluzione: errore esplicito.
	if _, err := ResolveGeometry(900, 600, GeometrySpec{Grid: Dimension{W: 500, H: 10}, Size: Dimension{W: 100, H: 100}, TilePx: 8}); err == nil {
		t.Fatal("attesa una geometria non valida")
	}
}

// Le celle devono ricoprire esattamente il canvas, senza buchi né sovrapposizioni,
// anche quando la divisione non è esatta.
func TestCellRectsTileCanvas(t *testing.T) {
	g, err := ResolveGeometry(1000, 1000, GeometrySpec{Grid: Dimension{W: 7, H: 3}, Size: Dimension{W: 1001, H: 500}, TilePx: 64})
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for cy := 0; cy < g.Rows; cy++ {
		for cx := 0; cx < g.Cols; cx++ {
			r := g.CellRect(cx, cy)
			if r.Dx() > g.TileW || r.Dy() > g.TileH {
				t.Fatalf("cella %d,%d (%dx%d) più grande della tessera %dx%d", cx, cy, r.Dx(), r.Dy(), g.TileW, g.TileH)
			}
			covered += r.Dx() * r.Dy()
			if cx > 0 && g.CellRect(cx-1, cy).Max.X != r.Min.X {
				t.Fatalf("buco orizzontale alla cella %d,%d", cx, cy)
			}
			if cy > 0 && g.CellRect(cx, cy-1).Max.Y != r.Min.Y {
				t.Fatalf("buco verticale alla cella %d,%d", cx, cy)
			}
		}
	}
	if covered != g.OutW*g.OutH {
		t.Fatalf("celle coprono %d px invece di %d", covered, g.OutW*g.OutH)
	}
}

func TestCoverRectKeepsAspectRatio(t *testing.T) {
	// Sorgente panoramica verso tessera quadrata: si taglia a destra/sinistra.
	r := coverRect(image.Rect(0, 0, 300, 100), 100, 100)
	if r.Dx() != 100 || r.Dy() != 100 {
		t.Fatalf("ritaglio = %v, atteso 100x100", r)
	}
	if r.Min.X != 100 {
		t.Fatalf("ritaglio non centrato: %v", r)
	}

	// Sorgente verticale verso tessera quadrata: si taglia sopra/sotto.
	r = coverRect(image.Rect(0, 0, 100, 400), 50, 50)
	if r.Dx() != 100 || r.Dy() != 100 || r.Min.Y != 150 {
		t.Fatalf("ritaglio verticale inatteso: %v", r)
	}

	// Tessera rettangolare 2:1 da sorgente quadrata.
	r = coverRect(image.Rect(0, 0, 200, 200), 100, 50)
	if r.Dx() != 200 || r.Dy() != 100 || r.Min.Y != 50 {
		t.Fatalf("ritaglio 2:1 inatteso: %v", r)
	}
}

// Il ridimensionamento deve conservare il centro dell'immagine e scartare le
// bande laterali eccedenti.
func TestNewTileFromImageCropsCenter(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 300; x++ {
			switch {
			case x < 100:
				src.Set(x, y, color.RGBA{255, 0, 0, 255}) // rosso: da scartare
			case x < 200:
				src.Set(x, y, color.RGBA{0, 255, 0, 255}) // verde: da tenere
			default:
				src.Set(x, y, color.RGBA{0, 0, 255, 255}) // blu: da scartare
			}
		}
	}

	tile := NewTileFromImage("test", 0, 0, src, 32, 32)
	if tile.W != 32 || tile.H != 32 || len(tile.Pix) != 32*32*4 {
		t.Fatalf("miniatura inattesa: %dx%d, %d byte", tile.W, tile.H, len(tile.Pix))
	}
	for i := 0; i < len(tile.Pix); i += 4 {
		r, g, b := tile.Pix[i], tile.Pix[i+1], tile.Pix[i+2]
		if g < 200 || r > 60 || b > 60 {
			t.Fatalf("pixel %d = (%d,%d,%d): atteso verde, il ritaglio non è centrato", i/4, r, g, b)
		}
	}
}

func TestBlackCells(t *testing.T) {
	cases := []struct{ have, cells, maxReuse, want int }{
		{have: 0, cells: 100, maxReuse: 1, want: 100},           // cartella vuota
		{have: 40, cells: 100, maxReuse: 1, want: 60},           // foto insufficienti
		{have: 100, cells: 100, maxReuse: 1, want: 0},           // esattamente sufficienti
		{have: 30, cells: 100, maxReuse: 2, want: 40},           // riutilizzo doppio: copre 60 celle
		{have: 5, cells: 100, maxReuse: 0, want: 0},             // riutilizzo illimitato
		{have: 0, cells: 100, maxReuse: 0, want: 100},           // nessuna foto: tutto nero
		{have: 200, cells: 100, maxReuse: 1, want: 0},           // libreria abbondante
		{have: 2395, cells: 1000000, maxReuse: 1, want: 997605}, // griglia enorme
	}
	for _, c := range cases {
		if got := BlackCells(c.have, c.cells, c.maxReuse); got != c.want {
			t.Errorf("BlackCells(%d,%d,%d) = %d, atteso %d", c.have, c.cells, c.maxReuse, got, c.want)
		}
	}
}

// writeSolidJPEG scrive su disco un'immagine a tinta unita.
func writeSolidJPEG(t *testing.T, path string, c color.RGBA, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
}

func TestLibrarySyncAndBuild(t *testing.T) {
	dir := t.TempDir()
	writeSolidJPEG(t, filepath.Join(dir, "rosso.jpg"), color.RGBA{220, 20, 20, 255}, 80, 40)
	writeSolidJPEG(t, filepath.Join(dir, "blu.jpg"), color.RGBA{20, 20, 220, 255}, 40, 80)
	if err := os.WriteFile(filepath.Join(dir, "appunti.txt"), []byte("ignorami"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rotto.jpg"), []byte("non sono un jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := NewLibrary(16, 16)
	res, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Total != 2 {
		t.Fatalf("sync = %+v, attese 2 foto", res)
	}
	if res.Failed != 1 {
		t.Fatalf("il file corrotto doveva essere scartato: %+v", res)
	}

	// Una seconda sync non deve ricaricare nulla.
	res2, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Added != 0 || res2.Updated != 0 || res2.Removed != 0 {
		t.Fatalf("sync incrementale ha rifatto lavoro inutile: %+v", res2)
	}

	// Target: metà sinistra rossa, metà destra blu.
	target := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 32 {
				target.Set(x, y, color.RGBA{220, 20, 20, 255})
			} else {
				target.Set(x, y, color.RGBA{20, 20, 220, 255})
			}
		}
	}

	geo, err := ResolveGeometry(64, 32, GeometrySpec{Grid: Dimension{W: 2, H: 1}, Size: Dimension{W: 32, H: 16}, TilePx: 16})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(context.Background(), target, lib, Options{
		Geometry: geo, Fit: FitCover, Blend: 0, MaxReuse: 1, Candidates: 8, Workers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.EmptyCells != 0 {
		t.Fatalf("non dovevano restare celle nere, ne sono rimaste %d", out.EmptyCells)
	}
	if out.UniqueUsed != 2 {
		t.Fatalf("attese 2 foto distinte, usate %d", out.UniqueUsed)
	}

	left := out.Image.RGBAAt(8, 8)
	right := out.Image.RGBAAt(24, 8)
	if left.R < 150 || left.B > 90 {
		t.Fatalf("cella sinistra = %+v, attesa rossa", left)
	}
	if right.B < 150 || right.R > 90 {
		t.Fatalf("cella destra = %+v, attesa blu", right)
	}
}

// Con la cartella sorgente vuota il mosaico deve comunque uscire, riempito di
// tessere nere.
func TestBuildWithEmptyLibraryUsesBlackTiles(t *testing.T) {
	lib := NewLibrary(8, 8)
	if _, err := lib.Sync(context.Background(), t.TempDir(), SyncOptions{Recursive: true, Workers: 1}); err != nil {
		t.Fatal(err)
	}

	target := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for i := range target.Pix {
		target.Pix[i] = 255
	}

	geo, err := ResolveGeometry(40, 40, GeometrySpec{Grid: Dimension{W: 4, H: 4}, Size: Dimension{W: 32, H: 32}, TilePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(context.Background(), target, lib, Options{
		Geometry: geo, Fit: FitCover, Blend: 0, MaxReuse: 1, Candidates: 4, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.EmptyCells != 16 {
		t.Fatalf("attese 16 celle nere, ottenute %d", out.EmptyCells)
	}
	if out.LibraryTile != 0 || out.UniqueUsed != 0 {
		t.Fatalf("la libreria doveva essere vuota: %+v", out)
	}
	c := out.Image.RGBAAt(16, 16)
	if c.R != 0 || c.G != 0 || c.B != 0 {
		t.Fatalf("pixel = %+v, atteso nero", c)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeSolidJPEG(t, filepath.Join(dir, "verde.jpg"), color.RGBA{20, 200, 60, 255}, 50, 50)

	lib := NewLibrary(12, 12)
	if _, err := lib.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 1}); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "cache.bin")
	if err := lib.SaveCache(cachePath); err != nil {
		t.Fatal(err)
	}

	restored := NewLibrary(12, 12)
	n, err := restored.LoadCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || restored.Len() != 1 {
		t.Fatalf("cache ripristinata con %d tessere", n)
	}
	// Dopo il ripristino la sync non deve ridecodificare nulla.
	res, err := restored.Sync(context.Background(), dir, SyncOptions{Recursive: true, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || res.Updated != 0 {
		t.Fatalf("la cache non ha evitato il ricaricamento: %+v", res)
	}

	// Una cache con tessere di dimensione diversa va ignorata.
	other := NewLibrary(24, 24)
	if n, err := other.LoadCache(cachePath); err != nil || n != 0 {
		t.Fatalf("cache incompatibile accettata: n=%d err=%v", n, err)
	}
}

func TestLinearRGBToLab(t *testing.T) {
	if got := LinearRGBToLab(0, 0, 0); got.L > 0.001 {
		t.Fatalf("nero: L = %v, atteso 0", got.L)
	}
	white := LinearRGBToLab(srgbToLinear[255], srgbToLinear[255], srgbToLinear[255])
	if white.L < 99.5 || white.L > 100.5 {
		t.Fatalf("bianco: L = %v, atteso 100", white.L)
	}
	if white.A < -1 || white.A > 1 || white.B < -1 || white.B > 1 {
		t.Fatalf("bianco: componenti cromatiche non neutre: %+v", white)
	}
}

func TestParseAspect(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "auto", want: 0},
		{in: "16:9", want: 16.0 / 9.0},
		{in: "16/9", want: 16.0 / 9.0},
		{in: "16x9", want: 16.0 / 9.0},
		{in: " 4 : 3 ", want: 4.0 / 3.0},
		{in: "1.5", want: 1.5},
		{in: "1:1", want: 1},
		{in: "0:9", wantErr: true},
		{in: "16:0", wantErr: true},
		{in: "-2", wantErr: true},
		{in: "wide", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseAspect(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseAspect(%q): atteso errore, ottenuto %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAspect(%q): %v", c.in, err)
			continue
		}
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("ParseAspect(%q) = %v, atteso %v", c.in, got, c.want)
		}
	}
}

// Le proporzioni richieste devono comandare sull'output, e se la griglia non
// combacia sono le tessere a diventare rettangolari.
func TestResolveGeometryWithAspect(t *testing.T) {
	// Target quadrato, output 16:9, griglia con la sola larghezza: le righe si
	// deducono dalle proporzioni e le tessere restano quadrate.
	g, err := ResolveGeometry(1000, 1000, GeometrySpec{
		Grid: Dimension{W: 80}, TilePx: 64, Aspect: 16.0 / 9.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Rows != 45 {
		t.Errorf("righe dedotte = %d, attese 45", g.Rows)
	}
	if g.TileW != g.TileH {
		t.Errorf("tessere %dx%d: con righe dedotte dovevano restare quadrate", g.TileW, g.TileH)
	}
	if ratio := float64(g.OutW) / float64(g.OutH); ratio < 1.77 || ratio > 1.79 {
		t.Errorf("output %dx%d: proporzioni %.3f invece di 16:9", g.OutW, g.OutH, ratio)
	}

	// Stessa richiesta ma con griglia 80x60: le tessere si allungano.
	g, err = ResolveGeometry(1000, 1000, GeometrySpec{
		Grid: Dimension{W: 80, H: 60}, TilePx: 64, Aspect: 16.0 / 9.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Cols != 80 || g.Rows != 60 {
		t.Fatalf("griglia = %dx%d, attesa 80x60", g.Cols, g.Rows)
	}
	if g.TileW <= g.TileH {
		t.Errorf("tessere %dx%d: attese più larghe che alte", g.TileW, g.TileH)
	}
	if ratio := float64(g.OutW) / float64(g.OutH); ratio < 1.77 || ratio > 1.79 {
		t.Errorf("output %dx%d: proporzioni %.3f invece di 16:9", g.OutW, g.OutH, ratio)
	}

	// Con una sola dimensione richiesta, l'altra segue le proporzioni.
	g, err = ResolveGeometry(1000, 1000, GeometrySpec{
		Grid: Dimension{W: 40}, Size: Dimension{W: 1920}, TilePx: 64, Aspect: 16.0 / 9.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.OutW != 1920 || g.OutH != 1080 {
		t.Errorf("output = %dx%d, atteso 1920x1080", g.OutW, g.OutH)
	}

	// Proporzioni e risoluzione completa insieme sono contraddittorie.
	if _, err := ResolveGeometry(1000, 1000, GeometrySpec{
		Grid: Dimension{W: 40}, Size: Dimension{W: 1920, H: 1080}, TilePx: 64, Aspect: 4.0 / 3.0,
	}); err == nil {
		t.Error("-aspect con -size completo doveva dare errore")
	}

	// Senza -aspect il comportamento di prima non cambia.
	g, err = ResolveGeometry(900, 600, GeometrySpec{Grid: Dimension{W: 60}, TilePx: 32})
	if err != nil {
		t.Fatal(err)
	}
	if g.Rows != 40 || g.OutW != 1920 || g.OutH != 1280 {
		t.Errorf("senza aspect: %dx%d celle su %dx%d px", g.Cols, g.Rows, g.OutW, g.OutH)
	}
}
