package alert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

func watchlistOf(t *testing.T, flights ...string) *Watchlist {
	t.Helper()
	l, err := OpenWatchlist("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Set(flights); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestWatchlistMatchesWhatPeopleType(t *testing.T) {
	l := watchlistOf(t, "dl 1234", "UAL422", "N12345", "*5123", "SWA0042")

	cases := []struct {
		callsign string
		want     string
	}{
		{"DAL1234", "DL1234"},  // the ticket's IATA number
		{"UAL422", "UAL422"},   // typed as transmitted
		{"ual422 ", "UAL422"},  // case and padding from the air
		{"N12345", "N12345"},   // a registration
		{"EDV5123", "*5123"},   // a regional flight under another code
		{"SWA42", "SWA0042"},   // padding in the list
		{"DAL01234", "DL1234"}, // padding in the air
	}
	for _, c := range cases {
		got, ok := l.Match(c.callsign)
		if !ok || got != c.want {
			t.Errorf("Match(%q) = %q, %v; want %q", c.callsign, got, ok, c.want)
		}
	}

	for _, cs := range []string{"DAL123", "DAL12345", "UAL4221", "N1234", "", "AAL1234"} {
		if got, ok := l.Match(cs); ok {
			t.Errorf("Match(%q) = %q, want no match", cs, got)
		}
	}
}

func TestWatchlistRefusesNonsense(t *testing.T) {
	l := watchlistOf(t)
	for _, bad := range [][]string{{"*"}, {"DAL1234567"}, {"DL-123"}, {"**"}} {
		if _, err := l.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
	got, err := l.Set([]string{"ual1", "UAL1", "  ", "dal2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "DAL2,UAL1" {
		t.Errorf("Set kept %v, want [DAL2 UAL1]", got)
	}
}

func TestWatchlistSurvivesARestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "watchlist.json")
	l, err := OpenWatchlist(p)
	if err != nil {
		t.Fatalf("a missing file should be an empty list: %v", err)
	}
	if _, err := l.Set([]string{"DL1234"}); err != nil {
		t.Fatal(err)
	}
	again, err := OpenWatchlist(p)
	if err != nil {
		t.Fatal(err)
	}
	if f := again.Flights(); len(f) != 1 || f[0] != "DL1234" {
		t.Errorf("reopened list is %v", f)
	}

	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := OpenWatchlist(p); err == nil {
		t.Error("a broken file was read without complaint")
	}
}

// TestAlarmLifecycle is the feature: raised once when the flight turns
// up, held until acknowledged, cleared when it leaves, and raised again
// if it comes back.
func TestAlarmLifecycle(t *testing.T) {
	var sunk []Alert
	w := New(nil, time.Hour, func(a Alert) { sunk = append(sunk, a) })
	w.WatchFlights(watchlistOf(t, "DL1234"), time.Minute)

	t0 := time.Now()
	plane := track.Aircraft{Hex: "a1b2c3", Callsign: "DAL1234", HasAltitude: true, Altitude: 12000,
		HasPosition: true, DistanceNM: 23}

	if got := w.Check(track.Aircraft{Hex: "a1b2c3"}, t0); len(got) != 0 {
		t.Fatalf("raised before the callsign was known: %+v", got)
	}
	got := w.Check(plane, t0)
	if len(got) != 1 || got[0].Rule != "watchlist" || !got[0].Urgent {
		t.Fatalf("first sighting raised %+v, want one urgent watchlist alert", got)
	}
	// Every later message refreshes the alarm without raising another.
	for i := 1; i <= 30; i++ {
		if got := w.Check(plane, t0.Add(time.Duration(i)*time.Second)); len(got) != 0 {
			t.Fatalf("message %d raised again: %+v", i, got)
		}
	}
	if len(sunk) != 1 {
		t.Errorf("sink heard %d alerts, want 1", len(sunk))
	}

	al := w.Alarms(t0.Add(30 * time.Second))
	if len(al) != 1 || al[0].Acked || al[0].Flight != "DL1234" || al[0].DistanceNM != 23 {
		t.Fatalf("alarms = %+v", al)
	}

	if !w.Ack("A1B2C3") {
		t.Fatal("Ack found nothing")
	}
	if al := w.Alarms(t0.Add(40 * time.Second)); len(al) != 1 || !al[0].Acked {
		t.Fatalf("after Ack, alarms = %+v; want it listed and acknowledged", al)
	}
	w.Check(plane, t0.Add(50*time.Second))
	if al := w.Alarms(t0.Add(50 * time.Second)); !al[0].Acked {
		t.Fatal("a later message un-acknowledged the alarm")
	}

	// Unheard for longer than the tracker keeps it: gone.
	if al := w.Alarms(t0.Add(3 * time.Minute)); len(al) != 0 {
		t.Fatalf("alarm outlived the aircraft: %+v", al)
	}

	// Back again later is a new visit, unacknowledged.
	if got := w.Check(plane, t0.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("return visit raised %+v, want one alert", got)
	}
	if al := w.Alarms(t0.Add(time.Hour)); len(al) != 1 || al[0].Acked {
		t.Fatalf("return visit alarms = %+v", al)
	}
}

func TestAlarmEndsWhenTakenOffTheList(t *testing.T) {
	w := New(nil, time.Hour, nil)
	l := watchlistOf(t, "UAL422")
	w.WatchFlights(l, time.Minute)
	now := time.Now()
	w.Check(track.Aircraft{Hex: "abc123", Callsign: "UAL422"}, now)
	if len(w.Alarms(now)) != 1 {
		t.Fatal("no alarm raised")
	}
	l.Set(nil)
	if al := w.Alarms(now); len(al) != 0 {
		t.Errorf("alarm outlived its watchlist entry: %+v", al)
	}
}

// Rules and the watchlist are independent: a watched flight that also
// matches a rule gets both, and a watcher with no list behaves as before.
func TestWatchlistAlongsideRules(t *testing.T) {
	w := New(Builtin(), time.Hour, nil)
	w.WatchFlights(watchlistOf(t, "RCH271"), time.Minute)
	got := w.Check(track.Aircraft{Hex: "ae1234", Callsign: "RCH271"}, time.Now())
	rules := map[string]bool{}
	for _, a := range got {
		rules[a.Rule] = true
	}
	if !rules["US military"] || !rules["military callsign"] || !rules["watchlist"] {
		t.Errorf("raised %v", rules)
	}

	plain := New(Builtin(), time.Hour, nil)
	if al := plain.Alarms(time.Now()); len(al) != 0 {
		t.Errorf("a watcher with no list has alarms: %+v", al)
	}
	if plain.Ack("") {
		t.Error("Ack found an alarm on a watcher with no list")
	}
}
