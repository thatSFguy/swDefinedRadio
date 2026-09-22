package track

import (
	"math"
	"testing"
	"time"
)

// The worked example from ICAO Doc 9871: frames
// 8D40621D58C382D690C8AC2863A7 (even) and 8D40621D58C386435CC412692AD6
// (odd), with the even frame the more recent, resolve to
// 52.25720 N, 3.91937 E.
var (
	refEven = CPR{Lat: 93000, Lon: 51372, Odd: false}
	refOdd  = CPR{Lat: 74158, Lon: 50194, Odd: true}
)

func TestNL(t *testing.T) {
	// Spot values from ICAO's longitude-zone table.
	for _, c := range []struct {
		lat  float64
		want float64
	}{
		{0, 59}, {10, 59}, {20, 56}, {45, 42},
		{60, 29}, {80, 10}, {86, 3}, {87, 2}, {89, 1}, {-45, 42},
	} {
		if got := nl(c.lat); got != c.want {
			t.Errorf("nl(%v) = %v, want %v", c.lat, got, c.want)
		}
	}
}

func TestGlobalAirborne(t *testing.T) {
	now := time.Now()
	e, o := refEven, refOdd
	e.At = now // even is the more recent of the pair
	o.At = now.Add(-2 * time.Second)

	lat, lon, ok := GlobalAirborne(e, o)
	if !ok {
		t.Fatal("position not resolved from a valid even/odd pair")
	}
	if math.Abs(lat-52.25720) > 1e-4 || math.Abs(lon-3.91937) > 1e-4 {
		t.Errorf("position = (%.5f, %.5f), want (52.25720, 3.91937)", lat, lon)
	}
}

func TestGlobalAirborneOddNewer(t *testing.T) {
	// With the odd frame newer the fix moves slightly, to the position
	// at that frame's time, but must stay in the same neighbourhood.
	now := time.Now()
	e, o := refEven, refOdd
	e.At = now.Add(-2 * time.Second)
	o.At = now

	lat, lon, ok := GlobalAirborne(e, o)
	if !ok {
		t.Fatal("position not resolved with the odd frame newer")
	}
	if math.Abs(lat-52.25720) > 0.5 || math.Abs(lon-3.91937) > 0.5 {
		t.Errorf("position = (%.5f, %.5f), want near (52.25720, 3.91937)", lat, lon)
	}
}

func TestLocalMatchesGlobal(t *testing.T) {
	// Given a reference within range, a single frame should land on the
	// same place the even/odd pair does.
	now := time.Now()
	e, o := refEven, refOdd
	e.At, o.At = now, now.Add(-2*time.Second)
	wantLat, wantLon, ok := GlobalAirborne(e, o)
	if !ok {
		t.Fatal("global fix failed")
	}

	lat, lon := Local(wantLat+0.4, wantLon+0.4, e)
	if math.Abs(lat-wantLat) > 1e-4 || math.Abs(lon-wantLon) > 1e-4 {
		t.Errorf("local fix = (%.5f, %.5f), want (%.5f, %.5f)", lat, lon, wantLat, wantLon)
	}
}

func TestDistanceNM(t *testing.T) {
	// London Heathrow to Paris Charles de Gaulle, about 187 NM.
	d := DistanceNM(51.4700, -0.4543, 49.0097, 2.5479)
	if math.Abs(d-187) > 3 {
		t.Errorf("distance = %.1f NM, want about 187", d)
	}
}

func TestModIsNonNegative(t *testing.T) {
	if got := mod(-1, 60); got != 59 {
		t.Errorf("mod(-1, 60) = %v, want 59", got)
	}
}
