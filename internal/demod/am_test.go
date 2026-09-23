package demod

import (
	"math"
	"math/rand"
	"testing"
)

// modulateAM builds the IQ an airband receiver would see: a carrier at
// the tuned frequency, its amplitude varying with the tone, in the same
// unsigned 8-bit form the dongle produces.
//
// depth is the modulation index; airband transmitters run around 0.8.
// noise is added before quantising, so a test can ask what happens to a
// weak signal as well as a strong one.
func modulateAM(t *testing.T, seconds, tone, depth, carrier, noise float64) []byte {
	t.Helper()
	n := int(AMInputRate * seconds)
	iq := make([]byte, 2*n)
	rng := rand.New(rand.NewSource(1))

	for i := range n {
		tt := float64(i) / AMInputRate
		// Amplitude is the carrier with the tone riding on it.
		amp := carrier * (1 + depth*math.Sin(2*math.Pi*tone*tt))
		// At zero offset from the tuned frequency the carrier sits at
		// DC, so the phase term is constant and only amplitude moves.
		re := amp + noise*rng.NormFloat64()
		im := noise * rng.NormFloat64()
		iq[2*i] = quantise(re)
		iq[2*i+1] = quantise(im)
	}
	return iq
}

func quantise(v float64) byte {
	s := v*127.5 + 127.5
	switch {
	case s < 0:
		return 0
	case s > 255:
		return 255
	}
	return byte(s)
}

// dominantTone reports the strongest frequency in a block of audio, by
// counting zero crossings — enough to tell 1 kHz from 400 Hz, which is
// all that is being asked.
func dominantTone(out []int16, rate float64) float64 {
	crossings := 0
	for i := 1; i < len(out); i++ {
		if (out[i-1] < 0) != (out[i] < 0) {
			crossings++
		}
	}
	return float64(crossings) * rate / (2 * float64(len(out)))
}

// The end-to-end property: a tone modulated onto a carrier has to come
// back out as that tone.
func TestAMRecoversAudioTone(t *testing.T) {
	const tone = 1000.0
	iq := modulateAM(t, 0.3, tone, 0.8, 0.5, 0)

	a := NewAM()
	out := make([]int16, len(iq)/2/amIFDecim/amAudioDecim+16)
	n := a.Process(iq, out)
	if n == 0 {
		t.Fatal("no audio came out")
	}

	// Skip the start, where the carrier tracker is still settling.
	settled := out[n/3 : n]
	if got := dominantTone(settled, AudioRate); math.Abs(got-tone) > 60 {
		t.Errorf("recovered %.0f Hz, want %.0f", got, tone)
	}

	peak := 0
	for _, v := range settled {
		if int(v) > peak {
			peak = int(v)
		}
	}
	if peak < 2000 {
		t.Errorf("audio peaks at %d, which is barely audible", peak)
	}
}

// The carrier level is what a signal meter shows and what the squelch
// compares against, so it has to track the transmitter's strength.
func TestAMLevelFollowsTheCarrier(t *testing.T) {
	strong := NewAM()
	strong.Process(modulateAM(t, 0.2, 1000, 0.8, 0.6, 0), make([]int16, 1<<16))

	weak := NewAM()
	weak.Process(modulateAM(t, 0.2, 1000, 0.8, 0.05, 0), make([]int16, 1<<16))

	if !(strong.Level() > weak.Level()*3) {
		t.Errorf("strong carrier reads %.4f, weak reads %.4f — too close to tell apart",
			strong.Level(), weak.Level())
	}
}

// An airband channel is silent most of the time, and an open squelch on
// an idle channel is a receiver nobody will leave switched on.
func TestAMSquelchClosesOnAnIdleChannel(t *testing.T) {
	// Noise, and no carrier at all.
	iq := modulateAM(t, 0.2, 1000, 0, 0, 0.05)

	a := NewAM()
	a.Squelch = 0.1
	out := make([]int16, 1<<16)
	n := a.Process(iq, out)

	if a.Open() {
		t.Errorf("squelch opened on noise at level %.4f", a.Level())
	}
	for i := range n {
		if out[i] != 0 {
			t.Fatalf("sample %d is %d; a closed squelch must be silent", i, out[i])
		}
	}
}

// And it has to open again when somebody transmits, or it is just a
// mute switch.
func TestAMSquelchOpensOnACarrier(t *testing.T) {
	a := NewAM()
	a.Squelch = 0.1

	a.Process(modulateAM(t, 0.2, 1000, 0, 0, 0.05), make([]int16, 1<<16))
	if a.Open() {
		t.Fatal("squelch was open before anyone transmitted")
	}

	out := make([]int16, 1<<16)
	n := a.Process(modulateAM(t, 0.3, 1000, 0.8, 0.5, 0.01), out)
	if !a.Open() {
		t.Fatalf("squelch stayed shut on a carrier at level %.4f", a.Level())
	}
	loud := false
	for i := range n {
		if out[i] > 1000 || out[i] < -1000 {
			loud = true
			break
		}
	}
	if !loud {
		t.Error("squelch opened but no audio came through")
	}
}

// Silence still has to be produced while the squelch is shut. A stream
// that simply stopped between transmissions looks like a stalled
// connection to whatever is playing it.
func TestAMKeepsProducingSamplesWhileMuted(t *testing.T) {
	a := NewAM()
	a.Squelch = 0.5

	iq := modulateAM(t, 0.2, 1000, 0, 0, 0.02)
	out := make([]int16, 1<<16)
	n := a.Process(iq, out)

	want := len(iq) / 2 / amIFDecim / amAudioDecim
	if n < want-4 || n > want+4 {
		t.Errorf("produced %d samples, want about %d", n, want)
	}
}
