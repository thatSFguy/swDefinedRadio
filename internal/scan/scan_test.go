package scan

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestBandFor(t *testing.T) {
	for _, c := range []struct {
		hz   float64
		want string
	}{
		{100.1e6, "FM broadcast"},
		{124.0e6, "Airband — aircraft voice"},
		{146.52e6, "2 m amateur"},
		{162.475e6, "NOAA Weather Radio"},
		{315e6, "ISM 315 MHz — TPMS, key fobs, remotes"},
		{433.92e6, "ISM 433 MHz — sensors, TPMS (EU)"},
		{915e6, "ISM 900 MHz — LoRa, meters, cordless"},
		{1090e6, "ADS-B (1090 MHz)"},
		{1575.42e6, "GPS L1"},
	} {
		if got := BandFor(c.hz); got != c.want {
			t.Errorf("BandFor(%.3f MHz) = %q, want %q", c.hz/1e6, got, c.want)
		}
	}
}

// TestBandForPrefersNarrower checks the tie-break: 1090 MHz sits inside
// the wide aeronautical allocation as well as the narrow ADS-B one, and
// the specific answer is the useful one.
func TestBandForPrefersNarrower(t *testing.T) {
	if got := BandFor(1090e6); got != "ADS-B (1090 MHz)" {
		t.Errorf("BandFor(1090 MHz) = %q, want the narrower ADS-B entry", got)
	}
	if got := BandFor(1000e6); got != "Aeronautical (DME/TACAN)" {
		t.Errorf("BandFor(1000 MHz) = %q, want the wide aeronautical entry", got)
	}
}

func TestNoiseFloorIgnoresCarriers(t *testing.T) {
	// A flat floor with a few strong carriers: the median must track the
	// floor, which a mean would not.
	p := make([]float64, 1000)
	for i := range p {
		p[i] = -70
	}
	for _, i := range []int{10, 200, 700} {
		p[i] = -10
	}
	if got := NoiseFloor(p); math.Abs(got-(-70)) > 0.001 {
		t.Errorf("NoiseFloor = %v, want -70", got)
	}
}

func TestNoiseFloorSkipsGaps(t *testing.T) {
	p := []float64{math.NaN(), -60, -60, -60, math.NaN()}
	if got := NoiseFloor(p); got != -60 {
		t.Errorf("NoiseFloor with gaps = %v, want -60", got)
	}
}

// sweepWith builds a synthetic sweep over the FM band with a flat floor.
func sweepWith(floor float64, n int) *Sweep {
	s := &Sweep{
		At: time.Now(), Start: 88_000_000, Stop: 108_000_000,
		BinHz: float64(108_000_000-88_000_000) / float64(n),
		Power: make(Power, n),
	}
	for i := range s.Power {
		s.Power[i] = floor
	}
	return s
}

func TestFindPeaks(t *testing.T) {
	s := sweepWith(-80, 2000)
	// A three-bin wide carrier, and a weaker two-bin one.
	for _, i := range []int{500, 501, 502} {
		s.Power[i] = -30
	}
	s.Power[501] = -25 // the strongest bin of the first peak
	for _, i := range []int{1500, 1501} {
		s.Power[i] = -50
	}

	peaks := FindPeaks(s, 10, 0, 0)
	if len(peaks) != 2 {
		t.Fatalf("found %d peaks, want 2: %+v", len(peaks), peaks)
	}
	if peaks[0].Power != -25 {
		t.Errorf("strongest peak power = %v, want -25", peaks[0].Power)
	}
	if math.Abs(peaks[0].SNR-55) > 0.001 {
		t.Errorf("SNR = %v, want 55", peaks[0].SNR)
	}
	// Adjacent bins must collapse into one peak, not three.
	if w := peaks[0].WidthHz; math.Abs(w-3*s.BinHz) > 0.001 {
		t.Errorf("width = %v Hz, want %v (3 bins)", w, 3*s.BinHz)
	}
	if peaks[0].Band != "FM broadcast" {
		t.Errorf("band = %q, want FM broadcast", peaks[0].Band)
	}
}

func TestFindPeaksThreshold(t *testing.T) {
	s := sweepWith(-80, 500)
	s.Power[100] = -75 // only 5 dB up
	if got := FindPeaks(s, 10, 0, 0); len(got) != 0 {
		t.Errorf("a 5 dB bump passed a 10 dB threshold: %+v", got)
	}
	if got := FindPeaks(s, 3, 0, 0); len(got) != 1 {
		t.Errorf("a 5 dB bump failed a 3 dB threshold")
	}
}

