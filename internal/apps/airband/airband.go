// Package airband receives aircraft voice: the AM channels between 118
// and 137 MHz that pilots and controllers actually talk on.
//
// It is the companion to the 1090 MHz receiver rather than a replacement
// for it — that one shows you where aircraft are, this one lets you hear
// them. One dongle cannot do both at once: the two bands are the better
// part of a gigahertz apart.
//
// A channel here is silent most of the time, which shapes everything.
// The squelch has to be good, because an open receiver on an idle
// channel is one nobody leaves switched on; and scanning matters more
// than tuning, because the interesting thing is usually "whoever is
// talking" rather than one particular frequency.
package airband

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/audio"
	"github.com/thatSFguy/swDefinedRadio/internal/demod"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// The band, and the channel spacing within it.
const (
	BandLowHz  = 118_000_000
	BandHighHz = 137_000_000

	// ChannelHz is the North American spacing. Europe has moved much of
	// the band to 8.33 kHz, but a receiver tuned to the 25 kHz grid still
	// hears those; it is the transmitters that had to change.
	ChannelHz = 25_000
)

// Guard is the international emergency frequency, monitored everywhere
// and the one channel worth having in any list.
const Guard = 121_500_000

// Channel is somewhere worth listening.
type Channel struct {
	Name string `json:"name"`
	Hz   uint32 `json:"hz"`
}

// DefaultChannels are the ones that mean the same thing anywhere, since
// tower and approach frequencies are different at every airport and
// guessing them would be worse than leaving the list short.
var DefaultChannels = []Channel{
	{"Guard", Guard},
	{"Unicom", 122_800_000},
	{"CTAF", 122_700_000},
	{"Unicom 123.0", 123_000_000},
}

// Config is everything the receiver needs to know.
type Config struct {
	Channels []Channel
	Gain     float64
	PPM      int
	Device   int

	// Squelch is the carrier level a channel must reach to count as
	// busy. Zero leaves the squelch open, which on this band means
	// listening to static.
	Squelch float64
	Volume  float64
	Speaker bool

	// Dir is where the channel list is kept, so channels added from the
	// page outlive a restart.
	Dir string
}

// App is a configured airband receiver.
type App struct {
	cfg   Config
	am    *demod.AM
	audio *audio.Broadcaster

	mu       sync.RWMutex
	channels []Channel
	freq     uint32
	scanning bool
	busy     bool      // the squelch is open
	since    time.Time // when the current channel was last busy
	src      sdr.Source
	heard    []Heard
}

// Heard is a record of a transmission, which is most of the value of
// leaving a scanner running.
type Heard struct {
	At      time.Time `json:"at"`
	Hz      uint32    `json:"hz"`
	Name    string    `json:"name,omitempty"`
	Seconds float64   `json:"seconds"`
	Level   float64   `json:"level"`
}

// New builds the receiver, loading any channels saved from the page.
func New(cfg Config) (*App, error) {
	if cfg.Volume == 0 {
		cfg.Volume = 1
	}
	a := &App{
		cfg:      cfg,
		am:       demod.NewAM(),
		audio:    audio.NewBroadcaster(),
		channels: slices.Clone(cfg.Channels),
		scanning: true,
	}
	if len(a.channels) == 0 {
		a.channels = slices.Clone(DefaultChannels)
	}
	if saved, err := a.load(); err == nil && len(saved) > 0 {
		a.channels = saved
	} else if err != nil {
		log.Printf("airband: channels: %v", err)
	}
	a.freq = a.channels[0].Hz
	a.am.Squelch = cfg.Squelch
	a.am.Gain *= cfg.Volume
	return a, nil
}

// Radio is the tuner state this receiver needs.
func (a *App) Radio() sdr.Config {
	cfg := sdr.Config{
		CenterFreq:     a.Freq(),
		SampleRate:     demod.AMInputRate,
		Gain:           sdr.AutoGain,
		FreqCorrection: a.cfg.PPM,
		DeviceIndex:    a.cfg.Device,
	}
	if a.cfg.Gain >= 0 {
		cfg.Gain = int(a.cfg.Gain * 10)
	}
	return cfg
}

func (a *App) Freq() uint32 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.freq
}

