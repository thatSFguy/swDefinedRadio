package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/uat"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Uat receives UAT on 978 MHz with an RTL-SDR and serves the
// same live map and JSON API the 1090 MHz receiver does.
//
// UAT is the other half of ADS-B in the United States. Light aircraft
// below 18,000 ft carry it instead of a 1090 MHz transponder, and
// ground stations broadcast weather and traffic on it, so a 978
// receiver sees traffic a 1090 one never will — and nothing outside
// the US, where the band is not used this way.
func Uat(args []string) {
	fs := flag.NewFlagSet("uat", flag.ExitOnError)
	log.SetFlags(log.Ltime)

	saved, cfgPath, err := config.Load()
	if err != nil {
		log.Printf("config: %v (continuing without it)", err)
	}

	var (
		freq   = fs.Uint("freq", 978_000_000, "centre frequency in Hz")
		gain   = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm    = fs.Int("ppm", 0, "frequency correction in ppm")
		device = fs.Int("device", 0, "RTL-SDR device index")
		rtltcp = fs.String("rtltcp", "", "connect to an rtl_tcp server (host:port) instead of spawning rtl_sdr")
		replay = fs.String("file", "", "replay a raw IQ capture instead of opening the radio")
		addr   = fs.String("http", config.DefaultHTTPAddr, "address for the web UI and JSON API")
		lat    = fs.Float64("lat", saved.Lat, "receiver latitude, for range")
		lon    = fs.Float64("lon", saved.Lon, "receiver longitude")
		ttl    = fs.Duration("ttl", 60*time.Second, "forget aircraft unheard for this long")
		rawLog = fs.Bool("raw", false, "print every frame as hex")
		quiet  = fs.Bool("quiet", false, "suppress the periodic status line")

		tileURL = fs.String("tile-url", "https://tile.openstreetmap.org/{z}/{x}/{y}.png",
			"street map for the online option in the map selector; empty removes the option")
		alertsPath = fs.String("alerts", "alerts.json", "rules file for alerts; the built-in rules are used if it does not exist")
		alertLog   = fs.String("alert-log", "data/alerts.jsonl", "append raised alerts here, one JSON object per line")
		alertCmd   = fs.String("alert-cmd", "", "shell command to run for each alert, with ALERT_* in its environment")
		noAlerts   = fs.Bool("no-alerts", false, "do not watch for anything")
		noSetPos   = fs.Bool("no-position-api", false, "refuse to set the receiver position over HTTP")
	)
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := uat.New(ctx, uat.Config{
		Freq: uint32(*freq), Gain: *gain, PPM: *ppm, Device: *device,
		Lat: *lat, Lon: *lon, TTL: *ttl, Raw: *rawLog, Quiet: *quiet,
		TileURL: *tileURL, ConfigPath: cfgPath, AllowSetPosition: !*noSetPos,
		NoAlerts: *noAlerts, AlertsPath: *alertsPath, AlertLog: *alertLog, AlertCmd: *alertCmd,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Bind before touching the radio so a port clash fails immediately.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
	}
	defer ln.Close()

	src, err := sdr.Open(ctx, *rtltcp, *replay, app.Radio())
	if err != nil {
		log.Fatalf("radio: %v", err)
	}
	defer src.Close()

	h, err := app.Handler()
	if err != nil {
		log.Fatalf("%v", err)
	}
	go web.ServeUntil(ctx, ln, h)
	go app.Expire(ctx)
	go app.Status(ctx)
	go web.Announce(ctx, *addr)

	err = app.Run(ctx, src)
	switch {
	case *replay != "" && errors.Is(err, io.ErrUnexpectedEOF), *replay != "" && errors.Is(err, io.EOF):
		// The end of a recording is not a failure. Keep serving, so
		// what it contained can be looked at rather than flashing past.
		log.Printf("capture finished — the UI is still up at %s, Ctrl+C to stop", config.URL(*addr))
		<-ctx.Done()
	case err != nil && ctx.Err() == nil:
		log.Fatalf("receive: %v", err)
	}
}
