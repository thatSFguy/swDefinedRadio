// Package adsb receives ADS-B on 1090 MHz: it demodulates Mode S frames
// from the IQ stream, decodes the ones that carry position, speed and
// identity, and keeps a table of the aircraft currently being heard.
//
// The receiver and the web side are separate on purpose. The tracker and
// the alert watcher belong to the App and live for as long as it does,
// while Run — the part that actually holds the radio — starts and stops.
// That is what lets a tabbed front end put this receiver aside and come
// back to a table that is still there.
package adsb

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/aircraftui"
	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
	"github.com/thatSFguy/swDefinedRadio/internal/modes"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

// blockSamples is how many IQ samples are demodulated at a time: about
// 64 ms at 2 Msps, small enough to keep latency low and large enough
// that per-block overhead does not matter.
const blockSamples = 128 * 1024

// Config is everything the receiver needs to know. It is filled from the
// command line, or by the hub from its own settings.
type Config struct {
	Freq   uint32  // centre frequency in Hz
	Gain   float64 // dB, or sdr.AutoGain for the tuner's own AGC
	PPM    int
	Device int

	// Lat and Lon are the antenna's position. Without one, positions need
	// an even/odd frame pair and no range can be reported.
	Lat, Lon float64

	// PositionNote says where that position came from, so a stale config
	// file putting the antenna somewhere surprising is visible in the log.
	PositionNote string

	TTL   time.Duration // forget aircraft unheard for this long
	Raw   bool          // print every valid frame as hex
	Quiet bool          // suppress the periodic status line

	TileURL          string
	ConfigPath       string
	AllowSetPosition bool

	// OnSetPosition, if set, is called when the position is changed from
	// this receiver's page, so anything else keeping a position can
	// follow it.
	OnSetPosition func(lat, lon float64)

	// Alerts, unless NoAlerts is set.
	NoAlerts   bool
	AlertsPath string
	AlertLog   string
	AlertCmd   string
}

// App is a configured ADS-B receiver.
type App struct {
	cfg     Config
	tracker *track.Tracker
	watcher *alert.Watcher
}

// New builds the receiver. ctx bounds the alert watcher, which outlives
// any one spell on the radio — an alert command already running should
// not be killed because the radio moved on.
func New(ctx context.Context, cfg Config) (*App, error) {
	a := &App{cfg: cfg, tracker: track.New(cfg.TTL)}

	switch pos := (config.Config{Lat: cfg.Lat, Lon: cfg.Lon}); {
	case pos.HasPosition():
		a.tracker.SetReference(pos.Lat, pos.Lon)
		log.Printf("receiver at %.4f, %.4f (%s)", pos.Lat, pos.Lon, cfg.PositionNote)
	default:
		log.Print("no receiver position: positions need an even/odd frame pair, and range is unavailable")
		if cfg.ConfigPath == "" {
			log.Printf("set one with ./sdr setpos <lat> <lon>, or pass -lat/-lon")
		}
	}

	if !cfg.NoAlerts {
		w, err := alert.Setup(ctx, cfg.AlertsPath, cfg.AlertLog, cfg.AlertCmd)
		if err != nil {
			return nil, fmt.Errorf("alerts: %w", err)
		}
		a.watcher = w
	}
	return a, nil
}

// Tracker is the aircraft table, so a caller can point the receiver's own
// position at it from elsewhere.
func (a *App) Tracker() *track.Tracker { return a.tracker }

// Radio is the tuner state this receiver needs.
//
// The sample rate is not a setting: the Mode S slicer assumes exactly two
// samples per bit, so 2 Msps is part of the demodulator rather than a
// preference.
func (a *App) Radio() sdr.Config {
	cfg := sdr.Config{
		CenterFreq:     a.cfg.Freq,
		SampleRate:     modes.SampleRate,
		Gain:           sdr.AutoGain,
		FreqCorrection: a.cfg.PPM,
		DeviceIndex:    a.cfg.Device,
	}
	if a.cfg.Gain >= 0 {
		cfg.Gain = int(a.cfg.Gain * 10)
	}
	return cfg
}

