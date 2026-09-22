package alert

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

func ac(hex, callsign string) track.Aircraft {
	return track.Aircraft{Hex: hex, Callsign: callsign}
}

func TestBuiltinCatchesWhatItClaims(t *testing.T) {
	w := New(Builtin(), time.Hour, nil)
	now := time.Now()

	cases := []struct {
		what string
		a    track.Aircraft
		rule string
	}{
		{"a US military address", ac("ae1234", ""), "US military"},
		{"the bottom of the US block", ac("ae0000", ""), "US military"},
		{"the top of the US block", ac("afffff", ""), "US military"},
		{"a UK military address", ac("43c801", ""), "UK military"},
		{"a Reach airlifter", ac("abcdef", "RCH271"), "military callsign"},
		{"a special air mission", ac("abcde0", "SAM060"), "government VIP"},
		{"an air ambulance", ac("abcde1", "LIFE12"), "air ambulance"},
	}
	for _, c := range cases {
		got := w.Check(c.a, now)
		if len(got) == 0 {
			t.Errorf("%s (%+v) raised nothing, want %q", c.what, c.a, c.rule)
			continue
		}
		if got[0].Rule != c.rule {
			t.Errorf("%s matched %q, want %q", c.what, got[0].Rule, c.rule)
		}
	}
}

// TestBuiltinLeavesAirlinersAlone is the property that decides whether
// the feature is usable: an alert on every flight is the same as none.
func TestBuiltinLeavesAirlinersAlone(t *testing.T) {
	w := New(Builtin(), time.Hour, nil)
	now := time.Now()

	ordinary := []track.Aircraft{
		{Hex: "a1b2c3", Callsign: "UAL422", HasAltitude: true, Altitude: 35000, GroundSpeed: 450, Squawk: "1200"},
		{Hex: "406b90", Callsign: "BAW117", HasAltitude: true, Altitude: 41000, GroundSpeed: 520, Squawk: "2456"},
		{Hex: "4ca1fa", Callsign: "RYR2AB", HasAltitude: true, Altitude: 3000, GroundSpeed: 180},
		{Hex: "ad0000", Callsign: "SWA1", HasAltitude: true, Altitude: 33000}, // just below the US block
		{Hex: "43bfff", Callsign: "EZY1", HasAltitude: true, Altitude: 30000}, // just below the UK block
		{Hex: "abc123", Callsign: "SAS900"},                                   // SAS, not SAM
	}
	for _, a := range ordinary {
		if got := w.Check(a, now); len(got) > 0 {
			t.Errorf("%s/%s raised %q, which would make the alerts useless",
				a.Hex, a.Callsign, got[0].Rule)
		}
	}
}

func TestEmergencies(t *testing.T) {
	w := New(Builtin(), time.Hour, nil)
	now := time.Now()

	got := w.Check(track.Aircraft{Hex: "a00001", Squawk: "7700"}, now)
	if len(got) != 1 || got[0].Rule != "emergency squawk" || !got[0].Urgent {
		t.Errorf("squawk 7700 raised %+v", got)
	}
	got = w.Check(track.Aircraft{Hex: "a00002", Emergency: "no communications"}, now)
	if len(got) != 1 || got[0].Rule != "declared emergency" {
		t.Errorf("a declared emergency raised %+v", got)
	}
	// "none" is what a status squitter says when all is well, and the
	// tracker leaves the field empty; neither is an emergency.
	for _, e := range []string{"", "none"} {
		if got := w.Check(track.Aircraft{Hex: "a00003", Emergency: e}, now); len(got) > 0 {
			t.Errorf("emergency %q raised %+v", e, got)
		}
	}
}

// TestCooldown stops one aircraft from alerting on every message: in
// range it sends several a second.
func TestCooldown(t *testing.T) {
	w := New(Builtin(), 30*time.Minute, nil)
	a := ac("ae4321", "")
	t0 := time.Now()

	if got := w.Check(a, t0); len(got) != 1 {
		t.Fatalf("first check raised %d, want 1", len(got))
	}
	for i := range 100 {
		if got := w.Check(a, t0.Add(time.Duration(i)*time.Second)); len(got) != 0 {
			t.Fatalf("check %d raised again inside the cooldown", i)
		}
	}
	if got := w.Check(a, t0.Add(31*time.Minute)); len(got) != 1 {
		t.Error("it never re-armed after the cooldown")
	}
	if n := len(w.Recent(0)); n != 2 {
		t.Errorf("kept %d alerts, want 2", n)
	}
}

