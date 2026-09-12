package mosaic

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseBytes interpreta una dimensione con suffisso: "512MiB", "1.5GB", "800M"
// oppure un numero puro di byte. Restituisce 0 per la stringa vuota.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	unit := int64(1)
	for _, suffix := range []struct {
		name string
		mult int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, suffix.name) {
			unit = suffix.mult
			upper = strings.TrimSpace(strings.TrimSuffix(upper, suffix.name))
			break
		}
	}
	value, err := strconv.ParseFloat(upper, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid size %q (e.g. 512MiB, 1.5GB)", s)
	}
	return int64(value * float64(unit)), nil
}

// FormatBytes restituisce una dimensione leggibile.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// LiveBudgetFor converte un tetto di memoria residente in budget di
// allocazioni vive: con GOGC al valore di default il garbage collector lascia
// crescere l'heap fino a circa il doppio dei dati vivi, quindi si dimezza.
func LiveBudgetFor(peak int64) int64 {
	if peak <= 0 {
		return 0
	}
	live := peak / 2
	if live < 1<<20 {
		live = 1 << 20
	}
	return live
}
