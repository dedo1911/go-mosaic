package mosaic

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
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
	// Reveal decide dove vanno le foto finché restano celle nere: dove si
	// abbinano meglio, oppure in un ordine casuale fisso.
	Reveal Reveal
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

// Reveal decide dove vanno le foto finché la griglia non è completa, cioè
// finché restano celle nere. A griglia piena le due modalità danno lo stesso
// mosaico: cambia solo il percorso per arrivarci.
type Reveal string

const (
	// RevealFit mette ogni foto dove si abbina meglio: il soggetto del target
	// si riconosce già con poche foto.
	RevealFit Reveal = "fit"
	// RevealRandom scopre le celle in un ordine casuale fisso: il soggetto
	// emerge solo man mano che la griglia si riempie.
	RevealRandom Reveal = "random"
)

// ParseReveal valida il valore passato da CLI.
func ParseReveal(s string) (Reveal, error) {
	switch Reveal(strings.ToLower(strings.TrimSpace(s))) {
	case "", RevealFit:
		return RevealFit, nil
	case RevealRandom:
		return RevealRandom, nil
	}
	return "", fmt.Errorf("invalid reveal %q (use fit or random)", s)
}

// cellOrder restituisce un ordine casuale ma riproducibile delle celle, e la
// posizione di ciascuna in quell'ordine.
//
// Serve a spareggiare le celle equivalenti. Su un target a tinta unita, come
// una scritta bianca su fondo nero, tutte le celle di sfondo hanno la stessa
// firma e l'abbinamento non ha motivo di preferirne una: spareggiando per
// indice le foto finivano in ordine di lettura, riempiendo le prime righe da
// sinistra a destra.
//
// Il seme dipende solo dalla griglia, così l'ordine resta lo stesso fra una
// rigenerazione e l'altra e fra un avvio e l'altro.
func cellOrder(g Geometry) (order, rank []int32) {
	cells := g.Cells()
	order = make([]int32, cells)
	for i := range order {
		order[i] = int32(i)
	}
	rng := rand.New(rand.NewPCG(uint64(g.Cols), uint64(g.Rows)))
	rng.Shuffle(cells, func(i, j int) { order[i], order[j] = order[j], order[i] })

	rank = make([]int32, cells)
	for pos, cell := range order {
		rank[cell] = int32(pos)
	}
	return order, rank
}

// assignment raccoglie lo stato condiviso dalle fasi di assegnazione.
type assignment struct {
	cands []candidate
	k     int
	tiles []*Tile
	feats []Feature
	g     Geometry
	opts  Options
	rep   *reporter

	order []int32 // celle in ordine casuale riproducibile
	rank  []int32 // posizione di ogni cella in order

	assign []int32 // foto assegnata a ogni cella, -1 se ancora libera
	usage  []int   // quante volte è stata usata ogni foto
}

// assignTiles decide quale tessera va in quale cella.
func assignTiles(cands []candidate, k int, tiles []*Tile, feats []Feature, g Geometry, opts Options, rep *reporter) ([]int32, []int) {
	a := &assignment{
		cands:  cands,
		k:      k,
		tiles:  tiles,
		feats:  feats,
		g:      g,
		opts:   opts,
		rep:    rep,
		assign: make([]int32, g.Cells()),
		usage:  make([]int, len(tiles)),
	}
	for i := range a.assign {
		a.assign[i] = -1
	}
	a.order, a.rank = cellOrder(g)

	if opts.MaxReuse > 0 {
		a.limited(opts.MaxReuse, opts.Reveal)
		return a.assign, a.usage
	}

	// Riutilizzo libero. Se è stato chiesto di usarle tutte, si comincia
	// riservando a ogni foto la sua cella migliore — la stessa assegnazione
	// globale del percorso con limite, con limite uno — e solo dopo si riempie
	// il resto. Così nessuna foto resta fuori, ma il grosso del mosaico usa
	// comunque gli abbinamenti migliori. La griglia finirà piena comunque,
	// quindi l'ordine di scoperta qui non conta.
	if opts.UseAll {
		a.limited(1, RevealFit)
	}

	a.fillRemaining()
	return a.assign, a.usage
}

