package mosaic

import "testing"

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "1024", want: 1024},
		{in: "512MiB", want: 512 << 20},
		{in: "512mib", want: 512 << 20},
		{in: " 1GiB ", want: 1 << 30},
		{in: "1.5GB", want: 1610612736},
		{in: "256M", want: 256 << 20},
		{in: "2K", want: 2048},
		{in: "abc", wantErr: true},
		{in: "-1MiB", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseBytes(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseBytes(%q): atteso errore, ottenuto %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBytes(%q) = %d, atteso %d", c.in, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		512:       "512 B",
		1 << 20:   "1.0 MiB",
		512 << 20: "512.0 MiB",
		3 << 30:   "3.0 GiB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, atteso %q", in, got, want)
		}
	}
}

// Il budget di allocazioni vive è metà del picco desiderato, con un minimo.
func TestLiveBudgetFor(t *testing.T) {
	cases := map[int64]int64{
		0:         0,
		512 << 20: 256 << 20,
		1 << 30:   512 << 20,
		1024:      1 << 20, // sotto il minimo
	}
	for in, want := range cases {
		if got := LiveBudgetFor(in); got != want {
			t.Errorf("LiveBudgetFor(%d) = %d, atteso %d", in, got, want)
		}
	}
}
