package scanner

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/scan"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// TunerLowHz and TunerHighHz bound what an R820T can reach. Requests
// outside this are refused rather than silently producing noise.
const (
	TunerLowHz  = 24_000_000
	TunerHighHz = 1_766_000_000
	MinSpanHz   = 200_000
)

// Limits are the frequencies the radio can actually reach. They are not
// constants because an upconverter in front of the tuner, or direct
// sampling taking the tuner out of circuit, both move them.
type Limits struct{ Low, High uint32 }

// DefaultLimits are an R820T's, the usual tuner.
var DefaultLimits = Limits{Low: TunerLowHz, High: TunerHighHz}

// control holds the sweep settings, which the web UI can change while a
// sweep is running. A version counter lets the sweep loop notice a
// change; cancelling the in-flight sweep makes the change take effect
// without waiting for a slow pass to finish.
type control struct {
	mu      sync.RWMutex
	cfg     scan.Config
	thresh  float64
	sep     float64
	version int
	cancel  context.CancelFunc

	// limits are what the radio can reach, which an upconverter or
	// direct sampling changes. They are held here rather than in a
	// package variable so that two sweepers in one process — a hub
	// serving this receiver alongside others — cannot tread on each
	// other's idea of what is tunable.
	limits Limits
}

func (c *control) snapshot() (scan.Config, float64, float64, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg, c.thresh, c.sep, c.version
}

