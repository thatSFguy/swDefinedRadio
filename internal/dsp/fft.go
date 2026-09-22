package dsp

import (
	"math"
	"math/bits"
)

// FFT computes an in-place radix-2 decimation-in-time transform of x,
// whose length must be a power of two. The result is in the usual order:
// bin 0 is DC, bins 1..n/2-1 are positive frequencies and the rest are
// negative. Use Shift to rearrange them into ascending frequency order.
func FFT(x []complex128) {
	n := len(x)
	if n <= 1 {
		return
	}
	if n&(n-1) != 0 {
		panic("dsp: FFT length must be a power of two")
	}

	// Reorder into bit-reversed index order, which is what lets the
	// butterflies below run in place.
	shift := uint(bits.TrailingZeros(uint(n)))
	for i := range n {
		j := int(bits.Reverse(uint(i)) >> (bits.UintSize - shift))
		if j > i {
			x[i], x[j] = x[j], x[i]
		}
	}

	for size := 2; size <= n; size <<= 1 {
		// The principal root of unity for this stage.
		theta := -2 * math.Pi / float64(size)
		step := complex(math.Cos(theta), math.Sin(theta))
		half := size / 2
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := range half {
				a, b := x[start+k], x[start+k+half]*w
				x[start+k] = a + b
				x[start+k+half] = a - b
				w *= step
			}
		}
	}
}

// Shift rearranges FFT output so the bins run from the most negative
// frequency to the most positive, putting DC in the middle. n must be
// even.
func Shift[T any](x []T) {
	n := len(x)
	if n%2 != 0 {
		panic("dsp: Shift needs an even length")
	}
	half := n / 2
	for i := range half {
		x[i], x[i+half] = x[i+half], x[i]
	}
}

// Hann returns an n-point Hann window. Windowing trades a little
// resolution for far lower spectral leakage, without which one strong
// carrier smears across the whole span.
func Hann(n int) []float64 {
	w := make([]float64, n)
	if n == 1 {
		w[0] = 1
		return w
	}
	for i := range n {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

// WindowGain is the coherent gain of a window, used to undo the
// amplitude a window removes so power readings stay comparable.
func WindowGain(w []float64) float64 {
	var sum float64
	for _, v := range w {
		sum += v
	}
	return sum / float64(len(w))
}

// ToComplex converts interleaved unsigned 8-bit IQ into complex samples
// centred on zero, applying the window as it goes. It returns the number
// of samples written.
func ToComplex(iq []byte, w []float64, dst []complex128) int {
	n := min(len(iq)/2, len(dst), len(w))
	for i := range n {
		// The RTL2832U centres both components on 127.5.
		re := (float64(iq[2*i]) - 127.5) / 127.5
		im := (float64(iq[2*i+1]) - 127.5) / 127.5
		dst[i] = complex(re*w[i], im*w[i])
	}
	return n
}
