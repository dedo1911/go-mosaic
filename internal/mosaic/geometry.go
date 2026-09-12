package mosaic

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"
)

// Fit descrive come adattare l'immagine target al canvas di output quando le
// proporzioni non coincidono.
type Fit string

const (
	// FitCover ritaglia il target (destra/sinistra oppure sopra/sotto) per
	// riempire tutto il canvas senza deformarlo. È il default.
	FitCover Fit = "cover"
	// FitContain riduce il target dentro al canvas lasciando bande vuote.
	FitContain Fit = "contain"
	// FitStretch deforma il target fino a coprire esattamente il canvas.
	FitStretch Fit = "stretch"
)

// ParseFit valida il valore passato da CLI.
func ParseFit(s string) (Fit, error) {
	switch Fit(strings.ToLower(strings.TrimSpace(s))) {
	case FitCover:
		return FitCover, nil
	case FitContain:
		return FitContain, nil
	case FitStretch:
		return FitStretch, nil
	}
	return "", fmt.Errorf("invalid fit %q (use cover, contain or stretch)", s)
}

// Dimension è una coppia larghezza/altezza in cui 0 significa "automatico".
type Dimension struct{ W, H int }

// ParseDimension interpreta stringhe tipo "64x48", "64" (altezza automatica)
// oppure "" (entrambe automatiche). Accetta anche la "×" tipografica.
func ParseDimension(s string) (Dimension, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "×", "x")
	s = strings.ReplaceAll(s, "*", "x")
	if s == "" || s == "auto" {
		return Dimension{}, nil
	}
	parts := strings.SplitN(s, "x", 2)
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return Dimension{}, fmt.Errorf("invalid value %q: the width must be a positive integer", s)
	}
	d := Dimension{W: w}
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || h <= 0 {
			return Dimension{}, fmt.Errorf("invalid value %q: the height must be a positive integer", s)
		}
		d.H = h
	}
	return d, nil
}

// ParseAspect interpreta le proporzioni richieste per l'output: "16:9", "16/9",
// "16x9" oppure un numero decimale come "1.777". Restituisce 0 per la stringa
// vuota, cioè "usa quelle dell'immagine target".
func ParseAspect(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "×", "x")
	if s == "" || s == "auto" || s == "target" {
		return 0, nil
	}

	invalid := fmt.Errorf("invalid aspect ratio %q (use 16:9 or 1.777)", s)
	for _, sep := range []string{":", "/", "x"} {
		w, h, ok := strings.Cut(s, sep)
		if !ok {
			continue
		}
		wf, err1 := strconv.ParseFloat(strings.TrimSpace(w), 64)
		hf, err2 := strconv.ParseFloat(strings.TrimSpace(h), 64)
		if err1 != nil || err2 != nil || wf <= 0 || hf <= 0 {
			return 0, invalid
		}
		return wf / hf, nil
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, invalid
	}
	return v, nil
}

// GeometrySpec è ciò che l'utente ha chiesto dalla riga di comando, prima che
// venga combinato con le proporzioni dell'immagine target.
type GeometrySpec struct {
	// Grid è la griglia in tessere; altezza 0 significa "deducila".
	Grid Dimension
	// Size è la risoluzione finale; 0 significa "deducila".
	Size Dimension
	// TilePx è il lato della tessera, usato solo quando Size è automatica.
	TilePx int
	// Aspect è il rapporto larghezza/altezza voluto per l'output. 0 significa
	// "usa quello dell'immagine target".
	Aspect float64
}

// Geometry è la griglia risolta: quante celle, quanto è grande il canvas e a
// che dimensione vanno preparate le tessere.
type Geometry struct {
	Cols, Rows   int // numero di tessere in orizzontale e verticale
	OutW, OutH   int // risoluzione esatta dell'immagine finale
	TileW, TileH int // dimensione a cui viene renderizzata ogni tessera
}

// Cells è il numero totale di celle della griglia.
func (g Geometry) Cells() int { return g.Cols * g.Rows }

// CanvasBytes è la memoria occupata dai due buffer a piena risoluzione che
// servono per comporre: il mosaico e il target riportato alla stessa misura.
func (g Geometry) CanvasBytes() int64 {
	return int64(g.OutW) * int64(g.OutH) * 4 * 2
}

// CellRect restituisce il rettangolo (in pixel di output) della cella indicata.
// I pixel di resto vengono distribuiti tra le celle, così la somma delle celle
// è sempre esattamente OutW x OutH.
func (g Geometry) CellRect(cx, cy int) image.Rectangle {
	x0 := cx * g.OutW / g.Cols
	x1 := (cx + 1) * g.OutW / g.Cols
	y0 := cy * g.OutH / g.Rows
	y1 := (cy + 1) * g.OutH / g.Rows
	return image.Rect(x0, y0, x1, y1)
}

