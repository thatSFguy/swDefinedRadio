package airband

import (
	"testing"

	"github.com/thatSFguy/swDefinedRadio/internal/scan"
)

// realSweep is what a sweep of 118–137 MHz actually returned, spurs and
// all. Using the real thing rather than invented numbers is the point:
// every way this can go wrong is in here somewhere.
var realSweep = []scan.Peak{
	{Freq: 129_600_000, Power: -18, SNR: 21, WidthHz: 2344,
		Band: "Airband — aircraft voice", Suspect: "likely receiver spur (multiple of 4.8 MHz)"},
	{Freq: 120_000_000, Power: -20, SNR: 20, WidthHz: 2344,
		Band: "Airband — aircraft voice", Suspect: "likely receiver spur (multiple of 4.8 MHz)"},
	{Freq: 120_832_000, Power: -25, SNR: 15, WidthHz: 4700, Band: "Airband — aircraft voice"},
	{Freq: 118_784_000, Power: -26, SNR: 13, WidthHz: 2300, Band: "Airband — aircraft voice"},
	{Freq: 133_200_000, Power: -26, SNR: 13, WidthHz: 84_400, Band: "Airband — aircraft voice"},
	{Freq: 122_881_000, Power: -27, SNR: 12, WidthHz: 2300, Band: "Airband — aircraft voice"},
	{Freq: 124_929_000, Power: -29, SNR: 11, WidthHz: 4700, Band: "Airband — aircraft voice"},
	{Freq: 130_711_000, Power: -29, SNR: 11, WidthHz: 2300, Band: "Airband — aircraft voice"},
}

// A sweep reports the strongest bin, and bins are kilohertz wide, so
// what comes back is near a channel rather than on it. Tuning to the
// measured frequency leaves the receiver a few kHz off, which on AM is
// quiet and thin rather than obviously broken.
func TestSnapsToTheChannelGrid(t *testing.T) {
	tests := []struct {
		found float64
		want  uint32
	}{
		{120_832_000, 120_825_000},
		{118_784_000, 118_775_000},
		{122_881_000, 122_875_000},
		{124_929_000, 124_925_000},
		{130_711_000, 130_700_000},
		{121_500_000, 121_500_000}, // already on the grid
	}
	for _, tt := range tests {
		if got := Snap(tt.found); got != tt.want {
			t.Errorf("Snap(%.3f) = %s, want %s", tt.found/1e6, MHz(got), MHz(tt.want))
		}
	}
}

// The two strongest things in that sweep were the receiver talking to
// itself. Taking peaks by strength alone would add those first.
func TestSpursAreNotChannels(t *testing.T) {
	for _, p := range realSweep {
		if p.Suspect == "" {
			continue
		}
		if _, ok := plausible(p); ok {
			t.Errorf("%s was accepted despite being flagged: %s", MHz(uint32(p.Freq)), p.Suspect)
		}
	}
}

// An AM voice channel is about 8 kHz inside a 25 kHz slot. Something
// 84 kHz wide is a noise source, and tuning to it produces a hiss the
// squelch will happily open on.
func TestTooWideIsNotAChannel(t *testing.T) {
	wide := scan.Peak{Freq: 133_200_000, SNR: 13, WidthHz: 84_400}
	if _, ok := plausible(wide); ok {
		t.Error("an 84 kHz peak was accepted as a voice channel")
	}
	narrow := scan.Peak{Freq: 133_200_000, SNR: 13, WidthHz: 4700}
	if _, ok := plausible(narrow); !ok {
		t.Error("a normal-width peak was rejected")
	}
}

// The whole filter, against the real sweep: the spurs go, the wide one
// goes, the ambiguous one goes, and what is left is on the grid.
func TestRealSweepYieldsOnlyPlausibleChannels(t *testing.T) {
	var got []uint32
	for _, p := range realSweep {
		if hz, ok := plausible(p); ok {
			got = append(got, hz)
		}
	}

	// Four, not five. 130.711 is 11 kHz from the nearest 25 kHz channel
	// and nearly midway between two of them — a bin is 2.3 kHz, so a
	// centre estimate should land far closer than that. It is either
	// mismeasured or one of the 8.33 kHz channels, and neither can be
	// placed confidently enough to tune to. Missing a channel is a
	// smaller harm than adding one the receiver sits on hearing a
	// distorted signal.
	want := []uint32{
		120_825_000, // 120.832
		118_775_000, // 118.784
		122_875_000, // 122.881
		124_925_000, // 124.929
	}
	if len(got) != len(want) {
		t.Fatalf("kept %d of %d peaks, want %d: %v", len(got), len(realSweep), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("peak %d snapped to %s, want %s", i, MHz(got[i]), MHz(want[i]))
		}
	}
}

// Weak peaks are noise dressed up. A channel added from one is a channel
// the receiver will sit on hearing nothing.
func TestWeakPeaksAreIgnored(t *testing.T) {
	if _, ok := plausible(scan.Peak{Freq: 121_500_000, SNR: 3, WidthHz: 2300}); ok {
		t.Error("a 3 dB peak was accepted")
	}
}

// Anything outside the band is not an airband channel whatever it looks
// like, and the receiver could not tune to it anyway.
func TestOutsideTheBandIsRejected(t *testing.T) {
	for _, hz := range []float64{98_700_000, 144_000_000} {
		if _, ok := plausible(scan.Peak{Freq: hz, SNR: 20, WidthHz: 2300}); ok {
			t.Errorf("%s was accepted", MHz(uint32(hz)))
		}
	}
}

// Two peaks a couple of kHz apart are one channel seen twice, not two
// channels; after snapping they collapse, and the list must not gain the
// same frequency twice.
func TestNearbyPeaksBecomeOneChannel(t *testing.T) {
	seen := map[uint32]bool{}
	n := 0
	for _, p := range []scan.Peak{
		{Freq: 120_832_000, SNR: 15, WidthHz: 4700},
		{Freq: 120_819_000, SNR: 12, WidthHz: 2300},
	} {
		if hz, ok := plausible(p); ok && !seen[hz] {
			seen[hz] = true
			n++
		}
	}
	if n != 1 {
		t.Errorf("two sightings of one channel produced %d entries", n)
	}
}
