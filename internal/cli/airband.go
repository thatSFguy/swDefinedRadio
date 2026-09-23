package cli

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/airband"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Airband receives aircraft voice on the AM channels between 118 and
// 137 MHz, scanning a list of them and stopping on whoever is talking.
func Airband(args []string) {
	fs := flag.NewFlagSet("airband", flag.ExitOnError)

	var (
		chans   = fs.String("channels", "", "channels to scan, e.g. \"Tower:118.3,Ground:121.9\"; the saved list is used when empty")
		squelch = fs.Float64("squelch", 0.06, "carrier level a channel must reach to count as busy; 0 opens it")
		gain    = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm     = fs.Int("ppm", 0, "frequency correction in ppm")
		device  = fs.Int("device", 0, "RTL-SDR device index")
		tcpAddr = fs.String("rtltcp", "127.0.0.1:1234", "rtl_tcp address; one is started if nothing is listening")
		addr    = fs.String("http", config.DefaultHTTPAddr, "address for the player and JSON API")
		volume  = fs.Float64("volume", 1.0, "output gain multiplier")
		dir     = fs.String("data", "data/airband", "directory for the channel list")
		find    = fs.Bool("discover", true, "on the first run, sweep the band and add whatever is transmitting")
		speaker = fs.Bool("speaker", false, "also play through this machine's audio device")
	)
	fs.Parse(args)

	log.SetFlags(log.Ltime)

	var channels []airband.Channel
	if *chans != "" {
		var err error
		if channels, err = airband.ParseChannels(*chans); err != nil {
			log.Fatalf("-channels: %v", err)
		}
	}

	app, err := airband.New(airband.Config{
		Channels: channels, Gain: *gain, PPM: *ppm, Device: *device,
		Squelch: *squelch, Volume: *volume, Speaker: *speaker, Dir: *dir,
		Discover: *find,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Bind before touching the radio so a port clash fails immediately.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
	}
	defer ln.Close()

	src, stopServer, err := sdr.EnsureRTLTCP(ctx, *tcpAddr, app.Radio())
	if err != nil {
		log.Fatalf("radio: %v", err)
	}
	defer stopServer()
	defer src.Close()

	h, err := app.Handler(ctx)
	if err != nil {
		log.Fatalf("%v", err)
	}
	go web.ServeUntil(ctx, ln, h)
	go web.Announce(ctx, *addr)

	if err := app.Run(ctx, src); err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
}
