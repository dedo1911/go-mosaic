package mosaic

import (
	"context"
	"fmt"
	"image"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	// featureGrid è la risoluzione della "firma" di ogni tessera: 3x3 celle di
	// colore invece del semplice colore medio. Serve a distinguere, per dire,
	// una foto con il cielo in alto da una con il cielo in basso.
	featureGrid = 3
	// FeatureDim è il numero di componenti della firma (3x3 celle x L,a,b).
	FeatureDim = featureGrid * featureGrid * 3
)

// Feature è la firma cromatica di una tessera o di una cella del target.
type Feature [FeatureDim]float64

// Distance è la distanza quadratica tra due firme: più è bassa, più la tessera
// somiglia alla porzione di target.
func (f *Feature) Distance(o *Feature) float64 {
	var d float64
	for i := 0; i < FeatureDim; i++ {
		diff := f[i] - o[i]
		d += diff * diff
	}
	return d
}

// Tile è una foto della libreria già ritagliata e ridimensionata alla misura
// della cella, pronta per essere incollata nel mosaico.
type Tile struct {
	Path      string  // percorso su disco (o etichetta sintetica)
	ModTime   int64   // per invalidare la cache
	Size      int64   // idem
	W, H      int     // dimensione in pixel della miniatura
	Feature   Feature // firma cromatica
	Pix       []uint8 // pixel RGBA, W*H*4 byte
	Synthetic bool    // true per le tessere generate (riempimento)
}

// featureOf estrae la firma cromatica da una porzione di immagine RGBA.
func featureOf(img *image.RGBA, r image.Rectangle) Feature {
	var f Feature
	r = r.Intersect(img.Bounds())
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return f
	}
	for gy := 0; gy < featureGrid; gy++ {
		y0 := r.Min.Y + gy*h/featureGrid
		y1 := r.Min.Y + (gy+1)*h/featureGrid
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for gx := 0; gx < featureGrid; gx++ {
			x0 := r.Min.X + gx*w/featureGrid
			x1 := r.Min.X + (gx+1)*w/featureGrid
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, n float64
			for y := y0; y < y1; y++ {
				row := img.Pix[img.PixOffset(x0, y) : img.PixOffset(x1-1, y)+4]
				for i := 0; i+3 < len(row); i += 4 {
					sr += srgbToLinear[row[i]]
					sg += srgbToLinear[row[i+1]]
					sb += srgbToLinear[row[i+2]]
					n++
				}
			}
			if n == 0 {
				continue
			}
			lab := LinearRGBToLab(sr/n, sg/n, sb/n)
			o := (gy*featureGrid + gx) * 3
			f[o], f[o+1], f[o+2] = lab.L, lab.A, lab.B
		}
	}
	return f
}

// NewTileFromImage prepara una tessera a partire da un'immagine sorgente:
// ritaglio "cover" + ridimensionamento + firma cromatica.
func NewTileFromImage(path string, modTime, size int64, src image.Image, w, h int) *Tile {
	thumb := renderCover(src, w, h)
	return &Tile{
		Path:    path,
		ModTime: modTime,
		Size:    size,
		W:       w,
		H:       h,
		Feature: featureOf(thumb, thumb.Bounds()),
		Pix:     thumb.Pix,
	}
}

// NewSyntheticTile crea la tessera a tinta unita (per default nera) con cui si
// riempiono le celle che nessuna foto può coprire. Ne basta una per l'intero
// mosaico: sono tutte identiche.
func NewSyntheticTile(w, h int) *Tile {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = Background.R
		img.Pix[i+1] = Background.G
		img.Pix[i+2] = Background.B
		img.Pix[i+3] = 255
	}
	return &Tile{
		Path:      "\x00filler",
		W:         w,
		H:         h,
		Feature:   featureOf(img, img.Bounds()),
		Pix:       img.Pix,
		Synthetic: true,
	}
}

// Library è l'insieme delle tessere disponibili. È pensata per vivere a lungo
// (modalità --watch): Sync ricarica solo i file nuovi o modificati.
type Library struct {
	mu       sync.Mutex
	tileW    int
	tileH    int
	tiles    map[string]*Tile
	failures map[string]string
}

// NewLibrary crea una libreria per tessere di dimensione tileW x tileH.
func NewLibrary(tileW, tileH int) *Library {
	return &Library{
		tileW:    tileW,
		tileH:    tileH,
		tiles:    make(map[string]*Tile),
		failures: make(map[string]string),
	}
}

// TileSize restituisce la dimensione delle tessere gestite dalla libreria.
func (l *Library) TileSize() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tileW, l.tileH
}

// EnsureTileSize riadatta la libreria a una nuova dimensione di tessera. Se la
// dimensione cambia le miniature esistenti non servono più e vanno rigenerate:
// restituisce true se ha dovuto svuotare la cache in memoria.
func (l *Library) EnsureTileSize(w, h int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tileW == w && l.tileH == h {
		return false
	}
	l.tileW, l.tileH = w, h
	l.tiles = make(map[string]*Tile)
	l.failures = make(map[string]string)
	return true
}

// BytesInUse stima la memoria occupata dalle miniature già caricate. Serve a
// non strozzare il garbage collector quando la libreria stessa è grande.
func (l *Library) BytesInUse() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.tiles)) * int64(l.tileW*l.tileH*4+256)
}

// Len è il numero di tessere caricate.
func (l *Library) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.tiles)
}

