package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/adsb"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/airband"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/fm"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/scanner"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/tpms"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/uat"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/hub"
	"github.com/thatSFguy/swDefinedRadio/internal/radio"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Hub serves every receiver from one process, with a tab bar to
// choose between them.
//
// It is the whole radio in one place: pick a tab and that receiver gets
// the dongle. Because the process outlives any one receiver, what they
// have heard survives being switched away from — the aircraft table is
// still there when you come back to it, and the tire sensors go on
// accumulating whether or not anyone is looking.
//
// The receivers themselves are unchanged, and each still runs as its own
// command; this only decides which of them holds the radio.
func Hub(args []string) {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	log.SetFlags(log.Ltime)

	saved, cfgPath, err := config.Load()
	if err != nil {
		log.Printf("config: %v (continuing without it)", err)
	}

	var (
		addr    = fs.String("http", config.DefaultHTTPAddr, "address for the web UI")
		tcpAddr = fs.String("rtltcp", "127.0.0.1:1234", "address for the rtl_tcp server that holds the radio")
		device  = fs.Int("device", 0, "RTL-SDR device index")
		gain    = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm     = fs.Int("ppm", 0, "frequency correction in ppm")
		settle  = fs.Duration("settle", 200*time.Millisecond, "wait after retuning before trusting samples")
		start   = fs.String("start", "", "receiver to put on the air at startup; empty starts idle")

		lat = fs.Float64("lat", saved.Lat, "receiver latitude")
		lon = fs.Float64("lon", saved.Lon, "receiver longitude")
		ttl = fs.Duration("ttl", 60*time.Second, "forget aircraft unheard for this long")

		tpmsFreqs = fs.String("tpms-freq", "315M,433.92M", "frequencies for the tire sensor receiver")
		tpmsHop   = fs.Int("tpms-hop", 30, "seconds to dwell on each tire sensor frequency")
		tpmsDir   = fs.String("tpms-data", "data/tpms", "directory for the tire sensor log and table")
		tpmsMode  = fs.String("tpms-mode", "tcp", "how the tire sensor receiver reaches the radio: tcp (share it) or exclusive (take it)")

		fmFreq   = fs.String("fm-freq", "98.7M", "station the FM receiver starts on")
		airChans = fs.String("airband", "", "airband channels, e.g. \"Tower:118.3,Ground:121.9\"; the saved list is used when empty")
		airSq    = fs.Float64("airband-squelch", 0.06, "carrier level an airband channel must reach to count as busy")
		airDir   = fs.String("airband-data", "data/airband", "directory for the airband channel list")
		airFind  = fs.Bool("airband-discover", true, "on the first run, sweep the airband and add whatever is transmitting")
		scanFrom = fs.String("scan-start", "88M", "start of the spectrum sweep")
		scanTo   = fs.String("scan-stop", "1090M", "end of the spectrum sweep")

		tileURL = fs.String("tile-url", "https://tile.openstreetmap.org/{z}/{x}/{y}.png",
			"street map tiles; empty uses the built-in offline map only")
		alertsPath = fs.String("alerts", "alerts.json", "rules file for aircraft alerts")
		alertLog   = fs.String("alert-log", "data/alerts.jsonl", "append raised alerts here")
		alertCmd   = fs.String("alert-cmd", "", "shell command to run for each alert")
		noAlerts   = fs.Bool("no-alerts", false, "do not watch for anything")
		quiet      = fs.Bool("quiet", false, "suppress the periodic status lines")
	)
	fs.Parse(args)

	mode, err := tpmsRadioMode(*tpmsMode)
	if err != nil {
		log.Fatalf("-tpms-mode: %v", err)
	}

	// The whole process's lifetime. Everything that must outlive a single
	// receiver's turn on the radio hangs off this: the rtl_tcp server,
	// the alert watcher, the tables that go on ageing while their tab is
	// not being looked at.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind before touching the radio so a port clash fails immediately
	// with a clear message instead of leaving a hub running headless.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
	}
	defer ln.Close()

	// The two aircraft receivers keep separate trackers, so the position
	// has to be pushed to both: setting it on one tab and leaving the
	// other working out ranges from the old one would be wrong in a way
	// that looks right.
	var trackers []*track.Tracker
	moveTo := func(lat, lon float64) {
		for _, t := range trackers {
			t.SetReference(lat, lon)
		}
	}

	adsbApp, err := adsb.New(ctx, adsb.Config{
		Freq: 1_090_000_000, Gain: *gain, PPM: *ppm, Device: *device,
		Lat: *lat, Lon: *lon, PositionNote: positionNote(cfgPath),
		TTL: *ttl, Quiet: *quiet,
		TileURL: *tileURL, ConfigPath: cfgPath, AllowSetPosition: true,
		OnSetPosition: moveTo,
		NoAlerts:      *noAlerts, AlertsPath: *alertsPath, AlertLog: *alertLog, AlertCmd: *alertCmd,
	})
	if err != nil {
		log.Fatalf("adsb: %v", err)
	}

	uatApp, err := uat.New(ctx, uat.Config{
		Freq: 978_000_000, Gain: *gain, PPM: *ppm, Device: *device,
		Lat: *lat, Lon: *lon, TTL: *ttl, Quiet: *quiet,
		TileURL: *tileURL, ConfigPath: cfgPath, AllowSetPosition: true,
		OnSetPosition: moveTo,
		NoAlerts:      *noAlerts, AlertsPath: *alertsPath, AlertLog: *alertLog, AlertCmd: *alertCmd,
	})
	if err != nil {
		log.Fatalf("uat: %v", err)
	}
	trackers = []*track.Tracker{adsbApp.Tracker(), uatApp.Tracker()}

	tpmsApp, err := tpms.New(tpms.Config{
		Freqs: tpms.Split(*tpmsFreqs), HopSeconds: *tpmsHop,
		PPM: *ppm, Device: *device, Dir: *tpmsDir, Quiet: *quiet,
	})
	if err != nil {
		log.Fatalf("tpms: %v", err)
	}
	defer tpmsApp.Close()

	scanStart, err := scanner.ParseHz(*scanFrom)
	if err != nil {
		log.Fatalf("-scan-start: %v", err)
	}
	scanStop, err := scanner.ParseHz(*scanTo)
	if err != nil {
		log.Fatalf("-scan-stop: %v", err)
	}
	scanApp, err := scanner.New(scanner.Config{
		Start: scanStart, Stop: scanStop, SampleRate: 2_400_000, Bins: 1024,
		Gain: *gain, PPM: *ppm, Device: *device, Limits: scanner.DefaultLimits,
		Threshold: 10, Sep: 150e3, History: 120,
	})
	if err != nil {
		log.Fatalf("scanner: %v", err)
	}

	fmHz, err := fm.ParseHz(*fmFreq)
	if err != nil {
		log.Fatalf("-fm-freq: %v", err)
	}
	fmApp, err := fm.New(fm.Config{
		Freq: fmHz, Gain: *gain, PPM: *ppm, Device: *device,
		Volume: 1, Deemph: "us",
	})
	if err != nil {
		log.Fatalf("fm: %v", err)
	}

	var airChannels []airband.Channel
	if *airChans != "" {
		airChannels, err = airband.ParseChannels(*airChans)
		if err != nil {
			log.Fatalf("-airband: %v", err)
		}
	}
	airApp, err := airband.New(airband.Config{
		Channels: airChannels, Gain: *gain, PPM: *ppm, Device: *device,
		Squelch: *airSq, Volume: 1, Dir: *airDir, Discover: *airFind,
	})
	if err != nil {
		log.Fatalf("airband: %v", err)
	}

	broker := radio.New(ctx, radio.Options{
		Addr: *tcpAddr, Device: *device, Settle: *settle,
	})
	defer broker.Close()

	h, err := hub.New(ctx, broker,
		hub.Receivers(adsbApp, uatApp, tpmsApp, mode, scanApp, fmApp, airApp))
	if err != nil {
		log.Fatalf("hub: %v", err)
	}

	go web.ServeUntil(ctx, ln, h.Handler())
	log.Printf("→ %s", config.URL(*addr))

	if *start != "" {
		if err := h.Select(*start); err != nil {
			log.Printf("-start: %v", err)
		}
	} else {
		log.Print("nothing on the air yet — choose a receiver in the browser")
	}

	<-ctx.Done()
	log.Print("stopping")
	h.Stop()
}

func tpmsRadioMode(s string) (radio.Mode, error) {
	switch s {
	case "tcp":
		return radio.Address, nil
	case "exclusive":
		return radio.Exclusive, nil
	}
	return 0, fmt.Errorf("%q is not tcp or exclusive", s)
}

func positionNote(cfgPath string) string {
	if cfgPath != "" {
		return "from " + cfgPath
	}
	return "from -lat/-lon"
}
