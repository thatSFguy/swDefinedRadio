package aircraftui

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// watchlistHandlers let the page edit the flights being watched for and
// acknowledge the alarms they raise. The alarms themselves are read from
// /api/alerts, which the page already polls.
//
// Acknowledging is kept here rather than in the page so that it holds
// for every tab and every browser looking at this receiver: silencing
// the alarm on the laptop should not leave the desktop still sounding.
func watchlistHandlers(mux *http.ServeMux, w *alert.Watcher) {
	list := func() *alert.Watchlist {
		if w == nil {
			return nil
		}
		return w.Watchlist()
	}

	mux.HandleFunc("GET /api/watchlist", func(rw http.ResponseWriter, r *http.Request) {
		web.WriteJSON(rw, map[string]any{"enabled": list() != nil, "flights": list().Flights()})
	})

	mux.HandleFunc("PUT /api/watchlist", func(rw http.ResponseWriter, r *http.Request) {
		l := list()
		if l == nil {
			http.Error(rw, "alerts are off (-no-alerts), so nothing is being watched for", http.StatusConflict)
			return
		}
		var req struct {
			Flights []string `json:"flights"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(rw, "want {\"flights\": [...]}: "+err.Error(), http.StatusBadRequest)
			return
		}
		flights, err := l.Set(req.Flights)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if len(flights) == 0 {
			log.Print("watchlist: cleared")
		} else {
			log.Printf("watchlist: %s", strings.Join(flights, " "))
		}
		web.WriteJSON(rw, map[string]any{"enabled": true, "flights": flights})
	})

	// An empty hex acknowledges every alarm at once.
	mux.HandleFunc("POST /api/alarms/ack", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Hex string `json:"hex"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<10)).Decode(&req); err != nil {
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}
		}
		alarms := []alert.Alarm{}
		if w != nil {
			w.Ack(req.Hex)
			alarms = w.Alarms(time.Now())
		}
		web.WriteJSON(rw, map[string]any{"alarms": alarms})
	})
}
