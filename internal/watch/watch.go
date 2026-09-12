// Package watch osserva il filesystem e segnala, con un debounce, quando c'è
// qualcosa da rigenerare.
package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher raggruppa più percorsi e produce un singolo evento "qualcosa è
// cambiato" dopo che l'attività si è calmata.
type Watcher struct {
	fsw      *fsnotify.Watcher
	debounce time.Duration
	events   chan struct{}
	errs     chan error
	accept   func(path string) bool

	mu      sync.Mutex
	dirs    map[string]bool
	watched map[string]bool
	closed  bool
	done    chan struct{}
}

// New crea un watcher. accept filtra i percorsi interessanti (nil = tutti).
func New(debounce time.Duration, accept func(path string) bool) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	if accept == nil {
		accept = func(string) bool { return true }
	}
	w := &Watcher{
		fsw:      fsw,
		debounce: debounce,
		events:   make(chan struct{}, 1),
		errs:     make(chan error, 8),
		accept:   accept,
		dirs:     make(map[string]bool),
		watched:  make(map[string]bool),
		done:     make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

// AddTree mette sotto osservazione una cartella (e, se richiesto, le
// sottocartelle).
func (w *Watcher) AddTree(root string, recursive bool) error {
	if !recursive {
		return w.addDir(root)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && len(d.Name()) > 1 && d.Name()[0] == '.' {
			return fs.SkipDir
		}
		return w.addDir(path)
	})
}

// AddFile osserva un singolo file: in pratica si osserva la cartella che lo
// contiene, perché molti editor sostituiscono il file invece di modificarlo.
func (w *Watcher) AddFile(path string) error {
	return w.addDir(filepath.Dir(path))
}

func (w *Watcher) addDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	w.mu.Lock()
	if w.closed || w.watched[abs] {
		w.mu.Unlock()
		return nil
	}
	w.watched[abs] = true
	w.dirs[abs] = true
	w.mu.Unlock()

	if err := w.fsw.Add(dir); err != nil {
		w.mu.Lock()
		delete(w.watched, abs)
		delete(w.dirs, abs)
		w.mu.Unlock()
		return err
	}
	return nil
}

// Events emette un valore quando è il momento di rigenerare.
func (w *Watcher) Events() <-chan struct{} { return w.events }

// Errors riporta gli errori non fatali del watcher.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Close ferma l'osservazione.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	err := w.fsw.Close()
	<-w.done
	return err
}

func (w *Watcher) loop() {
	defer close(w.done)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false

	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !w.interesting(ev) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					// Nuova sottocartella: va osservata anche lei.
					_ = w.AddTree(ev.Name, true)
				}
			}
			if pending && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.debounce)
			pending = true

		case <-timer.C:
			pending = false
			select {
			case w.events <- struct{}{}:
			default: // un evento è già in coda: non serve accodarne un altro
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			select {
			case w.errs <- err:
			default:
			}
		}
	}
}

func (w *Watcher) interesting(ev fsnotify.Event) bool {
	if ev.Op == fsnotify.Chmod {
		return false // i soli cambi di permessi non alterano le immagini
	}
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			return true
		}
	}
	return w.accept(ev.Name)
}
