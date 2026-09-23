// Package uat receives UAT on 978 MHz, the second ADS-B link used in the
// United States by lighter aircraft. It demodulates the CPFSK signal,
// repairs what Reed-Solomon can, and keeps a table of the aircraft and
// ground stations it hears.
//
// As with the 1090 MHz receiver, the tracker and the alert watcher belong
// to the App and outlive any one spell on the radio; only Run starts and
// stops.
package uat

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/aircraftui"
	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
	"github.com/thatSFguy/swDefinedRadio/internal/uat"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Config is everything the receiver needs to know.
type Config struct {
	Freq   uint32
	Gain   float64
	PPM    int
	Device int

	Lat, Lon float64
	TTL      time.Duration
	Raw      bool
	Quiet    bool

	TileURL          string
	ConfigPath       string
	AllowSetPosition bool

	// OnSetPosition, if set, is called when the position is changed from
	// this receiver's page, so anything else keeping a position can
	// follow it.
	OnSetPosition func(lat, lon float64)

	NoAlerts   bool
	AlertsPath string
	AlertLog   string
	AlertCmd   string
}

// App is a configured UAT receiver.
type App struct {
	cfg      Config
	tracker  *track.Tracker
	watcher  *alert.Watcher
	stations *ground
}

// New builds the receiver. ctx bounds the alert watcher, which outlives
// any one spell on the radio.
func New(ctx context.Context, cfg Config) (*App, error) {
	a := &App{
		cfg:      cfg,
		tracker:  track.New(cfg.TTL),
		stations: &ground{seen: map[byte]*Station{}},
	}
	if pos := (config.Config{Lat: cfg.Lat, Lon: cfg.Lon}); pos.HasPosition() {
		a.tracker.SetReference(pos.Lat, pos.Lon)
		log.Printf("receiver at %.4f, %.4f", pos.Lat, pos.Lon)
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

// Tracker is the aircraft table.
func (a *App) Tracker() *track.Tracker { return a.tracker }

// Radio is the tuner state this receiver needs. As with Mode S the sample
// rate belongs to the demodulator rather than to preference: it is two
// samples per symbol at the UAT rate.
func (a *App) Radio() sdr.Config {
	cfg := sdr.Config{
		CenterFreq:     a.cfg.Freq,
		SampleRate:     uat.SampleRate,
		Gain:           sdr.AutoGain,
		FreqCorrection: a.cfg.PPM,
		DeviceIndex:    a.cfg.Device,
	}
	if a.cfg.Gain >= 0 {
		cfg.Gain = int(a.cfg.Gain * 10)
	}
	return cfg
}

// Handler builds the web UI and JSON API, which is the aircraft page the
// 1090 MHz receiver serves plus the ground stations only UAT hears.
func (a *App) Handler() (http.Handler, error) {
	return aircraftui.Handler(a.tracker, a.watcher, aircraftui.Options{
		Name: "UAT", Band: "978 MHz",
		TileURL:          a.cfg.TileURL,
		ConfigPath:       a.cfg.ConfigPath,
		AllowSetPosition: a.cfg.AllowSetPosition,
		OnSet:            a.cfg.OnSetPosition,
		Extra:            a.stations.handler,
	})
}

// Run is the demodulation loop.
func (a *App) Run(ctx context.Context, src sdr.Source) error {
	log.Printf("listening on %.3f MHz at %.4f Msps",
		float64(a.cfg.Freq)/1e6, float64(uat.SampleRate)/1e6)
	return a.receive(ctx, src)
}

// Expire prunes the aircraft table on a timer, on the receiver's whole
// lifetime rather than its spell on the radio.
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

// Status prints a one-line summary every ten seconds.
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
			pos := 0
			for _, c := range craft {
				if c.HasPosition {
					pos++
				}
			}
			log.Printf("%2d aircraft (%d positioned) %5.1f msg/s  %d ground stations",
				len(craft), pos, float64(st.Frames-last)/10, len(a.stations.snapshot()))
			last = st.Frames
		}
	}
}

// blockSamples is about 60 ms at the UAT sample rate.
const blockSamples = 64 * 1024

func (a *App) receive(ctx context.Context, src sdr.Source) error {
	var (
		demod uat.Demodulator
		iq    = make([]byte, blockSamples*2)
	)
	for ctx.Err() == nil {
		n, err := io.ReadFull(src, iq)
		if err != nil && n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return fmt.Errorf("no more samples: %w", err)
			}
			return err
		}
		at := time.Now().Add(-time.Duration(n/2) * time.Second / uat.SampleRate)

		demod.Process(iq[:n], at, func(f uat.Frame) {
			if a.cfg.Raw {
				kind := "adsb"
				if f.Uplink {
					kind = "uplink"
				}
				fmt.Printf("*%x; %s %.1f dBFS %d corrected\n", f.Payload, kind, f.Signal, f.Errors)
			}
			if f.Uplink {
				a.stations.add(f)
				return
			}
			m, ok := uat.DecodeADSB(f.Payload)
			if !ok {
				a.tracker.CountFrame()
				return
			}
			ac := a.tracker.UpdateReport(track.Report{
				ICAO:         m.Address,
				Callsign:     m.Callsign,
				Squawk:       m.Squawk,
				HasPosition:  m.HasPosition,
				Lat:          m.Lat,
				Lon:          m.Lon,
				HasAltitude:  m.HasAltitude,
				Altitude:     m.Altitude,
				HasVelocity:  m.HasVelocity,
				GroundSpeed:  m.GroundSpeed,
				Track:        m.Track,
				HasTrack:     m.HasVelocity,
				VerticalRate: m.VerticalRate,
				OnGround:     m.OnGround,
			}, f.Signal, f.At)
			if a.watcher != nil {
				a.watcher.Check(ac, f.At)
			}
		})
	}
	return ctx.Err()
}

// Station is a ground station heard broadcasting. What it is sending —
// weather, traffic, notices — is a protocol of its own that this
// receiver does not decode; that it is audible, and from where, is
// still worth knowing, because those broadcasts are the reason most of
// the 978 MHz band is busy.
type Station struct {
	Site      byte      `json:"site"`
	Lat       float64   `json:"lat,omitempty"`
	Lon       float64   `json:"lon,omitempty"`
	HasPos    bool      `json:"has_position"`
	Messages  int       `json:"messages"`
	Bytes     int       `json:"bytes"`
	Signal    float64   `json:"signal_dbfs"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type ground struct {
	mu   sync.Mutex
	seen map[byte]*Station
}

func (g *ground) add(f uat.Frame) {
	u, ok := uat.DecodeUplink(f.Payload)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.seen[u.TISBSite]
	if s == nil {
		s = &Station{Site: u.TISBSite, FirstSeen: f.At}
		g.seen[u.TISBSite] = s
		log.Printf("ground station %d heard%s", u.TISBSite, where(u))
	}
	s.LastSeen = f.At
	s.Messages++
	s.Bytes += u.Bytes
	s.Signal = f.Signal
	if u.HasPosition {
		s.Lat, s.Lon, s.HasPos = u.Lat, u.Lon, true
	}
}

func where(u uat.Uplink) string {
	if !u.HasPosition {
		return ""
	}
	return fmt.Sprintf(" at %.4f, %.4f", u.Lat, u.Lon)
}

func (g *ground) snapshot() []Station {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Station, 0, len(g.seen))
	for _, s := range g.seen {
		out = append(out, *s)
	}
	return out
}

func (g *ground) handler(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ground", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		web.WriteJSON(w, map[string]any{"stations": g.snapshot()})
	})
}