// State is what the page draws itself from.
type State struct {
	Freq     uint32    `json:"freq_hz"`
	Name     string    `json:"name,omitempty"`
	Scanning bool      `json:"scanning"`
	Busy     bool      `json:"busy"`
	Level    float64   `json:"level"`
	Squelch  float64   `json:"squelch"`
	Channels []Channel `json:"channels"`
	Heard    []Heard   `json:"heard"`
	OnAir    bool      `json:"on_air"`
	Rate     int       `json:"audio_rate"`
}

func (a *App) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	heard := slices.Clone(a.heard)
	slices.Reverse(heard)
	return State{
		Freq: a.freq, Name: a.nameOfLocked(a.freq),
		Scanning: a.scanning, Busy: a.busy,
		Level: a.am.Level(), Squelch: a.am.Squelch,
		Channels: slices.Clone(a.channels),
		Heard:    heard,
		OnAir:    a.src != nil,
		Rate:     demod.AudioRate,
	}
}

func (a *App) nameOfLocked(hz uint32) string {
	for _, c := range a.channels {
		if c.Hz == hz {
			return c.Name
		}
	}
	return ""
}

// Audio is the fan-out the page and the speaker listen to.
func (a *App) Audio() *audio.Broadcaster { return a.audio }

// ErrNotOnAir is returned when the radio is doing something else.
var ErrNotOnAir = fmt.Errorf("this receiver does not currently have the radio")

