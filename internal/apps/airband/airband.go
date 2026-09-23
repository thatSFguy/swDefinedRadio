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

	// Squelch overrides the receiver's own for this channel. Channels
	// differ: a distant approach frequency needs a lower threshold than
	// a tower two miles away, and one threshold for all of them means
	// either missing the far one or opening constantly on the near one.
	// Zero means use the receiver's setting.
	Squelch float64 `json:"squelch,omitempty"`
}

// DefaultChannels are the ones that mean the same thing anywhere, since
// tower and approach frequencies are different at every airport and
// guessing them would be worse than leaving the list short.
var DefaultChannels = []Channel{
	{Name: "Guard", Hz: Guard},
	{Name: "Unicom", Hz: 122_800_000},
	{Name: "CTAF", Hz: 122_700_000},
	{Name: "Unicom 123.0", Hz: 123_000_000},
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

	// Discover sweeps the band on the first run and adds whatever is
	// transmitting, which saves looking local frequencies up. It only
	// happens when there is no saved list, since after that the list is
	// the user's and a sweep should be something they ask for.
	Discover bool
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

	// candidates are what a sweep turned up and nobody has decided about
	// yet. They are deliberately not channels: a sweep can say something
	// transmitted, not that it is worth keeping.
	candidates []Channel

	squelch   float64 // the receiver's own, where a channel says nothing
	surveying bool
	survey    map[uint32]*SurveyStat
	surveyAt  time.Time

	// fresh means no saved channel list was found, so a sweep on the
	// first run is a help rather than an interruption.
	fresh    bool
	sweeping bool

	// tunedAt is when the tuner last moved, and lastBusy when this
	// channel last carried anything.
	tunedAt  time.Time
	lastBusy time.Time
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
	loaded := false
	if saved, err := a.load(); err == nil && len(saved) > 0 {
		a.channels, loaded = saved, true
	} else if err != nil {
		log.Printf("airband: channels: %v", err)
	}
	a.freq = a.channels[0].Hz
	a.fresh = !loaded
	a.squelch = cfg.Squelch
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
	Channels   []Channel `json:"channels"`
	Candidates []Channel `json:"candidates"`
	Sweeping   bool           `json:"sweeping"`
	Survey     SurveyProgress `json:"survey"`
	Heard    []Heard   `json:"heard"`
	OnAir    bool      `json:"on_air"`
	Rate     int       `json:"audio_rate"`
}

func (a *App) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	// An empty list, not null: a caller should be able to count it
	// without checking first.
	heard := make([]Heard, 0, len(a.heard))
	heard = append(heard, a.heard...)
	slices.Reverse(heard)
	return State{
		Freq: a.freq, Name: a.nameOfLocked(a.freq),
		Scanning: a.scanning, Busy: a.busy,
		Level: a.am.Level(), Squelch: a.am.Squelch,
		Channels:   slices.Clone(a.channels),
		Candidates: slices.Clone(a.candidates),
		Sweeping:   a.sweeping,
		Survey:     a.surveyProgressLocked(),
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
	// A candidate being listened to is still worth naming, or the dial
	// goes blank at exactly the moment somebody is deciding about it.
	for _, c := range a.candidates {
		if c.Hz == hz {
			return c.Name + " — candidate"
		}
	}
	return ""
}

// Keep promotes a candidate to a channel, under whatever name is given.
func (a *App) Keep(hz uint32, name string) error {
	a.mu.Lock()
	i := slices.IndexFunc(a.candidates, func(c Channel) bool { return c.Hz == hz })
	if i < 0 {
		a.mu.Unlock()
		return fmt.Errorf("%s is not one of the channels found", MHz(hz))
	}
	c := a.candidates[i]
	if name = strings.TrimSpace(name); name != "" {
		c.Name = name
	}
	list := append(slices.Clone(a.channels), c)
	a.candidates = slices.Delete(a.candidates, i, i+1)
	a.mu.Unlock()

	slices.SortFunc(list, func(x, y Channel) int { return int(x.Hz) - int(y.Hz) })
	return a.SetChannels(list)
}

