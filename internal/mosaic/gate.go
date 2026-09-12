package mosaic

import (
	"bufio"
	"image"
	"image/color"
	"os"
	"sync"
)

// memGate è un semaforo pesato: ogni decodifica prenota i byte che prevede di
// allocare e li restituisce quando ha finito. Serve perché il costo di una foto
// dipende dai suoi megapixel, non dal numero di file: con un semplice limite di
// goroutine, venti foto da 24 MP fanno esplodere la RAM esattamente come
// quaranta da 6 MP non la farebbero.
type memGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int64
	used     int64
}

func newMemGate(capacity int64) *memGate {
	if capacity <= 0 {
		return nil // nessun tetto richiesto
	}
	g := &memGate{capacity: capacity}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// acquire attende che ci sia spazio per n byte. Una singola foto più grande
// dell'intero budget passa comunque, da sola: meglio un picco isolato che un
// blocco definitivo.
func (g *memGate) acquire(n int64) {
	if g == nil || n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n > g.capacity {
		n = g.capacity
	}
	for g.used+n > g.capacity && g.used > 0 {
		g.cond.Wait()
	}
	g.used += n
}

func (g *memGate) release(n int64) {
	if g == nil || n <= 0 {
		return
	}
	g.mu.Lock()
	if n > g.capacity {
		n = g.capacity
	}
	g.used -= n
	if g.used < 0 {
		g.used = 0
	}
	g.mu.Unlock()
	g.cond.Broadcast()
}

// decodeCost stima quanta memoria servirà per trasformare un file in tessera,
// leggendo solo l'intestazione dell'immagine (poche decine di byte) invece di
// decodificarla.
//
// Le componenti sono: il buffer della decodifica a piena risoluzione (1,5 byte
// per pixel per un JPEG 4:2:0, 4 per tutto il resto) e i buffer della riduzione
// a blocchi, proporzionali alla tessera e non alla sorgente.
func decodeCost(path string, tileW, tileH int) int64 {
	const fallback = 32 << 20 // intestazione illeggibile: stima prudente

	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return fallback
	}

	pixels := int64(cfg.Width) * int64(cfg.Height)
	perPixel := int64(4)
	if cfg.ColorModel == color.YCbCrModel {
		perPixel = 2 // 1,5 arrotondato per eccesso
	}
	// Immagine ridotta (4 byte/px) più i quattro accumulatori (16 byte/px).
	scratch := int64(tileW*shrinkTarget) * int64(tileH*shrinkTarget) * 20
	return pixels*perPixel + scratch + (1 << 20)
}
