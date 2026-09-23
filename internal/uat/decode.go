package uat

import (
	"math"
	"strings"
)

// Message is a decoded UAT ADS-B message. UAT reports position
// absolutely rather than in the compact form Mode S uses, so a single
// message places an aircraft with no reference position and no waiting
// for a second frame — which is why a UAT target appears the moment it
// is heard.
type Message struct {
	Type      byte   // MDB type code: which elements the message carries
	Address   uint32 // 24-bit ICAO address, or another identifier
	Qualifier byte   // what the address means: 0 and 1 are ICAO

	HasPosition bool
	Lat, Lon    float64

	HasAltitude bool
	Altitude    int  // feet
	Geometric   bool // true if measured against the ellipsoid, not pressure

	OnGround bool

	HasVelocity  bool
	GroundSpeed  float64 // knots
	Track        float64 // degrees true
	VerticalRate int     // feet per minute, positive climbing
	HasVertical  bool

	Callsign string
	Emitter  byte // aircraft category

	NIC byte // containment radius category: how much to trust the position
}

// Type codes that carry a state vector. Every message a receiver cares
// about is one of these; the rest carry only status and intent.
const maxType = 10

// DecodeADSB interprets the message from an ADS-B frame.
func DecodeADSB(p []byte) (Message, bool) {
	if len(p) < 18 {
		return Message{}, false
	}
	m := Message{
		Type:      p[0] >> 3,
		Qualifier: p[0] & 7,
		Address:   uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3]),
	}
	if m.Type > maxType {
		return m, false
	}

	// The state vector, bytes 4-17, is in every message type this
	// decoder accepts.
	rawLat := uint32(p[4])<<15 | uint32(p[5])<<7 | uint32(p[6])>>1
	rawLon := uint32(p[6]&0x01)<<23 | uint32(p[7])<<15 | uint32(p[8])<<7 | uint32(p[9])>>1
	m.Lat, m.Lon, m.HasPosition = position(rawLat, rawLon)

	m.Geometric = p[9]&0x01 == 1
	if raw := uint16(p[10])<<4 | uint16(p[11])>>4; raw != 0 {
		// Zero means unavailable; otherwise 25 ft steps from -1000 ft.
		m.Altitude = int(raw-1)*25 - 1000
		m.HasAltitude = true
	}
	m.NIC = p[11] & 0x0f

	// Air/ground state decides how the next four bytes are read: an
	// aircraft in flight reports north/south and east/west velocity, one
	// on the ground reports a speed and a heading.
	switch state := p[12] >> 6; state {
	case 0, 1: // airborne, subsonic or supersonic
		step := 1.0
		if state == 1 {
			step = 4.0
		}
		ns := int(p[12]&0x1f)<<6 | int(p[13])>>2
		ew := int(p[13]&0x03)<<9 | int(p[14])<<1 | int(p[15])>>7
		m.GroundSpeed, m.Track, m.HasVelocity = velocity(ns, ew, step)

		// Eleven bits: bit 10 says whether the rate is against pressure
		// altitude or the ellipsoid, bit 9 is the sign, and the rest is
		// the magnitude in 64 ft/min steps with zero meaning unknown.
		if raw := int(p[15]&0x7f)<<4 | int(p[16])>>4; raw&0x1ff != 0 {
			rate := (raw&0x1ff - 1) * 64
			if raw&0x200 != 0 {
				rate = -rate
			}
			m.VerticalRate, m.HasVertical = rate, true
		}
	case 2: // on the ground
		m.OnGround = true
		if raw := int(p[12]&0x1f)<<6 | int(p[13])>>2; raw != 0 {
			m.GroundSpeed = float64(raw - 1)
			m.HasVelocity = true
		}
		if raw := int(p[13]&0x03)<<9 | int(p[14])<<1 | int(p[15])>>7; raw != 0 {
			m.Track = float64(raw&0x1ff) * 360 / 512
		}
	}

	// The mode status element, bytes 18 onward, carries the call sign —
	// but only in message types 1 and 3. Types 2, 5 and 6 are just as long
	// and put the auxiliary state vector in the same bytes; read as a call
	// sign, its small numbers come out as "0000000", and the aircraft's
	// name flickered between that and its real one.
	if len(p) >= 34 && (m.Type == 1 || m.Type == 3) {
		m.Emitter, m.Callsign = modeStatus(p)
	}
	return m, true
}

