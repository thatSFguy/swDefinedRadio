// Package fm receives broadcast FM and serves it as live audio a browser
// can play, along with a tuning dial and a signal meter.
//
// The demodulation is mono on purpose: the audio filter cuts below the
// 19 kHz stereo pilot, which costs the stereo image and buys a quieter
// signal on anything but a strong station.
//
// Audio leaves through a fan-out rather than a single pipe, so the
// browser and the machine's own speaker can both listen, and neither can
// stall the radio by failing to keep up.
package fm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/thatSFguy/swDefinedRadio/internal/audio"
	"github.com/thatSFguy/swDefinedRadio/internal/demod"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// The FM broadcast band. Tuning outside it is refused rather than
// silently producing noise.
const (
	BandLowHz  = 87_500_000
	BandHighHz = 108_000_000
)

// Config is everything the receiver needs to know.
type Config struct {
	Freq   uint32
	Gain   float64
	PPM    int
	Device int

	Volume  float64 // output gain multiplier
	Deemph  string  // "us" (75us) or "eu" (50us)
	Speaker bool    // also play through this machine's audio device
}

// App is a configured FM receiver.
type App struct {
	cfg   Config
	tau   float64
	st    *station
	audio *audio.Broadcaster
}

// New builds the receiver, refusing a frequency outside the band before
// anything touches the radio.
func New(cfg Config) (*App, error) {
	if err := checkBand(cfg.Freq); err != nil {
		return nil, err
	}
	tau := demod.DeemphasisUS
	if strings.EqualFold(cfg.Deemph, "eu") {
		tau = demod.DeemphasisEU
	}
	return &App{
		cfg:   cfg,
		tau:   tau,
		st:    &station{freq: cfg.Freq},
		audio: audio.NewBroadcaster(),
	}, nil
}

// Radio is the tuner state this receiver needs.
func (a *App) Radio() sdr.Config {
	cfg := sdr.Config{
		CenterFreq:     a.cfg.Freq,
		SampleRate:     demod.InputRate,
		Gain:           sdr.AutoGain,
		FreqCorrection: a.cfg.PPM,
		DeviceIndex:    a.cfg.Device,
	}
	if a.cfg.Gain >= 0 {
		cfg.Gain = int(a.cfg.Gain * 10)
	}
	return cfg
}

// Station reports what is tuned and how strong it is.
func (a *App) Station() (uint32, float64) { return a.st.get() }

// Handler builds the player and JSON API. ctx bounds the audio stream,
// which is the one endpoint that does not end on its own.
func (a *App) Handler(ctx context.Context) (http.Handler, error) {
	return handler(ctx, a.st, a.audio)
}

// Run demodulates until ctx is cancelled.
//
// The source is attached for the duration rather than at construction,
// because the receiver outlives any one spell on the radio: it can be set
// aside and given the radio back later, and the station it was tuned to
// is still the station it returns to.
func (a *App) Run(ctx context.Context, src sdr.Source) error {
	a.st.attach(src)
	defer a.st.detach()

	// Every listener has to be let go when the radio is handed on, or a
	// browser sits holding a stream that nothing will ever write to. The
	// broadcaster itself survives, ready for the next time this receiver
	// is given the radio.
	defer a.audio.Drop()

	if a.cfg.Speaker {
		if err := startSpeaker(ctx, a.audio); err != nil {
			log.Printf("speaker: %v (the stream still works)", err)
		} else {
			log.Print("playing through this machine's audio device")
		}
	}

	log.Printf("tuned to %s", mhz(a.cfg.Freq))
	a.receive(ctx, src)
	return ctx.Err()
}

// ErrNotOnAir is returned by a tuning request that arrives while the
// radio is doing something else. It is a separate error because it is not
// the caller's mistake — the frequency may be perfectly good — so it
// deserves a different answer from a frequency that is out of band.
var ErrNotOnAir = errors.New("this receiver does not currently have the radio")

// station holds the current tuning, shared between the receive loop and
// the HTTP handlers.
type station struct {
	mu    sync.RWMutex
	freq  uint32
	level float64
	src   sdr.Source
}

