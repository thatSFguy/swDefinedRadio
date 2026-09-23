// Package tpms logs tire-pressure sensor transmissions heard on 315 and
// 433.92 MHz, groups the sensors into vehicles, and serves a live view.
//
// Unlike the other receivers this one does no signal processing of its
// own: rtl_433 already knows the several dozen sensor protocols in use,
// and reimplementing them would be a large amount of work to arrive
// somewhere worse. rtl_433 runs as a child process and its JSON output is
// parsed here.
//
// The store outlives any one spell on the radio, and is backed by disk in
// any case, so readings accumulate across a whole afternoon rather than
// only for as long as the receiver happens to be listening.
package tpms

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"slices"
	"strings"
	"sync"
	"time"

	lib "github.com/thatSFguy/swDefinedRadio/internal/tpms"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

//go:embed web
var webFS embed.FS

// fsSub strips the embed directory prefix so index.html is served at /.
func fsSub() (fs.FS, error) { return fs.Sub(webFS, "web") }

// Config is everything the receiver needs to know.
type Config struct {
	// Freqs are the frequencies to listen on, in rtl_433 notation. TPMS
	// is at 315 MHz in North America and 433.92 MHz in Europe; plenty of
	// cars sold in the US use either, so both are worth covering.
	Freqs      []string
	HopSeconds int

	Gain   string // rtl_433 notation, empty for automatic
	PPM    int
	Device int

	Dir   string // where the reading log and sensor table live
	Other bool   // also report non-TPMS devices rtl_433 decodes
	Quiet bool
}

// Hop is the band setting that visits every configured frequency in
// turn, rather than staying on one.
const Hop = "hop"

// App is a configured TPMS logger.
type App struct {
	cfg   Config
	src   lib.SourceConfig
	store *lib.Store

	// band is which frequency to stay on, or Hop for all of them.
	// Changing it restarts rtl_433, because the frequencies are command
	// line arguments and it has no way to be told otherwise.
	mu      sync.Mutex
	band    string
	version int
	cancel  context.CancelFunc
}

// New opens the store, which carries whatever previous runs heard.
func New(cfg Config) (*App, error) {
	store, err := lib.NewStore(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if sensors, vehicles, total := store.Snapshot(); total > 0 || len(sensors) > 0 {
		log.Printf("resuming: %d sensors, %d vehicles known", len(sensors), len(vehicles))
	}
	return &App{
		cfg:   cfg,
		band:  Hop,
		store: store,
		src: lib.SourceConfig{
			Freqs:      cfg.Freqs,
			HopSeconds: cfg.HopSeconds,
			Gain:       cfg.Gain,
			PPM:        cfg.PPM,
			Device:     strconv.Itoa(cfg.Device),
		},
	}, nil
}

// Bands are the frequencies this receiver can be parked on.
func (a *App) Bands() []string { return a.cfg.Freqs }

// Band is the frequency it is on, or Hop.
func (a *App) Band() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.band
}

// SetBand parks the receiver on one frequency, or sets it hopping.
//
// One dongle cannot cover 315 and 433.92 MHz at once — they are 119 MHz
// apart and it has 2.4 MHz of bandwidth — so hopping means hearing each
// of them half the time. Staying on one doubles the chance of catching
// what transmits there, which is worth doing when you know what you are
// waiting for.
func (a *App) SetBand(band string) error {
	if band == "" {
		band = Hop
	}
	if band != Hop && !slices.Contains(a.cfg.Freqs, band) {
		return fmt.Errorf("%q is not one of the frequencies this receiver was given (%s)",
			band, strings.Join(a.cfg.Freqs, ", "))
	}

	a.mu.Lock()
	if a.band == band {
		a.mu.Unlock()
		return nil
	}
	a.band = band
	a.version++
	cancel := a.cancel
	a.mu.Unlock()

	// Stop the child so the loop starts a new one on the new frequency.
	if cancel != nil {
		cancel()
	}
	return nil
}

// sourceFor is the rtl_433 configuration for the current band.
func (a *App) sourceFor() (lib.SourceConfig, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.src
	if a.band != Hop {
		cfg.Freqs = []string{a.band}
	}
	return cfg, a.version
}

func (a *App) setCancel(f context.CancelFunc) {
	a.mu.Lock()
	a.cancel = f
	a.mu.Unlock()
}

// Store is the sensor table.
func (a *App) Store() *lib.Store { return a.store }

// Close saves the sensor table.
func (a *App) Close() error { return a.store.Close() }

