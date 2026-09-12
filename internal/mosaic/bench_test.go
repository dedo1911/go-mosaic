package mosaic

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func bigJPEG(b *testing.B, w, h int) []byte {
	b.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x / 16), uint8(y / 16), uint8((x + y) / 24), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// Quanta memoria costa la sola decodifica di una foto da 12 megapixel.
func BenchmarkDecodeOnly(b *testing.B) {
	data := bigJPEG(b, 4000, 3000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		_ = img
	}
}

// Quanta ne costa la pipeline completa: decodifica + ritaglio + miniatura.
func BenchmarkDecodeAndTile(b *testing.B) {
	data := bigJPEG(b, 4000, 3000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		_ = NewTileFromImage("x", 0, 0, img, 64, 64)
	}
}

// Solo il ridimensionamento, partendo da un'immagine già decodificata.
func BenchmarkTileFromDecoded(b *testing.B) {
	data := bigJPEG(b, 4000, 3000)
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewTileFromImage("x", 0, 0, img, 64, 64)
	}
}
