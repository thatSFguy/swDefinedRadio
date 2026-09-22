// Package aircraftui is the web side the aircraft receivers share: a
// map, a table, alerts and the receiver's own position. Both the 1090
// MHz and the 978 MHz receiver produce the same kind of picture, so
// they serve the same page rather than two that drift apart.
package aircraftui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

//go:embed web
var webFS embed.FS

// fsSub strips the embed directory prefix so index.html is served at /.
func fsSub() (fs.FS, error) { return fs.Sub(webFS, "web") }

// Options are the parts that differ between receivers.
type Options struct {
	Name    string // "ADS-B" or "UAT", shown in the page
	Band    string // "1090 MHz", for the same reason
	TileURL string // street map for the map selector; empty removes it

	ConfigPath       string
	AllowSetPosition bool

	// Extra registers endpoints only one receiver has, such as the
	// ground stations a UAT receiver hears.
	Extra func(mux *http.ServeMux)

	// OnSet is called when the position is changed from this page.
	//
	// It exists because the two aircraft receivers each keep their own
	// tracker, and in one process they have to agree: setting the
	// position on one tab and finding the other still working out ranges
	// from the old one is the kind of wrong that looks right. A receiver
	// running on its own leaves this nil and nothing is lost.
	OnSet func(lat, lon float64)
}

// Serve runs the web UI and JSON API on an already-bound listener until
// ctx is cancelled. The caller binds so that a port clash is reported
// before the radio starts, rather than as a stray line in the log later.
func Handler(t *track.Tracker, w *alert.Watcher, o Options) (http.Handler, error) {
	mux := http.NewServeMux()

	// The UI polls this once a second.
	mux.HandleFunc("GET /api/aircraft", func(w http.ResponseWriter, r *http.Request) {
		craft, stats := t.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Now      time.Time        `json:"now"`
			Stats    track.Stats      `json:"stats"`
			Aircraft []track.Aircraft `json:"aircraft"`
		}{time.Now(), stats, craft})
	})

	// What has been raised, and what is being watched for. The UI polls
	// this alongside the aircraft table.
	mux.HandleFunc("GET /api/alerts", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		out := struct {
			Watching bool          `json:"watching"`
			Rules    []alert.Rule  `json:"rules"`
			Alerts   []alert.Alert `json:"alerts"`
		}{Watching: w != nil, Rules: []alert.Rule{}, Alerts: []alert.Alert{}}
		if w != nil {
			out.Rules = w.Rules()
			out.Alerts = w.Recent(100)
		}
		_ = json.NewEncoder(rw).Encode(out)
	})

	// Which receiver this is, so one page can title itself for either.
	mux.HandleFunc("GET /api/info", func(rw http.ResponseWriter, r *http.Request) {
		web.WriteJSON(rw, map[string]string{"name": o.Name, "band": o.Band})
	})

	if o.Extra != nil {
		o.Extra(mux)
	}

	// The map selector's online option. The page goes to the tile
	// server directly, as any Leaflet page does — this receiver neither
	// proxies nor caches tiles, which is what got it refused when it
	// tried. An empty URL means the option is not offered at all.
	mux.HandleFunc("GET /api/map", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(rw).Encode(map[string]string{"tile_url": o.TileURL})
	})

	// The receiver's own position, which the browser can offer.
	positionHandlers(mux, t, o.ConfigPath, o.AllowSetPosition, o.OnSet)

	sub, err := fsSub()
	if err != nil {
		return nil, fmt.Errorf("web assets: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	return mux, nil
}

// Serve runs Handler on an already-bound listener until ctx is cancelled.
// The caller binds so that a port clash is reported before the radio
// starts, rather than as a stray line in the log later.
func Serve(ctx context.Context, ln net.Listener, t *track.Tracker, w *alert.Watcher, o Options) {
	h, err := Handler(t, w, o)
	if err != nil {
		log.Printf("%v", err)
		return
	}
	web.ServeUntil(ctx, ln, h)
}
