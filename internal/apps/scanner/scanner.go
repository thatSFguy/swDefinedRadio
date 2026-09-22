// Package scanner sweeps the tuner across a range of frequencies and
// reports what is transmitting: a spectrum, a waterfall of recent passes,
// and the peaks that stand above the noise floor.
//
// It is the one receiver that retunes constantly, so it needs a source it
// can move without restarting — in practice an rtl_tcp connection. The
// settings can be changed while it runs, which cancels the pass in
// progress rather than waiting for a slow one to finish.
package scanner

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/scan"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// Config is everything the sweeper needs to know.
type Config struct {
	Start, Stop uint32
	SampleRate  uint32
	Bins        int
	Dwell       time.Duration
	Settle      time.Duration
	Crop        float64
	LOOffset    float64

	Gain   float64
	PPM    int
	Device int

	// DirectSampling takes the tuner out of circuit to reach shortwave,
	// and Upconvert is the shift an external converter applies. Either
	// changes what the radio can reach, which is why Limits is not a
	// constant.
	DirectSampling int
	Upconvert      float64
	Limits         Limits

	Threshold float64 // dB above the noise floor to count as a signal
	Sep       float64 // merge signals closer together than this
	History   int     // sweeps to keep for the waterfall
}

// App is a configured spectrum sweeper.
type App struct {
	cfg   Config
	ctrl  *control
	store *store
}

// New builds the sweeper, rejecting settings the radio cannot honour
// before anything touches the hardware.
func New(cfg Config) (*App, error) {
	base := scan.Config{
		Start: cfg.Start, Stop: cfg.Stop,
		SampleRate: cfg.SampleRate, BinCount: cfg.Bins,
		Dwell: cfg.Dwell, Settle: cfg.Settle,
		Crop: cfg.Crop, LOOffset: cfg.LOOffset, Upconvert: cfg.Upconvert,
	}
	ctrl := &control{
		cfg:    base.Defaults(),
		thresh: cfg.Threshold,
		sep:    cfg.Sep,
		limits: cfg.Limits,
	}
	if err := ctrl.validate(ctrl.cfg); err != nil {
		return nil, err
	}
	return &App{cfg: cfg, ctrl: ctrl, store: newStore(cfg.History)}, nil
}

// Radio is the tuner state the sweep starts from. It moves constantly
// after that, which is the whole point of this receiver.
func (a *App) Radio() sdr.Config {
	cfg := sdr.Config{
		CenterFreq:     a.cfg.Start + uint32(a.cfg.Upconvert),
		SampleRate:     a.cfg.SampleRate,
		Gain:           sdr.AutoGain,
		FreqCorrection: a.cfg.PPM,
		DeviceIndex:    a.cfg.Device,
		DirectSampling: a.cfg.DirectSampling,
	}
	if a.cfg.Gain >= 0 {
		cfg.Gain = int(a.cfg.Gain * 10)
	}
	return cfg
}

// Handler builds the web UI and JSON API.
func (a *App) Handler() (http.Handler, error) { return handler(a.store, a.ctrl) }

// Describe says what the sweep is about to do, which is worth saying
// because a wide span at a long dwell can take minutes per pass.
func (a *App) Describe() string { return describe(a.ctrl.cfg) }

// DropHistory throws away the waterfall.
//
// A sweep row is one number per bin, and a wide sweep has hundreds of
// thousands of bins, so a hundred-odd rows is a few hundred megabytes
// even after thinning. That is worth holding while someone is watching it
// and not worth holding for the rest of the afternoon because they once
// were. The latest sweep stays, so the tab has a picture to show the
// moment it is chosen again.
func (a *App) DropHistory() { a.store.dropHistory() }

// Run sweeps continuously until ctx is cancelled.
func (a *App) Run(ctx context.Context, src sdr.Source) error {
	run(ctx, src, a.store, a.ctrl)
	return ctx.Err()
}

// Once runs a single pass and returns it, for the command line's -once.
func (a *App) Once(ctx context.Context, src sdr.Source) (*scan.Sweep, error) {
	sweeper := scan.New(src, a.ctrl.cfg)
	log.Print(describe(a.ctrl.cfg))
	sw, err := sweeper.Sweep(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	return sw, nil
}

// Report prints the peaks of a single sweep.
func (a *App) Report(sw *scan.Sweep) { report(sw, a.cfg.Threshold, a.cfg.Sep) }

// ParseHz accepts plain hertz or a k/M/G suffix, so -start 88M reads the
// way a frequency is normally written.
func ParseHz(s string) (uint32, error) { return parseHz(s) }

// MHz and Hz render a frequency the way the logs and errors do.
func MHz(v uint32) string { return mhz(v) }
func Hz(v float64) string { return hz(v) }