// TestConditionsCombine checks that a rule with several conditions
// needs all of them, which is how a rule is narrowed to one aircraft.
func TestConditionsCombine(t *testing.T) {
	r := Rule{Name: "low and close", BelowFt: 3000, WithinNM: 10}
	w := New([]Rule{r}, time.Hour, nil)
	now := time.Now()

	yes := track.Aircraft{Hex: "a1", HasAltitude: true, Altitude: 2000, HasPosition: true, DistanceNM: 5}
	if got := w.Check(yes, now); len(got) != 1 {
		t.Error("low and close did not match")
	}
	no := []track.Aircraft{
		{Hex: "a2", HasAltitude: true, Altitude: 2000, HasPosition: true, DistanceNM: 40}, // too far
		{Hex: "a3", HasAltitude: true, Altitude: 9000, HasPosition: true, DistanceNM: 5},  // too high
		{Hex: "a4", HasAltitude: true, Altitude: 2000},                                    // no position
		{Hex: "a5", HasPosition: true, DistanceNM: 5},                                     // no altitude
	}
	for _, a := range no {
		if got := w.Check(a, now); len(got) > 0 {
			t.Errorf("%s matched on a partial condition", a.Hex)
		}
	}
}

func TestPatternMatching(t *testing.T) {
	cases := []struct {
		pat, val string
		want     bool
	}{
		{"RCH*", "RCH271", true},
		{"RCH*", "ARCH1", false},
		{"*LIFE*", "AIRLIFE5", true},
		{"*1", "N4321", true},
		{"N4321", "N4321", true},
		{"n4321", "N4321", true}, // callsigns arrive upper-case
		{"N4321", "N43210", false},
	}
	for _, c := range cases {
		if got := matchAny([]string{c.pat}, c.val); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pat, c.val, got, c.want)
		}
	}

	hexes := []struct {
		pat, val string
		want     bool
	}{
		{"ae*", "ae1234", true},
		{"ae0000-afffff", "af0001", true},
		{"ae0000-afffff", "ad9999", false},
		{"AE0000-AFFFFF", "ae0001", true},
		{"4b1814", "4b1814", true},
		{"4b1814", "4b1815", false},
	}
	for _, c := range hexes {
		if got := matchHex([]string{c.pat}, c.val); got != c.want {
			t.Errorf("matchHex(%q, %q) = %v, want %v", c.pat, c.val, got, c.want)
		}
	}
}

func TestLoadRules(t *testing.T) {
	dir := t.TempDir()

	// A missing file means the built-ins, not an error: most receivers
	// will never write one.
	rules, cd, found, err := LoadOrBuiltin(filepath.Join(dir, "nope.json"))
	if err != nil || found || len(rules) != len(Builtin()) || cd != DefaultCooldown {
		t.Errorf("a missing file gave %d rules, found=%v, err=%v", len(rules), found, err)
	}

	p := filepath.Join(dir, "alerts.json")
	if err := os.WriteFile(p, []byte(Example), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, cd, found, err = LoadOrBuiltin(p)
	if err != nil || !found {
		t.Fatalf("loading the example: found=%v err=%v", found, err)
	}
	if want := len(Builtin()) + 4; len(rules) != want {
		t.Errorf("got %d rules, want %d (built-ins plus the example's four)", len(rules), want)
	}
	if cd != 30*time.Minute {
		t.Errorf("cooldown = %v, want 30m", cd)
	}

	// builtin:false replaces rather than extends.
	if err := os.WriteFile(p, []byte(`{"builtin":false,"rules":[{"name":"mine","hex":["abc123"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, _, _, err = LoadOrBuiltin(p)
	if err != nil || len(rules) != 1 || rules[0].Name != "mine" {
		t.Errorf("builtin:false gave %d rules, err=%v", len(rules), err)
	}

	// The failures that must be loud rather than silent.
	bad := []string{
		`{"rules":[{"name":"no conditions"}]}`,
		`{"rules":[{"hex":["abc123"]}]}`,
		`{"cooldown":"half an hour","rules":[]}`,
		`not json at all`,
	}
	for _, b := range bad {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := LoadOrBuiltin(p); err == nil {
			t.Errorf("%s was accepted", b)
		}
	}
}

func TestSinkAndLine(t *testing.T) {
	var got []Alert
	w := New(Builtin(), time.Hour, func(a Alert) { got = append(got, a) })
	w.Check(track.Aircraft{
		Hex: "ae1234", Callsign: "RCH271", HasAltitude: true, Altitude: 28000,
		HasPosition: true, DistanceNM: 42.6, Squawk: "1234",
	}, time.Now())

	if len(got) != 2 { // the address and the callsign both match
		t.Fatalf("the sink saw %d alerts, want 2", len(got))
	}
	line := got[0].Line()
	for _, want := range []string{"US military", "RCH271", "ae1234", "28000 ft", "43 NM"} {
		if !contains(line, want) {
			t.Errorf("%q is missing %q", line, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