// apply validates and installs new settings, then interrupts the sweep
// in progress so the change is visible immediately.
func (c *control) apply(cfg scan.Config, thresh, sep float64) error {
	cfg = cfg.Defaults()
	if err := c.validate(cfg); err != nil {
		return err
	}
	c.mu.Lock()
	c.cfg, c.thresh, c.sep = cfg, thresh, sep
	c.version++
	cancel := c.cancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

func (c *control) setCancel(f context.CancelFunc) {
	c.mu.Lock()
	c.cancel = f
	c.mu.Unlock()
}

// tunerLimits is what the radio can currently reach.
func (ctl *control) tunerLimits() Limits {
	ctl.mu.RLock()
	defer ctl.mu.RUnlock()
	return ctl.limits
}

// validate rejects settings the radio cannot honour.
func (ctl *control) validate(c scan.Config) error {
	return ctl.tunerLimits().validate(c)
}

// validate rejects settings these limits cannot honour.
func (lim Limits) validate(c scan.Config) error {
	tunerLow, tunerHigh := lim.Low, lim.High
	switch {
	case c.Stop <= c.Start:
		return fmt.Errorf("stop (%s) must be above start (%s)", mhz(c.Stop), mhz(c.Start))
	case c.Stop-c.Start < MinSpanHz:
		return fmt.Errorf("span must be at least %s", hz(MinSpanHz))
	case c.Start < tunerLow:
		return fmt.Errorf("start %s is below the %s floor", mhz(c.Start), mhz(tunerLow))
	case c.Stop > tunerHigh:
		return fmt.Errorf("stop %s is above the %s ceiling", mhz(c.Stop), mhz(tunerHigh))
	}
	return nil
}

// EstimateSweep is how long one pass should take: every step pays the
// settle wait plus the dwell.
func EstimateSweep(c scan.Config) time.Duration {
	return time.Duration(c.Steps()) * (c.Settle + c.Dwell)
}

func describe(c scan.Config) string {
	return fmt.Sprintf("sweeping %s to %s in %d steps, %d bins of %s, about %s per pass",
		mhz(c.Start), mhz(c.Stop), c.Steps(), c.Bins(), hz(c.BinHz()),
		EstimateSweep(c).Round(time.Second))
}

// run sweeps continuously, rebuilding the sweeper whenever the settings
// change, until the context is cancelled.
func run(ctx context.Context, src sdr.Source, store *store, ctrl *control) {
	var (
		sweeper *scan.Sweeper
		built   = -1
	)
	for ctx.Err() == nil {
		cfg, threshold, minSep, version := ctrl.snapshot()
		if version != built {
			sweeper = scan.New(src, cfg)
			// Old rows cover a different span and different bins, so the
			// waterfall cannot be carried across a change.
			store.reset(cfg)
			built = version
			log.Print(describe(cfg))
		}

		sctx, cancel := context.WithCancel(ctx)
		ctrl.setCancel(cancel)
		sw, err := sweeper.Sweep(sctx)
		cancel()
		ctrl.setCancel(nil)

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A cancelled sweep usually means the settings changed; loop
			// round and pick them up rather than reporting an error.
			if _, _, _, v := ctrl.snapshot(); v != built {
				continue
			}
			log.Printf("sweep: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		peaks := scan.FindPeaks(sw, threshold, minSep, 40)
		store.add(sw, peaks)
		log.Printf("sweep in %.1fs — floor %.0f dB, %d signals", sw.Took, scan.NoiseFloor(sw.Power), len(peaks))
	}
}

// report prints the peaks of a single sweep, for -once.
func report(sw *scan.Sweep, threshold, minSep float64) {
	peaks := scan.FindPeaks(sw, threshold, minSep, 0)
	fmt.Printf("\nnoise floor %.1f dBFS, %d signals at least %.0f dB above it\n\n",
		scan.NoiseFloor(sw.Power), len(peaks), threshold)
	if len(peaks) == 0 {
		fmt.Println("  nothing found — try a lower -threshold, a longer -dwell, or a better antenna")
		return
	}
	fmt.Printf("  %-14s %8s %7s %10s  %s\n", "FREQUENCY", "POWER", "SNR", "WIDTH", "BAND")
	spurs := 0
	for _, p := range peaks {
		mark := "  "
		if p.Suspect != "" {
			mark = " *"
			spurs++
		}
		fmt.Printf("%s %-14s %7.1f dB %6.1f %10s  %s\n",
			mark, mhz(uint32(p.Freq)), p.Power, p.SNR, hz(p.WidthHz), p.Band)
	}
	if spurs > 0 {
		fmt.Printf("\n  * %d of these fall on exact multiples of 4.8 MHz and are almost\n"+
			"    certainly the receiver's own crystal, not transmitters.\n", spurs)
	}
}

// historyWidth is how many points of each sweep the waterfall keeps.
//
// A wide sweep runs to hundreds of thousands of bins — 88 MHz to 1090 MHz
// at the default settings is about 427,000, or 3.4 MB a row — and the
// waterfall is only ever *served* decimated, to something on the order of
// the width of a screen. Keeping full-resolution rows would spend a few
// hundred megabytes to show a picture a couple of thousand points wide.
//
// The figure is well above any real display width, so a client asking for
// more points than this gets what is stored rather than what it asked
// for, which at these widths is not a difference anyone can see. The
// latest sweep is kept whole, so the spectrum view and the peak finder
// are unaffected.
const historyWidth = 2048

// store keeps the latest sweep plus a ring of recent ones for the
// waterfall. It is safe for concurrent use.
type store struct {
	mu      sync.RWMutex
	latest  *scan.Sweep
	peaks   []scan.Peak
	history []*scan.Sweep
	limit   int
	count   int
}

func newStore(limit int) *store {
	if limit < 1 {
		limit = 1
	}
	return &store{limit: limit}
}

func (s *store) add(sw *scan.Sweep, peaks []scan.Peak) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest, s.peaks = sw, peaks
	s.history = append(s.history, thinned(sw))
	if len(s.history) > s.limit {
		s.history = s.history[len(s.history)-s.limit:]
	}
	s.count++
}

// thinned reduces a sweep to what the waterfall needs. The span stays the
// same and BinHz is widened to match, so the row still describes the
// frequencies it covers rather than silently misreporting them.
func thinned(sw *scan.Sweep) *scan.Sweep {
	if sw == nil || len(sw.Power) <= historyWidth {
		return sw
	}
	row := *sw
	row.Power = scan.Decimate(sw.Power, historyWidth)
	row.BinHz = float64(sw.Stop-sw.Start) / float64(len(row.Power))
	return &row
}

// dropHistory clears the waterfall but keeps the latest sweep.
func (s *store) dropHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = nil
}

// reset clears everything after a settings change, since the stored
// sweeps describe a span that is no longer being measured.
func (s *store) reset(scan.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest, s.peaks, s.history, s.count = nil, nil, nil, 0
}

func (s *store) snapshot() (*scan.Sweep, []scan.Peak, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest, s.peaks, s.count
}

func (s *store) waterfall() []*scan.Sweep {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*scan.Sweep, len(s.history))
	copy(out, s.history)
	return out
}

// parseHz accepts plain hertz or a k/M/G suffix, so -start 88M reads the
// way a frequency is normally written.
func parseHz(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	mult := 1.0
	switch last := s[len(s)-1]; last {
	case 'k', 'K':
		mult, s = 1e3, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1e6, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1e9, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a frequency", s)
	}
	v *= mult
	if v <= 0 || v > 4e9 {
		return 0, fmt.Errorf("%.0f Hz is out of range", v)
	}
	return uint32(v), nil
}

func mhz(v uint32) string { return fmt.Sprintf("%.3f MHz", float64(v)/1e6) }

func hz(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.2f MHz", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.2f kHz", v/1e3)
	}
	return fmt.Sprintf("%.0f Hz", v)
}
