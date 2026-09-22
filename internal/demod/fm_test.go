package demod

import (
	"math"
	"math/cmplx"
	"testing"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
)

// TestDiscriminatorRecoversDeviation checks the core of FM demodulation:
// a carrier deviated by a constant amount must come out as a constant.
func TestDiscriminatorRecoversDeviation(t *testing.T) {
	const rate = 240_000.0
	const deviation = 50_000.0 // Hz

	in := make([]complex128, 4096)
	phase := 0.0
	for i := range in {
		phase += 2 * math.Pi * deviation / rate
		in[i] = cmplx.Exp(complex(0, phase))
	}

	out := make([]float64, len(in))
	var d Discriminator
	d.prev = in[0]
	n := d.Process(in, out)

	// Skip the first sample, which has no predecessor to compare with.
	want := 2 * math.Pi * deviation / rate
	for i := 1; i < n; i++ {
		if math.Abs(out[i]-want) > 1e-9 {
			t.Fatalf("sample %d = %v, want %v", i, out[i], want)
		}
	}
}

// TestFMRecoversAudioTone is the end-to-end property: modulate a 1 kHz
// tone onto a carrier, run it through the whole receiver, and the tone
// should come back out at 1 kHz.
func TestFMRecoversAudioTone(t *testing.T) {
	const toneHz = 1000.0
	const deviation = 60_000.0
	const seconds = 0.25

	n := int(InputRate * seconds)
	iq := make([]byte, n*2)
	phase := 0.0
	for i := range n {
		tone := math.Sin(2 * math.Pi * toneHz * float64(i) / InputRate)
		phase += 2 * math.Pi * deviation * tone / InputRate
		// Back to the unsigned, 127.5-centred form the dongle produces.
		iq[2*i] = byte(math.Round(math.Cos(phase)*127 + 127.5))
		iq[2*i+1] = byte(math.Round(math.Sin(phase)*127 + 127.5))
	}

	fm := NewFM(DeemphasisUS)
	out := make([]int16, n)
	k := fm.Process(iq, out)

	if want := n / (ifDecim * audioDecim); k < want-10 || k > want+10 {
		t.Fatalf("produced %d audio samples, want about %d", k, want)
	}

	// Find the dominant frequency of the recovered audio. Skip the start,
	// where the filters are still filling.
	const fftN = 4096
	skip := k - fftN
	if skip < 0 {
		t.Fatalf("not enough audio: %d samples", k)
	}
	buf := make([]complex128, fftN)
	win := dsp.Hann(fftN)
	for i := range fftN {
		buf[i] = complex(float64(out[skip+i])/32768*win[i], 0)
	}
	dsp.FFT(buf)

	peak, peakMag := 0, 0.0
	for i := 1; i < fftN/2; i++ {
		if m := cmplx.Abs(buf[i]); m > peakMag {
			peak, peakMag = i, m
		}
	}
	got := float64(peak) * AudioRate / fftN
	if math.Abs(got-toneHz) > 30 {
		t.Errorf("recovered tone at %.0f Hz, want %.0f", got, toneHz)
	}
}

func TestDeemphasisAttenuatesTreble(t *testing.T) {
	// A 75 us time constant puts the corner near 2.1 kHz, so 10 kHz
	// should come out markedly quieter than 300 Hz.
	amp := func(freq float64) float64 {
		d := NewDeemphasis(DeemphasisUS, AudioRate)
		n := AudioRate / 4
		x := make([]float64, n)
		for i := range x {
			x[i] = math.Sin(2 * math.Pi * freq * float64(i) / AudioRate)
		}
		d.Process(x)
		peak := 0.0
		for _, v := range x[n/2:] { // settled portion
			peak = math.Max(peak, math.Abs(v))
		}
		return peak
	}
	low, high := amp(300), amp(10_000)
	if high >= low {
		t.Fatalf("10 kHz (%v) should be quieter than 300 Hz (%v)", high, low)
	}
	if ratio := low / high; ratio < 3 {
		t.Errorf("treble only reduced %.1fx, expected more from 75us de-emphasis", ratio)
	}
}

func TestLowPassRejectsAboveCutoff(t *testing.T) {
	taps := dsp.LowPass(63, 15_000, IFRate)
	// Response at a frequency, evaluated directly from the taps.
	resp := func(f float64) float64 {
		var re, im float64
		for i, c := range taps {
			th := -2 * math.Pi * f * float64(i) / IFRate
			re += c * math.Cos(th)
			im += c * math.Sin(th)
		}
		return math.Hypot(re, im)
	}
	if p := resp(1000); math.Abs(p-1) > 0.05 {
		t.Errorf("passband gain at 1 kHz = %v, want about 1", p)
	}
	if s := resp(40_000); s > 0.01 {
		t.Errorf("stopband at 40 kHz = %v, want well under 0.01", s)
	}
}