// ResolveGeometry combina quanto richiesto dall'utente con le proporzioni del
// target in una geometria coerente.
//
// Le tessere non devono essere per forza quadrate: se la griglia non ha le
// stesse proporzioni dell'output, è la tessera ad allungarsi. Chiedere 80x60
// tessere in un'immagine 16:9 dà tessere 4:3 (per esempio 64x48 pixel).
func ResolveGeometry(targetW, targetH int, spec GeometrySpec) (Geometry, error) {
	if targetW <= 0 || targetH <= 0 {
		return Geometry{}, fmt.Errorf("target image has invalid dimensions (%dx%d)", targetW, targetH)
	}
	if spec.Grid.W <= 0 {
		return Geometry{}, fmt.Errorf("the grid needs at least one column")
	}
	tilePx := spec.TilePx
	if tilePx <= 0 {
		tilePx = 64
	}

	// Proporzioni di riferimento: quelle richieste, altrimenti quelle del target.
	ratio := float64(targetW) / float64(targetH)
	if spec.Aspect > 0 {
		ratio = spec.Aspect
	}
	if spec.Aspect > 0 && spec.Size.W > 0 && spec.Size.H > 0 {
		return Geometry{}, fmt.Errorf("-aspect and a full -size contradict each other: drop one")
	}

	g := Geometry{Cols: spec.Grid.W, Rows: spec.Grid.H}

	switch {
	case spec.Size.W > 0 && spec.Size.H > 0:
		g.OutW, g.OutH = spec.Size.W, spec.Size.H
	case spec.Size.W > 0:
		g.OutW = spec.Size.W
		g.OutH = int(math.Round(float64(spec.Size.W) / ratio))
	case spec.Size.H > 0:
		g.OutH = spec.Size.H
		g.OutW = int(math.Round(float64(spec.Size.H) * ratio))
	default:
		// Nessuna risoluzione richiesta: la si deduce da griglia x tessera.
		if g.Rows <= 0 {
			g.Rows = int(math.Round(float64(g.Cols) / ratio))
		}
		if g.Rows < 1 {
			g.Rows = 1
		}
		g.OutW = g.Cols * tilePx
		if spec.Aspect > 0 {
			// Proporzioni imposte: l'altezza segue quelle, e se la griglia non
			// combacia sono le tessere a diventare rettangolari.
			g.OutH = int(math.Round(float64(g.OutW) / ratio))
		} else {
			g.OutH = g.Rows * tilePx
		}
	}

	if g.Rows <= 0 {
		// Righe automatiche con risoluzione nota: si sceglie il numero che
		// rende le tessere il più possibile quadrate.
		g.Rows = int(math.Round(float64(g.Cols) * float64(g.OutH) / float64(g.OutW)))
	}
	if g.Rows < 1 {
		g.Rows = 1
	}
	if g.OutH < 1 {
		g.OutH = 1
	}
	if g.Cols > g.OutW {
		return Geometry{}, fmt.Errorf("%d columns do not fit in %d pixels of width", g.Cols, g.OutW)
	}
	if g.Rows > g.OutH {
		return Geometry{}, fmt.Errorf("%d rows do not fit in %d pixels of height", g.Rows, g.OutH)
	}

	// Le tessere vengono preparate alla dimensione massima di cella: le celle
	// più piccole di un pixel useranno semplicemente un ritaglio della tessera.
	g.TileW = ceilDiv(g.OutW, g.Cols)
	g.TileH = ceilDiv(g.OutH, g.Rows)
	return g, nil
}

func ceilDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// coverRect calcola il ritaglio centrale della sorgente che ha le stesse
// proporzioni della destinazione: si scala finché il lato più corto raggiunge
// la dimensione richiesta e si taglia l'eccedenza (destra/sinistra oppure
// sopra/sotto).
func coverRect(src image.Rectangle, dstW, dstH int) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw <= 0 || sh <= 0 || dstW <= 0 || dstH <= 0 {
		return src
	}
	// Confronto incrociato in interi: sw/sh vs dstW/dstH.
	if sw*dstH > sh*dstW {
		// Sorgente più larga in proporzione: taglio a destra e sinistra.
		w := int(math.Round(float64(sh) * float64(dstW) / float64(dstH)))
		if w < 1 {
			w = 1
		}
		if w > sw {
			w = sw
		}
		x := src.Min.X + (sw-w)/2
		return image.Rect(x, src.Min.Y, x+w, src.Max.Y)
	}
	// Sorgente più alta in proporzione: taglio sopra e sotto.
	h := int(math.Round(float64(sw) * float64(dstH) / float64(dstW)))
	if h < 1 {
		h = 1
	}
	if h > sh {
		h = sh
	}
	y := src.Min.Y + (sh-h)/2
	return image.Rect(src.Min.X, y, src.Max.X, y+h)
}

// containRect calcola il rettangolo di destinazione centrato che contiene
// interamente la sorgente senza deformarla.
func containRect(src image.Rectangle, dstW, dstH int) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw <= 0 || sh <= 0 {
		return image.Rect(0, 0, dstW, dstH)
	}
	scale := math.Min(float64(dstW)/float64(sw), float64(dstH)/float64(sh))
	w := int(math.Round(float64(sw) * scale))
	h := int(math.Round(float64(sh) * scale))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	x := (dstW - w) / 2
	y := (dstH - h) / 2
	return image.Rect(x, y, x+w, y+h)
}
