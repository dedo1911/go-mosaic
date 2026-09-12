package mosaic

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const cacheVersion = 1

type cachePayload struct {
	Version      int
	TileW, TileH int
	Tiles        []*Tile
}

// LoadCache ripopola la libreria con le miniature salvate in precedenza, così
// un riavvio non deve ridecodificare migliaia di foto. Le voci obsolete
// vengono comunque scartate dal primo Sync. Restituisce quante tessere ha
// recuperato; una cache mancante o incompatibile non è un errore.
func (l *Library) LoadCache(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	var payload cachePayload
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&payload); err != nil {
		return 0, nil // cache corrotta: la rigeneriamo da zero
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if payload.Version != cacheVersion || payload.TileW != l.tileW || payload.TileH != l.tileH {
		return 0, nil
	}
	n := 0
	for _, t := range payload.Tiles {
		if t == nil || t.Synthetic || len(t.Pix) != t.W*t.H*4 {
			continue
		}
		l.tiles[t.Path] = t
		n++
	}
	return n, nil
}

// SaveCache scrive su disco le miniature attuali.
func (l *Library) SaveCache(path string) error {
	if path == "" {
		return nil
	}
	l.mu.Lock()
	payload := cachePayload{Version: cacheVersion, TileW: l.tileW, TileH: l.tileH}
	for _, t := range l.tiles {
		if !t.Synthetic {
			payload.Tiles = append(payload.Tiles, t)
		}
	}
	l.mu.Unlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return fmt.Errorf("encoding the cache: %w", err)
	}
	return WriteFileAtomic(path, buf.Bytes())
}
