package mosaic

import (
	"image"
	"image/color"
)

// shrinkTarget è quante volte la destinazione deve essere più piccola della
// sorgente prima che convenga la riduzione a blocchi.
const shrinkTarget = 2

// preShrink riduce la sorgente con una media a blocchi quando è molto più
// grande della tessera da produrre.
//
// Serve perché il filtro di qualità (CatmullRom) applicato direttamente a una
// foto da 24 megapixel per ricavarne 64 pixel costa sia tempo sia buffer
// temporanei proporzionali all'altezza della sorgente. Una media a blocchi è
// un singolo passaggio lineare, alloca solo l'immagine ridotta, ed è anche il
// filtro corretto per riduzioni così forti; il ritocco finale alla dimensione
// esatta resta a CatmullRom.
//
// I pixel trasparenti vengono composti su nero, come il resto del mosaico.
func preShrink(src image.Image, sr image.Rectangle, dstW, dstH int) (image.Image, image.Rectangle, bool) {
	if dstW < 1 || dstH < 1 {
		return src, sr, false
	}
	factor := min(sr.Dx()/(dstW*shrinkTarget), sr.Dy()/(dstH*shrinkTarget))
	if factor < 2 {
		return src, sr, false
	}

	w, h := sr.Dx()/factor, sr.Dy()/factor
	if w < 1 || h < 1 {
		return src, sr, false
	}

	sumR := make([]uint32, w*h)
	sumG := make([]uint32, w*h)
	sumB := make([]uint32, w*h)
	count := make([]uint32, w*h)

	accumulate := func(bin int, r, g, b uint32) {
		sumR[bin] += r
		sumG[bin] += g
		sumB[bin] += b
		count[bin]++
	}

	switch img := src.(type) {
	case *image.YCbCr:
		// Percorso veloce per i JPEG: si leggono direttamente i piani Y/Cb/Cr
		// invece di passare dall'interfaccia color.Color per ogni pixel.
		for y := sr.Min.Y; y < sr.Max.Y; y++ {
			by := (y - sr.Min.Y) / factor
			if by >= h {
				break
			}
			row := by * w
			yi := img.YOffset(sr.Min.X, y)
			for x := sr.Min.X; x < sr.Max.X; x++ {
				bx := (x - sr.Min.X) / factor
				if bx >= w {
					break
				}
				ci := img.COffset(x, y)
				r, g, b := color.YCbCrToRGB(img.Y[yi+x-sr.Min.X], img.Cb[ci], img.Cr[ci])
				accumulate(row+bx, uint32(r), uint32(g), uint32(b))
			}
		}
	default:
		for y := sr.Min.Y; y < sr.Max.Y; y++ {
			by := (y - sr.Min.Y) / factor
			if by >= h {
				break
			}
			row := by * w
			for x := sr.Min.X; x < sr.Max.X; x++ {
				bx := (x - sr.Min.X) / factor
				if bx >= w {
					break
				}
				// RGBA() restituisce valori premoltiplicati a 16 bit: usarli
				// così equivale a comporre su nero.
				r, g, b, _ := src.At(x, y).RGBA()
				accumulate(row+bx, r>>8, g>>8, b>>8)
			}
		}
	}

	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		n := count[i]
		o := i * 4
		if n == 0 {
			out.Pix[o+3] = 255
			continue
		}
		out.Pix[o] = uint8(sumR[i] / n)
		out.Pix[o+1] = uint8(sumG[i] / n)
		out.Pix[o+2] = uint8(sumB[i] / n)
		out.Pix[o+3] = 255
	}
	return out, out.Bounds(), true
}
