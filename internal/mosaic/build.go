package mosaic

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/image/draw"
)

// Options raccoglie i parametri di costruzione del mosaico.
type Options struct {
	Geometry Geometry
	Fit      Fit
	// Blend (0..1) miscela ogni tessera con il colore del target sottostante:
	// 0 lascia le foto intatte, valori alti rendono il soggetto più leggibile.
	Blend float64
	// MaxReuse è il numero massimo di volte che una stessa foto può comparire.
	// 0 significa nessun limite.
	MaxReuse int
	// AllowAdjacent permette a due celle confinanti di usare la stessa foto.
	AllowAdjacent bool
	// Variety (0..1) dice quanto ci si può allontanare dall'abbinamento
	// migliore pur di non ripetere sempre le stesse foto. Conta solo con
	// riutilizzo illimitato: con un limite di riutilizzo è già il limite a
	// costringere la varietà.
	//
	// 0 prende sempre il candidato più affine; 1 considera equivalenti tutti i
	// candidati della cella e sceglie il meno usato.
	Variety float64
	// UseAll garantisce che ogni foto della libreria compaia almeno una volta:
	// prima si riserva a ciascuna la sua cella migliore, poi si riempie il
	// resto normalmente. Vale solo con riutilizzo illimitato, perché con un
	// limite la copertura dipende già dal limite scelto.
	UseAll bool
	// Candidates è quante tessere candidate valutare per cella.
	Candidates int
	Workers    int
	// Progress, se presente, segue le fasi lunghe della costruzione. Su griglie
	// grandi la composizione dura più della decodifica delle foto, quindi vale
	// la pena mostrarla. Viene chiamata da più goroutine.
	Progress func(phase string, done, total int)
}

// reporter accorpa gli aggiornamenti di avanzamento: su un milione di celle,
// chiamare la callback per ognuna costerebbe più del lavoro stesso.
type reporter struct {
	fn    func(phase string, done, total int)
	phase string
	total int
	step  int64
	done  atomic.Int64
}

// start apre una fase nuova. Va chiamata dalla goroutine principale, prima di
// far partire i worker che poi useranno add.
func (r *reporter) start(phase string, total int) {
	if r == nil || r.fn == nil {
		return
	}
	r.phase, r.total = phase, total
	r.done.Store(0)
	r.step = int64(total) / 200
	if r.step < 1 {
		r.step = 1
	}
	r.fn(phase, 0, total)
}

// add segnala un elemento completato.
func (r *reporter) add() {
	if r == nil || r.fn == nil {
		return
	}
	done := r.done.Add(1)
	if done%r.step == 0 || done == int64(r.total) {
		r.fn(r.phase, int(done), r.total)
	}
}

// Result è l'esito di una costruzione.
type Result struct {
	Image       *image.RGBA
	Geometry    Geometry
	LibraryTile int           // foto reali disponibili
	UniqueUsed  int           // quante foto diverse compaiono nel mosaico
	EmptyCells  int           // celle rimaste nere, senza una foto
	Duration    time.Duration // tempo di costruzione

	debugUsage []int // conteggio d'uso per foto, usato dai test
}

type candidate struct {
	tile int32
	dist float32
}

