package aircraftui

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"

	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// vagueM is the accuracy past which a browser's idea of where it is
// stops being useful. A desktop with no GPS is located by wifi or by
// IP address, and an IP-based fix can be a city away — which would put
// every range in the table out by that much.
const vagueM = 5_000

// uselessM is where it is refused outright: beyond this the position
// is worse than having none, since local position decoding assumes the
// aircraft is near the receiver.
const uselessM = 100_000

// positionHandlers serve the receiver's own position. The browser can
// offer its location, which is the easiest way to set this correctly —
// on the understanding that the browser is next to the antenna.
func positionHandlers(mux *http.ServeMux, t *track.Tracker, cfgPath string, allowSet bool, onSet func(lat, lon float64)) {
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}

	mux.HandleFunc("GET /api/position", func(w http.ResponseWriter, r *http.Request) {
		lat, lon, ok := t.Reference()
		web.WriteJSON(w, map[string]any{
			"lat": lat, "lon": lon, "set": ok,
			"path": cfgPath, "settable": allowSet,
		})
	})

	mux.HandleFunc("POST /api/position", func(w http.ResponseWriter, r *http.Request) {
		if !allowSet {
			http.Error(w, "setting the position over HTTP is disabled (-no-position-api)", http.StatusForbidden)
			return
		}
		var req struct {
			Lat       float64 `json:"lat"`
			Lon       float64 `json:"lon"`
			AccuracyM float64 `json:"accuracy_m"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		c := config.Config{Lat: req.Lat, Lon: req.Lon}
		if !c.HasPosition() {
			http.Error(w, fmt.Sprintf("%.5f, %.5f is not a usable position", req.Lat, req.Lon),
				http.StatusBadRequest)
			return
		}
		if req.AccuracyM > uselessM {
			http.Error(w, fmt.Sprintf("that fix is only good to %.0f km, which is too vague to receive with",
				req.AccuracyM/1000), http.StatusBadRequest)
			return
		}

		t.SetReference(c.Lat, c.Lon)
		if onSet != nil {
			onSet(c.Lat, c.Lon)
		}
		resp := map[string]any{"lat": c.Lat, "lon": c.Lon, "set": true, "path": cfgPath}
		log.Printf("receiver moved to %.4f, %.4f (from the browser%s)", c.Lat, c.Lon, accuracyNote(req.AccuracyM))

		// Persist, so the next start does not go back to the old
		// position. A failure here is worth reporting but does not undo
		// the change already in effect.
		if err := config.Save(c, cfgPath); err != nil {
			log.Printf("position: cannot save %s: %v", cfgPath, err)
			resp["saved"] = false
			resp["warning"] = "position set for this session only: " + err.Error()
		} else {
			resp["saved"] = true
		}
		if req.AccuracyM > vagueM {
			resp["warning"] = fmt.Sprintf(
				"this fix is only good to %.0f km — ranges will be out by about that much",
				req.AccuracyM/1000)
		}
		web.WriteJSON(w, resp)
	})
}

func accuracyNote(m float64) string {
	switch {
	case m <= 0:
		return ""
	case m < 1000:
		return fmt.Sprintf(", ±%.0f m", math.Round(m))
	default:
		return fmt.Sprintf(", ±%.1f km", m/1000)
	}
}