// conflict dice se la foto è già in una cella confinante.
func (a *assignment) conflict(cell int, tile int32) bool {
	if a.opts.AllowAdjacent {
		return false
	}
	cols, cells := a.g.Cols, len(a.assign)
	cx := cell % cols
	return (cx > 0 && a.assign[cell-1] == tile) ||
		(cell >= cols && a.assign[cell-cols] == tile) ||
		(cx < cols-1 && a.assign[cell+1] == tile) ||
		(cell+cols < cells && a.assign[cell+cols] == tile)
}

// fillRemaining assegna a ogni cella ancora libera il suo candidato migliore.
//
// Prendere sempre il più affine però impoverisce il mosaico quando la libreria
// è cromaticamente omogenea: poche foto "rappresentative" vincono tutte le
// celle simili e il resto della libreria non compare mai. Si guarda quindi una
// banda di tolleranza attorno al miglior abbinamento e, fra i candidati che ci
// rientrano — quelli praticamente equivalenti a occhio — si sceglie quello
// usato meno finora.
//
// Le celle si visitano in ordine casuale: in ordine di lettura la rotazione fra
// foto equivalenti disegnerebbe righe riconoscibili.
func (a *assignment) fillRemaining() {
	tol := min(max(a.opts.Variety, 0), 1)
	k := a.k

	for _, c := range a.order {
		cell := int(c)
		if a.assign[cell] != -1 {
			continue
		}
		row := a.cands[cell*k : (cell+1)*k]
		limit := row[0].dist
		if tol > 0 {
			limit += float32(tol) * (row[len(row)-1].dist - row[0].dist)
		}

		chosen := int32(-1)
		for _, cand := range row {
			if a.conflict(cell, cand.tile) {
				continue
			}
			if chosen < 0 {
				chosen = cand.tile
				if cand.dist > limit {
					break // fuori banda: non c'è niente di meglio da valutare
				}
				continue
			}
			if cand.dist > limit {
				break
			}
			if a.usage[cand.tile] < a.usage[chosen] {
				chosen = cand.tile
			}
		}
		if chosen < 0 {
			chosen = row[0].tile // tutti in conflitto: si ripiega sul migliore
		}
		a.assign[cell] = chosen
		a.usage[chosen]++
	}
}

// limited assegna le celle rispettando un tetto di riutilizzo per foto.
//
// L'ordine conta: assegnando in ordine di lettura, le prime celle si
// prenderebbero tutte le foto migliori. Si considerano quindi globalmente le
// coppie (cella, foto) partendo dalle più affini. Le celle che non ricevono
// niente restano a -1, cioè nere.
func (a *assignment) limited(limit int, reveal Reveal) {
	k := a.k
	candidateCells := a.hopefulCells(len(a.tiles)*limit, reveal)

	type pair struct {
		cell int32
		tile int32
		dist float32
	}
	a.rep.start("Placing photos", len(candidateCells))
	pairs := make([]pair, 0, len(candidateCells)*k)
	for _, cell := range candidateCells {
		for _, c := range a.cands[int(cell)*k : (int(cell)+1)*k] {
			pairs = append(pairs, pair{cell: cell, tile: c.tile, dist: c.dist})
		}
		a.rep.add()
	}
	// A parità di distanza decide l'ordine casuale delle celle, non il loro
	// indice: altrimenti su uno sfondo uniforme le foto si accumulerebbero
	// dall'angolo in alto a sinistra.
	slices.SortFunc(pairs, func(x, y pair) int {
		if c := cmp.Compare(x.dist, y.dist); c != 0 {
			return c
		}
		if c := cmp.Compare(a.rank[x.cell], a.rank[y.cell]); c != 0 {
			return c
		}
		return cmp.Compare(x.tile, y.tile)
	})

	remaining := len(candidateCells)
	for _, p := range pairs {
		if remaining == 0 {
			break
		}
		if a.assign[p.cell] != -1 || a.usage[p.tile] >= limit {
			continue
		}
		if a.conflict(int(p.cell), p.tile) {
			continue
		}
		a.assign[p.cell] = p.tile
		a.usage[p.tile]++
		remaining--
	}
	if remaining == 0 {
		return
	}

	// Foto rimaste senza posto perché non comparivano fra le candidate di
	// nessuna cella libera. Si riparte dalle celle candidate, già nell'ordine
	// in cui conviene riempirle.
	avail := make([]int32, 0, len(a.tiles))
	for i := range a.tiles {
		if a.usage[i] < limit {
			avail = append(avail, int32(i))
		}
	}
	for _, cell := range candidateCells {
		if len(avail) == 0 {
			break
		}
		if a.assign[cell] != -1 {
			continue
		}
		bestIdx, bestDist := 0, float64(-1)
		for i, ti := range avail {
			d := a.feats[cell].Distance(&a.tiles[ti].Feature)
			if bestDist < 0 || d < bestDist {
				bestIdx, bestDist = i, d
			}
		}
		t := avail[bestIdx]
		a.assign[cell] = t
		a.usage[t]++
		if a.usage[t] >= limit {
			avail = append(avail[:bestIdx], avail[bestIdx+1:]...)
		}
	}
}

