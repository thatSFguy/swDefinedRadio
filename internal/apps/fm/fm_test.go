package fm

import (
	"io/fs"
	"strings"
	"testing"
)

// TestParseHz covers the shorthand the flag accepts. The bare-number
// case is the one that matters: "./sdr fm 98.7" has to mean megahertz.
func TestParseHz(t *testing.T) {
	ok := []struct {
		in   string
		want uint32
	}{
		{"98.7", 98_700_000},
		{"98.7M", 98_700_000},
		{"98.7m", 98_700_000},
		{" 107.9 M ", 107_900_000},
		{"98700k", 98_700_000},
		{"98700000", 98_700_000},
		{"0.0987G", 98_700_000},
	}
	for _, c := range ok {
		got, err := parseHz(c.in)
		if err != nil {
			t.Errorf("parseHz(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseHz(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	for _, in := range []string{"", "   ", "abc", "98.7X", "-98.7", "0", "9G"} {
		if got, err := parseHz(in); err == nil {
			t.Errorf("parseHz(%q) = %d, want an error", in, got)
		}
	}
}

// TestCheckBand guards the tuner against a frequency that is a valid
// number but not a station — a typo like 987 rather than 98.7.
func TestCheckBand(t *testing.T) {
	for _, hz := range []uint32{BandLowHz, 98_700_000, BandHighHz} {
		if err := checkBand(hz); err != nil {
			t.Errorf("checkBand(%d): %v", hz, err)
		}
	}
	for _, hz := range []uint32{0, 87_499_999, 108_000_001, 987_000_000} {
		if err := checkBand(hz); err == nil {
			t.Errorf("checkBand(%d) = nil, want an error", hz)
		}
	}
}

// TestEmbeddedUI checks the player is compiled into the binary; an
// embed directive gets this wrong silently.
func TestEmbeddedUI(t *testing.T) {
	sub, err := fsSub()
	if err != nil {
		t.Fatalf("fsSub: %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	for _, want := range []string{"/audio.wav", "/api/status", "/api/tune", "<title>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}