// Build compone il mosaico: adatta il target al canvas, estrae la firma di
// ogni cella, sceglie la tessera migliore e incolla il risultato.
func Build(ctx context.Context, target image.Image, lib *Library, opts Options) (*Result, error) {
	start := time.Now()
	g := opts.Geometry
	if g.Cols < 1 || g.Rows < 1 || g.OutW < 1 || g.OutH < 1 {
		return nil, fmt.Errorf("invalid geometry: %dx%d cells on %dx%d px", g.Cols, g.Rows, g.OutW, g.OutH)
	}
	cells := g.Cells()

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	rep := &reporter{fn: opts.Progress}

	tiles := lib.Snapshot()
	realTiles := len(tiles)

	// Le celle che nessuna foto può coprire restano nere. Basta una sola
	// tessera nera per tutte: sono identiche, e crearne una per cella
	// significherebbe, su una griglia da un milione di celle, confrontare ogni
	// cella con un milione di tessere uguali fra loro.
	var filler *Tile
	if BlackCells(realTiles, cells, opts.MaxReuse) > 0 || realTiles == 0 {
		filler = NewSyntheticTile(g.TileW, g.TileH)
	}
	if realTiles == 0 && filler == nil {
		return nil, fmt.Errorf("no tiles available")
	}

	// Target riportato alla risoluzione esatta di output.
	scaled, err := renderFitInto(ctx, target, g.OutW, g.OutH, opts.Fit, workers, rep)
	if err != nil {
		return nil, err
	}

	assign := make([]int32, cells)
	usage := make([]int, realTiles)
	unique := 0

	if realTiles == 0 {
		// Nessuna foto: tutte le celle vanno alla tessera nera.
		for i := range assign {
			assign[i] = -1
		}
	} else {
		// Firma cromatica di ogni cella.
		feats := make([]Feature, cells)
		rep.start("Analysing target", cells)
		if err := parallelFor(ctx, cells, workers, func(i int) {
			feats[i] = featureOf(scaled, g.CellRect(i%g.Cols, i/g.Cols))
			rep.add()
		}); err != nil {
			return nil, err
		}

		k := opts.Candidates
		if k <= 0 {
			k = 32
		}
		if k > realTiles {
			k = realTiles
		}

		// Per ogni cella la classifica delle k foto più simili. La tessera nera
		// non entra mai in classifica: è il ripiego per le celle che restano
		// senza foto, non un'alternativa da preferire a una foto vera.
		cands := make([]candidate, cells*k)
		rep.start("Matching tiles", cells)
		if err := parallelFor(ctx, cells, workers, func(i int) {
			topK(&feats[i], tiles, cands[i*k:(i+1)*k])
			rep.add()
		}); err != nil {
			return nil, err
		}

		assign, usage = assignTiles(cands, k, tiles, feats, g, opts, rep)
		for _, u := range usage {
			if u > 0 {
				unique++
			}
		}
	}

	empty := 0
	for _, a := range assign {
		if a < 0 {
			empty++
		}
	}

	out := image.NewRGBA(image.Rect(0, 0, g.OutW, g.OutH))
	rep.start("Composing mosaic", g.Rows)
	if err := parallelFor(ctx, g.Rows, workers, func(cy int) {
		defer rep.add()
		for cx := 0; cx < g.Cols; cx++ {
			// Le celle senza una foto restano nere piene: il mosaico si compone
			// man mano che arrivano i file, invece di mostrare subito il target
			// in trasparenza sotto le tessere di riempimento.
			tile, blend := filler, 0.0
			if idx := assign[cy*g.Cols+cx]; idx >= 0 {
				tile, blend = tiles[idx], opts.Blend
			}
			compose(out, scaled, tile, g.CellRect(cx, cy), blend)
		}
	}); err != nil {
		return nil, err
	}

	return &Result{
		Image:       out,
		Geometry:    g,
		LibraryTile: realTiles,
		UniqueUsed:  unique,
		EmptyCells:  empty,
		Duration:    time.Since(start),
		debugUsage:  usage,
	}, nil
}

// BlackCells dice quante celle resteranno nere: sono quelle che la libreria non
// riesce a coprire rispettando il limite di riutilizzo.
func BlackCells(have, cells, maxReuse int) int {
	if cells <= 0 {
		return 0
	}
	if have == 0 {
		return cells
	}
	if maxReuse <= 0 {
		return 0 // riutilizzo illimitato: una foto qualsiasi copre ogni cella
	}
	if capacity := have * maxReuse; capacity < cells {
		return cells - capacity
	}
	return 0
}

// topK riempie out con le tessere più vicine alla firma richiesta, ordinate per
// distanza crescente.
func topK(f *Feature, tiles []*Tile, out []candidate) {
	n := 0
	for i, t := range tiles {
		d := f.Distance(&t.Feature)
		if n == len(out) {
			if float32(d) >= out[n-1].dist {
				continue
			}
			n-- // scarta il peggiore
		}
		pos := n
		for pos > 0 && float64(out[pos-1].dist) > d {
			out[pos] = out[pos-1]
			pos--
		}
		out[pos] = candidate{tile: int32(i), dist: float32(d)}
		n++
	}
}