// Dismiss drops a candidate without keeping it.
func (a *App) Dismiss(hz uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.candidates = slices.DeleteFunc(a.candidates, func(c Channel) bool { return c.Hz == hz })
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
	// Choosing a channel means listening to that channel. Both of the
	// things that move the tuner on their own have to stop, not just the
	// scan — a survey walking the grid would retune within a tenth of a
	// second and the click would appear to do nothing.
	a.scanning = false
	a.surveying = false
	return a.tuneLocked(hz)
}

func (a *App) tuneLocked(hz uint32) error {
	// Whichever way this goes, the channel being left takes its history
	// with it: how recently *it* was busy says nothing about the new one,
	// and carrying the hold across would make every channel wait.
	a.freq = hz
	a.since = time.Now()
	a.tunedAt = a.since
	a.lastBusy = time.Time{}
	a.busy = false
	a.applySquelchLocked()

	if a.src == nil {
		return ErrNotOnAir // remembered for when the radio comes back
	}
	if err := a.src.Tune(hz); err != nil {
		return fmt.Errorf("tune: %w", err)
	}
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
	defer a.mu.Unlock()
	a.squelch = max(v, 0)
	a.applySquelchLocked()
}

// applySquelchLocked puts the threshold for the current channel into the
// demodulator, falling back to the receiver's own where a channel has
// nothing to say about it.
func (a *App) applySquelchLocked() {
	sq := a.squelch
	for _, c := range a.channels {
		if c.Hz == a.freq && c.Squelch > 0 {
			sq = c.Squelch
			break
		}
	}
	a.am.Squelch = sq
}

// SetChannelSquelch sets the threshold for one channel. Zero returns it
// to the receiver's own.
func (a *App) SetChannelSquelch(hz uint32, v float64) error {
	a.mu.Lock()
	i := slices.IndexFunc(a.channels, func(c Channel) bool { return c.Hz == hz })
	if i < 0 {
		a.mu.Unlock()
		return fmt.Errorf("%s is not one of the channels", MHz(hz))
	}
	a.channels[i].Squelch = max(v, 0)
	list := slices.Clone(a.channels)
	a.applySquelchLocked()
	a.mu.Unlock()
	return a.SetChannels(list)
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

const (
	// dwell is how long a channel is given to show activity before the
	// scan moves on. The squelch decides once per block of about 55 ms,
	// so this is a couple of decisions — enough to be sure, and short
	// enough that a list of twenty channels comes round in a few
	// seconds rather than after the transmission has finished.
	dwell = 120 * time.Millisecond

	// hang is how long a channel is held after it goes quiet, because
	// the reply almost always comes back on the same frequency and
	// moving on between the two halves of an exchange is the single
	// most annoying thing a scanner can do.
	hang = 2500 * time.Millisecond

	// settle is how long after retuning to ignore the squelch. Samples
	// captured before the tuner moved are still arriving, and they
	// carry the old channel's carrier with them.
	settle = 60 * time.Millisecond
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

	// A first run with no channel list starts a survey, because the list
	// it would otherwise scan is four generic frequencies and the
	// interesting ones are exactly what is not known.
	if a.cfg.Discover && a.fresh {
		a.mu.Lock()
		a.fresh = false
		a.mu.Unlock()
		log.Print("no channels known yet — surveying the band, which takes a few minutes")
		a.StartSurvey()
	}

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

	now := time.Now()

	// Whatever is arriving just after a retune was captured before it,
	// so it describes the channel just left.
	if now.Sub(a.tunedAt) < settle {
		return
	}

	open := a.am.Open()
	if a.surveying {
		if open != a.busy {
			if open {
				a.since = now
			} else {
				a.recordLocked(now.Sub(a.since))
			}
		}
		a.surveyStep(now, open)
		a.busy = open
		return
	}
	switch {
	case open && !a.busy:
		a.since = now // somebody keyed up
	case !open && a.busy:
		// Record what was just heard, which is most of the point of
		// leaving a scanner running while you are not in the room.
		a.recordLocked(now.Sub(a.since))
		a.lastBusy = now
	}
	a.busy = open

	if open {
		return // stay while they are talking
	}
	if !a.scanning || len(a.channels) < 2 {
		return
	}

	// A channel that has just been busy is held, because the reply comes
	// back on the same frequency and moving on between the two halves of
	// an exchange is the worst thing a scanner can do.
	wait := dwell
	if !a.lastBusy.IsZero() && now.Sub(a.lastBusy) < hang {
		wait = hang
	}
	if now.Sub(a.since) < wait {
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
