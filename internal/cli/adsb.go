package cli

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/adsb"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Adsb receives ADS-B on 1090 MHz with an RTL-SDR and serves a
// live map and JSON API of the aircraft it hears.
func Adsb(args []string) {
	fs := flag.NewFlagSet("adsb", flag.ExitOnError)
	log.SetFlags(log.Ltime)

	// Settings from the config file become the flag defaults, so anything
	// passed explicitly on the command line still overrides them.
	saved, cfgPath, err := config.Load()
	if err != nil {
		log.Printf("config: %v (continuing without it)", err)
	}

	var (
		freq   = fs.Uint("freq", 1_090_000_000, "centre frequency in Hz")
		gain   = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm    = fs.Int("ppm", 0, "frequency correction in ppm")
		device = fs.Int("device", 0, "RTL-SDR device index")
		rtltcp = fs.String("rtltcp", "", "connect to an rtl_tcp server (host:port) instead of spawning rtl_sdr")
		addr   = fs.String("http", config.DefaultHTTPAddr, "address for the web UI and JSON API")
		lat    = fs.Float64("lat", saved.Lat, "receiver latitude, enables range and single-frame fixes")
		lon    = fs.Float64("lon", saved.Lon, "receiver longitude")
		ttl    = fs.Duration("ttl", 60*time.Second, "forget aircraft unheard for this long")
		rawLog = fs.Bool("raw", false, "print every valid frame as hex")
		quiet  = fs.Bool("quiet", false, "suppress the periodic status line")

		tileURL = fs.String("tile-url", "https://tile.openstreetmap.org/{z}/{x}/{y}.png",
			"street map tiles; empty uses the built-in offline map only")

		alertsPath = fs.String("alerts", "alerts.json", "rules file for alerts; the built-in rules are used if it does not exist")
		alertLog   = fs.String("alert-log", "data/alerts.jsonl", "append raised alerts here, one JSON object per line")
		alertCmd   = fs.String("alert-cmd", "", "shell command to run for each alert, with ALERT_* in its environment")
		noAlerts   = fs.Bool("no-alerts", false, "do not watch for anything")
		alertsInit = fs.Bool("alerts-example", false, "write a starter alerts file and exit")
		noSetPos   = fs.Bool("no-position-api", false, "refuse to set the receiver position over HTTP")
	)
	fs.Parse(args)

	// Note which of the position flags were given, so the log can say
	// where the receiver's position actually came from.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *alertsInit {
		if err := alert.WriteExample(*alertsPath); err != nil {
			log.Fatalf("alerts-example: %v", err)
		}
		return
	}

	app, err := adsb.New(ctx, adsb.Config{
		Freq: uint32(*freq), Gain: *gain, PPM: *ppm, Device: *device,
		Lat: *lat, Lon: *lon, PositionNote: positionSource(given, cfgPath),
		TTL: *ttl, Raw: *rawLog, Quiet: *quiet,
		TileURL: *tileURL, ConfigPath: cfgPath, AllowSetPosition: !*noSetPos,
		NoAlerts: *noAlerts, AlertsPath: *alertsPath, AlertLog: *alertLog, AlertCmd: *alertCmd,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Bind before touching the radio so a port clash fails immediately
	// with a clear message instead of leaving a receiver running headless.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
	}
	defer ln.Close()

	src, err := sdr.Open(ctx, *rtltcp, "", app.Radio())
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

	if err := app.Run(ctx, src); err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
}

// positionSource explains where the receiver position came from, which is
// worth stating plainly when a stale config file could otherwise put the
// antenna somewhere surprising.
func positionSource(given map[string]bool, cfgPath string) string {
	switch {
	case given["lat"] && given["lon"]:
		return "from -lat/-lon"
	case given["lat"] || given["lon"]:
		return "part from the command line, part from " + cfgPath
	case cfgPath != "":
		return "from " + cfgPath
	}
	return "from -lat/-lon"
}