// Handler builds the web UI and JSON API.
func (a *App) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	Routes(mux, a.store)
	a.bandRoutes(mux)

	sub, err := fsSub()
	if err != nil {
		return nil, fmt.Errorf("web assets: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux, nil
}

// RunAt is Run against a radio somebody else is holding.
//
// addr is an rtl_tcp address; rtl_433 connects to it itself rather than
// opening the USB device, which is what lets this receiver take its turn
// without the dongle being handed back and forth. An empty addr means
// open the device directly, as it always did.
func (a *App) RunAt(ctx context.Context, addr string) error {
	if addr != "" {
		a.src.Device = "rtl_tcp:" + addr
		log.Printf("reaching the radio through rtl_tcp at %s", addr)
	}
	return a.Run(ctx)
}

// Run starts rtl_433 and records what it decodes, until ctx is cancelled.
//
// It restarts the child whenever the band changes, since the frequencies
// are command line arguments; a change cancels the run in progress rather
// than waiting for it to end on its own.
func (a *App) Run(ctx context.Context) error {
	log.Printf("logging to %s", filepath.Join(a.cfg.Dir, "readings.jsonl"))

	for ctx.Err() == nil {
		cfg, version := a.sourceFor()
		log.Printf("listening on %s%s", strings.Join(cfg.Freqs, ", "), hopNote(cfg))

		runCtx, cancel := context.WithCancel(ctx)
		a.setCancel(cancel)
		err := a.listen(runCtx, cfg)
		cancel()
		a.setCancel(nil)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A run that ended because the band changed is not a failure;
		// go round and start on the new one.
		if _, now := a.sourceFor(); now != version {
			continue
		}
		return err
	}
	return ctx.Err()
}

func (a *App) listen(ctx context.Context, cfg lib.SourceConfig) error {
	src, err := lib.Start(ctx, cfg)
	if err != nil {
		return fmt.Errorf("radio: %w", err)
	}
	defer src.Close()

	return src.Run(
		func(r lib.Reading) {
			a.store.Add(r)
			if !a.cfg.Quiet {
				log.Print(describe(r))
			}
		},
		func(line []byte) {
			if !a.cfg.Other {
				return
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				log.Printf("(other) %v %v", m["model"], m["id"])
			}
		},
	)
}

// Autosave persists the sensor table periodically so an unclean exit
// loses at most a few seconds of accumulated state.
func (a *App) Autosave(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if a.store.Dirty() {
				if err := a.store.Save(); err != nil {
					log.Printf("save: %v", err)
				}
			}
		}
	}
}

func hopNote(c lib.SourceConfig) string {
	if len(c.Freqs) < 2 {
		return ""
	}
	return fmt.Sprintf(" (hopping every %ds — one dongle cannot cover both at once)", c.HopSeconds)
}

// describe renders a reading for the console.
func describe(r lib.Reading) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-28s %-12s", r.Model+" "+r.ID, r.Integrity)
	if r.HasPressure {
		fmt.Fprintf(&b, " %6.1f psi", r.PSI())
	} else {
		b.WriteString("           ")
	}
	if r.HasTemperature {
		fmt.Fprintf(&b, " %5.1f C", r.TemperatureC)
	}
	if r.BatteryOK != nil && !*r.BatteryOK {
		b.WriteString("  BATTERY LOW")
	}
	return b.String()
}

// bandRoutes report and change which frequency the receiver is on.
func (a *App) bandRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/band", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, map[string]any{
			"band":  a.Band(),
			"bands": a.Bands(),
		})
	})

	mux.HandleFunc("POST /api/band", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Band string `json:"band"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := a.SetBand(req.Band); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, map[string]any{"band": a.Band(), "bands": a.Bands()})
	})
}

// Split turns the comma-separated -freq flag into the list rtl_433 wants.
func Split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Routes registers the JSON API. It is separate from Handler so a front
// end serving several receivers can mount it on a mux of its own.
func Routes(mux *http.ServeMux, store *lib.Store) {
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		sensors, vehicles, total := store.Snapshot()
		web.WriteJSON(w, struct {
			Now      time.Time     `json:"now"`
			Total    int64         `json:"total_readings"`
			Sensors  []lib.Sensor  `json:"sensors"`
			Vehicles []lib.Vehicle `json:"vehicles"`
			Recent   []lib.Reading `json:"recent"`
		}{time.Now(), total, sensors, vehicles, store.Recent(40)})
	})

	// Labelling is the one write the UI needs: name a sensor's wheel
	// position, or name the vehicle a cluster turned out to be.
	mux.HandleFunc("POST /api/label", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind  string `json:"kind"` // "sensor" or "vehicle"
			ID    string `json:"id"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Kind != "sensor" && req.Kind != "vehicle" {
			http.Error(w, "kind must be sensor or vehicle", http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		store.SetLabel(req.Kind, req.ID, req.Label)
		if err := store.Save(); err != nil {
			log.Printf("save after label: %v", err)
		}
		web.WriteJSON(w, map[string]string{"status": "ok"})
	})

	// Which vehicle a sensor belongs to is worked out from what is heard
	// together, and sometimes that is wrong: two cars that always travel
	// together look like one, and a wheel heard on its own looks like
	// nothing. This is how someone says otherwise.
	mux.HandleFunc("POST /api/assign", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Sensor  string `json:"sensor"`
			Vehicle string `json:"vehicle"` // "" returns it to the clustering
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Sensor == "" {
			http.Error(w, "sensor required", http.StatusBadRequest)
			return
		}
		store.SetVehicle(req.Sensor, req.Vehicle)
		if err := store.Save(); err != nil {
			log.Printf("save after assign: %v", err)
		}
		web.WriteJSON(w, map[string]string{"status": "ok"})
	})
}
