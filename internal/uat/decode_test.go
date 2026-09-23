package uat

import (
	"math"
	"testing"
)

// encodeSV builds the state-vector half of a message, so the decoder is
// checked against values chosen rather than against itself.
func encodeSV(lat, lon float64, altFt int, geometric bool, nsVel, ewVel, vrate int) []byte {
	p := make([]byte, 34)
	p[0] = 1 << 3 // message type 1: a long message with a state vector

	const step = 360.0 / (1 << 24)
	rawLat := uint32(math.Round(lat / step))
	if lat < 0 {
		rawLat = uint32(math.Round((lat + 180) / step))
	}
	rawLon := uint32(math.Round(lon / step))
	if lon < 0 {
		rawLon = uint32(math.Round((lon + 360) / step))
	}
	p[4] = byte(rawLat >> 15)
	p[5] = byte(rawLat >> 7)
	p[6] = byte(rawLat<<1) & 0xfe

	p[6] |= byte(rawLon >> 23 & 1)
	p[7] = byte(rawLon >> 15)
	p[8] = byte(rawLon >> 7)
	p[9] = byte(rawLon<<1) & 0xfe
	if geometric {
		p[9] |= 1
	}

	rawAlt := uint16((altFt+1000)/25) + 1
	p[10] = byte(rawAlt >> 4)
	p[11] = byte(rawAlt<<4) & 0xf0

	// Airborne subsonic, with north/south and east/west velocity.
	put11 := func(v int) uint32 {
		mag := uint32(abs(v) + 1)
		if v < 0 {
			mag |= 0x400
		}
		return mag
	}
	ns, ew := put11(nsVel), put11(ewVel)
	p[12] = byte(ns >> 6 & 0x1f)
	p[13] = byte(ns<<2) & 0xfc
	p[13] |= byte(ew >> 9 & 0x03)
	p[14] = byte(ew >> 1)
	p[15] = byte(ew<<7) & 0x80

	raw := uint32(abs(vrate)/64 + 1)
	if vrate < 0 {
		raw |= 0x200
	}
	p[15] |= byte(raw>>4) & 0x7f
	p[16] = byte(raw<<4) & 0xf0
	return p
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// putCallsign packs a call sign the way a transmitter does, base-40 in
// three 16-bit groups.
func putCallsign(p []byte, emitter byte, cs string) {
	code := func(c byte) uint32 {
		switch {
		case c >= '0' && c <= '9':
			return uint32(c - '0')
		case c >= 'A' && c <= 'Z':
			return uint32(c-'A') + 10
		}
		return 36
	}
	for len(cs) < 8 {
		cs += " "
	}
	v := uint32(emitter)*1600 + code(cs[0])*40 + code(cs[1])
	p[17], p[18] = byte(v>>8), byte(v)
	v = code(cs[2])*1600 + code(cs[3])*40 + code(cs[4])
	p[19], p[20] = byte(v>>8), byte(v)
	v = code(cs[5])*1600 + code(cs[6])*40 + code(cs[7])
	p[21], p[22] = byte(v>>8), byte(v)
	p[26] |= 0x02 // CSID: this is a call sign, not a squawk
}

func TestDecodeStateVector(t *testing.T) {
	// Somewhere identifiable, climbing north-east.
	const wantLat, wantLon = 40.6892, -74.0445
	p := encodeSV(wantLat, wantLon, 4500, false, 120, 90, 1216)
	p[1], p[2], p[3] = 0xa1, 0xb2, 0xc3
	putCallsign(p, 1, "N123AB")

	m, ok := DecodeADSB(p)
	if !ok {
		t.Fatal("a valid message was rejected")
	}
	if m.Address != 0xa1b2c3 {
		t.Errorf("address = %06x", m.Address)
	}
	if !m.HasPosition {
		t.Fatal("no position")
	}
	// A degree is 360/2^24, so a fifth of a thousandth is the most the
	// encoding can be out by.
	if math.Abs(m.Lat-wantLat) > 0.0001 || math.Abs(m.Lon-wantLon) > 0.0001 {
		t.Errorf("position = %.5f, %.5f, want %.5f, %.5f", m.Lat, m.Lon, wantLat, wantLon)
	}
	if !m.HasAltitude || m.Altitude != 4500 {
		t.Errorf("altitude = %d, want 4500", m.Altitude)
	}
	if m.Geometric {
		t.Error("a pressure altitude was read as geometric")
	}
	if !m.HasVelocity {
		t.Fatal("no velocity")
	}
	if want := math.Hypot(120, 90); math.Abs(m.GroundSpeed-want) > 1 {
		t.Errorf("speed = %.1f kt, want %.1f", m.GroundSpeed, want)
	}
	// North 120, east 90: a bit east of north-east.
	if want := 36.87; math.Abs(m.Track-want) > 1 {
		t.Errorf("track = %.1f°, want %.1f°", m.Track, want)
	}
	if !m.HasVertical || m.VerticalRate != 1216 {
		t.Errorf("vertical rate = %d, want 1216", m.VerticalRate)
	}
	if m.Callsign != "N123AB" {
		t.Errorf("callsign = %q, want N123AB", m.Callsign)
	}
	if m.Emitter != 1 {
		t.Errorf("emitter = %d, want 1", m.Emitter)
	}
}

// TestDecodeSigns covers the fields where a sign bit in the wrong place
// would still produce a plausible-looking aircraft: a descent read as a
// climb, or a southbound aircraft read as northbound.
func TestDecodeSigns(t *testing.T) {
	for _, c := range []struct {
		name          string
		ns, ew, vrate int
		wantTrack     float64
		wantRate      int
	}{
		{"north-east climbing", 100, 100, 1024, 45, 1024},
		{"south-west descending", -100, -100, -1024, 225, -1024},
		{"due south level", -150, 0, -64, 180, -64},
		{"due west descending", 0, -150, -1984, 270, -1984},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, ok := DecodeADSB(encodeSV(40, -85, 10000, false, c.ns, c.ew, c.vrate))
			if !ok {
				t.Fatal("rejected")
			}
			if !m.HasVelocity {
				t.Fatal("no velocity")
			}
			if math.Abs(m.Track-c.wantTrack) > 1 {
				t.Errorf("track = %.1f°, want %.1f°", m.Track, c.wantTrack)
			}
			if m.VerticalRate != c.wantRate {
				t.Errorf("vertical rate = %d, want %d", m.VerticalRate, c.wantRate)
			}
		})
	}
}