// Handler builds the web UI and JSON API.
func (a *App) Handler() (http.Handler, error) {
	return aircraftui.Handler(a.tracker, a.watcher, aircraftui.Options{
		Name: "ADS-B", Band: "1090 MHz",
		TileURL:          a.cfg.TileURL,
		ConfigPath:       a.cfg.ConfigPath,
		AllowSetPosition: a.cfg.AllowSetPosition,
		OnSet:            a.cfg.OnSetPosition,
	})
}

// Run is the demodulation loop: read IQ, convert to magnitude,
// demodulate, decode, update the tracker. It returns when ctx is
// cancelled or the source stops delivering samples.
func (a *App) Run(ctx context.Context, src sdr.Source) error {
	log.Printf("listening on %.3f MHz at %d Msps",
		float64(a.cfg.Freq)/1e6, modes.SampleRate/1_000_000)

	var (
		mags  = dsp.NewMagTable()
		iq    = make([]byte, blockSamples*2)
		mag   = make([]uint16, blockSamples)
		demod modes.Demodulator
	)

	for ctx.Err() == nil {
		n, err := io.ReadFull(src, iq)
		if err != nil && n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return fmt.Errorf("radio stopped delivering samples: %w", err)
			}
			return err
		}
		// Timestamp the block at its start, backdating by its duration.
		at := time.Now().Add(-time.Duration(n/2) * time.Second / modes.SampleRate)

		count := mags.Compute(iq[:n], mag)
		demod.Process(mag[:count], at, func(f modes.Frame) {
			if a.cfg.Raw {
				fmt.Printf("*%x; DF%d %.1f dBFS\n", f.Bytes, f.DF(), f.Signal)
			}
			msg, ok := modes.DecodeADSB(f.Bytes)
			if !ok {
				// A DF11 all-call reply, or an extended squitter whose
				// type code we skip. It still proves the receiver works.
				a.tracker.CountFrame()
				return
			}
			ac := a.tracker.Update(msg, f.Signal, f.At)
			if a.watcher != nil {
				// Checking here rather than on a timer means an alert
				// fires on the message that made it true, which for an
				// emergency squawk is the whole point.
				a.watcher.Check(ac, f.At)
			}
		})
	}
	return ctx.Err()
}

// Expire prunes the aircraft table on a timer.
//
// This runs on the receiver's whole lifetime, not on its spell holding
// the radio. An aircraft last heard four minutes ago has gone whether or
// not anyone was listening, and a table frozen while the tab was away
// would come back showing aircraft that are no longer there.
func (a *App) Expire(ctx context.Context) {
	tick := time.NewTicker(a.cfg.TTL / 4)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			a.tracker.Expire(now)
		}
	}
}

// Status prints a one-line summary every ten seconds, with the message
// rate since the last line. Unlike Expire it belongs to the spell on the
// radio: a receiver that is not listening has nothing to report.
func (a *App) Status(ctx context.Context) {
	if a.cfg.Quiet {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			craft, st := a.tracker.Snapshot()
			rate := float64(st.Frames-last) / 10
			last = st.Frames
			log.Printf("%2d aircraft (%d positioned)  %6.1f msg/s  %d total%s",
				st.Tracked, st.WithPos, rate, st.Frames, strongest(craft))
		}
	}
}

// strongest names the most distant aircraft currently held, which is the
// most useful single number for judging antenna performance.
func strongest(craft []track.Aircraft) string {
	best := ""
	var far float64
	for _, a := range craft {
		if a.DistanceNM > far {
			far, best = a.DistanceNM, strings.TrimSpace(a.Callsign+" "+a.Hex)
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("  furthest %.0f NM (%s)", far, best)
}