// assignTiles decide quale tessera va in quale cella.
func assignTiles(cands []candidate, k int, tiles []*Tile, feats []Feature, g Geometry, opts Options, rep *reporter) ([]int32, []int) {
	cells := g.Cells()
	assign := make([]int32, cells)
	for i := range assign {
		assign[i] = -1
	}
	usage := make([]int, len(tiles))

	neighbourConflict := func(cell int, tile int32) bool {
		if opts.AllowAdjacent {
			return false
		}
		cx := cell % g.Cols
		if cx > 0 && assign[cell-1] == tile {
			return true
		}
		if cell >= g.Cols && assign[cell-g.Cols] == tile {
			return true
		}
		if cx < g.Cols-1 && assign[cell+1] == tile {
			return true
		}
		if cell+g.Cols < cells && assign[cell+g.Cols] == tile {
			return true
		}
		return false
	}

	if opts.MaxReuse > 0 {
		assignLimited(cands, k, tiles, feats, g, opts.MaxReuse, assign, usage, neighbourConflict, rep)
		return assign, usage
	}

	// Riutilizzo libero. Se è stato chiesto di usarle tutte, si comincia
	// riservando a ogni foto la sua cella migliore — la stessa assegnazione
	// globale del percorso con limite, con limite uno — e solo dopo si riempie
	// il resto. Così nessuna foto resta fuori, ma il grosso del mosaico usa
	// comunque gli abbinamenti migliori.
	if opts.UseAll {
		assignLimited(cands, k, tiles, feats, g, 1, assign, usage, neighbourConflict, rep)
	}

	fillRemaining(cands, k, cells, opts, assign, usage, neighbourConflict)
	return assign, usage
}

// fillRemaining assegna a ogni cella ancora libera il suo candidato migliore.
//
// Prendere sempre il più affine però impoverisce il mosaico quando la libreria
// è cromaticamente omogenea: poche foto "rappresentative" vincono tutte le
// celle simili e il resto della libreria non compare mai. Si guarda quindi una
// banda di tolleranza attorno al miglior abbinamento e, fra i candidati che ci
// rientrano — quelli praticamente equivalenti a occhio — si sceglie quello
// usato meno finora.
func fillRemaining(cands []candidate, k, cells int, opts Options, assign []int32, usage []int, conflict func(int, int32) bool) {
	tol := opts.Variety
	if tol < 0 {
		tol = 0
	}
	if tol > 1 {
		tol = 1
	}

	for cell := 0; cell < cells; cell++ {
		if assign[cell] != -1 {
			continue
		}
		row := cands[cell*k : (cell+1)*k]
		limit := row[0].dist
		if tol > 0 {
			limit += float32(tol) * (row[len(row)-1].dist - row[0].dist)
		}

		chosen := int32(-1)
		for _, c := range row {
			if conflict(cell, c.tile) {
				continue
			}
			if chosen < 0 {
				chosen = c.tile
				if c.dist > limit {
					break // fuori banda: non c'è niente di meglio da valutare
				}
				continue
			}
			if c.dist > limit {
				break
			}
			if usage[c.tile] < usage[chosen] {
				chosen = c.tile
			}
		}
		if chosen < 0 {
			chosen = row[0].tile // tutti in conflitto: si ripiega sul migliore
		}
		assign[cell] = chosen
		usage[chosen]++
	}
}

// assignLimited assegna le celle rispettando un tetto di riutilizzo per foto.
//
// L'ordine conta: assegnando in ordine di lettura, le prime celle si
// prenderebbero tutte le foto migliori. Si considerano quindi globalmente le
// coppie (cella, foto) partendo dalle più affini. Le celle che non ricevono
// niente restano a -1.
func assignLimited(cands []candidate, k int, tiles []*Tile, feats []Feature, g Geometry, limit int, assign []int32, usage []int, conflict func(int, int32) bool, rep *reporter) {
	cells := g.Cells()

	// Quando le celle sono molte più di quante ne possa coprire la libreria,
	// solo quelle con l'abbinamento migliore riceveranno una foto: costruire e
	// ordinare le coppie di tutto il milione sarebbe lavoro buttato.
	candidateCells := hopefulCells(cands, k, cells, len(tiles)*limit)

	type pair struct {
		cell int32
		tile int32
		dist float32
	}
	rep.start("Placing photos", len(candidateCells))
	pairs := make([]pair, 0, len(candidateCells)*k)
	for _, cell := range candidateCells {
		for _, c := range cands[int(cell)*k : (int(cell)+1)*k] {
			pairs = append(pairs, pair{cell: cell, tile: c.tile, dist: c.dist})
		}
		rep.add()
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		switch {
		case a.dist < b.dist:
			return -1
		case a.dist > b.dist:
			return 1
		case a.cell != b.cell:
			return int(a.cell - b.cell)
		default:
			return int(a.tile - b.tile)
		}
	})

	remaining := len(candidateCells)
	for _, p := range pairs {
		if remaining == 0 {
			break
		}
		if assign[p.cell] != -1 || usage[p.tile] >= limit {
			continue
		}
		if conflict(int(p.cell), p.tile) {
			continue
		}
		assign[p.cell] = p.tile
		usage[p.tile]++
		remaining--
	}

	if remaining == 0 {
		return
	}

	// Foto rimaste senza posto perché i loro candidati erano tutti occupati.
	// Si riparte dalle stesse celle, già ordinate dalla più promettente, così
	// che finiscano dove rendono meglio.
	avail := make([]int32, 0, len(tiles))
	for i := range tiles {
		if usage[i] < limit {
			avail = append(avail, int32(i))
		}
	}
	for _, cell := range candidateCells {
		if len(avail) == 0 {
			break
		}
		if assign[cell] != -1 {
			continue
		}
		bestIdx, bestDist := 0, float64(-1)
		for i, ti := range avail {
			d := feats[cell].Distance(&tiles[ti].Feature)
			if bestDist < 0 || d < bestDist {
				bestIdx, bestDist = i, d
			}
		}
		t := avail[bestIdx]
		assign[cell] = t
		usage[t]++
		if usage[t] >= limit {
			avail = append(avail[:bestIdx], avail[bestIdx+1:]...)
		}
	}
}

