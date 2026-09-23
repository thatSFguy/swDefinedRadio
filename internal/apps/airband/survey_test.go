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