func (s *station) get() (uint32, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.freq, s.level
}

func (s *station) setLevel(v float64) {
	s.mu.Lock()
	s.level = v
	s.mu.Unlock()
}

func (s *station) attach(src sdr.Source) {
	s.mu.Lock()
	s.src = src
	s.mu.Unlock()
}

func (s *station) detach() {
	s.mu.Lock()
	s.src = nil
	s.mu.Unlock()
}

// tune moves to a new station, rejecting anything outside the band.
//
// It is called from an HTTP handler while the receive loop is reading, so
// it also has to cope with there being no radio attached at all — which
// is what a tuning request arriving just as the receiver is set aside
// looks like.
func (s *station) tune(hz uint32) error {
	if err := checkBand(hz); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src == nil {
		return ErrNotOnAir
	}
	if err := s.src.Tune(hz); err != nil {
		return fmt.Errorf("tune: %w", err)
	}
	s.freq = hz
	log.Printf("tuned to %s", mhz(hz))
	return nil
}

func checkBand(hz uint32) error {
	if hz < BandLowHz || hz > BandHighHz {
		return fmt.Errorf("%s is outside the FM broadcast band (%s to %s)",
			mhz(hz), mhz(BandLowHz), mhz(BandHighHz))
	}
	return nil
}

// receive is the demodulation loop.
func (a *App) receive(ctx context.Context, src sdr.Source) {
	fm := demod.NewFM(a.tau)
	fm.Gain *= a.cfg.Volume

	// A block of about 85 ms: short enough that retuning feels immediate,
	// long enough that per-block overhead is irrelevant.
	const block = 1 << 17
	iq := make([]byte, block)
	pcm := make([]int16, block/2)

	for ctx.Err() == nil {
		n, err := io.ReadFull(src, iq)
		if err != nil && n == 0 {
			if ctx.Err() == nil {
				log.Printf("radio stopped delivering samples: %v", err)
			}
			return
		}
		k := fm.Process(iq[:n], pcm)
		a.st.setLevel(fm.Level())
		a.audio.Send(pcm[:k])
	}
}

// startSpeaker pipes audio to sox, which is the one tool present on both
// a normal desktop and WSL. Failure is not fatal: the browser stream is
// the primary output.
func startSpeaker(ctx context.Context, b *audio.Broadcaster) error {
	cmd := exec.CommandContext(ctx, "play", "-q",
		"-t", "raw", "-r", strconv.Itoa(demod.AudioRate),
		"-e", "signed", "-b", "16", "-c", "1", "-")
	cmd.Env = speakerEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sox (is it installed?): %w", err)
	}

	ch := b.Subscribe()
	go func() {
		defer in.Close()
		defer b.Unsubscribe(ch)
		buf := make([]byte, 0, 8192)
		for block := range ch {
			buf = audio.LittleEndianPCM(buf[:0], block)
			if _, err := in.Write(buf); err != nil {
				return
			}
		}
	}()
	return nil
}

func mhz(v uint32) string { return fmt.Sprintf("%.1f MHz", float64(v)/1e6) }

// parseHz accepts plain hertz or a k/M/G suffix, and bare numbers under
// 200 are read as megahertz so "98.7" works.
func parseHz(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	mult := 0.0
	switch last := s[len(s)-1]; last {
	case 'k', 'K':
		mult, s = 1e3, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1e6, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1e9, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a frequency", s)
	}
	if mult == 0 {
		mult = 1
		if v < 200 { // "98.7" plainly means megahertz here
			mult = 1e6
		}
	}
	v *= mult
	if v <= 0 || v > 4e9 {
		return 0, fmt.Errorf("%.0f Hz is out of range", v)
	}
	return uint32(v), nil
}

// ParseHz accepts plain hertz or a k/M/G suffix, and bare numbers under
// 200 are read as megahertz so "98.7" works.
func ParseHz(s string) (uint32, error) { return parseHz(s) }

// MHz renders a frequency the way the dial and the logs do.
func MHz(v uint32) string { return mhz(v) }
