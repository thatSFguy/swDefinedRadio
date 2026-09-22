// Package demod turns IQ samples into audio.
package demod

import (
	"math"
	"math/cmplx"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
)

// Rates for broadcast FM. The tuner runs at InputRate, the signal is
// decimated to IFRate for demodulation — comfortably wider than the
// 200 kHz a station occupies — and the recovered audio comes out at
// AudioRate. Both steps are a factor of five, which keeps the filters
// short.
const (
	InputRate = 1_200_000
	IFRate    = 240_000
	AudioRate = 48_000

	ifDecim    = InputRate / IFRate
	audioDecim = IFRate / AudioRate
)

// DeemphasisUS is the time constant broadcasters in the Americas and
// South Korea pre-emphasise with; most of the rest of the world uses
// DeemphasisEU. Getting it wrong leaves the audio dull or hissy.
const (
	DeemphasisUS = 75e-6
	DeemphasisEU = 50e-6
)

// Discriminator recovers instantaneous frequency from an IQ stream.
type Discriminator struct{ prev complex128 }

// Process writes the phase advance between consecutive samples, which is
// proportional to the frequency deviation and so is the audio.
func (d *Discriminator) Process(in []complex128, out []float64) int {
	n := min(len(in), len(out))
	for i := range n {
		// Multiplying by the conjugate of the previous sample gives a
		// value whose angle is the change in phase.
		p := in[i] * cmplx.Conj(d.prev)
		out[i] = math.Atan2(imag(p), real(p))
		d.prev = in[i]
	}
	return n
}

// Deemphasis is the one-pole filter that undoes a broadcaster's treble
// boost.
type Deemphasis struct {
	alpha float64
	prev  float64
}

func NewDeemphasis(tau, rate float64) *Deemphasis {
	dt := 1 / rate
	return &Deemphasis{alpha: dt / (tau + dt)}
}

func (d *Deemphasis) Process(x []float64) {
	for i, v := range x {
		d.prev += d.alpha * (v - d.prev)
		x[i] = d.prev
	}
}

// FM is a complete broadcast FM receiver: IQ in, 48 kHz mono audio out.
type FM struct {
	front  *dsp.ComplexDecimator
	disc   Discriminator
	audio  *dsp.RealDecimator
	deemph *Deemphasis

	// Gain is applied before conversion to 16-bit samples. The
	// discriminator's output is bounded by the deviation, so a fixed
	// scale works; the limiter below catches the rest.
	Gain float64

	cplx  []complex128
	ifbuf []complex128
	raw   []float64
	aud   []float64

	// level tracks recent peak amplitude, reported as a signal meter.
	level float64
}

// NewFM builds a receiver. tau selects the de-emphasis time constant.
func NewFM(tau float64) *FM {
	// The IF filter has to pass one station's 200 kHz and reject its
	// neighbours 200 kHz away.
	ifTaps := dsp.LowPass(63, 100_000, InputRate)
	// The audio filter keeps 15 kHz, which is the top of the mono
	// programme signal and below the 19 kHz stereo pilot.
	audioTaps := dsp.LowPass(63, 15_000, IFRate)

	return &FM{
		front:  dsp.NewComplexDecimator(ifTaps, ifDecim),
		audio:  dsp.NewRealDecimator(audioTaps, audioDecim),
		deemph: NewDeemphasis(tau, AudioRate),
		Gain:   9000,
	}
}

// Level returns a rough signal strength, the recent peak deviation.
func (f *FM) Level() float64 { return f.level }

// Process converts a block of interleaved unsigned 8-bit IQ into 16-bit
// mono samples, returning how many were produced.
func (f *FM) Process(iq []byte, out []int16) int {
	n := len(iq) / 2
	f.cplx = resizeC(f.cplx, n)
	for i := range n {
		// Centre on zero: the RTL2832U offsets both components by 127.5.
		f.cplx[i] = complex(
			(float64(iq[2*i])-127.5)/127.5,
			(float64(iq[2*i+1])-127.5)/127.5,
		)
	}

	f.ifbuf = resizeC(f.ifbuf, n/ifDecim+1)
	m := f.front.Process(f.cplx, f.ifbuf)

	f.raw = resizeF(f.raw, m)
	m = f.disc.Process(f.ifbuf[:m], f.raw)

	f.aud = resizeF(f.aud, m/audioDecim+1)
	k := f.audio.Process(f.raw[:m], f.aud)
	f.deemph.Process(f.aud[:k])

	var peak float64
	written := 0
	for i := range k {
		v := f.aud[i]
		peak = math.Max(peak, math.Abs(v))
		s := v * f.Gain
		// Clip rather than wrap: a wrapped sample sounds like a gunshot.
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		if written < len(out) {
			out[written] = int16(s)
			written++
		}
	}
	// Decay slowly so the meter is readable rather than twitchy.
	f.level = math.Max(peak, f.level*0.85)
	return written
}

func resizeC(b []complex128, n int) []complex128 {
	if cap(b) < n {
		return make([]complex128, n)
	}
	return b[:n]
}

func resizeF(b []float64, n int) []float64 {
	if cap(b) < n {
		return make([]float64, n)
	}
	return b[:n]
}
