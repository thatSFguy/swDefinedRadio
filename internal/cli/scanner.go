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

	"github.com/thatSFguy/swDefinedRadio/internal/apps/scanner"
	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// Scanner sweeps the tuner across a range of frequencies and
// serves a live spectrum, a waterfall and the signals it finds.
func Scanner(args []string) {
	fs := flag.NewFlagSet("scanner", flag.ExitOnError)
	log.SetFlags(log.Ltime)

	var (
		startF  = fs.String("start", "88M", "start of the sweep")
		stopF   = fs.String("stop", "1090M", "end of the sweep")
		rate    = fs.Uint("rate", 2_400_000, "sample rate in Hz")
		bins    = fs.Int("bins", 1024, "FFT size; bin width is rate/bins")
		dwell   = fs.Duration("dwell", 40*time.Millisecond, "integration time per tuning step")
		settle  = fs.Duration("settle", 180*time.Millisecond, "wait after each retune for stale samples to clear; too low and strong signals ghost one step away")
		crop    = fs.Float64("crop", 0.75, "fraction of each segment to keep, avoiding the tuner's band edges")
		offset  = fs.Float64("offset", 0, "shift the oscillator off each segment's centre, in Hz; with -crop below 0.5 this removes the DC blind spot (try -crop 0.4 -offset 600000)")
		gain    = fs.Float64("gain", -1, "tuner gain in dB, or -1 for automatic")
		ppm     = fs.Int("ppm", 0, "frequency correction in ppm")
		device  = fs.Int("device", 0, "RTL-SDR device index")
		direct  = fs.Int("direct", 0, "direct sampling: 0 off, 1 I-branch, 2 Q-branch — bypasses the tuner to reach shortwave, but needs hardware wired for it")
		upconv  = fs.String("upconvert", "0", "shift applied by an external upconverter, e.g. 125M; -start and -stop stay real frequencies")
		tcpAddr = fs.String("rtltcp", "127.0.0.1:1234", "rtl_tcp address; one is started if nothing is listening")
		addr    = fs.String("http", config.DefaultHTTPAddr, "address for the web UI and JSON API")
		thresh  = fs.Float64("threshold", 10, "dB above the noise floor to count as a signal")
		sep     = fs.Float64("sep", 150e3, "merge signals closer together than this many Hz; one FM station spans about 200 kHz")
		history = fs.Int("history", 120, "sweeps to keep for the waterfall")
		once    = fs.Bool("once", false, "run a single sweep, print the peaks and exit")
	)
	fs.Parse(args)

	limits := scanner.DefaultLimits

	up := 0.0
	if *upconv != "0" && *upconv != "" {
		v, err := scanner.ParseHz(*upconv)
		if err != nil {
			log.Fatalf("-upconvert: %v", err)
		}
		up = float64(v)
		// With a converter in front, what the tuner can reach maps to a
		// different set of real frequencies.
		lo, hi := int64(limits.Low)-int64(up), int64(limits.High)-int64(up)
		limits = scanner.Limits{Low: uint32(max(lo, 0)), High: uint32(max(hi, 0))}
		log.Printf("upconverter %s: %s to %s reachable",
			scanner.Hz(up), scanner.MHz(limits.Low), scanner.MHz(limits.High))
	}

	if *direct != 0 {
		// The tuner is out of circuit, so its limits no longer apply.
		// What is reachable instead is DC to half the 28.8 MHz reference.
		limits = scanner.Limits{Low: 0, High: 14_400_000}
		log.Printf("direct sampling mode %d: tuner bypassed, %s to %s reachable",
			*direct, scanner.MHz(limits.Low), scanner.MHz(limits.High))
	}

	start, err := scanner.ParseHz(*startF)
	if err != nil {
		log.Fatalf("-start: %v", err)
	}
	stop, err := scanner.ParseHz(*stopF)
	if err != nil {
		log.Fatalf("-stop: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app, err := scanner.New(scanner.Config{
		Start: start, Stop: stop, SampleRate: uint32(*rate), Bins: *bins,
		Dwell: *dwell, Settle: *settle, Crop: *crop, LOOffset: *offset,
		Gain: *gain, PPM: *ppm, Device: *device,
		DirectSampling: *direct, Upconvert: up, Limits: limits,
		Threshold: *thresh, Sep: *sep, History: *history,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	var ln net.Listener
	if !*once {
		// Bind before touching the radio so a port clash fails fast.
		ln, err = net.Listen("tcp", *addr)
		if err != nil {
			log.Fatalf("cannot listen on %s: %v\n\tanother receiver may already be running — try ./sdr status", *addr, err)
		}
		defer ln.Close()
	}

	src, stopServer, err := sdr.EnsureRTLTCP(ctx, *tcpAddr, app.Radio())
	if err != nil {
		log.Fatalf("radio: %v", err)
	}
	defer stopServer()
	defer src.Close()
	log.Printf("tuner: %s", src.Tuner)

	if *once {
		sw, err := app.Once(ctx, src)
		if err != nil {
			log.Fatalf("%v", err)
		}
		app.Report(sw)
		return
	}

	h, err := app.Handler()
	if err != nil {
		log.Fatalf("%v", err)
	}
	go web.ServeUntil(ctx, ln, h)
	go web.Announce(ctx, *addr)

	if err := app.Run(ctx, src); err != nil && ctx.Err() == nil {
		log.Fatalf("sweep: %v", err)
	}
}
