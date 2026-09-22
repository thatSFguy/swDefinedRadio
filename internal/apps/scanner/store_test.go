package scanner

import (
	"math"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/scan"
)

func wideSweep(bins int) *scan.Sweep {
	p := make(scan.Power, bins)
	for i := range p {
		p[i] = -100
	}
	// One narrow carrier, the thing a waterfall exists to show.
	p[bins/3] = -20
	return &scan.Sweep{
		At:    time.Now(),
		Start: 88_000_000,
		Stop:  1_090_000_000,
		BinHz: float64(1_090_000_000-88_000_000) / float64(bins),
		Power: p,
	}
}

// A full-resolution sweep is far larger than any waterfall can draw, and
// the ring holds a hundred-odd of them. Rows have to go in thinned or the
// scanner sits on hundreds of megabytes.
func TestHistoryRowsAreThinned(t *testing.T) {
	st := newStore(120)
	sw := wideSweep(427_000)
	st.add(sw, nil)

	rows := st.waterfall()
	if len(rows) != 1 {
		t.Fatalf("waterfall has %d rows, want 1", len(rows))
	}
	if got := len(rows[0].Power); got != historyWidth {
		t.Errorf("stored row has %d points, want %d", got, historyWidth)
	}

	// The latest sweep is what the spectrum view and the peak finder use,
	// so it must not have been thinned along with it.
	latest, _, _ := st.snapshot()
	if len(latest.Power) != len(sw.Power) {
		t.Errorf("latest sweep has %d bins, want the full %d",
			len(latest.Power), len(sw.Power))
	}
}

// Thinning keeps the strongest bin per group, so a narrow carrier must
// survive it — averaging it into the noise is exactly the failure a
// scanner cannot have.
func TestThinningKeepsNarrowCarriers(t *testing.T) {
	row := thinned(wideSweep(427_000))
	best := math.Inf(-1)
	for _, v := range row.Power {
		if !math.IsNaN(v) && v > best {
			best = v
		}
	}
	if best != -20 {
		t.Errorf("strongest point is %.0f dB, want -20 — the carrier was lost", best)
	}
}

// The row still has to describe the frequencies it covers: the span is
// unchanged, so a wider bin has to be reported as wider.
func TestThinnedRowReportsItsOwnBinWidth(t *testing.T) {
	sw := wideSweep(427_000)
	row := thinned(sw)

	span := float64(sw.Stop - sw.Start)
	if want := span / float64(len(row.Power)); math.Abs(row.BinHz-want) > 1e-6 {
		t.Errorf("BinHz = %.3f, want %.3f", row.BinHz, want)
	}
	if covered := row.BinHz * float64(len(row.Power)); math.Abs(covered-span) > 1 {
		t.Errorf("row covers %.0f Hz, want the sweep's %.0f Hz", covered, span)
	}
}

// A sweep narrow enough to draw as it is passes through untouched.
func TestNarrowSweepIsNotThinned(t *testing.T) {
	sw := wideSweep(512)
	if row := thinned(sw); row != sw {
		t.Error("a sweep smaller than the stored width was copied needlessly")
	}
}

// The ring is bounded: an afternoon of sweeping must not grow without end.
func TestHistoryRingIsBounded(t *testing.T) {
	st := newStore(10)
	for range 50 {
		st.add(wideSweep(4096), nil)
	}
	if got := len(st.waterfall()); got != 10 {
		t.Errorf("waterfall holds %d rows, want the 10 it was limited to", got)
	}
}
