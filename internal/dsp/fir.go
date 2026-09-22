package dsp

import "math"

// LowPass designs a windowed-sinc low-pass filter with the given number
// of taps. cutoff and rate are in hertz. A Hamming window keeps the
// stopband about 53 dB down, which is ample for separating a broadcast
// channel from its neighbours.
func LowPass(taps int, cutoff, rate float64) []float64 {
	if taps < 1 {
		taps = 1
	}
	if taps%2 == 0 {
		taps++ // an odd length keeps the delay a whole number of samples
	}
	h := make([]float64, taps)
	fc := cutoff / rate // normalised, cycles per sample
	mid := float64(taps-1) / 2

	var sum float64
	for i := range h {
		x := float64(i) - mid
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		// Hamming window.
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(taps-1))
		h[i] = s * w
		sum += h[i]
	}
	// Normalise to unity gain at DC.
	for i := range h {
		h[i] /= sum
	}
	return h
}

// ComplexDecimator low-passes an IQ stream and drops all but every Nth
// sample. Filtering first is what stops the discarded bandwidth folding
// back on top of the signal being kept.
type ComplexDecimator struct {
	taps   []float64
	hist   []complex128
	factor int
	phase  int
}

func NewComplexDecimator(taps []float64, factor int) *ComplexDecimator {
	if factor < 1 {
		factor = 1
	}
	return &ComplexDecimator{
		taps:   taps,
		hist:   make([]complex128, len(taps)),
		factor: factor,
	}
}

// Process filters and downsamples in, appending to out. It returns the
// number of samples written.
func (d *ComplexDecimator) Process(in, out []complex128) int {
	n := 0
	for _, s := range in {
		// Shift the delay line and take the newest sample in.
		copy(d.hist, d.hist[1:])
		d.hist[len(d.hist)-1] = s

		d.phase++
		if d.phase < d.factor {
			continue // this output sample is being discarded anyway
		}
		d.phase = 0

		var acc complex128
		for i, c := range d.taps {
			acc += d.hist[len(d.hist)-1-i] * complex(c, 0)
		}
		if n < len(out) {
			out[n] = acc
			n++
		}
	}
	return n
}

// RealDecimator is the same thing for a real-valued stream, used on the
// audio side after demodulation.
type RealDecimator struct {
	taps   []float64
	hist   []float64
	factor int
	phase  int
}

func NewRealDecimator(taps []float64, factor int) *RealDecimator {
	if factor < 1 {
		factor = 1
	}
	return &RealDecimator{
		taps:   taps,
		hist:   make([]float64, len(taps)),
		factor: factor,
	}
}

func (d *RealDecimator) Process(in, out []float64) int {
	n := 0
	for _, s := range in {
		copy(d.hist, d.hist[1:])
		d.hist[len(d.hist)-1] = s

		d.phase++
		if d.phase < d.factor {
			continue
		}
		d.phase = 0

		var acc float64
		for i, c := range d.taps {
			acc += d.hist[len(d.hist)-1-i] * c
		}
		if n < len(out) {
			out[n] = acc
			n++
		}
	}
	return n
}
