package airband

import (
	"slices"
	"testing"
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
		{"Tower", 118_300_000},
		{"Ground", 121_900_000},
		{"121.500 MHz", 121_500_000}, // unnamed channels name themselves
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
	a := testApp(t, Channel{"Guard", Guard})
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
	a := testApp(t, Channel{"Guard", Guard}, Channel{"Unicom", 122_800_000})
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
	list := []Channel{{"Tower", 118_300_000}, {"Approach", 125_350_000}}
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
	a := testApp(t, Channel{"Guard", Guard})
	if err := a.SetChannels(nil); err == nil {
		t.Error("an empty channel list was accepted")
	}
	if err := a.SetChannels([]Channel{{"FM", 98_700_000}}); err == nil {
		t.Error("a frequency outside the band was accepted")
	}
}

// Deleting the channel being listened to has to move the receiver, or it
// sits on a frequency the page no longer lists.
func TestDeletingTheTunedChannelMovesOn(t *testing.T) {
	a := testApp(t, Channel{"Guard", Guard}, Channel{"Unicom", 122_800_000})
	_ = a.Tune(122_800_000)

	if err := a.SetChannels([]Channel{{"Guard", Guard}}); err != nil {
		t.Fatalf("SetChannels: %v", err)
	}
	if got := a.State().Freq; got != Guard {
		t.Errorf("left tuned to %s after that channel was deleted", MHz(got))
	}
}

// An unnamed channel names itself after its frequency, so the list never
// shows a blank row.
func TestUnnamedChannelsGetAName(t *testing.T) {
	a := testApp(t, Channel{"Guard", Guard})
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
	a := testApp(t, Channel{"Guard", Guard})
	a.SetSquelch(-1)
	if got := a.State().Squelch; got != 0 {
		t.Errorf("squelch = %v, want 0", got)
	}
}
