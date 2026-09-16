package mosaic

import (
	"context"
	"image"
	"image/color"
	"runtime"
	"testing"
)

func benchTarget(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	return img
}

// Quanto costa portare il target alla risoluzione di output: è il passo che sta
// fra il messaggio sulle celle nere e la prima barra di avanzamento.
func BenchmarkRenderFitSerial6000(b *testing.B) {
	src := benchTarget(2048, 1365)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		renderFit(src, 6000, 6000, FitCover)
	}
}

func BenchmarkEncodeJPEG6000(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 6000, 6000))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Encode(img, FormatJPEG, 92); err != nil {
			b.Fatal(err)
		}
	}
}

// Il target quasi sempre va ingrandito, non ridotto: per un ingrandimento il
// filtro bilineare non ha buffer temporanei e costa molto meno.
func BenchmarkRenderFitParallel6000(b *testing.B) {
	src := benchTarget(2048, 1365)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := renderFitInto(context.Background(), src, 6000, 6000, FitCover, runtime.NumCPU(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// Scelta delle celle promettenti su un cartello a tinta unita da un milione di
// celle: con un ordinamento completo costava oltre un terzo di secondo.
func BenchmarkBestCells1M(b *testing.B) {
	const k, cells = 32, 1_000_000
	g := Geometry{Cols: 1000, Rows: 1000}
	cands := make([]candidate, cells*k)
	for c := 0; c < cells; c++ {
		d := float32(1)
		if c%5 == 0 {
			d = 2
		}
		cands[c*k] = candidate{dist: d}
	}
	a := &assignment{cands: cands, k: k, g: g}
	a.order, a.rank = cellOrder(g)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a.hopefulCells(1300, RevealFit)
	}
}
