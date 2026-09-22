package scan

import (
	"math"
	"sort"
)

// Band names a slice of spectrum, so a peak can be reported as something
// meaningful rather than just a number.
type Band struct {
	Low, High float64 // Hz
	Name      string
}

// bands covers the range an R820T can reach. Allocations are the North
// American ones; the shared ISM segments are noted where they differ.
var bands = []Band{
	{150e3, 30e6, "HF (needs a converter or direct sampling)"},
	{30e6, 50e6, "VHF low band / 6 m amateur"},
	{54e6, 88e6, "VHF TV 2-6"},
	{88e6, 108e6, "FM broadcast"},
	{108e6, 118e6, "Aeronautical navigation (VOR/ILS)"},
	{118e6, 137e6, "Airband — aircraft voice"},
	{137e6, 138e6, "Weather satellites (NOAA APT)"},
	{144e6, 148e6, "2 m amateur"},
	{148e6, 156e6, "VHF land mobile"},
	{156e6, 162e6, "Marine VHF"},
	{162.4e6, 162.55e6, "NOAA Weather Radio"},
	{162.55e6, 174e6, "VHF land mobile / federal"},
	{174e6, 216e6, "VHF TV 7-13"},
	{216e6, 225e6, "1.25 m amateur / maritime"},
	{225e6, 400e6, "Military aeronautical"},
	{314e6, 316e6, "ISM 315 MHz — TPMS, key fobs, remotes"},
	{400e6, 406e6, "Weather balloons / satellite"},
	{406e6, 420e6, "Federal / distress beacons"},
	{420e6, 450e6, "70 cm amateur"},
	{433.05e6, 434.79e6, "ISM 433 MHz — sensors, TPMS (EU)"},
	{450e6, 462e6, "UHF land mobile"},
	{462e6, 468e6, "FRS / GMRS"},
	{468e6, 470e6, "UHF land mobile"},
	{470e6, 608e6, "UHF TV"},
	{608e6, 614e6, "Radio astronomy"},
	{614e6, 698e6, "UHF TV / wireless mics"},
	{698e6, 806e6, "700 MHz cellular and public safety"},
	{806e6, 824e6, "SMR / public safety"},
	{824e6, 849e6, "Cellular uplink 850"},
	{849e6, 869e6, "SMR / public safety"},
	{869e6, 894e6, "Cellular downlink 850"},
	{902e6, 928e6, "ISM 900 MHz — LoRa, meters, cordless"},
	{928e6, 960e6, "Paging / SCADA"},
	{960e6, 1164e6, "Aeronautical (DME/TACAN)"},
	{977e6, 979e6, "UAT (978 MHz) — ADS-B and FIS-B, US"},
	{1025e6, 1035e6, "Mode S interrogation (1030 MHz)"},
	{1085e6, 1095e6, "ADS-B (1090 MHz)"},
	{1164e6, 1300e6, "GNSS / 23 cm amateur"},
	{1300e6, 1350e6, "Radar"},
	{1525e6, 1560e6, "Inmarsat downlink"},
	{1570e6, 1580e6, "GPS L1"},
	{1610e6, 1630e6, "Iridium"},
	{1670e6, 1700e6, "GOES / weather satellite downlink"},
}

// BandFor names the band a frequency falls in. Narrow allocations are
// preferred over the wide ones they sit inside, so 1090 MHz reports as
// ADS-B rather than as aeronautical.
func BandFor(hz float64) string {
	best := ""
	bestWidth := 0.0
	for _, b := range bands {
		if hz < b.Low || hz >= b.High {
			continue
		}
		if w := b.High - b.Low; best == "" || w < bestWidth {
			best, bestWidth = b.Name, w
		}
	}
	return best
}

// SpurSpacingHz is the interval at which the RTL2832U generates signals
// of its own. The dongle's reference crystal runs at 28.8 MHz, and
// mixing products land on exact multiples of a sixth of it. They look
// entirely convincing — narrow, strong and perfectly stable — but there
// is no transmitter, and unlike an oscillator artefact they do not move
// when the sweep grid is shifted, so the usual test does not catch them.
const SpurSpacingHz = 4_800_000

