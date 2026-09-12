package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherDebouncesAndNotifies(t *testing.T) {
	dir := t.TempDir()

	w, err := New(150*time.Millisecond, func(path string) bool {
		return filepath.Ext(path) == ".jpg"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.AddTree(dir, true); err != nil {
		t.Fatal(err)
	}

	// Più modifiche ravvicinate devono produrre un solo evento.
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, "foto.jpg"), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-w.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("nessun evento dopo la scrittura")
	}

	select {
	case <-w.Events():
		t.Fatal("il debounce doveva unire le modifiche in un solo evento")
	case <-time.After(400 * time.Millisecond):
	}
}

// I file che non superano il filtro non devono svegliare il generatore.
func TestWatcherIgnoresFilteredFiles(t *testing.T) {
	dir := t.TempDir()

	w, err := New(100*time.Millisecond, func(path string) bool {
		return filepath.Ext(path) == ".jpg"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.AddTree(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ciao"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-w.Events():
		t.Fatal("un file non immagine non deve innescare la rigenerazione")
	case <-time.After(700 * time.Millisecond):
	}
}

// Le sottocartelle create dopo l'avvio devono entrare sotto osservazione.
func TestWatcherPicksUpNewSubdirectories(t *testing.T) {
	dir := t.TempDir()

	w, err := New(120*time.Millisecond, func(path string) bool {
		return filepath.Ext(path) == ".jpg"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.AddTree(dir, true); err != nil {
		t.Fatal(err)
	}

	sub := filepath.Join(dir, "nuova")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Consuma l'evento generato dalla creazione della cartella.
	select {
	case <-w.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("nessun evento per la nuova cartella")
	}

	if err := os.WriteFile(filepath.Join(sub, "foto.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("la nuova sottocartella non è sotto osservazione")
	}
}
