// Comando go-mosaic: costruisce un mosaico fotografico usando le immagini di
// una cartella come tessere di un'immagine target. Con --watch la cartella
// resta sotto osservazione e il mosaico viene rigenerato a ogni modifica; la
// pagina web integrata si aggiorna da sola quando c'è una nuova versione.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dedo1911/go-mosaic/internal/mosaic"
	"github.com/dedo1911/go-mosaic/internal/server"
	"github.com/dedo1911/go-mosaic/internal/ui"
	"github.com/dedo1911/go-mosaic/internal/watch"
)

const usage = `go-mosaic — build a photo mosaic from a folder of images.

Usage:
  go-mosaic -src FOLDER -target IMAGE [options]

Examples:
  # 80x60 tiles, 4K output
  go-mosaic -src ./photos -target portrait.jpg -grid 80x60 -size 3840x2160

  # rebuild on every change + web page on http://localhost:8080
  go-mosaic -src ./photos -target portrait.jpg -grid 60x40 -watch

Options:
`

// version viene impostata dal linker durante la release
// (-ldflags "-X main.version=v1.2.3"). Compilando a mano resta vuota e si
// ripiega sulle informazioni di build del modulo.
var version string

// buildVersion descrive la copia in esecuzione: il tag della release, oppure
// la versione del modulo per chi installa con "go install", oppure la revisione
// git per chi compila dal sorgente.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	revision, dirty := "", ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
			if len(revision) > 7 {
				revision = revision[:7]
			}
		case "vcs.modified":
			if setting.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if revision == "" {
		return "dev"
	}
	return revision + dirty
}

