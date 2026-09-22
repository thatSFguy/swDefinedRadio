package cli

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/tpms"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Tpms logs tyre-pressure sensor transmissions heard on 315 and
// 433.92 MHz, groups the sensors into vehicles, and serves a live view.
func Tpms(args []string) {
	fs := flag.NewFlagSet("tpms", flag.ExitOnError)
	var (
		freqs  = fs.String("freq", "315M,433.92M", "comma-separated frequencies to listen on")
		hop    = fs.Int("hop", 30, "seconds to dwell on each frequency when more than one is given")
		gain   = fs.String("gain", "", "tuner gain in dB (default: automatic)")
		ppm    = fs.Int("ppm", 0, "frequency correction in ppm")
		device = fs.Int("device", 0, "RTL-SDR device index")
		dir    = fs.String("data", "data/tpms", "directory for the reading log and sensor table")
		addr   = fs.String("http", config.DefaultHTTPAddr, "address for the web UI and JSON API")
		other  = fs.Bool("other", false, "also print non-TPMS devices rtl_433 decodes")
		quiet  = fs.Bool("quiet", false, "do not print each reading as it arrives")
	)
	fs.Parse(args)

	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := tpms.New(tpms.Config{
		Freqs: tpms.Split(*freqs), HopSeconds: *hop,
		Gain: *gain, PPM: *ppm, Device: *device,
		Dir: *dir, Other: *other, Quiet: *quiet,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer app.Close()

	// Bind before touching the radio so a port clash fails immediately
	// with a clear message instead of leaving a logger running headless.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
	}
	defer ln.Close()

	h, err := app.Handler()
	if err != nil {
		log.Fatalf("%v", err)
	}
	go web.ServeUntil(ctx, ln, h)
	go app.Autosave(ctx)
	go web.Announce(ctx, *addr)

	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("rtl_433 stopped: %v", err)
	}
	if err := app.Close(); err != nil {
		log.Printf("save: %v", err)
	}
	log.Print("stopped")
}
