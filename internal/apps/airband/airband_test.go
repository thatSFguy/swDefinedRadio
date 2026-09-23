package airband

import (
	"slices"
	"testing"
	"time"
)

func testApp(t *testing.T, ch ...Channel) *App {
	t.Helper()
	a, err := New(Config{Channels: ch, Dir: t.TempDir(), Squelch: 0.1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// A frequency on this band is written the way it is spoken: 121.5, not
// 121500000. Typing the long form should still work, since a scanner
// list copied from somewhere else might use it.
func TestParseHz(t *testing.T) {
	tests := []struct {
		in   string
		want uint32
	}{
		{"121.5", 121_500_000},
		{"118.30", 118_300_000},
		{"121.5M", 121_500_000},
		{" 132.025 ", 132_025_000},
		{"121500000", 121_500_000},
	}
	for _, tt := range tests {
		got, err := ParseHz(tt.in)
		if err != nil {
			t.Errorf("ParseHz(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseHz(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	if _, err := ParseHz("tower"); err == nil {
		t.Error("a word was accepted as a frequency")
	}
}

func TestParseChannels(t *testing.T) {
	got, err := ParseChannels("Tower:118.3, Ground:121.9, 121.5")
	if err != nil {
		t.Fatalf("ParseChannels: %v", err)
	}
	want := []Channel{
		{Name: "Tower", Hz: 118_300_000},
		{Name: "Ground", Hz: 121_900_000},
		{Name: "121.500 MHz", Hz: 121_500_000}, // unnamed channels name themselves
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := ParseChannels(""); err == nil {
		t.Error("an empty list was accepted")
	}
}

// Nothing outside 118–137 MHz belongs here, and accepting it would leave
// the receiver somewhere it can hear nothing.
func TestTuneRefusesOutsideTheBand(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	for _, hz := range []uint32{98_700_000, 1_090_000_000, 137_500_000} {
		if err := a.Tune(hz); err == nil {
			t.Errorf("%s was accepted", MHz(hz))
		}
	}
}

// Choosing a channel means listening to that channel, so it has to stop
// the scan — otherwise it moves on a moment later and the click did
// nothing.
func TestTuningStopsScanning(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard}, Channel{Name: "Unicom", Hz: 122_800_000})
	if !a.State().Scanning {
		t.Fatal("expected to start scanning")
	}
	// Off the air, so tuning records the wish without reaching a radio.
	_ = a.Tune(122_800_000)
	st := a.State()
	if st.Scanning {
		t.Error("still scanning after a channel was chosen")
	}
	if st.Freq != 122_800_000 {
		t.Errorf("tuned to %s", MHz(st.Freq))
	}
}

// A channel list that survives a restart is the difference between a
// scanner someone sets up once and one they set up every time.
func TestChannelsAreSaved(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	list := []Channel{{Name: "Tower", Hz: 118_300_000}, {Name: "Approach", Hz: 125_350_000}}
	if err := a.SetChannels(list); err != nil {
		t.Fatalf("SetChannels: %v", err)
	}

	again, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := again.State().Channels; !slices.Equal(got, list) {
		t.Errorf("after reloading: %v, want %v", got, list)
	}
}

func TestSetChannelsRejectsRubbish(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	if err := a.SetChannels(nil); err == nil {
		t.Error("an empty channel list was accepted")
	}
	if err := a.SetChannels([]Channel{{Name: "FM", Hz: 98_700_000}}); err == nil {
		t.Error("a frequency outside the band was accepted")
	}
}

// Deleting the channel being listened to has to move the receiver, or it
// sits on a frequency the page no longer lists.
func TestDeletingTheTunedChannelMovesOn(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard}, Channel{Name: "Unicom", Hz: 122_800_000})
	_ = a.Tune(122_800_000)

	if err := a.SetChannels([]Channel{{Name: "Guard", Hz: Guard}}); err != nil {
		t.Fatalf("SetChannels: %v", err)
	}
	if got := a.State().Freq; got != Guard {
		t.Errorf("left tuned to %s after that channel was deleted", MHz(got))
	}
}

// An unnamed channel names itself after its frequency, so the list never
// shows a blank row.
func TestUnnamedChannelsGetAName(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	if err := a.SetChannels([]Channel{{Hz: 118_300_000}}); err != nil {
		t.Fatalf("SetChannels: %v", err)
	}
	if got := a.State().Channels[0].Name; got != "118.300 MHz" {
		t.Errorf("name = %q", got)
	}
}

// Squelch is the one control that matters on a band which is silent most
// of the time, and a negative one would mean nothing.
func TestSquelchCannotGoNegative(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	a.SetSquelch(-1)
	if got := a.State().Squelch; got != 0 {
		t.Errorf("squelch = %v, want 0", got)
	}
}

// The scan timing is the difference between a scanner that catches an
// exchange and one that keeps wandering off mid-sentence, so the
// constants are worth stating rather than leaving to be discovered.
func TestScanTimingIsUsable(t *testing.T) {
	// A transmission is a couple of seconds; a pass over a realistic
	// list has to be quicker than that or most of them are missed.
	const channels = 20
	if pass := time.Duration(channels) * dwell; pass > 3*time.Second {
		t.Errorf("a pass over %d channels takes %v, which is longer than most transmissions",
			channels, pass)
	}
	// The squelch decides about every 55 ms, so a channel must be given
	// more than one look.
	if dwell < 100*time.Millisecond {
		t.Errorf("dwell of %v is less than two squelch decisions", dwell)
	}
	// The reply comes back on the same frequency, and it comes back
	// within a second or two.
	if hang < 2*time.Second {
		t.Errorf("hang of %v is too short to catch a reply", hang)
	}
	if settle >= dwell {
		t.Errorf("settle %v swallows the whole dwell %v", settle, dwell)
	}
}

// Moving to a channel must not carry the previous one's history with
// it, or every channel inherits the last one's hang and the scan crawls.
func TestTuningClearsTheHoldFromTheLastChannel(t *testing.T) {
	a := testApp(t, Channel{Name: "A", Hz: 118_100_000}, Channel{Name: "B", Hz: 118_200_000})
	a.mu.Lock()
	a.lastBusy = time.Now()
	a.mu.Unlock()

	_ = a.Tune(118_200_000)

	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.lastBusy.IsZero() {
		t.Error("the new channel inherited the old one's activity")
	}
}
