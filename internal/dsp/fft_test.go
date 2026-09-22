package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// naiveDFT is the textbook O(n^2) transform, used as the reference the
// fast version must agree with.
func naiveDFT(x []complex128) []complex128 {
	n := len(x)
	out := make([]complex128, n)
	for k := range n {
		var sum complex128
		for t := range n {
			angle := -2 * math.Pi * float64(k) * float64(t) / float64(n)
			sum += x[t] * cmplx.Exp(complex(0, angle))
		}
		out[k] = sum
	}
	return out
}

func TestFFTMatchesNaiveDFT(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16, 64, 256} {
		x := make([]complex128, n)
		// A deterministic but non-trivial signal.
		seed := uint32(7)
		for i := range x {
			seed = seed*1664525 + 1013904223
			x[i] = complex(float64(int32(seed))/(1<<31), float64(int32(seed>>7))/(1<<31))
		}
		want := naiveDFT(x)

		got := make([]complex128, n)
		copy(got, x)
		FFT(got)

		for k := range n {
			if d := cmplx.Abs(got[k] - want[k]); d > 1e-9 {
				t.Fatalf("n=%d bin %d: got %v, want %v (diff %g)", n, k, got[k], want[k], d)
			}
		}
	}
}

// TestFFTFindsATone is the property that matters for the scanner: a pure
// complex tone must land in exactly one bin.
func TestFFTFindsATone(t *testing.T) {
	const n = 1024
	const bin = 137 // the bin the tone is placed in

	x := make([]complex128, n)
	for i := range x {
		angle := 2 * math.Pi * float64(bin) * float64(i) / float64(n)
		x[i] = cmplx.Exp(complex(0, angle))
	}
	FFT(x)

	peak, peakMag := 0, 0.0
	for k := range n {
		if m := cmplx.Abs(x[k]); m > peakMag {
			peak, peakMag = k, m
		}
	}
	if peak != bin {
		t.Errorf("peak in bin %d, want %d", peak, bin)
	}
	if math.Abs(peakMag-float64(n)) > 1e-6 {
		t.Errorf("peak magnitude %v, want %v", peakMag, float64(n))
	}
	// Everything else should be essentially zero.
	for k := range n {
		if k == bin {
			continue
		}
		if m := cmplx.Abs(x[k]); m > 1e-6 {
			t.Fatalf("leakage into bin %d: %v", k, m)
		}
	}
}

func TestFFTRejectsNonPowerOfTwo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("FFT accepted a length that is not a power of two")
		}
	}()
	FFT(make([]complex128, 6))
}

func TestShift(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7}
	Shift(x)
	want := []int{4, 5, 6, 7, 0, 1, 2, 3}
	for i := range x {
		if x[i] != want[i] {
			t.Fatalf("Shift = %v, want %v", x, want)
		}
	}
}

// TestShiftPutsDCInTheMiddle ties Shift to its purpose: after shifting, a
// DC signal's energy sits at index n/2.
func TestShiftPutsDCInTheMiddle(t *testing.T) {
	const n = 64
	x := make([]complex128, n)
	for i := range x {
		x[i] = complex(1, 0) // constant => all energy at DC
	}
	FFT(x)
	Shift(x)
	if peak := cmplx.Abs(x[n/2]); math.Abs(peak-float64(n)) > 1e-9 {
		t.Errorf("DC landed with magnitude %v at the centre, want %v", peak, float64(n))
	}
}

func TestHann(t *testing.T) {
	w := Hann(8)
	if w[0] != 0 || math.Abs(w[len(w)-1]) > 1e-12 {
		t.Errorf("Hann should start and end at zero, got %v and %v", w[0], w[len(w)-1])
	}
	if peak := w[len(w)/2]; peak < 0.9 {
		t.Errorf("Hann should peak near 1 in the middle, got %v", peak)
	}

	// Coherent gain tends to 0.5 as the window grows; a short symmetric
	// window sits a little under that (exactly 0.4375 at n=8) because
	// both endpoints are zero.
	if got := WindowGain(Hann(4096)); math.Abs(got-0.5) > 0.001 {
		t.Errorf("Hann coherent gain = %v, want about 0.5", got)
	}
}

func TestToComplexCentresSamples(t *testing.T) {
	// 127/128 straddle the RTL2832U's 127.5 centre, so a flat run of them
	// must come out near zero rather than near full scale.
	iq := []byte{127, 128, 128, 127}
	w := []float64{1, 1}
	dst := make([]complex128, 2)
	if n := ToComplex(iq, w, dst); n != 2 {
		t.Fatalf("converted %d samples, want 2", n)
	}
	for _, c := range dst {
		if cmplx.Abs(c) > 0.01 {
			t.Errorf("sample %v should be near zero", c)
		}
	}
}
