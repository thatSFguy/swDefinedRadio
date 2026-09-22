// Package scan sweeps the tuner across a frequency range and measures
// how much energy is present at each point.
package scan

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// Config describes a sweep.
type Config struct {
	Start, Stop uint32 // Hz, inclusive of Start
	SampleRate  uint32
	BinCount    int // FFT size; bin width is SampleRate/BinCount

	// Dwell is how long to integrate at each tuning step. Longer dwells
	// average away more noise and reveal weaker signals, at the cost of
	// a slower sweep.
	Dwell time.Duration

	// Settle is how much of the stream to throw away after retuning.
	// Samples already in flight were captured at the old frequency, and
	// including them smears signals across the band.
	Settle time.Duration

	// Crop is the fraction of each segment's bandwidth to keep. The
	// tuner's response rolls off at the edges of its passband, so the
	// outer part of every segment reads low; discarding it and taking
	// more steps gives a flatter result.
	Crop float64

	// LOOffset shifts the local oscillator away from the centre of each
	// segment. The RTL2832U puts a large DC offset spike wherever the LO
	// sits, which is an artefact of the receiver rather than a signal.
	// With LOOffset zero that spike lands mid-segment and has to be
	// blanked, leaving a narrow blind spot every step. Moving the LO out
	// of the kept window removes the blind spot, but the window must
	// then be narrower than half the sample rate, so Crop has to come
	// below 0.5 and the sweep takes more steps.
	//
	// A DC-free sweep looks like: Crop 0.4, LOOffset SampleRate/4.
	LOOffset float64

	// Upconvert is the shift an external upconverter applies, typically
	// 125 MHz. Start and Stop stay the frequencies you actually want —
	// shortwave, say — and this is added when tuning, so the results
	// read in real frequencies rather than converted ones.
	Upconvert float64
}

// Defaults fills in anything left at zero.
func (c Config) Defaults() Config {
	if c.SampleRate == 0 {
		c.SampleRate = 2_400_000
	}
	if c.BinCount == 0 {
		c.BinCount = 1024
	}
	if c.Dwell == 0 {
		c.Dwell = 40 * time.Millisecond
	}
	if c.Settle == 0 {
		// Measured against ghost carriers on a real sweep: below about
		// 150 ms, stale samples from the previous step start folding in.
		c.Settle = 180 * time.Millisecond
	}
	if c.Crop <= 0 || c.Crop > 1 {
		c.Crop = 0.75
	}
	return c
}

// BinHz is the width of one output bin.
func (c Config) BinHz() float64 { return float64(c.SampleRate) / float64(c.BinCount) }

// usableHz is how much of each segment survives cropping.
func (c Config) usableHz() float64 { return float64(c.SampleRate) * c.Crop }

// Steps is how many tuning positions the sweep visits.
func (c Config) Steps() int {
	span := float64(c.Stop - c.Start)
	return int(math.Ceil(span / c.usableHz()))
}

// Bins is the length of a sweep's Power slice.
func (c Config) Bins() int {
	return int(math.Ceil(float64(c.Stop-c.Start) / c.BinHz()))
}

// drainer is implemented by sources that can discard whatever they have
// buffered, which lets a retune take effect immediately.
type drainer interface {
	Drain(quiet time.Duration, maxBytes int) (int, error)
}

// Sweep is one pass across the range.
type Sweep struct {
	At    time.Time `json:"at"`
	Start uint32    `json:"start"`
	Stop  uint32    `json:"stop"`
	BinHz float64   `json:"bin_hz"`
	Power Power     `json:"power"` // dBFS per bin, low frequency first
	Took  float64   `json:"took_s"`
}

// FreqAt returns the centre frequency of bin i.
func (s *Sweep) FreqAt(i int) float64 {
	return float64(s.Start) + (float64(i)+0.5)*s.BinHz
}

// Sweeper drives a tunable source across the range described by Config.
type Sweeper struct {
	cfg     Config
	src     sdr.Source
	window  []float64
	gain    float64
	dcGuard int // bins blanked either side of the LO

	buf  []byte
	cplx []complex128

	// acc and hits accumulate power per output bin across a sweep.
	acc  []float64
	hits []int
}

// New returns a Sweeper reading from src, which must be able to retune —
// in practice an rtl_tcp connection.
func New(src sdr.Source, cfg Config) *Sweeper {
	cfg = cfg.Defaults()
	w := dsp.Hann(cfg.BinCount)
	return &Sweeper{
		cfg:    cfg,
		src:    src,
		window: w,
		gain:   dsp.WindowGain(w),
		// The DC spike is not one bin wide; its skirt spreads over
		// several, and blanking too few leaves fake narrow peaks sitting
		// exactly at each segment centre.
		dcGuard: max(8, cfg.BinCount/64),
		buf:     make([]byte, cfg.BinCount*2),
		cplx:    make([]complex128, cfg.BinCount),
		acc:     make([]float64, cfg.Bins()),
		hits:    make([]int, cfg.Bins()),
	}
}

