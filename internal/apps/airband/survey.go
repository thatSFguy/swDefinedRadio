package airband

import (
	"fmt"
	"slices"
	"time"
)

// Finding channels by sweeping the spectrum does not work, and it is
// worth saying why, because it looks like it should.
//
// A sweep takes one look at nineteen megahertz. Airband channels are
// silent between transmissions, so in the few seconds a sweep lasts,
// perhaps one aircraft anywhere in the band is talking. Everything else
// it reports is a bump: a spur, an intermodulation product, a gain
// ripple, a pager two bands over. Filtering the obvious rubbish still
// leaves a list that is mostly rubbish, because the thing being measured
// — power, once — does not distinguish a voice channel from a lump.
//
// What does distinguish one is that traffic comes and goes. A carrier
// that is always present is a spur; one that is never present is noise;
// one that appears for four seconds and then vanishes is somebody
// talking. That needs watching a channel over time rather than glancing
// at it, which is exactly what the receiver does anyway — so a survey is
// the scanner run across the whole grid, keeping score.

// SurveyStat is what was observed on one channel.
type SurveyStat struct {
	Hz    uint32  `json:"hz"`
	Opens int     `json:"opens"`   // transmissions heard
	Secs  float64 `json:"seconds"` // how long they lasted in total
	Peak  float64 `json:"peak"`    // the strongest carrier seen
	Looks int     `json:"looks"`   // how many times this channel was visited

	// Constant counts visits where the carrier never went away for the
	// whole time the survey sat there. Speech stops; a spur does not.
	// This is what tells the receiver's own 4.8 MHz harmonics from an
	// aircraft, and a sweep measuring power once cannot do it at all.
	Constant int `json:"constant"`
}

// Grid is every channel in the band, which is what a survey walks.
func Grid() []uint32 {
	var out []uint32
	for hz := uint32(BandLowHz); hz <= BandHighHz; hz += ChannelHz {
		out = append(out, hz)
	}
	return out
}

// minTransmission is how long a squelch opening must last to count as
// somebody talking. Shorter than this is a click, a noise burst, or the
// squelch chattering on the edge of its threshold.
const minTransmission = 600 * time.Millisecond

// ResetSurvey throws away what was gathered and starts the scoring over.
func (a *App) ResetSurvey() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.survey = map[uint32]*SurveyStat{}
	a.surveyAt = time.Time{}
}

// StartSurvey begins walking the grid, keeping score. It resumes rather
// than restarts: evidence accumulates across however many times it is
// stopped to listen to something.
func (a *App) StartSurvey() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.surveying = true
	a.scanning = false
	// Keep whatever has been gathered. Stopping to listen to a candidate
	// and starting again is the ordinary way to use this, and throwing
	// the evidence away each time would punish exactly that.
	if a.survey == nil {
		a.survey = map[uint32]*SurveyStat{}
	}
	if a.surveyAt.IsZero() {
		a.surveyAt = time.Now()
	}
	// Carry on from where it was rather than starting at the bottom of
	// the band again, or the channels near 118 get surveyed repeatedly
	// and the ones near 137 never do.
	if a.freq < BandLowHz || a.freq > BandHighHz {
		_ = a.tuneLocked(BandLowHz)
	}
}

// StopSurvey ends it, leaving what was gathered.
func (a *App) StopSurvey() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.surveying = false
}

// SurveyProgress is how far round the band it has been, for a page that
// has to say something during the several minutes this takes.
type SurveyProgress struct {
	Running  bool    `json:"running"`
	Looked   int     `json:"looked"`   // channels visited at least once
	Total    int     `json:"total"`    // channels in the grid
	Passes   float64 `json:"passes"`   // times round, so far
	Found    int     `json:"found"`    // channels with a transmission on them
	Seconds  float64 `json:"seconds"`  // how long it has been running
	Position uint32  `json:"position"` // where it is now
}

