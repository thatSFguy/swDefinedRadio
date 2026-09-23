package cli

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/fm"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Fm receives broadcast FM and serves it as live audio a browser
// can play, with a tuning dial and a signal meter.
func Fm(args []string) {
	fs := flag.NewFlagSet("fm", flag.ExitOnError)
	log.SetFlags(log.Ltime)

	var (
		freqF   = fs.String("freq", "98.7M", "station to tune")
		gain    = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm     = fs.Int("ppm", 0, "frequency correction in ppm")
		device  = fs.Int("device", 0, "RTL-SDR device index")
		tcpAddr = fs.String("rtltcp", "127.0.0.1:1234", "rtl_tcp address; one is started if nothing is listening")
		addr    = fs.String("http", config.DefaultHTTPAddr, "address for the player and JSON API")
		volume  = fs.Float64("volume", 1.0, "output gain multiplier")
		region  = fs.String("deemph", "us", "de-emphasis: us (75us) or eu (50us)")
		speaker = fs.Bool("speaker", false, "also play through this machine's audio device")
		dir     = fs.String("data", "data/fm", "directory for recordings")
	)
	fs.Parse(args)

	freq, err := fm.ParseHz(*freqF)
	if err != nil {
		log.Fatalf("-freq: %v", err)
	}

	app, err := fm.New(fm.Config{
		Freq: freq, Gain: *gain, PPM: *ppm, Device: *device,
		Volume: *volume, Deemph: *region, Speaker: *speaker, Dir: *dir,
	})
	if err != nil {
		log.Fatalf("-freq: %v", err)
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
	go web.AnnounceWith(ctx, *addr, func() string {
		f, _ := app.Station()
		return "listening to " + fm.MHz(f)
	})

	if err := app.Run(ctx, src); err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
}