// IsSpur reports whether a frequency sits within tolHz of a multiple of
// the receiver's own spur spacing.
func IsSpur(hz, tolHz float64) bool {
	if hz <= 0 {
		return false
	}
	off := math.Mod(hz, SpurSpacingHz)
	return off <= tolHz || SpurSpacingHz-off <= tolHz
}

// Peak is a signal found in a sweep.
type Peak struct {
	Freq    float64 `json:"freq"`   // Hz, at the strongest bin
	Center  float64 `json:"center"` // Hz, power-weighted centre of the signal
	Power   float64 `json:"power"`  // dBFS
	SNR     float64 `json:"snr"`    // dB above the noise floor
	WidthHz float64 `json:"width_hz"`
	Band    string  `json:"band,omitempty"`

	// Suspect is set when the peak looks like the receiver's own doing
	// rather than a transmission.
	Suspect string `json:"suspect,omitempty"`
}

// NoiseFloor estimates the background level as the median of the sweep,
// which is robust to a handful of strong carriers in a way a mean is not.
func NoiseFloor(power Power) float64 {
	vals := make([]float64, 0, len(power))
	for _, p := range power {
		if !isNaN(p) {
			vals = append(vals, p)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	return vals[len(vals)/2]
}

// run is a stretch of consecutive bins above the detection threshold.
type run struct{ lo, hi int } // hi is exclusive

// FindPeaks returns the signals standing at least threshold dB above the
// noise floor, strongest first.
//
// minSepHz merges runs closer together than that, which matters because a
// real transmission is not a single spike: a broadcast FM carrier spans
// about 200 kHz and its stereo subcarriers dip below the threshold
// between them. Without merging, one station is reported as half a dozen
// signals. Pass 0 to report every run separately.
func FindPeaks(s *Sweep, threshold, minSepHz float64, limit int) []Peak {
	floor := NoiseFloor(s.Power)
	cut := floor + threshold

	// Collect the runs above the threshold.
	var runs []run
	for i := 0; i < len(s.Power); {
		if isNaN(s.Power[i]) || s.Power[i] < cut {
			i++
			continue
		}
		start := i
		for i < len(s.Power) && !isNaN(s.Power[i]) && s.Power[i] >= cut {
			i++
		}
		runs = append(runs, run{start, i})
	}

	// Merge runs separated by less than minSepHz.
	if minSepHz > 0 && len(runs) > 1 {
		gap := int(minSepHz / s.BinHz)
		merged := runs[:1]
		for _, r := range runs[1:] {
			last := &merged[len(merged)-1]
			if r.lo-last.hi <= gap {
				last.hi = r.hi
			} else {
				merged = append(merged, r)
			}
		}
		runs = merged
	}

	peaks := make([]Peak, 0, len(runs))
	for _, r := range runs {
		bestIdx, bestPow := r.lo, math.Inf(-1)
		// The centroid is weighted by linear power, not decibels, so a
		// symmetric signal reports its carrier rather than being pulled
		// about by the logarithm.
		var wsum, fsum float64
		for i := r.lo; i < r.hi; i++ {
			p := s.Power[i]
			if isNaN(p) {
				continue
			}
			if p > bestPow {
				bestIdx, bestPow = i, p
			}
			lin := math.Pow(10, p/10)
			wsum += lin
			fsum += lin * s.FreqAt(i)
		}
		if math.IsInf(bestPow, -1) {
			continue
		}
		center := s.FreqAt(bestIdx)
		if wsum > 0 {
			center = fsum / wsum
		}
		pk := Peak{
			Freq:    s.FreqAt(bestIdx),
			Center:  center,
			Power:   bestPow,
			SNR:     bestPow - floor,
			WidthHz: float64(r.hi-r.lo) * s.BinHz,
			Band:    BandFor(center),
		}
		// Flag rather than drop: a real transmitter can sit on one of
		// these frequencies, and silently hiding it would be worse than
		// asking the operator to check.
		if tol := math.Max(3*s.BinHz, 2000); IsSpur(pk.Freq, tol) {
			pk.Suspect = "likely receiver spur (multiple of 4.8 MHz)"
		}
		peaks = append(peaks, pk)
	}

	sort.Slice(peaks, func(a, b int) bool { return peaks[a].Power > peaks[b].Power })
	if limit > 0 && len(peaks) > limit {
		peaks = peaks[:limit]
	}
	return peaks
}

func isNaN(f float64) bool { return f != f }