// Config returns the resolved configuration, with defaults applied.
func (s *Sweeper) Config() Config { return s.cfg }

// Sweep performs one pass and returns the assembled spectrum.
func (s *Sweeper) Sweep(ctx context.Context) (*Sweep, error) {
	started := time.Now()
	for i := range s.acc {
		s.acc[i], s.hits[i] = 0, 0
	}

	usable := s.cfg.usableHz()
	for step := range s.cfg.Steps() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		center := float64(s.cfg.Start) + usable*(float64(step)+0.5)
		if err := s.measure(ctx, center); err != nil {
			return nil, fmt.Errorf("step %d at %.3f MHz: %w", step, center/1e6, err)
		}
	}

	out := &Sweep{
		At:    started,
		Start: s.cfg.Start,
		Stop:  s.cfg.Stop,
		BinHz: s.cfg.BinHz(),
		Power: make(Power, len(s.acc)),
		Took:  time.Since(started).Seconds(),
	}
	for i := range s.acc {
		if s.hits[i] == 0 {
			// No segment covered this bin; mark it clearly rather than
			// letting a zero read as a very strong signal.
			out.Power[i] = math.NaN()
			continue
		}
		out.Power[i] = toDB(s.acc[i] / float64(s.hits[i]))
	}
	return out, nil
}

// measure tunes for one segment, integrates for the dwell time and folds
// the result into the accumulator. segCenter is the middle of the span
// being measured; the oscillator may sit elsewhere if LOOffset is set.
func (s *Sweeper) measure(ctx context.Context, segCenter float64) error {
	lo := segCenter + s.cfg.LOOffset + s.cfg.Upconvert
	if err := s.src.Tune(uint32(lo)); err != nil {
		return fmt.Errorf("tune: %w", err)
	}

	// A retune does not affect samples already captured, and those sit in
	// the USB buffers inside librtlsdr rather than in any socket, so the
	// only thing that clears them is elapsed time. Wait for the new
	// frequency to work its way through, then throw away everything that
	// piled up while waiting.
	//
	// Sleeping rather than reading during the wait costs nothing and
	// takes exactly as long; what follows is then genuinely fresh.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.cfg.Settle):
	}
	if d, ok := s.src.(drainer); ok {
		if _, err := d.Drain(3*time.Millisecond, 16<<20); err != nil {
			return fmt.Errorf("drain: %w", err)
		}
	} else if err := s.discard(s.cfg.Settle); err != nil {
		return fmt.Errorf("settle: %w", err)
	}

	frames := int(math.Max(1, math.Round(
		s.cfg.Dwell.Seconds()*float64(s.cfg.SampleRate)/float64(s.cfg.BinCount))))

	power := make([]float64, s.cfg.BinCount)
	for range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.ReadFull(s.src, s.buf); err != nil {
			return err
		}
		dsp.ToComplex(s.buf, s.window, s.cplx)
		dsp.FFT(s.cplx)
		dsp.Shift(s.cplx)
		for i, c := range s.cplx {
			re, im := real(c), imag(c)
			power[i] += re*re + im*im
		}
	}

	// Normalise: average over frames, undo the FFT's length scaling and
	// the amplitude the window removed.
	norm := float64(frames) * float64(s.cfg.BinCount) * float64(s.cfg.BinCount) * s.gain * s.gain
	for i := range power {
		power[i] /= norm
	}

	s.fold(lo-s.cfg.Upconvert, segCenter, power)
	return nil
}

// fold maps one segment's bins onto the output grid. Bins are kept when
// they fall inside the segment's cropped span and clear of the DC spike,
// both of which are expressed in absolute frequency so that an offset
// oscillator is handled the same way as a centred one.
func (s *Sweeper) fold(lo, segCenter float64, power []float64) {
	n := len(power)
	binHz := s.cfg.BinHz()
	dc := n / 2
	half := s.cfg.usableHz() / 2

	for i := range power {
		if i >= dc-s.dcGuard && i <= dc+s.dcGuard {
			continue // the receiver's own DC offset, not a signal
		}
		freq := lo + (float64(i-dc)+0.5)*binHz
		if math.Abs(freq-segCenter) > half {
			continue // outside this segment's share of the sweep
		}
		idx := int((freq - float64(s.cfg.Start)) / binHz)
		if idx < 0 || idx >= len(s.acc) {
			continue
		}
		s.acc[idx] += power[i]
		s.hits[idx]++
	}
}

// discard reads and throws away roughly d worth of samples.
func (s *Sweeper) discard(d time.Duration) error {
	n := int(d.Seconds() * float64(s.cfg.SampleRate) * 2)
	for n > 0 {
		chunk := min(n, len(s.buf))
		if _, err := io.ReadFull(s.src, s.buf[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// toDB converts linear power to decibels relative to full scale, with a
// floor so silence does not become negative infinity.
func toDB(p float64) float64 {
	const floor = -140.0
	if p <= 0 {
		return floor
	}
	return math.Max(floor, 10*math.Log10(p))
}
