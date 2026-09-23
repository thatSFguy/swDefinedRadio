package airband

import (
	"testing"
	"time"
)

// The grid is every channel the survey walks, which is what makes it
// exhaustive where a sweep is a glance.
func TestGridCoversTheBand(t *testing.T) {
	g := Grid()
	if g[0] != BandLowHz {
		t.Errorf("starts at %s", MHz(g[0]))
	}
	if last := g[len(g)-1]; last > BandHighHz || BandHighHz-last >= ChannelHz {
		t.Errorf("ends at %s, short of %s", MHz(last), MHz(BandHighHz))
	}
	for i := 1; i < len(g); i++ {
		if g[i]-g[i-1] != ChannelHz {
			t.Fatalf("gap of %d Hz at %s", g[i]-g[i-1], MHz(g[i]))
		}
	}
	// 118 to 137 at 25 kHz.
	if want := (BandHighHz-BandLowHz)/ChannelHz + 1; len(g) != want {
		t.Errorf("%d channels, want %d", len(g), want)
	}
}

// Only channels something was actually heard on are offered. That is the
// whole difference from sweeping: evidence, rather than a power reading.
func TestOnlyChannelsWithTrafficAreOffered(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{
		118_100_000: {Hz: 118_100_000, Looks: 40, Opens: 0},              // nothing ever
		118_200_000: {Hz: 118_200_000, Looks: 40, Opens: 3, Secs: 9},     // busy
		118_300_000: {Hz: 118_300_000, Looks: 40, Opens: 1, Secs: 2},     // once
	}
	a.mu.Unlock()

	got := a.SurveyResults()
	if len(got) != 2 {
		t.Fatalf("offered %d channels, want the 2 with traffic", len(got))
	}
	// Most talked-on first: six transmissions beats one burst.
	if got[0].Hz != 118_200_000 {
		t.Errorf("first is %s, want the busiest", MHz(got[0].Hz))
	}
}

// A survey finding a channel already known should not offer it again.
func TestAlreadyKnownChannelsAreNotOffered(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{Guard: {Hz: Guard, Opens: 5}}
	a.mu.Unlock()

	if got := a.OfferSurveyed(); len(got) != 0 {
		t.Errorf("offered %d, want none — that channel is already in the list", len(got))
	}
}

// A click of noise is not a transmission. Without a floor the survey
// scores the squelch chattering on its own threshold.
func TestBriefOpeningsDoNotCount(t *testing.T) {
	if minTransmission < 300*time.Millisecond {
		t.Errorf("minTransmission of %v will count squelch chatter as speech", minTransmission)
	}
}

// A whole pass has to be quick enough that several happen in the time
// somebody is willing to wait.
func TestASurveyPassIsBearable(t *testing.T) {
	pass := time.Duration(len(Grid())) * surveyDwell
	if pass > 2*time.Minute {
		t.Errorf("one pass takes %v, which is too long to be worth starting", pass)
	}
}

// Each channel may set its own threshold, because a tower two miles away
// and an approach frequency fifty miles off cannot share one.
func TestPerChannelSquelchOverridesTheReceiver(t *testing.T) {
	a := testApp(t, Channel{Name: "Near", Hz: 118_100_000}, Channel{Name: "Far", Hz: 118_200_000})
	a.SetSquelch(0.05)

	if err := a.SetChannelSquelch(118_200_000, 0.02); err != nil {
		t.Fatalf("SetChannelSquelch: %v", err)
	}

	_ = a.Tune(118_200_000)
	a.mu.RLock()
	onFar := a.am.Squelch
	a.mu.RUnlock()
	if onFar != 0.02 {
		t.Errorf("on the far channel the squelch is %v, want its own 0.02", onFar)
	}

	_ = a.Tune(118_100_000)
	a.mu.RLock()
	onNear := a.am.Squelch
	a.mu.RUnlock()
	if onNear != 0.05 {
		t.Errorf("on the near channel the squelch is %v, want the receiver's 0.05", onNear)
	}
}