// Tune parks on one channel and stops scanning, which is what choosing a
// channel means.
func (a *App) Tune(hz uint32) error {
	if hz < BandLowHz || hz > BandHighHz {
		return fmt.Errorf("%s is outside the airband (%s to %s)",
			MHz(hz), MHz(BandLowHz), MHz(BandHighHz))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scanning = false
	return a.tuneLocked(hz)
}

func (a *App) tuneLocked(hz uint32) error {
	if a.src == nil {
		a.freq = hz // remembered for when the radio comes back
		return ErrNotOnAir
	}
	if err := a.src.Tune(hz); err != nil {
		return fmt.Errorf("tune: %w", err)
	}
	a.freq = hz
	a.since = time.Now()
	return nil
}

// SetScanning starts or stops moving between channels.
func (a *App) SetScanning(on bool) {
	a.mu.Lock()
	a.scanning = on
	a.since = time.Now()
	a.mu.Unlock()
}

// SetSquelch changes the level a channel must reach to count as busy.
func (a *App) SetSquelch(v float64) {
	a.mu.Lock()
	a.am.Squelch = max(v, 0)
	a.mu.Unlock()
}

// SetChannels replaces the list and saves it.
func (a *App) SetChannels(list []Channel) error {
	if len(list) == 0 {
		return fmt.Errorf("a scanner with no channels has nothing to do")
	}
	for i, c := range list {
		if c.Hz < BandLowHz || c.Hz > BandHighHz {
			return fmt.Errorf("%s is outside the airband", MHz(c.Hz))
		}
		if c.Name == "" {
			list[i].Name = MHz(c.Hz)
		}
	}
	a.mu.Lock()
	a.channels = slices.Clone(list)
	still := slices.ContainsFunc(a.channels, func(c Channel) bool { return c.Hz == a.freq })
	a.mu.Unlock()

	// Staying tuned to a channel that has just been deleted would leave
	// the receiver somewhere the page no longer shows.
	if !still {
		a.mu.Lock()
		_ = a.tuneLocked(a.channels[0].Hz)
		a.mu.Unlock()
	}
	return a.save()
}

func (a *App) channelsPath() string {
	dir := a.cfg.Dir
	if dir == "" {
		dir = "data/airband"
	}
	return filepath.Join(dir, "channels.json")
}

func (a *App) load() ([]Channel, error) {
	b, err := os.ReadFile(a.channelsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Channel
	return list, json.Unmarshal(b, &list)
}

func (a *App) save() error {
	a.mu.RLock()
	b, err := json.MarshalIndent(a.channels, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	p := a.channelsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// MHz renders a frequency the way the band is spoken about.
func MHz(hz uint32) string { return fmt.Sprintf("%.3f MHz", float64(hz)/1e6) }

// ParseHz accepts a frequency as written on a chart — "121.5", "118.30",
// or a plain number of hertz.
func ParseHz(s string) (uint32, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "M"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a frequency", s)
	}
	if v < 1000 { // written in megahertz, as it always is for this band
		v *= 1e6
	}
	if v <= 0 || v > 4e9 {
		return 0, fmt.Errorf("%.0f Hz is out of range", v)
	}
	return uint32(v), nil
}

// ParseChannels reads the -channels flag: "Tower:118.3,Ground:121.9".
func ParseChannels(s string) ([]Channel, error) {
	var out []Channel
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, freq, ok := strings.Cut(part, ":")
		if !ok {
			name, freq = "", part
		}
		hz, err := ParseHz(freq)
		if err != nil {
			return nil, err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			name = MHz(hz)
		}
		out = append(out, Channel{Name: name, Hz: hz})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no channels in %q", s)
	}
	return out, nil
}

// how long a channel is given to show activity before the scan moves on,
// and how long a busy channel is held after it goes quiet.
const (
	dwell = 400 * time.Millisecond
	hang  = 2 * time.Second
)

// Run demodulates until ctx is cancelled, moving between channels while
// scanning and staying put while someone is talking.
func (a *App) Run(ctx context.Context, src sdr.Source) error {
	a.mu.Lock()
	a.src = src
	a.since = time.Now()
	start := a.freq
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.src = nil
		a.busy = false
		a.mu.Unlock()
		// Let go of anyone listening, or a browser holds a stream that
		// nothing will write to again.
		a.audio.Drop()
	}()

	// The broker tuned to wherever this receiver was last, but the
	// channel may have been changed while it was off the air.
	if err := src.Tune(start); err != nil {
		return fmt.Errorf("tune: %w", err)
	}
	log.Printf("listening on %s", MHz(start))

	if a.cfg.Speaker {
		if err := audio.Speaker(ctx, a.audio); err != nil {
			log.Printf("speaker: %v (the stream still works)", err)
		}
	}

	// About 85 ms a block, which is short enough that the scan reacts
	// within a syllable of somebody keying up.
	const block = 1 << 17
	iq := make([]byte, block)
	pcm := make([]int16, block/2)

	for ctx.Err() == nil {
		n, err := io.ReadFull(src, iq)
		if err != nil && n == 0 {
			if ctx.Err() == nil {
				log.Printf("radio stopped delivering samples: %v", err)
			}
			return err
		}
		k := a.am.Process(iq[:n], pcm)
		a.audio.Send(pcm[:k])
		a.step()
	}
	return ctx.Err()
}

// step decides whether to stay where it is or move on.
func (a *App) step() {
	a.mu.Lock()
	defer a.mu.Unlock()

	open := a.am.Open()
	if open != a.busy {
		if open {
			a.since = time.Now()
		} else {
			// Record what was just heard, which is the point of leaving
			// a scanner running while you are not in the room.
			a.recordLocked(time.Since(a.since))
		}
		a.busy = open
		if open {
			a.since = time.Now()
		}
	}
	if open {
		a.since = time.Now() // hold the channel while it is busy
		return
	}
	if !a.scanning || len(a.channels) < 2 {
		return
	}

	// Give a channel that was busy a moment in case the reply comes on
	// the same frequency, which it usually does.
	wait := dwell
	if a.busy {
		wait = hang
	}
	if time.Since(a.since) < wait {
		return
	}

	i := slices.IndexFunc(a.channels, func(c Channel) bool { return c.Hz == a.freq })
	next := a.channels[(i+1)%len(a.channels)]
	_ = a.tuneLocked(next.Hz)
}

// recordLocked adds a transmission to the log kept for the page.
func (a *App) recordLocked(d time.Duration) {
	if d < 250*time.Millisecond {
		return // a click of static rather than somebody talking
	}
	a.heard = append(a.heard, Heard{
		At: time.Now().Add(-d), Hz: a.freq, Name: a.nameOfLocked(a.freq),
		Seconds: d.Seconds(), Level: a.am.Level(),
	})
	// Keep the list to something a page can draw.
	if len(a.heard) > 200 {
		a.heard = a.heard[len(a.heard)-200:]
	}
}
