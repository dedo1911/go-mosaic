package mosaic

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // decoder GIF
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/bmp"  // decoder BMP
	_ "golang.org/x/image/tiff" // decoder TIFF
	_ "golang.org/x/image/webp" // decoder WebP

	"golang.org/x/image/draw"
)

var supportedExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".jfif": true,
	".png": true, ".gif": true, ".bmp": true,
	".tif": true, ".tiff": true, ".webp": true,
}

// IsSupportedImage indica se il file ha un'estensione che sappiamo decodificare.
func IsSupportedImage(name string) bool {
	return supportedExt[strings.ToLower(filepath.Ext(name))]
}

// LoadImage decodifica un'immagine dal disco.
func LoadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// Background è il colore usato dietro alle immagini con trasparenza e per le
// tessere generate quando la libreria non basta a riempire la griglia.
var Background = color.RGBA{R: 0, G: 0, B: 0, A: 255}

// renderCover disegna src dentro a un nuovo RGBA di dimensione w x h usando la
// regola "cover": si mantengono le proporzioni scalando finché il lato corto
// copre la destinazione, poi si ritaglia al centro l'eccedenza.
func renderCover(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(Background), image.Point{}, draw.Src)
	sr := coverRect(src.Bounds(), w, h)
	// Le foto da fotocamera sono centinaia di volte più grandi di una tessera:
	// una prima riduzione a blocchi evita di far lavorare il filtro di qualità
	// sull'intera immagine.
	src, sr, _ = preShrink(src, sr, w, h)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sr, draw.Over, nil)
	return dst
}

// fitRects calcola quale porzione della sorgente finisce in quale porzione del
// canvas, secondo la modalità di adattamento richiesta.
func fitRects(src image.Rectangle, w, h int, fit Fit) (sr, dr image.Rectangle) {
	switch fit {
	case FitStretch:
		return src, image.Rect(0, 0, w, h)
	case FitContain:
		return src, containRect(src, w, h)
	default:
		return coverRect(src, w, h), image.Rect(0, 0, w, h)
	}
}

// renderFit porta il target alla risoluzione di output.
func renderFit(src image.Image, w, h int, fit Fit) *image.RGBA {
	dst, _ := renderFitInto(context.Background(), src, w, h, fit, 1, nil)
	return dst
}

// renderFitInto è renderFit con parallelismo e avanzamento.
//
// È il passo più lento prima dell'abbinamento — su un canvas da decine di
// milioni di pixel vale centinaia di millisecondi — e prima era muto: fra il
// messaggio sulle celle e la prima barra il terminale sembrava bloccato.
//
// Per un ingrandimento si usa il filtro bilineare, che non alloca buffer
// temporanei e può quindi lavorare a bande in parallelo. Il filtro di qualità
// ne terrebbe uno di larghezza_destinazione × altezza_sorgente — su un canvas
// largo 64000 pixel sono gigabyte, per goroutine — quindi resta riservato alle
// riduzioni, dove serve davvero e gira in un colpo solo.
func renderFitInto(ctx context.Context, src image.Image, w, h int, fit Fit, workers int, rep *reporter) (*image.RGBA, error) {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(Background), image.Point{}, draw.Src)

	sr, dr := fitRects(src.Bounds(), w, h, fit)
	if dr.Empty() || sr.Empty() {
		return dst, nil
	}

	if dr.Dx() < sr.Dx() || dr.Dy() < sr.Dy() {
		rep.start("Preparing target", 1)
		draw.CatmullRom.Scale(dst, dr, src, sr, draw.Over, nil)
		rep.add()
		return dst, nil
	}

	bands := workers * 4
	if bands > dr.Dy() {
		bands = dr.Dy()
	}
	if bands < 1 {
		bands = 1
	}

	rep.start("Preparing target", bands)
	err := parallelFor(ctx, bands, workers, func(i int) {
		y0 := dr.Min.Y + i*dr.Dy()/bands
		y1 := dr.Min.Y + (i+1)*dr.Dy()/bands
		// La banda è una sotto-immagine del canvas: il ridimensionatore limita
		// la scrittura ai suoi bounds ma calcola le coordinate sul rettangolo
		// intero, quindi il risultato è identico a una passata sola.
		band := dst.SubImage(image.Rect(dr.Min.X, y0, dr.Max.X, y1)).(*image.RGBA)
		draw.ApproxBiLinear.Scale(band, dr, src, sr, draw.Over, nil)
		rep.add()
	})
	return dst, err
}

// Format è il formato di codifica dell'immagine finale.
type Format string

const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
)

// FormatForPath deduce il formato dall'estensione del file di output.
func FormatForPath(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return FormatJPEG, nil
	case ".png":
		return FormatPNG, nil
	case "":
		return "", fmt.Errorf("the output file %q has no extension (use .jpg or .png)", path)
	default:
		return "", fmt.Errorf("unsupported output format %q (use .jpg or .png)", filepath.Ext(path))
	}
}

// ContentType restituisce il MIME type corrispondente al formato.
func (f Format) ContentType() string {
	if f == FormatPNG {
		return "image/png"
	}
	return "image/jpeg"
}

// Encode serializza l'immagine nel formato richiesto.
func Encode(img image.Image, format Format, quality int) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	switch format {
	case FormatPNG:
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		err = enc.Encode(&buf, img)
	default:
		if quality <= 0 || quality > 100 {
			quality = 92
		}
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteFileAtomic scrive i byte su disco passando da un file temporaneo, così
// chi sta leggendo l'output (o un altro watcher) non vede mai un file a metà.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
