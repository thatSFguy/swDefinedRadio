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

	// Hits counts samples seen above this channel's own threshold.
	//
	// Catching a whole transmission needs the survey to be on the
	// channel when one happens, and it is on each of 761 channels for a
	// tenth of a second every two and a half minutes. A busy centre
	// frequency carries traffic perhaps seven percent of the time, so
	// whole transmissions turn up roughly never. A single sample above
	// the channel's own noise is far more likely and says the same
	// thing: something is transmitting here.
	Hits int `json:"hits"`

	// Floor is the quietest this channel has ever been, which is its own
	// noise floor. It has to be measured per channel: across this band
	// the floor varies by a factor of two, so one threshold for all of
	// them is either deaf at the quiet end or, at the noisy end, open on
	// nothing at all.
	Floor float64 `json:"floor"`
}

// openRatio is how far above a channel's own noise floor the carrier
// must sit to count as somebody transmitting. A real signal is several
// times the floor; noise wanders around it.
const openRatio = 1.8

// minHits is how many separate moments above a channel's own noise are
// needed before it is worth listening to. One is a stray sample; two
// across different visits is a pattern.
const minHits = 2

// Threshold is the squelch this channel's own measurements imply.
func (s SurveyStat) Threshold() float64 { return s.Floor * openRatio }

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
// A survey arrives in the middle of a transmission and only sees what is
// left of it, so this is lower than a whole one would be. Below about
// this, though, it is a click of static or the threshold being grazed.
const minTransmission = 350 * time.Millisecond

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
		// Either a whole transmission, or enough separate moments above
		// this channel's own noise to not be a single stray sample.
		if s.Opens == 0 && s.Hits < minHits {
			continue
		}
		// A channel that is usually a constant carrier is a spur that
		// occasionally looked like it stopped, not a channel that is
		// occasionally busy.
		//
		// Weighed against all the evidence, not just whole
		// transmissions: written as Constant >= Opens it threw away
		// every channel found by its excursions, because nought is not
		// less than nought.
		if s.Constant > 0 && s.Constant >= s.Opens+s.Hits {
			continue
		}
		out = append(out, *s)
	}
	// Most evidence first, so the best bets are at the top of the list
	// somebody is going to work through by ear.
	slices.SortFunc(out, func(x, y SurveyStat) int {
		if a, b := x.Opens+x.Hits, y.Opens+y.Hits; a != b {
			return b - a
		}
		return int(y.Secs*1000) - int(x.Secs*1000)
	})
	return out
}

// SurveyFloors is every channel that has been measured, quietest first.
//
// Worth being able to see: the threshold each channel is being judged
// against is derived from its own noise, and if nothing is ever heard
// the first question is whether those numbers are sensible.
func (a *App) SurveyFloors() []SurveyStat {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]SurveyStat, 0, len(a.survey))
	for _, s := range a.survey {
		if s.Looks > 0 {
			out = append(out, *s)
		}
	}
	slices.SortFunc(out, func(x, y SurveyStat) int {
		switch {
		case x.Floor < y.Floor:
			return -1
		case x.Floor > y.Floor:
			return 1
		}
		return 0
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
		what := fmt.Sprintf("%d hits", s.Hits)
		if s.Opens > 0 {
			what = fmt.Sprintf("%d heard", s.Opens)
		}
		fresh = append(fresh, Channel{
			Name: fmt.Sprintf("%s (%s)", MHz(s.Hz), what),
			Hz:   s.Hz,
			// Keep the threshold this channel was measured at, so it
			// arrives already set for its own noise rather than the
			// band's average.
			Squelch: s.Threshold(),
		})
	}
	a.candidates = append(a.candidates, fresh...)
	slices.SortFunc(a.candidates, func(x, y Channel) int { return int(x.Hz) - int(y.Hz) })
	return fresh
}

// surveyOpen judges a channel against its own measured floor instead of
// the receiver's fixed threshold.
//
// Until a channel has been visited a couple of times its floor is not
// known, so nothing is scored on it — which is why a survey wants more
// than one pass before its answers mean much.
func (a *App) surveyOpen(st *SurveyStat) bool {
	if st.Looks < 1 || st.Floor <= 0 {
		return false
	}
	return a.am.Level() >= st.Threshold()
}

// surveyStep is the survey's half of the scan loop: score what was just
// heard, then move to the next channel on the grid.
//
// It decides for itself whether the channel is busy, against that
// channel's own noise, and keeps its own flag for it. Sharing the
// receiver's — which is a fixed threshold for the whole band — meant
// deciding to stay on one test and scoring on another.
func (a *App) surveyStep(now time.Time) {
	st := a.survey[a.freq]
	if st == nil {
		st = &SurveyStat{Hz: a.freq}
		a.survey[a.freq] = st
	}
	lvl := a.am.Level()
	if lvl > st.Peak {
		st.Peak = lvl
	}
	// The floor falls to whatever the quietest reading has been. A
	// channel with traffic on it is still quiet between transmissions,
	// so this converges on the noise rather than on the signal.
	if st.Floor == 0 || lvl < st.Floor {
		st.Floor = lvl
	}

	open := a.surveyOpen(st)
	if open {
		st.Hits++
	}
	held := now.Sub(a.since)

	// Coming onto a busy channel starts the clock, so what gets measured
	// is how long it stays busy rather than how long since the tuner
	// moved.
	if open && !a.busy {
		a.since = now
		a.busy = true
		held = 0
	}

	if open {
		if held < surveyMaxHold {
			return // stay while there is something to hear
		}
		// Still going after that long: a carrier that does not stop is
		// not speech. Count it against the channel and move on, or the
		// survey ends here.
		st.Constant++
		st.Looks++
		a.busy = false
		a.advanceLocked()
		return
	}

	// A channel that just went quiet is scored, then left.
	if a.busy {
		if held >= minTransmission && held < surveyMaxHold {
			st.Opens++
			st.Secs += held.Seconds()
			a.recordLocked(held)
		}
		a.busy = false
		a.since = now
		return // give the channel a moment in case there is more
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
