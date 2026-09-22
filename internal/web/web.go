// Package web holds the small pieces every receiver's HTTP side needs:
// writing a JSON reply, shutting a server down cleanly, and saying where
// the UI is. Each receiver had its own copy of all three, which is how
// they drifted — one of the five wrote no content type at all.
package web

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/config"
)

// WriteJSON sends v as the whole response.
//
// Nothing is cached: every one of these endpoints reports what the radio
// is hearing right now, and a minute-old aircraft table served from a
// browser cache would be worse than no table at all.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// shutdownGrace is how long a server is given to finish the requests it
// is already serving. Short, because the only slow one is an endless
// audio stream that will never finish on its own.
const shutdownGrace = 2 * time.Second

// ServeUntil runs h on an already-bound listener until ctx is cancelled.
//
// The caller binds, rather than passing an address, so that a port clash
// is reported before the radio is touched instead of arriving as a stray
// line in the log once a receiver is already running.
func ServeUntil(ctx context.Context, ln net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("http: %v", err)
	}
}

// announceDelay is how long to wait before printing the URL. rtl_sdr and
// rtl_433 write a banner to stderr as they come up, and waiting for it
// means the address is the last thing on screen rather than being buried
// by it. It also means a UI is only advertised once the radio has not
// immediately failed.
const announceDelay = 2 * time.Second

// Announce prints where the web UI is, once the radio has had a moment to
// start.
func Announce(ctx context.Context, addr string) { AnnounceWith(ctx, addr, nil) }

// AnnounceWith prints a note alongside the address, for a receiver with
// something worth saying next to it — the station it ended up tuned to.
// The note is produced after the wait, so it describes the radio as it is
// by then rather than as it was at startup.
func AnnounceWith(ctx context.Context, addr string, note func() string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(announceDelay):
	}
	if note != nil {
		if s := note(); s != "" {
			log.Printf("%s → %s", s, config.URL(addr))
			return
		}
	}
	log.Printf("→ %s", config.URL(addr))
}
