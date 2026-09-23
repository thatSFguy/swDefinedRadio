package airband

import (
	"context"
	"fmt"
	"log"
	"math"
	"slices"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/demod"
	"github.com/thatSFguy/swDefinedRadio/internal/scan"
	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// Finding channels by sweeping the band works, but only for a given
// value of works, and the three ways it goes wrong are all worth
// guarding against rather than discovering later with a receiver tuned
// somewhere useless.
const (
	// maxWidthHz is the widest a peak may be and still be a voice
	// channel. AM speech occupies about 8 kHz inside a 25 kHz channel;
	// anything far broader is a noise source, a pager, or the receiver
	// arguing with itself, and none of those are worth a channel entry.
	maxWidthHz = 20_000

	// minSNR is how far above the noise floor a peak must sit. The
	// scanner's own threshold is lower because it is describing the
	// band; this is deciding what to tune to, which deserves more.
	minSNR = 10

	// offGridHz is how far from the 25 kHz grid a peak may sit and still
	// be taken as that channel. A sweep resolves a few kHz at best, so
	// the strongest bin lands near the carrier rather than on it — but a
	// peak sitting halfway between two channels is either mismeasured or
	// one of the 8.33 kHz channels Europe uses, and neither can be
	// placed confidently. Missing one is the smaller harm.
	offGridHz = 10_000
)

// Snap moves a frequency onto the 25 kHz channel grid.
//
// A sweep reports the strongest bin, and bins are kilohertz wide, so a
// found frequency is near a channel rather than on it. Tuning to what
// was measured rather than to the channel it implies leaves the receiver
// a few kHz off, which on AM sounds thin and quiet rather than obviously
// wrong — the worst way for this to fail.
func Snap(hz float64) uint32 {
	return uint32(math.Round(hz/ChannelHz) * ChannelHz)
}

// plausible reports whether a peak is worth treating as a channel, and
// why not when it is not.
func plausible(p scan.Peak) (uint32, bool) {
	// The sweeper already recognises its own spurs — multiples of the
	// 4.8 MHz clock — and they are usually the strongest things in the
	// band, so taking peaks by strength alone finds those first.
	if p.Suspect != "" {
		return 0, false
	}
	if p.SNR < minSNR || p.WidthHz > maxWidthHz {
		return 0, false
	}
	hz := Snap(p.Freq)
	if math.Abs(p.Freq-float64(hz)) > offGridHz {
		return 0, false
	}
	if hz < BandLowHz || hz > BandHighHz {
		return 0, false
	}
	return hz, true
}

// Discover sweeps the airband and returns the channels it found, on the
// 25 kHz grid and in frequency order.
//
// It sweeps at the rate the receiver is already running at, so nothing
// has to be reconfigured — more tuning steps than the scanner would use,
// but each one is quick and the radio stays as it is.
func (a *App) Discover(ctx context.Context, src sdr.Source) ([]Channel, error) {
	cfg := scan.Config{
		Start:      BandLowHz,
		Stop:       BandHighHz,
		SampleRate: demod.AMInputRate,
		BinCount:   1024,
		Dwell:      30 * time.Millisecond,
		Settle:     120 * time.Millisecond,
		Crop:       0.75,
	}.Defaults()

	sw, err := scan.New(src, cfg).Sweep(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}

	// A generous number of peaks, since most will be thrown away.
	peaks := scan.FindPeaks(sw, minSNR, ChannelHz, 60)

	seen := map[uint32]bool{}
	var found []Channel
	for _, p := range peaks {
		hz, ok := plausible(p)
		if !ok || seen[hz] {
			continue
		}
		seen[hz] = true
		found = append(found, Channel{Name: MHz(hz), Hz: hz})
	}
	slices.SortFunc(found, func(x, y Channel) int { return int(x.Hz) - int(y.Hz) })
	return found, nil
}

// DiscoverInto sweeps and adds whatever it finds to the channel list,
// leaving the channels already there alone — including their names,
// which a sweep cannot know and a person has probably supplied.
//
// A sweep only hears what is transmitting while it runs, and this band
// is silent between transmissions, so one pass finds whoever happened to
// be talking. That makes it a good way to bootstrap a list and a poor
// way to be sure of one; it is worth running again.
func (a *App) DiscoverInto(ctx context.Context, src sdr.Source) ([]Channel, error) {
	found, err := a.Discover(ctx, src)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	have := slices.Clone(a.channels)
	a.mu.RUnlock()

	var added []Channel
	for _, c := range found {
		if slices.ContainsFunc(have, func(x Channel) bool { return x.Hz == c.Hz }) {
			continue
		}
		have = append(have, c)
		added = append(added, c)
	}
	if len(added) == 0 {
		return nil, nil
	}
	slices.SortFunc(have, func(x, y Channel) int { return int(x.Hz) - int(y.Hz) })
	if err := a.SetChannels(have); err != nil {
		return nil, err
	}
	for _, c := range added {
		log.Printf("found %s", c.Name)
	}
	return added, nil
}