type config struct {
	src       string
	target    string
	out       string
	gridRaw   string
	sizeRaw   string
	aspectRaw string
	tilePx    int
	fitRaw    string
	blend     float64
	maxReuse  int
	adjacent  bool
	variety   float64
	useAll    bool
	revealRaw string
	cands     int
	recurse   bool
	workers   int
	memory    string
	quality   int
	cache     string
	watch     bool
	debounce  time.Duration
	serve     string

	geometry   mosaic.GeometrySpec
	fit        mosaic.Fit
	reveal     mosaic.Reveal
	addr       string
	memoryPeak int64
	decodeGate int64
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	cfg := &config{}
	fs := flag.NewFlagSet("go-mosaic", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}

	showVersion := fs.Bool("version", false, "print the version and exit")

	fs.StringVar(&cfg.src, "src", "", "folder holding the images used as tiles (required)")
	fs.StringVar(&cfg.target, "target", "", "image to reproduce (required)")
	fs.StringVar(&cfg.out, "out", "mosaic.jpg", "output file (.jpg or .png)")
	fs.StringVar(&cfg.gridRaw, "grid", "64", "grid size in tiles, e.g. 80x60 (width only = height derived from the target)")
	fs.StringVar(&cfg.sizeRaw, "size", "", "resolution of the final image, e.g. 3840x2160 (empty = grid x -tile)")
	fs.StringVar(&cfg.aspectRaw, "aspect", "", "aspect ratio of the output, e.g. 16:9; tiles turn rectangular when the grid does not match (empty = follow the target image)")
	fs.IntVar(&cfg.tilePx, "tile", 64, "tile side in pixels, used only when -size is not given")
	fs.StringVar(&cfg.fitRaw, "fit", "cover", "how the target fits the canvas: cover, contain or stretch")
	fs.Float64Var(&cfg.blend, "blend", 0.25, "blend tiles towards the target, from 0 (untouched photos) to 1 (target only)")
	fs.IntVar(&cfg.maxReuse, "max-reuse", 1, "how many times a photo may be reused (0 = unlimited)")
	fs.BoolVar(&cfg.adjacent, "allow-adjacent", false, "allow the same photo in two neighbouring cells")
	fs.Float64Var(&cfg.variety, "variety", 0.25, "with unlimited reuse, how far from the best match to go in order to use a photo that has been picked less often, from 0 (always the closest) to 1 (spread across the whole library)")
	fs.BoolVar(&cfg.useAll, "use-all", false, "make sure every photo in the library appears at least once, as long as the grid has enough cells")
	fs.StringVar(&cfg.revealRaw, "reveal", "fit", "where photos go while cells are still black: fit puts each one where it matches best, so the subject shows early; random uncovers cells in a fixed random order, so the subject emerges only as the grid fills")
	fs.IntVar(&cfg.cands, "candidates", 32, "candidate tiles evaluated per cell")
	fs.BoolVar(&cfg.recurse, "recursive", true, "also look for images in subfolders")
	fs.IntVar(&cfg.workers, "workers", 0, "processing goroutines (0 = number of CPUs)")
	fs.StringVar(&cfg.memory, "memory", "512MiB", "memory ceiling for the process: less RAM means fewer photos decoded at once, so slower scans (0 = no ceiling)")
	fs.IntVar(&cfg.quality, "quality", 92, "JPEG quality of the output, 1-100")
	fs.StringVar(&cfg.cache, "cache", "", "thumbnail cache file (makes restarts instant)")
	fs.BoolVar(&cfg.watch, "watch", false, "watch the source folder and rebuild on every change")
	fs.DurationVar(&cfg.debounce, "debounce", 700*time.Millisecond, "how long to wait after the last change before rebuilding")
	fs.StringVar(&cfg.serve, "serve", "auto", `web page address ("auto" = localhost:8080 when -watch is set, "off" to disable it; use 0.0.0.0:8080 to reach it from other devices)`)

	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}

	if *showVersion {
		fmt.Println("go-mosaic", buildVersion())
		os.Exit(0)
	}

	if cfg.src == "" || cfg.target == "" {
		fs.Usage()
		return nil, errors.New("both -src and -target are required")
	}

	var err error
	if cfg.geometry.Grid, err = mosaic.ParseDimension(cfg.gridRaw); err != nil {
		return nil, fmt.Errorf("-grid: %w", err)
	}
	if cfg.geometry.Grid.W <= 0 {
		return nil, errors.New("-grid: at least the number of columns is required, e.g. -grid 80x60")
	}
	if cfg.geometry.Size, err = mosaic.ParseDimension(cfg.sizeRaw); err != nil {
		return nil, fmt.Errorf("-size: %w", err)
	}
	if cfg.geometry.Aspect, err = mosaic.ParseAspect(cfg.aspectRaw); err != nil {
		return nil, fmt.Errorf("-aspect: %w", err)
	}
	cfg.geometry.TilePx = cfg.tilePx
	if cfg.fit, err = mosaic.ParseFit(cfg.fitRaw); err != nil {
		return nil, err
	}
	if cfg.reveal, err = mosaic.ParseReveal(cfg.revealRaw); err != nil {
		return nil, fmt.Errorf("-reveal: %w", err)
	}
	if cfg.blend < 0 || cfg.blend > 1 {
		return nil, errors.New("-blend must be between 0 and 1")
	}
	if cfg.variety < 0 || cfg.variety > 1 {
		return nil, errors.New("-variety must be between 0 and 1")
	}
	if cfg.maxReuse < 0 {
		return nil, errors.New("-max-reuse cannot be negative")
	}
	if _, err := mosaic.FormatForPath(cfg.out); err != nil {
		return nil, err
	}
	if cfg.workers <= 0 {
		cfg.workers = runtime.NumCPU()
	}

	if cfg.memoryPeak, err = mosaic.ParseBytes(cfg.memory); err != nil {
		return nil, fmt.Errorf("-memory: %w", err)
	}
	if cfg.memoryPeak > 0 && cfg.memoryPeak < 64<<20 {
		return nil, errors.New("-memory: at least 64MiB is required")
	}
	cfg.decodeGate = mosaic.LiveBudgetFor(cfg.memoryPeak)

	switch strings.ToLower(strings.TrimSpace(cfg.serve)) {
	case "auto":
		if cfg.watch {
			// Solo loopback: la pagina mostra i percorsi assoluti delle tue
			// cartelle e serve il mosaico a chiunque la raggiunga. Per aprirla
			// ad altri dispositivi serve dirlo esplicitamente, per esempio
			// -serve 0.0.0.0:8080.
			cfg.addr = "localhost:8080"
		}
	case "off", "no", "none", "":
		cfg.addr = ""
	default:
		cfg.addr = cfg.serve
		if !strings.Contains(cfg.addr, ":") {
			cfg.addr = "localhost:" + cfg.addr
		}
	}

	return cfg, nil
}

