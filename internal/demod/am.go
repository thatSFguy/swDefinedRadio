package demod

import (
	"math"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
)

// Aircraft talk to towers in AM, not FM, on 25 kHz channels between 118
// and 137 MHz. AM survived there for a reason worth knowing: when two
// aircraft transmit at once, AM lets you hear both and the resulting
// heterodyne squeal, where FM's capture effect would hand the channel to
// the stronger one and quietly lose the other. A pilot needs to know the
// call was stepped on.
const (
	// AMInputRate is what the radio is asked for. It divides down to the
	// audio rate in two even steps.
	AMInputRate = 1_200_000

	// AMIFRate is the intermediate rate, after the channel has been
	// filtered out of the surrounding band.
	AMIFRate = 240_000

	amIFDecim    = AMInputRate / AMIFRate // 5
	amAudioDecim = AMIFRate / AudioRate   // 5

	// AMBandwidth is half the channel, so the filter keeps one 25 kHz
	// channel and rejects its neighbours. Speech needs nothing above
	// this, and everything above it is somebody else's conversation.
	AMBandwidth = 8_000
)

// agcFloor is the weakest carrier the recovered audio is divided by.
//
// Dividing by the carrier is what makes a distant aircraft as loud as a
// close one — the speech is a fraction of the carrier, so the fraction
// is the signal and the carrier is just how loudly it arrived. Below
// this there is no carrier worth speaking of, and dividing by it would
// turn the noise floor into a roar.
const agcFloor = 0.01

// AM demodulates amplitude modulation by envelope detection.
//
// The envelope of an AM signal is the carrier level plus the speech on
// top of it. Tracking the carrier slowly and subtracting it leaves the
// speech — and leaves the carrier level itself, which is the most useful
// squelch signal there is: an idle airband channel has no carrier at
// all, so there is nothing to argue about.
type AM struct {
	front *dsp.ComplexDecimator // the band down to one channel
	down  *dsp.ComplexDecimator // that channel down to audio rate

	// carrier is the slowly-tracked envelope average. It is the part of
	// the signal that is not speech, which makes it both the thing to
	// subtract and the thing to measure.
	carrier float64
	alpha   float64

	// Squelch is the carrier level below which the channel counts as
	// idle and is muted. Zero opens the squelch entirely.
	Squelch float64

	Gain float64

	open  bool
	level float64
	hold  float64

	cplx, ifbuf, narrow []complex128
	aud                 []float64
}

// NewAM builds an airband receiver.
func NewAM() *AM {
	// Two even steps rather than one long filter: 25:1 in a single pass
	// needs a filter far longer than two of 5:1 back to back.
	front := dsp.LowPass(63, AMBandwidth, AMInputRate)
	down := dsp.LowPass(63, AMBandwidth, AMIFRate)

	return &AM{
		front: dsp.NewComplexDecimator(front, amIFDecim),
		down:  dsp.NewComplexDecimator(down, amAudioDecim),
		// About a 20 Hz corner: fast enough to follow a transmitter
		// keying up within a syllable, slow enough not to eat the speech
		// it is supposed to be sitting underneath.
		alpha: 1 - math.Exp(-2*math.Pi*20/AudioRate),
		// Audio is now a modulation depth of roughly -1 to 1, so this is
		// close to full scale for a fully modulated transmission.
		Gain: 9000,
	}
}

// Level is the carrier strength this instant, which is what the squelch
// was compared against for the block just processed.
func (a *AM) Level() float64 { return a.level }

// Peak is the strongest the carrier has been over the last second or so.
//
// The squelch decides once per block, about every fifty milliseconds,
// and anything watching this is doing so far less often — so what it
// sees is one block in ten or twenty. Static that got through did so on
// a block nobody looked at, which makes a meter showing only the instant
// disagree with what is audibly happening. This is what the squelch has
// actually been reacting to.
func (a *AM) Peak() float64 { return a.hold }

// holdDecay is applied once per block. At about fifty milliseconds a
// block this falls by half in roughly a second: long enough to see a
// burst of static that has just passed, short enough to follow the band
// rather than remember it.
const holdDecay = 0.965

// Open reports whether the squelch is passing audio.
func (a *AM) Open() bool { return a.open }

// Process converts interleaved unsigned 8-bit IQ into 16-bit mono
// samples, returning how many were produced. With the squelch closed it
// still produces samples — silence — because an audio stream that
// stopped between transmissions would be treated as a stall by anything
// listening to it.
func (a *AM) Process(iq []byte, out []int16) int {
	n := len(iq) / 2
	a.cplx = resizeC(a.cplx, n)
	for i := range n {
		// Centre on zero: the RTL2832U offsets both components by 127.5.
		a.cplx[i] = complex(
			(float64(iq[2*i])-127.5)/127.5,
			(float64(iq[2*i+1])-127.5)/127.5,
		)
	}

	a.ifbuf = resizeC(a.ifbuf, n/amIFDecim+1)
	m := a.front.Process(a.cplx, a.ifbuf)

	a.narrow = resizeC(a.narrow, m/amAudioDecim+1)
	k := a.down.Process(a.ifbuf[:m], a.narrow)

	a.aud = resizeF(a.aud, k)
	peak := 0.0
	for i := range k {
		env := math.Hypot(real(a.narrow[i]), imag(a.narrow[i]))
		a.carrier += a.alpha * (env - a.carrier)
		if a.carrier > peak {
			peak = a.carrier
		}
		// What is left once the carrier is taken away is the speech, and
		// dividing by the carrier makes it modulation depth rather than
		// a number that depends on how far away the aircraft is.
		a.aud[i] = (env - a.carrier) / max(a.carrier, agcFloor)
	}

	// Decide once per block rather than per sample, so the squelch
	// cannot chatter in the middle of a word.
	a.level = peak
	if h := a.hold * holdDecay; peak > h {
		a.hold = peak
	} else {
		a.hold = h
	}
	a.open = a.Squelch <= 0 || peak >= a.Squelch

	if k > len(out) {
		k = len(out)
	}
	if !a.open {
		for i := range k {
			out[i] = 0
		}
		return k
	}
	for i := range k {
		v := a.aud[i] * a.Gain
		// Clip rather than wrap: a loud transmission should sound loud,
		// not turn inside out.
		switch {
		case v > 32767:
			out[i] = 32767
		case v < -32768:
			out[i] = -32768
		default:
			out[i] = int16(v)
		}
	}
	return k
}