// hopefulCells restituisce le celle che possono ricevere una foto, già
// nell'ordine in cui conviene considerarle.
func (a *assignment) hopefulCells(capacity int, reveal Reveal) []int32 {
	cells := len(a.order)
	if capacity <= 0 || capacity >= cells {
		return a.order // la griglia si riempirà tutta
	}

	if reveal == RevealRandom {
		// Le celle si scoprono nell'ordine casuale fissato: una foto in più
		// scopre una cella in più e quelle già scoperte restano tali. Il
		// soggetto emerge così in modo uniforme man mano che la griglia si
		// riempie, invece di comparire subito dove le foto si abbinano meglio.
		return a.order[:capacity]
	}

	// Solo le celle con l'abbinamento migliore riceveranno una foto: costruire
	// e ordinare le coppie di tutto il milione sarebbe lavoro buttato. Il
	// margine è generoso (quattro volte la capienza) perché una cella può
	// perdere la sua foto ideale a favore di una cella ancora più affine.
	return a.bestCells(min(max(capacity*4, 1024), cells))
}

// bestCells restituisce le keep celle con l'abbinamento migliore, a parità di
// abbinamento quelle che vengono prima nell'ordine casuale, già ordinate.
//
// Non ordina tutte le celle: su un cartello a tinta unita un milione di celle
// hanno la stessa distanza, e ordinarle con lo spareggio costava più di un
// terzo di secondo. Si scorrono invece una volta, in ordine di indice così da
// leggere i candidati in sequenza, tenendo le migliori in uno heap di massimo:
// quasi tutte le celle costano un confronto con la peggiore tenuta.
func (a *assignment) bestCells(keep int) []int32 {
	type item struct {
		dist float32
		rank int32
		cell int32
	}
	// better dice se x va considerata prima di y.
	better := func(x, y item) bool {
		if x.dist != y.dist {
			return x.dist < y.dist
		}
		return x.rank < y.rank
	}

	k := a.k
	heap := make([]item, 0, keep)
	for cell := range a.rank {
		it := item{dist: a.cands[cell*k].dist, rank: a.rank[cell], cell: int32(cell)}
		if len(heap) < keep {
			// Inserimento: la peggiore risale verso la cima.
			heap = append(heap, it)
			for i := len(heap) - 1; i > 0; {
				parent := (i - 1) / 2
				if !better(heap[parent], heap[i]) {
					break
				}
				heap[parent], heap[i] = heap[i], heap[parent]
				i = parent
			}
			continue
		}
		if !better(it, heap[0]) {
			continue // peggiore di tutte quelle già tenute
		}
		// Sostituisce la peggiore e la fa scendere al suo posto.
		heap[0] = it
		for i := 0; ; {
			worst, left := i, 2*i+1
			if left < len(heap) && better(heap[worst], heap[left]) {
				worst = left
			}
			if right := left + 1; right < len(heap) && better(heap[worst], heap[right]) {
				worst = right
			}
			if worst == i {
				break
			}
			heap[i], heap[worst] = heap[worst], heap[i]
			i = worst
		}
	}

	slices.SortFunc(heap, func(x, y item) int {
		if better(x, y) {
			return -1
		}
		if better(y, x) {
			return 1
		}
		return 0
	})
	out := make([]int32, len(heap))
	for i, it := range heap {
		out[i] = it.cell
	}
	return out
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