// app tiene insieme configurazione, libreria di tessere e interfaccia web.
type app struct {
	cfg     *config
	lib     *mosaic.Library
	srv     *server.Server
	log     *log.Logger
	format  mosaic.Format
	absOut  string
	absTgt  string
	version int64

	// canvasBytes è quanta memoria chiede l'immagine in costruzione: cambia
	// con il target, quindi va riletta a ogni generazione.
	canvasBytes atomic.Int64

	mu sync.Mutex // serializza le rigenerazioni
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "", log.Ltime)
	if cfg.memoryPeak > 0 {
		logger.Printf("memory ceiling: %s (raise it with -memory for faster scans)", mosaic.FormatBytes(cfg.memoryPeak))
	}
	format, _ := mosaic.FormatForPath(cfg.out)
	absOut, _ := filepath.Abs(cfg.out)
	absTgt, _ := filepath.Abs(cfg.target)

	a := &app{
		cfg:    cfg,
		lib:    mosaic.NewLibrary(1, 1), // ridimensionata alla prima build
		log:    logger,
		format: format,
		absOut: absOut,
		absTgt: absTgt,
	}

	if cfg.addr != "" {
		a.srv, err = server.New(logger)
		if err != nil {
			return err
		}
		a.srv.Update(func(s *server.State) {
			s.SourceDir = cfg.src
			s.TargetPath = cfg.target
			s.OutputPath = cfg.out
			s.Watching = cfg.watch
			s.Blend = cfg.blend
			s.MaxReuse = cfg.maxReuse
			s.Status = "building"
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// Il server parte prima della generazione iniziale. Chi riavvia l'app per
	// cambiare un'impostazione ritrova subito la pagina, che mostra la
	// generazione in corso invece di restare in riconnessione per tutta la
	// scansione; e una porta già occupata si scopre all'istante, non dopo.
	if cfg.addr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.srv.ListenAndServe(ctx, cfg.addr); err != nil {
				errCh <- err
				stop()
			}
		}()
	}

	if err := a.rebuild(ctx); err != nil && ctx.Err() == nil {
		if !cfg.watch && cfg.addr == "" {
			return err
		}
		logger.Printf("build failed: %v", err)
	}

	if !cfg.watch && cfg.addr == "" {
		return nil
	}

	if cfg.watch && ctx.Err() == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.watchLoop(ctx); err != nil && ctx.Err() == nil {
				errCh <- err
				stop()
			}
		}()
	}

	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return err
	}
	logger.Println("shutting down")
	return nil
}

// watchLoop rigenera il mosaico ogni volta che la cartella sorgente o il target
// cambiano.
func (a *app) watchLoop(ctx context.Context) error {
	accept := func(path string) bool {
		abs, err := filepath.Abs(path)
		if err == nil && abs == a.absOut {
			return false // il nostro stesso output non deve innescare un ciclo
		}
		if err == nil && abs == a.absTgt {
			return true
		}
		return mosaic.IsSupportedImage(path)
	}

	w, err := watch.New(a.cfg.debounce, accept)
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	defer w.Close()

	if err := w.AddTree(a.cfg.src, a.cfg.recurse); err != nil {
		return fmt.Errorf("watching %s: %w", a.cfg.src, err)
	}
	if err := w.AddFile(a.cfg.target); err != nil {
		return fmt.Errorf("watching %s: %w", a.cfg.target, err)
	}
	a.log.Printf("watching %s (debounce %s)", a.cfg.src, a.cfg.debounce)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-w.Errors():
			a.log.Printf("watch: %v", err)
		case <-w.Events():
			if err := a.rebuild(ctx); err != nil && ctx.Err() == nil {
				a.log.Printf("build failed: %v", err)
			}
		}
	}
}