func TestFindPeaksLimit(t *testing.T) {
	s := sweepWith(-80, 1000)
	for i := 100; i < 200; i += 10 {
		s.Power[i] = -40
	}
	if got := FindPeaks(s, 10, 0, 3); len(got) != 3 {
		t.Errorf("limit ignored: got %d peaks, want 3", len(got))
	}
}

func TestConfigGeometry(t *testing.T) {
	c := Config{Start: 88e6, Stop: 108e6, SampleRate: 2_400_000, BinCount: 1024, Crop: 0.75}.Defaults()
	if got := c.BinHz(); math.Abs(got-2343.75) > 0.01 {
		t.Errorf("BinHz = %v, want 2343.75", got)
	}
	// 20 MHz of span with 1.8 MHz usable per step.
	if got, want := c.Steps(), 12; got != want {
		t.Errorf("Steps = %d, want %d", got, want)
	}
	// 20 MHz of span at 2343.75 Hz per bin is 8533.3, rounded up.
	if got := c.Bins(); got != 8534 {
		t.Errorf("Bins = %d, want 8534", got)
	}
}

func TestFreqAt(t *testing.T) {
	s := &Sweep{Start: 88_000_000, Stop: 108_000_000, BinHz: 1000, Power: make(Power, 20000)}
	if got := s.FreqAt(0); math.Abs(got-88_000_500) > 0.001 {
		t.Errorf("FreqAt(0) = %v, want the centre of the first bin", got)
	}
	// Bin 1000 is 1000.5 bin-widths above Start.
	if got := s.FreqAt(1000); math.Abs(got-89_000_500) > 0.001 {
		t.Errorf("FreqAt(1000) = %v, want 89000500", got)
	}
}

// TestFindPeaksMerges covers the case that made the first live sweep
// misleading: one FM station whose sidebands dip below the threshold was
// reported as several separate signals.
func TestFindPeaksMerges(t *testing.T) {
	s := sweepWith(-80, 4000) // 20 MHz over 4000 bins = 5 kHz each
	// A carrier with two sidebands, each separated by a sub-threshold gap,
	// spanning about 100 kHz in total.
	for _, i := range []int{2000, 2001, 2010, 2011, 2020, 2021} {
		s.Power[i] = -40
	}
	s.Power[2010] = -30 // the carrier itself

	if got := FindPeaks(s, 10, 0, 0); len(got) != 3 {
		t.Fatalf("without merging: %d peaks, want the raw 3", len(got))
	}

	peaks := FindPeaks(s, 10, 150e3, 0)
	if len(peaks) != 1 {
		t.Fatalf("with merging: %d peaks, want 1: %+v", len(peaks), peaks)
	}
	p := peaks[0]
	if p.Power != -30 {
		t.Errorf("power = %v, want the carrier's -30", p.Power)
	}
	// The centroid should sit on the carrier, which dominates in linear
	// power even though the sidebands flank it.
	if want := s.FreqAt(2010); math.Abs(p.Center-want) > 3*s.BinHz {
		t.Errorf("centre = %.0f Hz, want near the carrier at %.0f", p.Center, want)
	}
	if p.WidthHz < 100e3 {
		t.Errorf("width = %.0f Hz, want the full merged span", p.WidthHz)
	}
}

// TestFindPeaksKeepsDistinctSignalsApart makes sure merging does not
// swallow genuinely separate transmissions.
func TestFindPeaksKeepsDistinctSignalsApart(t *testing.T) {
	s := sweepWith(-80, 4000) // 5 kHz bins
	s.Power[1000] = -30       // two stations 1 MHz apart
	s.Power[1200] = -30
	if got := FindPeaks(s, 10, 150e3, 0); len(got) != 2 {
		t.Errorf("merged two stations 1 MHz apart: got %d peaks, want 2", len(got))
	}
}

// TestPowerMarshalsGapsAsNull covers the bug that made the API return an
// empty body: encoding/json refuses NaN outright and fails the whole
// response, so uncovered bins must become null.
func TestPowerMarshalsGapsAsNull(t *testing.T) {
	p := Power{-80.25, math.NaN(), -20.5, math.Inf(-1)}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `[-80.2,null,-20.5,null]`; got != want {
		t.Errorf("marshalled %s, want %s", got, want)
	}
}

