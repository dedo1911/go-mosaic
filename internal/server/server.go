// Package server espone il mosaico su una pagina web renderizzata lato server,
// che si aggiorna da sola quando una nuova versione è pronta.
package server

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/dedo1911/go-mosaic/internal/mosaic"
)

//go:embed templates/*.html
var templatesFS embed.FS

// State è tutto ciò che la pagina mostra.
type State struct {
	Status     string // idle | building | ready | error
	Message    string
	Version    int64
	SourceDir  string
	TargetPath string
	OutputPath string
	Watching   bool
	Blend      float64
	MaxReuse   int

	Cols, Rows   int
	OutW, OutH   int
	TileW, TileH int

	Photos     int
	UniqueUsed int
	EmptyCells int

	Duration  time.Duration
	HeapBytes int64
	UpdatedAt time.Time

	Image     []byte
	ImageType string
	ImageName string
}

// Server tiene lo stato corrente e i client collegati via SSE.
type Server struct {
	mu    sync.RWMutex
	state State
	tmpl  *template.Template
	subs  map[chan int64]struct{}
	log   *log.Logger
}

// New prepara il server (senza metterlo in ascolto).
func New(logger *log.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		tmpl:  tmpl,
		subs:  make(map[chan int64]struct{}),
		state: State{Status: "idle"},
		log:   logger,
	}, nil
}

// Update modifica lo stato e avvisa i browser collegati.
func (s *Server) Update(fn func(*State)) {
	s.mu.Lock()
	fn(&s.state)
	version := s.state.Version
	subs := make([]chan int64, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- version:
		default: // il client è indietro: riceverà comunque l'ultimo stato
		}
	}
}

// Handler costruisce il router HTTP.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /fragment", s.handleFragment)
	mux.HandleFunc("GET /mosaic", s.handlePreview)
	mux.HandleFunc("GET /image", s.handleImage)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// ListenAndServe serve finché il contesto non viene annullato.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.log.Printf("web interface on http://%s", friendlyAddr(ln.Addr().String()))

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func friendlyAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "index", s.view()); err != nil {
		s.log.Printf("rendering the page: %v", err)
	}
}

func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "fragment", s.view()); err != nil {
		s.log.Printf("rendering the fragment: %v", err)
	}
}

// handlePreview serve la pagina di anteprima a tutto schermo: è renderizzata
// lato server come la home e, via SSE, sostituisce l'immagine appena ne esiste
// una nuova.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "preview", s.view()); err != nil {
		s.log.Printf("rendering the preview: %v", err)
	}
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	img, ctype, name, modTime := s.state.Image, s.state.ImageType, s.state.ImageName, s.state.UpdatedAt
	s.mu.RUnlock()

	if len(img) == 0 {
		http.Error(w, "no mosaic available yet", http.StatusNotFound)
		return
	}
	if ctype == "" {
		ctype = "image/jpeg"
	}
	if name == "" {
		name = "mosaico.jpg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	}
	http.ServeContent(w, r, name, modTime, bytes.NewReader(img))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := make(chan int64, 4)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	version := s.state.Version
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	fmt.Fprintf(w, "retry: 2000\nevent: hello\ndata: %d\n\n", version)
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case v := <-ch:
			fmt.Fprintf(w, "event: mosaic\ndata: %d\n\n", v)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// view è il modello passato ai template: tutto già formattato per la lettura.
type view struct {
	Status      string
	StatusLabel string
	Message     string
	Version     int64
	HasImage    bool
	ImageURL    string // byte grezzi dell'ultimo mosaico
	PreviewURL  string // pagina di anteprima che si aggiorna da sola
	DownloadURL string
	EmptyText   string

	Grid       string
	Resolution string
	TileSize   string
	Cells      string

	Photos        string
	HasEmptyCells bool
	EmptyCells    string
	UniqueUsed    string
	MaxReuse      string
	Blend         string

	SourceDir      string
	SourceDirFull  string
	TargetPath     string
	TargetPathFull string
	OutputPath     string
	OutputPathFull string

	Duration  string
	Memory    string
	UpdatedAt string
	Watching  bool
}

func (s *Server) view() view {
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()

	v := view{
		Status:         st.Status,
		PreviewURL:     "/mosaic",
		Message:        st.Message,
		Version:        st.Version,
		HasImage:       len(st.Image) > 0,
		Grid:           fmt.Sprintf("%d × %d", st.Cols, st.Rows),
		Resolution:     fmt.Sprintf("%d × %d px", st.OutW, st.OutH),
		TileSize:       fmt.Sprintf("%d × %d px", st.TileW, st.TileH),
		Cells:          formatInt(st.Cols * st.Rows),
		Photos:         formatInt(st.Photos),
		HasEmptyCells:  st.EmptyCells > 0,
		EmptyCells:     formatInt(st.EmptyCells),
		UniqueUsed:     formatInt(st.UniqueUsed),
		Blend:          fmt.Sprintf("%.0f%%", st.Blend*100),
		SourceDir:      shortPath(st.SourceDir),
		SourceDirFull:  absPath(st.SourceDir),
		TargetPath:     filepath.Base(st.TargetPath),
		TargetPathFull: absPath(st.TargetPath),
		OutputPath:     filepath.Base(st.OutputPath),
		OutputPathFull: absPath(st.OutputPath),
		Watching:       st.Watching,
	}

	if st.MaxReuse <= 0 {
		v.MaxReuse = "unlimited"
	} else {
		v.MaxReuse = fmt.Sprintf("%d×", st.MaxReuse)
	}

	switch st.Status {
	case "ready":
		v.StatusLabel = "ready"
	case "building":
		v.StatusLabel = "building…"
	case "error":
		v.StatusLabel = "error"
	default:
		v.StatusLabel = "idle"
		v.EmptyText = "Waiting for the first build."
	}

	if v.HasImage {
		v.ImageURL = fmt.Sprintf("/image?v=%d", st.Version)
		v.DownloadURL = v.ImageURL + "&download=1"
	} else if v.EmptyText == "" {
		if st.Status == "error" {
			v.EmptyText = "No mosaic to show."
		} else {
			v.EmptyText = "Building the mosaic…"
		}
	}

	if st.Duration > 0 {
		v.Duration = formatDuration(st.Duration)
	} else {
		v.Duration = "—"
	}
	if st.HeapBytes > 0 {
		v.Memory = mosaic.FormatBytes(st.HeapBytes)
	} else {
		v.Memory = "—"
	}
	if st.UpdatedAt.IsZero() {
		v.UpdatedAt = "—"
	} else {
		v.UpdatedAt = st.UpdatedAt.Format("15:04:05")
	}
	return v
}

// shortPath accorcia un percorso alle ultime due parti, così il pannello resta
// leggibile anche con cartelle annidate; il percorso completo finisce nel
// tooltip.
func shortPath(p string) string {
	if p == "" {
		return "—"
	}
	clean := filepath.Clean(p)
	dir, base := filepath.Split(clean)
	dir = filepath.Clean(dir)
	if dir == "." || dir == string(filepath.Separator) || base == "" {
		return clean
	}
	return "…/" + filepath.Join(filepath.Base(dir), base)
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

// formatInt inserisce il separatore delle migliaia.
func formatInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