// phaseBar mostra l'avanzamento di una fase alla volta, sostituendo la barra
// quando la fase cambia. Il cambio di fase avviene sempre dalla goroutine che
// guida la costruzione, prima che partano i worker che poi aggiornano il
// conteggio, quindi non serve sincronizzare il campo phase.
type phaseBar struct {
	bar   *ui.Progress
	phase string
}

func (p *phaseBar) report(phase string, done, total int) {
	if phase != p.phase {
		p.bar.Done()
		p.bar = ui.New(phase, total)
		p.phase = phase
	}
	p.bar.Set(done)
}

func (p *phaseBar) close() {
	p.bar.Done()
	p.bar, p.phase = nil, ""
}

// applyMemoryLimit tiene il limite del garbage collector appena sopra a quanto
// serve davvero: il tetto richiesto dall'utente più lo spazio occupato dalle
// miniature già caricate. Senza quest'ultimo termine una libreria molto grande
// (dato vivo, non spazzatura) farebbe girare a vuoto il collector.
func (a *app) applyMemoryLimit() {
	if a.cfg.memoryPeak <= 0 {
		return
	}
	debug.SetMemoryLimit(a.cfg.memoryPeak + 3*a.lib.BytesInUse() + a.canvasBytes.Load())
}

// rebuild esegue una generazione completa aggiornando stato e pagina web.
func (a *app) rebuild(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// La libreria cresce durante la scansione: si rialza il limite man mano,
	// invece di fissarlo una volta sola all'avvio.
	a.applyMemoryLimit()
	if a.cfg.memoryPeak > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					a.applyMemoryLimit()
				}
			}
		}()
	}

	a.setStatus("building", "")
	if err := a.buildOnce(ctx); err != nil {
		if ctx.Err() == nil {
			a.setStatus("error", err.Error())
		}
		return err
	}
	return nil
}