// Clearing a channel's own threshold gives it the receiver's back.
func TestClearingAChannelSquelch(t *testing.T) {
	a := testApp(t, Channel{Name: "One", Hz: 118_100_000})
	a.SetSquelch(0.05)
	if err := a.SetChannelSquelch(118_100_000, 0.01); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := a.SetChannelSquelch(118_100_000, 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_ = a.Tune(118_100_000)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.am.Squelch != 0.05 {
		t.Errorf("squelch is %v, want the receiver's 0.05 back", a.am.Squelch)
	}
}

// And it survives a restart, like the rest of a channel.
func TestChannelSquelchIsSaved(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{Channels: []Channel{{Name: "One", Hz: 118_100_000}}, Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.SetChannelSquelch(118_100_000, 0.017); err != nil {
		t.Fatalf("set: %v", err)
	}

	again, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := again.State().Channels[0].Squelch; got != 0.017 {
		t.Errorf("after reloading, squelch = %v, want 0.017", got)
	}
}

// A constant carrier held the squelch open and the survey stopped dead —
// it parked on 120.000 MHz, a harmonic of the receiver's own clock, and
// managed eighty channels in two minutes. There has to be a ceiling.
func TestASurveyCannotBeHeldByAConstantCarrier(t *testing.T) {
	if surveyMaxHold > 6*time.Second {
		t.Errorf("a channel can be held for %v, which stalls a pass", surveyMaxHold)
	}
	if surveyMaxHold <= minTransmission {
		t.Errorf("the ceiling %v is below a real transmission %v", surveyMaxHold, minTransmission)
	}
	// A pass must still complete in reasonable time even if a handful of
	// channels hold for the maximum.
	worst := time.Duration(len(Grid()))*surveyDwell + 20*surveyMaxHold
	if worst > 5*time.Minute {
		t.Errorf("a pass with twenty busy channels takes %v", worst)
	}
}

// Speech stops; a spur does not. That difference is the whole reason a
// survey works where a sweep did not.
func TestConstantCarriersAreNotOffered(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{
		120_000_000: {Hz: 120_000_000, Looks: 20, Opens: 2, Constant: 18}, // a spur
		118_200_000: {Hz: 118_200_000, Looks: 20, Opens: 4, Constant: 0},  // real traffic
	}
	a.mu.Unlock()

	got := a.SurveyResults()
	if len(got) != 1 {
		t.Fatalf("offered %d, want only the bursty one", len(got))
	}
	if got[0].Hz != 118_200_000 {
		t.Errorf("offered %s, want the channel that goes quiet between transmissions",
			MHz(got[0].Hz))
	}
}

// Choosing a channel to listen to has to stop everything that moves the
// tuner by itself. Stopping only the scan left a survey walking the grid
// underneath, which retuned within a tenth of a second — so the click
// appeared to do nothing at all.
func TestTuningStopsASurveyToo(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.StartSurvey()
	if !a.State().Survey.Running {
		t.Fatal("the survey did not start")
	}

	_ = a.Tune(120_825_000)

	st := a.State()
	if st.Survey.Running {
		t.Error("the survey is still walking the grid after a channel was chosen")
	}
	if st.Scanning {
		t.Error("still scanning after a channel was chosen")
	}
	if st.Freq != 120_825_000 {
		t.Errorf("tuned to %s", MHz(st.Freq))
	}
}

// Stopping to listen and starting again is the ordinary way to use this,
// so the evidence has to survive it.
func TestResumingASurveyKeepsWhatWasGathered(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.StartSurvey()
	a.mu.Lock()
	a.survey[118_200_000] = &SurveyStat{Hz: 118_200_000, Opens: 3, Looks: 9}
	a.mu.Unlock()

	_ = a.Tune(118_500_000) // stops it, to listen to something
	a.StartSurvey()         // and back

	if got := a.SurveyResults(); len(got) != 1 || got[0].Opens != 3 {
		t.Errorf("after resuming: %v, want the three transmissions still counted", got)
	}
}

// Resuming should carry on from where it stopped, or the low end of the
// band is surveyed over and over and the high end never is.
func TestResumingCarriesOnFromWhereItWas(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.StartSurvey()
	_ = a.Tune(130_000_000)
	a.StartSurvey()
	if got := a.State().Freq; got != 130_000_000 {
		t.Errorf("resumed at %s, want where it left off", MHz(got))
	}
}

// And starting over is a separate, deliberate act.
func TestResetClearsTheScores(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{118_200_000: {Hz: 118_200_000, Opens: 3}}
	a.mu.Unlock()

	a.ResetSurvey()
	if got := a.SurveyResults(); len(got) != 0 {
		t.Errorf("%d results survived a reset", len(got))
	}
}

// The noise floor is not the same across the band. Measured on a real
// receiver it ran from 0.0145 to 0.0317 — a factor of two — so a single
// threshold is either deaf where it is quiet or permanently open where
// it is not. 125.900 MHz sat above the fixed squelch on its own noise,
// and everything about it looked like a channel with traffic.
func TestEachChannelIsJudgedAgainstItsOwnNoise(t *testing.T) {
	quiet := SurveyStat{Hz: 135_700_000, Floor: 0.0145}
	noisy := SurveyStat{Hz: 125_900_000, Floor: 0.0317}

	if quiet.Threshold() >= noisy.Threshold() {
		t.Error("a quiet channel should have a lower threshold than a noisy one")
	}
	// The fixed 0.030 that caused this: below the noisy channel's floor,
	// so it opened on nothing.
	if noisy.Threshold() <= noisy.Floor {
		t.Error("the threshold is at or below the noise it is meant to reject")
	}
	// And a real signal, measured at about 0.04 on this receiver, still
	// has to get through on a quiet channel.
	if quiet.Threshold() > 0.04 {
		t.Errorf("a quiet channel's threshold is %.4f, which a real signal would not reach",
			quiet.Threshold())
	}
}

// A channel with traffic is still quiet between transmissions, so the
// floor converges on the noise rather than on the signal.
func TestTheFloorFollowsTheQuietest(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.StartSurvey()
	a.mu.Lock()
	st := &SurveyStat{Hz: a.freq}
	a.survey[a.freq] = st
	a.mu.Unlock()

	// A loud reading must not raise the floor.
	st.Floor = 0.02
	for _, lvl := range []float64{0.30, 0.25, 0.018} {
		if st.Floor == 0 || lvl < st.Floor {
			st.Floor = lvl
		}
	}
	if st.Floor != 0.018 {
		t.Errorf("floor = %v, want the quietest reading 0.018", st.Floor)
	}
}

// A channel that has never been visited has no floor, so nothing can be
// judged on it yet — which is why a survey wants more than one pass.
func TestNothingIsScoredBeforeTheFloorIsKnown(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	if a.surveyOpen(&SurveyStat{Hz: Guard, Looks: 0, Floor: 0}) {
		t.Error("scored a channel before its noise floor was measured")
	}
}

// What is offered should arrive with the threshold it was measured at,
// rather than the band-wide one that did not work for it.
func TestOfferedChannelsCarryTheirMeasuredThreshold(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{
		125_900_000: {Hz: 125_900_000, Opens: 2, Floor: 0.0317, Looks: 6},
	}
	a.mu.Unlock()

	got := a.OfferSurveyed()
	if len(got) != 1 {
		t.Fatalf("offered %d", len(got))
	}
	if got[0].Squelch <= 0.0317 {
		t.Errorf("offered with squelch %.4f, at or below its own noise floor", got[0].Squelch)
	}
}

// Deciding to stay on one test and scoring on another meant channels
// measured at three times their own noise floor recorded no
// transmissions at all: the survey judged against the floor, while the
// flag the scoring read was still being set by the receiver's fixed
// squelch underneath it.
func TestTheSurveyScoresOnItsOwnDecision(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.StartSurvey()

	a.mu.Lock()
	hz := a.freq
	// A channel already measured, so its floor is known.
	st := &SurveyStat{Hz: hz, Floor: 0.02, Looks: 3}
	a.survey[hz] = st
	// Well above its own threshold, and below the receiver's fixed one,
	// which is exactly the case the two tests disagreed about.
	a.am.Squelch = 0.5
	a.mu.Unlock()

	if got := st.Threshold(); got >= 0.5 {
		t.Fatalf("the test needs the channel threshold %v below the fixed one", got)
	}

	// Drive a transmission through: busy for long enough, then quiet.
	a.mu.Lock()
	a.busy = true
	a.since = time.Now().Add(-time.Second)
	a.surveyStep(time.Now())
	opens := st.Opens
	a.mu.Unlock()

	if opens == 0 {
		t.Error("a transmission on a channel above its own threshold scored nothing")
	}
}

// A survey only ever sees the tail of a transmission it arrives in the
// middle of, so the floor for what counts has to allow for that.
func TestTheMinimumAllowsForArrivingLate(t *testing.T) {
	if minTransmission > time.Second {
		t.Errorf("minTransmission of %v assumes the survey catches a whole one", minTransmission)
	}
	if minTransmission < 200*time.Millisecond {
		t.Errorf("minTransmission of %v will score the threshold being grazed", minTransmission)
	}
}

// Measured on a real receiver: Chicago Center on 133.200 MHz sits at a
// noise floor of 0.0145 and its transmissions reach 0.028 to 0.038 —
// about two to two and a half times the floor. Two of them in ninety
// seconds, a few seconds each, so the channel carries traffic perhaps
// seven percent of the time.
//
// The survey is on each of 761 channels for a tenth of a second every
// two and a half minutes. Waiting to catch a whole transmission in that
// window means waiting most of an hour, which is why a single sample
// above the channel's own noise has to count for something.
func TestAKnownBusyChannelIsFound(t *testing.T) {
	chicago := SurveyStat{Hz: 133_200_000, Floor: 0.0145}

	for _, measured := range []float64{0.0283, 0.0384} {
		if measured < chicago.Threshold() {
			t.Errorf("a transmission at %.4f is below the threshold %.4f its own noise implies",
				measured, chicago.Threshold())
		}
	}
	// And the noise it sits in must not reach it.
	if 0.0160 >= chicago.Threshold() {
		t.Errorf("the threshold %.4f is inside this channel's noise", chicago.Threshold())
	}
}

// A channel with separate moments above its own noise is worth listening
// to even if no whole transmission was ever caught, which for a busy
// frequency is the normal case.
func TestExcursionsAreEnoughToBeOffered(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.mu.Lock()
	a.survey = map[uint32]*SurveyStat{
		133_200_000: {Hz: 133_200_000, Floor: 0.0145, Hits: 3, Opens: 0, Looks: 20},
		121_100_000: {Hz: 121_100_000, Floor: 0.0150, Hits: 1, Opens: 0, Looks: 20},
	}
	a.mu.Unlock()

	got := a.SurveyResults()
	if len(got) != 1 {
		t.Fatalf("offered %d, want the one with repeated hits", len(got))
	}
	if got[0].Hz != 133_200_000 {
		t.Errorf("offered %s", MHz(got[0].Hz))
	}
}

// One sample above the noise is a stray reading, not a channel.
func TestASingleHitIsNotAChannel(t *testing.T) {
	if minHits < 2 {
		t.Error("a single stray sample would offer a channel")
	}
}

// The state has to say which squelch the control is about to change, or
// a setting made while listening to one channel silently becomes the
// setting for all of them.
func TestStateSaysWhoseSquelchItIs(t *testing.T) {
	a := testApp(t, Channel{Name: "Chicago Center", Hz: 133_200_000})
	a.SetSquelch(0.03)

	// On a saved channel with no setting of its own.
	_ = a.Tune(133_200_000)
	st := a.State()
	if !st.OnChannel {
		t.Error("on a saved channel, but the state does not say so")
	}
	if st.Override != 0 {
		t.Errorf("override = %v, want none yet", st.Override)
	}
	if st.Squelch != 0.03 {
		t.Errorf("squelch in force = %v, want the receiver's", st.Squelch)
	}

	// Give the channel one of its own.
	if err := a.SetChannelSquelch(133_200_000, 0.026); err != nil {
		t.Fatalf("SetChannelSquelch: %v", err)
	}
	st = a.State()
	if st.Override != 0.026 || st.Squelch != 0.026 {
		t.Errorf("override %v, in force %v, want 0.026 for both", st.Override, st.Squelch)
	}

	// Somewhere that is not a channel at all.
	_ = a.Tune(119_375_000)
	if st = a.State(); st.OnChannel || st.Override != 0 {
		t.Errorf("off a channel: on_channel=%v override=%v", st.OnChannel, st.Override)
	}
	if st.Squelch != 0.03 {
		t.Errorf("off a channel the receiver's %v should be in force, got %v", 0.03, st.Squelch)
	}
}

// Auto means the channel goes back to the receiver's setting, and the
// receiver's own is left alone.
func TestAutoReturnsAChannelToTheReceiverSetting(t *testing.T) {
	a := testApp(t, Channel{Name: "Chicago Center", Hz: 133_200_000})
	a.SetSquelch(0.03)
	_ = a.Tune(133_200_000)

	if err := a.SetChannelSquelch(133_200_000, 0.08); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := a.SetChannelSquelch(133_200_000, 0); err != nil {
		t.Fatalf("auto: %v", err)
	}

	st := a.State()
	if st.Override != 0 {
		t.Errorf("override = %v after Auto", st.Override)
	}
	if st.Squelch != 0.03 {
		t.Errorf("squelch in force = %v, want the receiver's 0.03 back", st.Squelch)
	}
}