func (a *App) surveyProgressLocked() SurveyProgress {
	grid := len(Grid())
	looked, found, visits := 0, 0, 0
	for _, s := range a.survey {
		if s.Looks > 0 {
			looked++
		}
		visits += s.Looks
		if s.Opens > 0 {
			found++
		}
	}
	p := SurveyProgress{
		Running: a.surveying, Looked: looked, Total: grid,
		Found: found, Position: a.freq,
	}
	if grid > 0 {
		p.Passes = float64(visits) / float64(grid)
	}
	if !a.surveyAt.IsZero() {
		p.Seconds = time.Since(a.surveyAt).Seconds()
	}
	return p
}

// SurveyResults are the channels something was actually heard on, best
// first — the ones worth listening to.
func (a *App) SurveyResults() []SurveyStat {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []SurveyStat
	for _, s := range a.survey {
		if s.Opens == 0 {
			continue
		}
		// A channel that is usually a constant carrier is a spur that
		// occasionally looked like it stopped, not a channel that is
		// occasionally busy.
		if s.Constant >= s.Opens {
			continue
		}
		out = append(out, *s)
	}
	// Most talked-on first: a channel with six transmissions on it is a
	// better bet than one with a single four-second burst.
	slices.SortFunc(out, func(x, y SurveyStat) int {
		if x.Opens != y.Opens {
			return y.Opens - x.Opens
		}
		return int(y.Secs*1000) - int(x.Secs*1000)
	})
	return out
}

// OfferSurveyed turns what was heard into candidates to audition.
func (a *App) OfferSurveyed() []Channel {
	results := a.SurveyResults()

	a.mu.Lock()
	defer a.mu.Unlock()

	var fresh []Channel
	for _, s := range results {
		known := slices.ContainsFunc(a.channels, func(c Channel) bool { return c.Hz == s.Hz })
		offered := slices.ContainsFunc(a.candidates, func(c Channel) bool { return c.Hz == s.Hz })
		if known || offered {
			continue
		}
		fresh = append(fresh, Channel{
			Name: fmt.Sprintf("%s (%d heard)", MHz(s.Hz), s.Opens),
			Hz:   s.Hz,
		})
	}
	a.candidates = append(a.candidates, fresh...)
	slices.SortFunc(a.candidates, func(x, y Channel) int { return int(x.Hz) - int(y.Hz) })
	return fresh
}

// surveyStep is the survey's half of the scan loop: score what was just
// heard, then move to the next channel on the grid.
func (a *App) surveyStep(now time.Time, open bool) {
	st := a.survey[a.freq]
	if st == nil {
		st = &SurveyStat{Hz: a.freq}
		a.survey[a.freq] = st
	}
	if lvl := a.am.Level(); lvl > st.Peak {
		st.Peak = lvl
	}

	held := now.Sub(a.since)

	if open {
		if held < surveyMaxHold {
			return // stay while there is something to hear
		}
		// Still going after that long: a carrier that does not stop is
		// not speech. Count it against the channel and move on, or the
		// survey ends here.
		st.Constant++
		st.Looks++
		a.advanceLocked()
		return
	}

	// A channel that just went quiet is scored, then left.
	if a.busy && held >= minTransmission && held < surveyMaxHold {
		st.Opens++
		st.Secs += held.Seconds()
	}
	if held < surveyDwell {
		return
	}

	st.Looks++
	a.advanceLocked()
}

// advanceLocked moves to the next channel on the grid.
func (a *App) advanceLocked() {
	grid := Grid()
	i := slices.Index(grid, a.freq)
	_ = a.tuneLocked(grid[(i+1)%len(grid)])
}

// surveyDwell is shorter than the scanning dwell: a survey is looking
// for whether anything ever happens here, over many passes, rather than
// trying not to miss the transmission in progress.
const surveyDwell = 90 * time.Millisecond

// surveyMaxHold is the longest a channel is sat on, however busy it
// seems. Without it a constant carrier — a spur, or noise above the
// threshold — holds the squelch open and the survey never moves again:
// the first run of this parked on 120.000 MHz, a harmonic of the
// receiver's own clock, and looked at eighty channels in two minutes.
//
// It is also the measurement. A transmission is seconds; something still
// going after this is not somebody talking.
const surveyMaxHold = 4 * time.Second