// position converts the raw fields to degrees. UAT counts the whole
// globe in 2^24 steps of longitude, and latitude in the same steps over
// half as much range.
func position(rawLat, rawLon uint32) (lat, lon float64, ok bool) {
	if rawLat == 0 && rawLon == 0 {
		return 0, 0, false // the standard's "no position" value
	}
	const step = 360.0 / (1 << 24)
	lat = float64(rawLat) * step
	if lat > 90 {
		lat -= 180
	}
	lon = float64(rawLon) * step
	if lon > 180 {
		lon -= 360
	}
	if lat < -90 || lat > 90 {
		return 0, 0, false
	}
	return lat, lon, true
}

// velocity turns the north/south and east/west components into the
// speed and track the rest of the receiver works in. Each component is
// a sign bit and a magnitude, where zero means "not available".
func velocity(ns, ew int, step float64) (speed, track float64, ok bool) {
	nsMag, ewMag := ns&0x3ff, ew&0x3ff
	if nsMag == 0 || ewMag == 0 {
		return 0, 0, false
	}
	vn := float64(nsMag-1) * step
	ve := float64(ewMag-1) * step
	if ns&0x400 != 0 {
		vn = -vn
	}
	if ew&0x400 != 0 {
		ve = -ve
	}
	return hypot(vn, ve), bearing(ve, vn), true
}

// modeStatus pulls the emitter category and call sign out of a long
// message. Both are packed base-40 into three 16-bit groups: the first
// holds the category and two characters, the next two hold three each.
// They come apart by division rather than by masking, which is why a
// bit-field reading of this element produces nonsense.
func modeStatus(p []byte) (emitter byte, callsign string) {
	first := uint32(p[17])<<8 | uint32(p[18])
	emitter = byte(first / 1600 % 40)

	var b strings.Builder
	b.WriteByte(base40(first / 40 % 40))
	b.WriteByte(base40(first % 40))
	for _, off := range []int{19, 21} {
		v := uint32(p[off])<<8 | uint32(p[off+1])
		b.WriteByte(base40(v / 1600 % 40))
		b.WriteByte(base40(v / 40 % 40))
		b.WriteByte(base40(v % 40))
	}
	return emitter, strings.TrimSpace(b.String())
}

// base40 is UAT's character set: ten digits, twenty-six letters, then
// padding. Zero is the digit '0', not a space.
func base40(v uint32) byte {
	switch {
	case v <= 9:
		return byte('0' + v)
	case v <= 35:
		return byte('A' + v - 10)
	case v <= 37:
		return ' '
	}
	return '.'
}

// Uplink is the header of a ground station broadcast. The products it
// carries — weather, traffic, notices — are a whole protocol of their
// own and are not decoded; what is useful without them is knowing which
// stations are audible and where they are.
type Uplink struct {
	HasPosition bool
	Lat, Lon    float64
	UTCCoupled  bool
	SlotID      byte
	TISBSite    byte
	AppValid    bool
	Bytes       int
}

// DecodeUplink reads the eight-byte header of a ground uplink.
func DecodeUplink(p []byte) (Uplink, bool) {
	if len(p) < 8 {
		return Uplink{}, false
	}
	var u Uplink
	rawLat := uint32(p[0])<<15 | uint32(p[1])<<7 | uint32(p[2])>>1
	rawLon := uint32(p[2]&0x01)<<23 | uint32(p[3])<<15 | uint32(p[4])<<7 | uint32(p[5])>>1
	if p[5]&0x01 == 1 { // position valid
		u.Lat, u.Lon, u.HasPosition = position(rawLat, rawLon)
	}
	u.UTCCoupled = p[6]&0x80 != 0
	u.AppValid = p[6]&0x20 != 0
	u.SlotID = p[6] & 0x1f
	u.TISBSite = p[7] >> 4
	u.Bytes = len(p)
	return u, true
}

func hypot(a, b float64) float64 { return math.Hypot(a, b) }

func bearing(east, north float64) float64 {
	deg := math.Atan2(east, north) * 180 / math.Pi
	if deg < 0 {
		deg += 360
	}
	return deg
}