// TestPositionRange checks the wrap-around in both hemispheres, which
// is the one part of the position encoding that is not obvious.
func TestPositionRange(t *testing.T) {
	for _, c := range []struct{ lat, lon float64 }{
		{42.88, -85.52},  // the receiver
		{-33.94, 151.17}, // southern and eastern
		{51.47, -0.45},   // just west of the meridian
		{0.01, 0.01},     // just north-east of the origin
		{-45.0, -170.0},  // southern and far western
	} {
		m, ok := DecodeADSB(encodeSV(c.lat, c.lon, 1000, false, 100, 100, 0))
		if !ok || !m.HasPosition {
			t.Fatalf("%.2f, %.2f was not decoded", c.lat, c.lon)
		}
		if math.Abs(m.Lat-c.lat) > 0.0001 || math.Abs(m.Lon-c.lon) > 0.0001 {
			t.Errorf("%.2f, %.2f came back as %.4f, %.4f", c.lat, c.lon, m.Lat, m.Lon)
		}
	}
}

func TestDecodeUplinkHeader(t *testing.T) {
	p := make([]byte, 432)
	const step = 360.0 / (1 << 24)
	rawLat := uint32(math.Round(40.6892 / step))
	rawLon := uint32(math.Round((-74.0445 + 360) / step))
	p[0] = byte(rawLat >> 15)
	p[1] = byte(rawLat >> 7)
	p[2] = byte(rawLat<<1) & 0xfe
	p[2] |= byte(rawLon >> 23 & 1)
	p[3] = byte(rawLon >> 15)
	p[4] = byte(rawLon >> 7)
	p[5] = byte(rawLon<<1)&0xfe | 1 // position valid
	p[6] = 0x80 | 0x20 | 7          // UTC coupled, app data valid, slot 7
	p[7] = 3 << 4                   // site 3

	u, ok := DecodeUplink(p)
	if !ok {
		t.Fatal("rejected")
	}
	if !u.HasPosition || math.Abs(u.Lat-40.6892) > 0.0001 || math.Abs(u.Lon+74.0445) > 0.0001 {
		t.Errorf("station at %.4f, %.4f", u.Lat, u.Lon)
	}
	if !u.UTCCoupled || !u.AppValid || u.SlotID != 7 || u.TISBSite != 3 {
		t.Errorf("header = %+v", u)
	}
}

// Only message types 1 and 3 carry the mode status element. The other
// long types put the auxiliary state vector where the call sign would
// be, and reading it as one named every aircraft "0000000" whenever one
// arrived.
func TestCallsignOnlyFromModeStatusTypes(t *testing.T) {
	for typ, want := range map[byte]string{1: "N123AB", 2: "", 3: "N123AB", 5: "", 6: ""} {
		p := encodeSV(40.6892, -74.0445, 4500, false, 120, 90, 0)
		p[0] = typ << 3
		putCallsign(p, 1, "N123AB")
		m, ok := DecodeADSB(p)
		if !ok {
			t.Fatalf("type %d was rejected", typ)
		}
		if m.Callsign != want {
			t.Errorf("type %d: callsign = %q, want %q", typ, m.Callsign, want)
		}
	}

	// What a type 2 message actually carries there: a secondary altitude
	// and little else, all small numbers.
	p := encodeSV(40.6892, -74.0445, 4500, false, 120, 90, 0)
	p[0] = 2 << 3
	p[17], p[18] = 0x01, 0x2c
	if m, _ := DecodeADSB(p); m.Callsign != "" {
		t.Errorf("an auxiliary state vector was read as call sign %q", m.Callsign)
	}
}

// The call sign field carries the squawk instead when the CSID flag is
// clear. VFR traffic alternates between the two, and reading both as a
// call sign named it "1200" every other message.
func TestSquawkInTheCallsignField(t *testing.T) {
	p := encodeSV(40.6892, -74.0445, 4500, false, 120, 90, 0)
	putCallsign(p, 1, "1200")
	p[26] &^= 0x02 // CSID clear: a squawk
	m, _ := DecodeADSB(p)
	if m.Callsign != "" || m.Squawk != "1200" {
		t.Errorf("callsign %q, squawk %q; want no call sign and squawk 1200", m.Callsign, m.Squawk)
	}

	// Not four octal digits, so not a squawk either; better nothing
	// than a wrong code.
	putCallsign(p, 1, "N123AB")
	p[26] &^= 0x02
	if m, _ := DecodeADSB(p); m.Callsign != "" || m.Squawk != "" {
		t.Errorf("callsign %q, squawk %q; want neither", m.Callsign, m.Squawk)
	}
}