func (a *app) buildOnce(ctx context.Context) error {
	cfg := a.cfg

	target, err := mosaic.LoadImage(cfg.target)
	if err != nil {
		return fmt.Errorf("target image: %w", err)
	}
	tb := target.Bounds()

	geo, err := mosaic.ResolveGeometry(tb.Dx(), tb.Dy(), cfg.geometry)
	if err != nil {
		return err
	}

	// Un canvas enorme non entra in nessun tetto di memoria: meglio dirlo
	// prima di provarci, invece di lasciare che il processo venga ucciso.
	a.canvasBytes.Store(geo.CanvasBytes())
	a.applyMemoryLimit()
	if geo.CanvasBytes() > 1<<30 {
		a.log.Printf("large output: %d×%d px needs %s of RAM just for the canvas; use -size to make it smaller",
			geo.OutW, geo.OutH, mosaic.FormatBytes(geo.CanvasBytes()))
	}

	if a.lib.EnsureTileSize(geo.TileW, geo.TileH) {
		if n, err := a.lib.LoadCache(cfg.cache); err != nil {
			a.log.Printf("cache not readable: %v", err)
		} else if n > 0 {
			a.log.Printf("cache: restored %d thumbnails", n)
		}
	}

	skip := func(path string) bool {
		abs, err := filepath.Abs(path)
		return err == nil && abs == a.absOut
	}
	// Una barra per fase, che compare solo se la fase dura abbastanza da
	// meritarla e solo su terminale interattivo.
	var bars phaseBar
	defer bars.close()

	sync, err := a.lib.Sync(ctx, cfg.src, mosaic.SyncOptions{
		Recursive:    cfg.recurse,
		Workers:      cfg.workers,
		MemoryBudget: cfg.decodeGate,
		Skip:         skip,
		Progress: func(done, total int) {
			bars.report("Processing images", done, total)
		},
	})
	bars.close()
	if err != nil {
		return err
	}
	if sync.Added+sync.Updated+sync.Removed > 0 {
		a.log.Printf("library: %d photos (%d new, %d updated, %d removed)",
			sync.Total, sync.Added, sync.Updated, sync.Removed)
	}
	for _, e := range sync.Errors {
		a.log.Printf("skipped: %s", e)
	}

	if black := mosaic.BlackCells(sync.Total, geo.Cells(), cfg.maxReuse); black > 0 {
		a.log.Printf("%d photos for %d cells: %d cells stay black (-max-reuse %d)",
			sync.Total, geo.Cells(), black, cfg.maxReuse)
	}
	if cfg.useAll && sync.Total > geo.Cells() {
		a.log.Printf("-use-all: the grid holds %d cells, so only that many of the %d photos can appear",
			geo.Cells(), sync.Total)
	}

	res, err := mosaic.Build(ctx, target, a.lib, mosaic.Options{
		Geometry:      geo,
		Fit:           cfg.fit,
		Blend:         cfg.blend,
		MaxReuse:      cfg.maxReuse,
		AllowAdjacent: cfg.adjacent,
		Variety:       cfg.variety,
		UseAll:        cfg.useAll,
		Reveal:        cfg.reveal,
		Candidates:    cfg.cands,
		Workers:       cfg.workers,
		Progress:      bars.report,
	})
	bars.close()
	if err != nil {
		return err
	}

	data, err := mosaic.Encode(res.Image, a.format, cfg.quality)
	if err != nil {
		return fmt.Errorf("encoding the output: %w", err)
	}
	if err := mosaic.WriteFileAtomic(cfg.out, data); err != nil {
		return fmt.Errorf("writing %s: %w", cfg.out, err)
	}

	if err := a.lib.SaveCache(cfg.cache); err != nil {
		a.log.Printf("cache not saved: %v", err)
	}

	// Il picco di memoria si tocca durante la scansione, ma il processo resta
	// vivo a lungo: restituiamo subito le pagine al sistema operativo invece di
	// aspettare lo scavenger del runtime.
	if cfg.watch || cfg.addr != "" {
		debug.FreeOSMemory()
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	a.version++
	reuse := "unlimited reuse"
	if cfg.maxReuse > 0 {
		reuse = fmt.Sprintf("max %d× per photo", cfg.maxReuse)
	}
	empty := ""
	if res.EmptyCells > 0 {
		empty = fmt.Sprintf(", %d cells still black", res.EmptyCells)
	}
	a.log.Printf("build #%d: %d×%d tiles on %d×%d px, %d of %d photos used (%s)%s, %s → %s (%s)",
		a.version, geo.Cols, geo.Rows, geo.OutW, geo.OutH,
		res.UniqueUsed, res.LibraryTile, reuse, empty,
		res.Duration.Round(time.Millisecond), cfg.out, mosaic.FormatBytes(int64(len(data))))

	if a.srv != nil {
		version := a.version
		msg := ""
		if res.EmptyCells > 0 {
			msg = fmt.Sprintf("Composing: %d of %d cells are still black, waiting for more photos.",
				res.EmptyCells, geo.Cells())
		}
		a.srv.Update(func(s *server.State) {
			s.Status = "ready"
			s.Message = msg
			s.Version = version
			s.Cols, s.Rows = geo.Cols, geo.Rows
			s.OutW, s.OutH = geo.OutW, geo.OutH
			s.TileW, s.TileH = geo.TileW, geo.TileH
			s.Photos = res.LibraryTile
			s.EmptyCells = res.EmptyCells
			s.UniqueUsed = res.UniqueUsed
			s.Duration = res.Duration
			s.HeapBytes = int64(mem.HeapAlloc)
			s.UpdatedAt = time.Now()
			s.Image = data
			s.ImageType = a.format.ContentType()
			s.ImageName = filepath.Base(cfg.out)
		})
	}
	return nil
}

func (a *app) setStatus(status, msg string) {
	if a.srv == nil {
		return
	}
	a.srv.Update(func(s *server.State) {
		s.Status = status
		s.Message = msg
	})
}