// Snapshot restituisce le tessere ordinate per percorso, così che a parità di
// input il mosaico sia sempre identico.
func (l *Library) Snapshot() []*Tile {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Tile, 0, len(l.tiles))
	for _, t := range l.tiles {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *Tile) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return out
}

// SyncResult riassume cosa è cambiato nella libreria dopo una scansione.
type SyncResult struct {
	Added   int
	Updated int
	Removed int
	Failed  int
	Total   int
	Errors  []string
}

type scanEntry struct {
	path    string
	modTime int64
	size    int64
}

// SyncOptions governa una scansione della cartella sorgente.
type SyncOptions struct {
	// Recursive estende la ricerca alle sottocartelle.
	Recursive bool
	// Workers è il numero massimo di decodifiche in parallelo.
	Workers int
	// MemoryBudget limita i byte che le decodifiche in corso possono allocare
	// complessivamente: è il freno che tiene sotto controllo il picco di RAM
	// quando le foto sorgente sono molto grandi. 0 disattiva il limite.
	MemoryBudget int64
	// Skip esclude dei percorsi (tipicamente il file di output, se finisce
	// dentro alla cartella sorgente).
	Skip func(path string) bool
	// Progress, se presente, viene chiamata una prima volta con (0, totale)
	// prima di iniziare e poi a ogni file elaborato. Viene invocata da più
	// goroutine: deve essere sicura da usare in concorrenza.
	Progress func(done, total int)
}

// Sync allinea la libreria al contenuto della cartella: carica i file nuovi,
// ricarica quelli modificati, dimentica quelli spariti.
func (l *Library) Sync(ctx context.Context, root string, opts SyncOptions) (SyncResult, error) {
	recursive, workers, skip := opts.Recursive, opts.Workers, opts.Skip
	var res SyncResult

	info, err := os.Stat(root)
	if err != nil {
		return res, fmt.Errorf("source folder: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("the source %q is not a folder", root)
	}

	var entries []scanEntry
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // una cartella illeggibile non deve bloccare tutto
		}
		if d.IsDir() {
			if path != root && !recursive {
				return fs.SkipDir
			}
			if name := d.Name(); path != root && len(name) > 1 && name[0] == '.' {
				return fs.SkipDir
			}
			return nil
		}
		if !IsSupportedImage(path) {
			return nil
		}
		if skip != nil && skip(path) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, scanEntry{path: path, modTime: fi.ModTime().UnixNano(), size: fi.Size()})
		return ctx.Err()
	})
	if walkErr != nil {
		return res, walkErr
	}

	l.mu.Lock()
	tileW, tileH := l.tileW, l.tileH
	var todo []scanEntry
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.path] = true
		cur, ok := l.tiles[e.path]
		if ok && cur.ModTime == e.modTime && cur.Size == e.size && cur.W == tileW && cur.H == tileH {
			continue
		}
		if prev, bad := l.failures[e.path]; bad && prev == failureKey(e) {
			// già fallito con questi stessi metadati: inutile riprovarci
			res.Failed++
			continue
		}
		todo = append(todo, e)
	}
	for path := range l.tiles {
		if !seen[path] {
			delete(l.tiles, path)
			res.Removed++
		}
	}
	for path := range l.failures {
		if !seen[path] {
			delete(l.failures, path)
		}
	}
	l.mu.Unlock()

	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(todo) {
		workers = len(todo)
	}
	gate := newMemGate(opts.MemoryBudget)
	if opts.Progress != nil {
		opts.Progress(0, len(todo))
	}
	var processed atomic.Int64

	if workers > 0 {
		jobs := make(chan scanEntry)
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for e := range jobs {
					if ctx.Err() != nil {
						return
					}
					// Si prenota la memoria stimata prima di aprire il file e
					// la si restituisce appena la miniatura è pronta: da lì in
					// poi resta viva solo la tessera, che pesa pochi KB.
					cost := decodeCost(e.path, tileW, tileH)
					gate.acquire(cost)
					tile, err := func() (*Tile, error) {
						defer gate.release(cost)
						if opts.Progress != nil {
							// Nel defer della chiusura, così scatta una volta
							// per immagine anche quando la decodifica fallisce.
							defer func() { opts.Progress(int(processed.Add(1)), len(todo)) }()
						}
						src, err := LoadImage(e.path)
						if err != nil {
							return nil, err
						}
						return NewTileFromImage(e.path, e.modTime, e.size, src, tileW, tileH), nil
					}()
					if err != nil {
						mu.Lock()
						res.Failed++
						if len(res.Errors) < 10 {
							res.Errors = append(res.Errors, err.Error())
						}
						mu.Unlock()
						l.mu.Lock()
						l.failures[e.path] = failureKey(e)
						l.mu.Unlock()
						continue
					}
					l.mu.Lock()
					_, existed := l.tiles[e.path]
					l.tiles[e.path] = tile
					delete(l.failures, e.path)
					l.mu.Unlock()
					mu.Lock()
					if existed {
						res.Updated++
					} else {
						res.Added++
					}
					mu.Unlock()
				}
			}()
		}

		for _, e := range todo {
			select {
			case jobs <- e:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				return res, ctx.Err()
			}
		}
		close(jobs)
		wg.Wait()
	}

	if err := ctx.Err(); err != nil {
		return res, err
	}
	res.Total = l.Len()
	return res, nil
}

func failureKey(e scanEntry) string {
	return fmt.Sprintf("%d:%d", e.modTime, e.size)
}