// TestSweepMarshalsWithGaps is the end-to-end version: a whole sweep with
// a gap must encode rather than failing.
func TestSweepMarshalsWithGaps(t *testing.T) {
	s := &Sweep{Start: 88e6, Stop: 108e6, BinHz: 1000, Power: Power{-80, math.NaN(), -30}}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("a sweep containing a gap failed to encode: %v", err)
	}
}

func TestDecimateKeepsPeaks(t *testing.T) {
	p := make(Power, 1000)
	for i := range p {
		p[i] = -90
	}
	p[500] = -20 // a single strong bin must survive

	out := Decimate(p, 100)
	if len(out) != 100 {
		t.Fatalf("length = %d, want 100", len(out))
	}
	best := math.Inf(-1)
	for _, v := range out {
		best = math.Max(best, v)
	}
	if best != -20 {
		t.Errorf("peak = %v after decimation, want -20 — a mean would have buried it", best)
	}
}

func TestDecimateShortInputIsUnchanged(t *testing.T) {
	p := Power{-10, -20, -30}
	out := Decimate(p, 100)
	if len(out) != 3 {
		t.Errorf("length = %d, want the original 3", len(out))
	}
}

func TestDecimateKeepsGapsOnlyWhenTotal(t *testing.T) {
	// A group holding one real value and one gap reports the real value.
	out := Decimate(Power{math.NaN(), -50, math.NaN(), math.NaN()}, 2)
	if out[0] != -50 {
		t.Errorf("out[0] = %v, want -50", out[0])
	}
	if !math.IsNaN(out[1]) {
		t.Errorf("out[1] = %v, want a gap", out[1])
	}
}

// TestIsSpur covers the receiver's own signals: the dongle's 28.8 MHz
// crystal puts convincing narrow peaks on exact multiples of 4.8 MHz.
func TestIsSpur(t *testing.T) {
	for _, c := range []struct {
		hz   float64
		want bool
	}{
		{480e6, true},      // 100 x 4.8 MHz, seen on a real sweep
		{460.8e6, true},    // 96 x
		{489.6e6, true},    // 102 x
		{691.2e6, true},    // 144 x
		{488.309e6, false}, // a real ATSC pilot
		{500.309e6, false},
		{1090e6, false},
		{98.7e6, false},
	} {
		if got := IsSpur(c.hz, 5000); got != c.want {
			t.Errorf("IsSpur(%.3f MHz) = %v, want %v", c.hz/1e6, got, c.want)
		}
	}
}

func TestFindPeaksFlagsSpurs(t *testing.T) {
	// A sweep spanning 480 MHz, which is an exact spur multiple.
	s := &Sweep{
		Start: 479_000_000, Stop: 481_000_000,
		BinHz: 2000, Power: make(Power, 1000),
	}
	for i := range s.Power {
		s.Power[i] = -80
	}
	s.Power[500] = -20 // lands at 479e6 + 500.5*2000 = 480.001 MHz

	peaks := FindPeaks(s, 10, 0, 0)
	if len(peaks) != 1 {
		t.Fatalf("got %d peaks, want 1", len(peaks))
	}
	if peaks[0].Suspect == "" {
		t.Errorf("peak at %.4f MHz should be flagged as a spur", peaks[0].Freq/1e6)
	}
}

func TestFindPeaksDoesNotFlagRealSignals(t *testing.T) {
	s := sweepWith(-80, 2000) // the FM band
	s.Power[500] = -20
	peaks := FindPeaks(s, 10, 0, 0)
	if len(peaks) != 1 {
		t.Fatalf("got %d peaks, want 1", len(peaks))
	}
	if peaks[0].Suspect != "" {
		t.Errorf("peak at %.4f MHz wrongly flagged: %q", peaks[0].Freq/1e6, peaks[0].Suspect)
	}
}

// TestUpconvertKeepsRealFrequencies checks that a converter's offset
// affects only tuning: the reported span stays the frequencies asked for.
func TestUpconvertKeepsRealFrequencies(t *testing.T) {
	c := Config{
		Start: 5_000_000, Stop: 10_000_000,
		SampleRate: 2_400_000, BinCount: 1024, Crop: 0.75,
		Upconvert: 125e6,
	}.Defaults()

	if c.Start != 5_000_000 || c.Stop != 10_000_000 {
		t.Errorf("span moved to %d-%d; it should stay the real frequencies", c.Start, c.Stop)
	}
	// Geometry must be unaffected by the shift.
	plain := c
	plain.Upconvert = 0
	if c.Steps() != plain.Steps() || c.Bins() != plain.Bins() {
		t.Errorf("upconversion changed the sweep geometry")
	}
}
