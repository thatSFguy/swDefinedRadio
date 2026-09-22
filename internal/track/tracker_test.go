package track

import (
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/modes"
)

func decodeFrame(t *testing.T, s string) modes.ADSB {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad vector: %v", err)
	}
	a, ok := modes.DecodeADSB(b)
	if !ok {
		t.Fatalf("frame %s not decoded", s)
	}
	return a
}

// TestTrackerResolvesPosition drives the real decoder output through the
// tracker: an even and an odd frame from one aircraft should produce a
// fix without any reference position being configured.
func TestTrackerResolvesPosition(t *testing.T) {
	tr := New(time.Minute)
	now := time.Now()

	// Odd first, then even, so the even frame is the more recent.
	tr.Update(decodeFrame(t, "8D40621D58C386435CC412692AD6"), -20, now.Add(-2*time.Second))
	craft, _ := tr.Snapshot()
	if craft[0].HasPosition {
		t.Error("a single frame yielded a position with no reference set")
	}

	tr.Update(decodeFrame(t, "8D40621D58C382D690C8AC2863A7"), -20, now)

	craft, stats := tr.Snapshot()
	if len(craft) != 1 {
		t.Fatalf("tracking %d aircraft, want 1", len(craft))
	}
	a := craft[0]
	if a.Hex != "40621d" {
		t.Errorf("hex = %q, want 40621d", a.Hex)
	}
	if !a.HasPosition {
		t.Fatal("no position after an even/odd pair")
	}
	if math.Abs(a.Lat-52.25720) > 1e-4 || math.Abs(a.Lon-3.91937) > 1e-4 {
		t.Errorf("position = (%.5f, %.5f), want (52.25720, 3.91937)", a.Lat, a.Lon)
	}
	if !a.HasAltitude || a.Altitude != 38000 {
		t.Errorf("altitude = %d, want 38000", a.Altitude)
	}
	if a.Messages != 2 || stats.Frames != 2 {
		t.Errorf("messages = %d, frames = %d, want 2 and 2", a.Messages, stats.Frames)
	}
}

// TestStalePairIsRejected guards the ten-second rule: an old even frame
// paired with a new odd one must not produce a fix, because the shared
// zone index may no longer hold.
func TestStalePairIsRejected(t *testing.T) {
	tr := New(time.Minute)
	now := time.Now()
	tr.Update(decodeFrame(t, "8D40621D58C382D690C8AC2863A7"), -20, now.Add(-5*time.Minute))
	tr.Update(decodeFrame(t, "8D40621D58C386435CC412692AD6"), -20, now)

	craft, _ := tr.Snapshot()
	if craft[0].HasPosition {
		t.Error("a stale even/odd pair produced a position fix")
	}
}

// TestMergesFieldsAcrossFrames checks that identification, velocity and
// position frames accumulate onto one aircraft rather than overwriting.
func TestMergesFieldsAcrossFrames(t *testing.T) {
	tr := New(time.Minute)
	now := time.Now()
	icao := uint32(0x485020)

	tr.Update(modes.ADSB{ICAO: icao, Callsign: "KLM1023"}, -20, now)
	tr.Update(decodeFrame(t, "8D485020994409940838175B284F"), -20, now) // velocity
	tr.Update(modes.ADSB{ICAO: icao, HasAltitude: true, Altitude: 12000}, -20, now)

	craft, _ := tr.Snapshot()
	if len(craft) != 1 {
		t.Fatalf("tracking %d aircraft, want 1 merged", len(craft))
	}
	a := craft[0]
	if a.Callsign != "KLM1023" {
		t.Errorf("callsign = %q, lost across frames", a.Callsign)
	}
	if math.Round(a.GroundSpeed) != 159 {
		t.Errorf("speed = %v, want 159", a.GroundSpeed)
	}
	if a.Altitude != 12000 {
		t.Errorf("altitude = %d, want 12000", a.Altitude)
	}
	if a.Messages != 3 {
		t.Errorf("messages = %d, want 3", a.Messages)
	}
}

func TestExpire(t *testing.T) {
	tr := New(30 * time.Second)
	now := time.Now()
	tr.Update(modes.ADSB{ICAO: 1, Callsign: "OLD"}, -20, now.Add(-time.Minute))
	tr.Update(modes.ADSB{ICAO: 2, Callsign: "NEW"}, -20, now)

	if n := tr.Expire(now); n != 1 {
		t.Errorf("expired %d aircraft, want 1", n)
	}
	craft, _ := tr.Snapshot()
	if len(craft) != 1 || craft[0].Callsign != "NEW" {
		t.Errorf("after expiry: %+v, want only NEW", craft)
	}
}

// TestReferenceGivesSingleFrameFix confirms that configuring the
// receiver position lets one frame place an aircraft.
func TestReferenceGivesSingleFrameFix(t *testing.T) {
	tr := New(time.Minute)
	tr.SetReference(52.0, 4.0)
	tr.Update(decodeFrame(t, "8D40621D58C382D690C8AC2863A7"), -20, time.Now())

	craft, _ := tr.Snapshot()
	a := craft[0]
	if !a.HasPosition {
		t.Fatal("no fix from a single frame despite a reference position")
	}
	if math.Abs(a.Lat-52.25720) > 1e-4 || math.Abs(a.Lon-3.91937) > 1e-4 {
		t.Errorf("position = (%.5f, %.5f), want (52.25720, 3.91937)", a.Lat, a.Lon)
	}
	if a.DistanceNM <= 0 || a.DistanceNM > 60 {
		t.Errorf("distance = %.1f NM, want a small positive range", a.DistanceNM)
	}
}