// hopefulCells restituisce le celle che vale la pena considerare per
// l'assegnazione globale: tutte, se la libreria ha capienza sufficiente,
// altrimenti solo quelle il cui abbinamento migliore è più promettente.
//
// Il margine è generoso (quattro volte la capienza) perché una cella può
// perdere la sua foto ideale a favore di una cella ancora più affine.
func hopefulCells(cands []candidate, k, cells, capacity int) []int32 {
	all := func() []int32 {
		out := make([]int32, cells)
		for i := range out {
			out[i] = int32(i)
		}
		return out
	}
	if capacity <= 0 || capacity >= cells {
		return all()
	}
	keep := capacity * 4
	if keep < 1024 {
		keep = 1024
	}
	if keep >= cells {
		return all()
	}

	order := all()
	// Ordinare un milione di indici costa una frazione dell'ordinare trenta
	// milioni di coppie, ed è l'unico modo per sapere quali celle scartare.
	slices.SortFunc(order, func(a, b int32) int {
		return cmp.Compare(cands[int(a)*k].dist, cands[int(b)*k].dist)
	})
	return order[:keep]
}

// compose incolla una tessera nella cella, eventualmente miscelandola con il
// target per rendere il soggetto più riconoscibile.
func compose(dst *image.RGBA, target *image.RGBA, tile *Tile, cell image.Rectangle, blend float64) {
	w, h := cell.Dx(), cell.Dy()
	if w <= 0 || h <= 0 {
		return
	}
	if tile.W < w || tile.H < h {
		// Tessera più piccola della cella (può capitare solo con cache
		// disallineate): ripieghiamo su un ridimensionamento al volo.
		src := &image.RGBA{Pix: tile.Pix, Stride: tile.W * 4, Rect: image.Rect(0, 0, tile.W, tile.H)}
		draw.ApproxBiLinear.Scale(dst, cell, src, src.Bounds(), draw.Src, nil)
		return
	}

	if blend < 0 {
		blend = 0
	}
	if blend > 1 {
		blend = 1
	}
	b := blend
	inv := 1 - b

	for y := 0; y < h; y++ {
		di := dst.PixOffset(cell.Min.X, cell.Min.Y+y)
		drow := dst.Pix[di : di+w*4]
		srow := tile.Pix[y*tile.W*4 : y*tile.W*4+w*4]
		if b == 0 {
			copy(drow, srow)
			continue
		}
		trow := target.Pix[di : di+w*4]
		for i := 0; i < len(drow); i += 4 {
			drow[i] = uint8(float64(srow[i])*inv + float64(trow[i])*b + 0.5)
			drow[i+1] = uint8(float64(srow[i+1])*inv + float64(trow[i+1])*b + 0.5)
			drow[i+2] = uint8(float64(srow[i+2])*inv + float64(trow[i+2])*b + 0.5)
			drow[i+3] = 255
		}
	}
}

// parallelFor esegue fn(i) per i in [0,n) distribuendo il lavoro sui worker.
func parallelFor(ctx context.Context, n, workers int, fn func(i int)) error {
	if n <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	next := 0
	const chunk = 16

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				start := next
				next += chunk
				mu.Unlock()
				if start >= n {
					return
				}
				end := min(start+chunk, n)
				if ctx.Err() != nil {
					return
				}
				for i := start; i < end; i++ {
					fn(i)
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}
