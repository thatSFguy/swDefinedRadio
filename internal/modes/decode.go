package modes

import (
	"math"
	"strings"
)

// charset is the 6-bit character encoding used by aircraft identification
// squitters (ICAO Annex 10 Vol IV). Unused code points map to '?'.
const charset = "?ABCDEFGHIJKLMNOPQRSTUVWXYZ?????" +
	" ???????????????0123456789??????"

// ADSB holds whichever fields an extended squitter carried. A single
// squitter only ever fills one group, so each optional group has a flag.
type ADSB struct {
	ICAO     uint32
	TypeCode byte

	Callsign string // type codes 1-4
	Category byte

	HasPosition bool // type codes 5-8 (surface), 9-18 and 20-22 (airborne)
	Odd         bool
	OnGround    bool
	LatCPR      uint32
	LonCPR      uint32

	HasAltitude bool
	Altitude    int // feet

	HasVelocity  bool // type code 19
	GroundSpeed  float64
	Track        float64 // degrees true
	VerticalRate int     // feet per minute, positive climbing

	HasStatus bool      // type code 28 subtype 1
	Emergency Emergency // the crew's declared state
	Squawk    string    // the four-digit Mode A code, e.g. "7700"
}

// Emergency is the state a crew has set, carried by an aircraft status
// squitter. It is the one place ADS-B says outright that something is
// wrong, so it is worth decoding even though the squitter is rare.
type Emergency byte

// The states of ICAO Annex 10 Vol IV table for subtype 1. Anything
// above Downed is reserved.
const (
	NoEmergency Emergency = iota
	GeneralEmergency
	LifeguardMedical
	MinimumFuel
	NoCommunications
	UnlawfulInterference
	DownedAircraft
)

func (e Emergency) String() string {
	switch e {
	case NoEmergency:
		return "none"
	case GeneralEmergency:
		return "general emergency"
	case LifeguardMedical:
		return "lifeguard/medical"
	case MinimumFuel:
		return "minimum fuel"
	case NoCommunications:
		return "no communications"
	case UnlawfulInterference:
		return "unlawful interference"
	case DownedAircraft:
		return "downed aircraft"
	}
	return "reserved"
}

// DecodeADSB interprets a DF17 or DF18 frame. It reports false for type
// codes this decoder does not implement.
func DecodeADSB(msg []byte) (ADSB, bool) {
	if len(msg) != longBits/8 {
		return ADSB{}, false
	}
	if df := msg[0] >> 3; df != 17 && df != 18 {
		return ADSB{}, false
	}

	a := ADSB{
		ICAO:     uint32(msg[1])<<16 | uint32(msg[2])<<8 | uint32(msg[3]),
		TypeCode: msg[4] >> 3,
	}
	switch tc := a.TypeCode; {
	case tc >= 1 && tc <= 4:
		a.Callsign = callsign(msg)
		a.Category = msg[4] & 7
	case tc >= 5 && tc <= 8:
		a.HasPosition, a.OnGround = true, true
		a.readCPR(msg)
	case tc >= 9 && tc <= 18, tc >= 20 && tc <= 22:
		a.HasPosition = true
		a.readCPR(msg)
		a.Altitude, a.HasAltitude = altitude(msg)
	case tc == 19:
		a.readVelocity(msg)
	case tc == 28:
		a.readStatus(msg)
	default:
		return a, false
	}
	return a, true
}

// callsign unpacks the eight 6-bit characters held in ME bits 9-56.
func callsign(msg []byte) string {
	bits := uint64(msg[5])<<40 | uint64(msg[6])<<32 | uint64(msg[7])<<24 |
		uint64(msg[8])<<16 | uint64(msg[9])<<8 | uint64(msg[10])
	var b [8]byte
	for i := range b {
		b[i] = charset[(bits>>(42-6*uint(i)))&0x3F]
	}
	return strings.TrimRight(string(b[:]), " ")
}

// readStatus decodes an aircraft status squitter. Subtype 1 carries the
// emergency state and the Mode A squawk; subtype 2 is the TCAS
// resolution advisory report, which says what a collision-avoidance
// system is doing rather than what the crew has declared.
func (a *ADSB) readStatus(msg []byte) {
	if msg[4]&7 != 1 {
		return
	}
	a.HasStatus = true
	a.Emergency = Emergency(msg[5] >> 5)
	a.Squawk = squawk(uint16(msg[5]&0x1F)<<8 | uint16(msg[6]))
}

// squawk unpacks the 13-bit Mode A identity code. The bits are
// interleaved — C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2 B4 D4, most significant
// first — because the code is transmitted as four octal digits' worth of
// pulses in the order the interrogator expects them, not as a number.
func squawk(id uint16) string {
	bit := func(n uint) int { return int(id>>(12-n)) & 1 }
	a := bit(5)<<2 | bit(3)<<1 | bit(1) // A4 A2 A1
	b := bit(11)<<2 | bit(9)<<1 | bit(7)
	c := bit(4)<<2 | bit(2)<<1 | bit(0)
	d := bit(12)<<2 | bit(10)<<1 | bit(8)
	return string([]byte{byte('0' + a), byte('0' + b), byte('0' + c), byte('0' + d)})
}

// readCPR pulls the compact position report out of a position squitter:
// a format flag and two 17-bit grid coordinates.
func (a *ADSB) readCPR(msg []byte) {
	a.Odd = msg[6]>>2&1 == 1
	a.LatCPR = uint32(msg[6]&0x03)<<15 | uint32(msg[7])<<7 | uint32(msg[8])>>1
	a.LonCPR = uint32(msg[8]&0x01)<<16 | uint32(msg[9])<<8 | uint32(msg[10])
}

// altitude reads the 12-bit altitude code of an airborne position.
//
// Only the Q=1 encoding, in 25 ft steps, is handled. Q=0 switches to
// Gillham code and applies only above 50,000 ft.
func altitude(msg []byte) (int, bool) {
	ac := uint32(msg[5])<<4 | uint32(msg[6])>>4
	if ac == 0 || ac&0x10 == 0 {
		return 0, false
	}
	// Drop the Q bit and close the gap it leaves.
	n := (ac>>5)<<4 | ac&0x0F
	return int(n)*25 - 1000, true
}

// readVelocity decodes an airborne velocity squitter. Subtypes 1 and 2
// report ground speed as a north/south and east/west pair; subtypes 3
// and 4 report airspeed and heading instead and are not handled.
func (a *ADSB) readVelocity(msg []byte) {
	st := msg[4] & 7
	if st != 1 && st != 2 {
		return
	}
	// The supersonic subtype counts in 4 kt steps rather than 1.
	step := 1.0
	if st == 2 {
		step = 4.0
	}

	ew := int(msg[5]&0x03)<<8 | int(msg[6])
	ns := int(msg[7]&0x7F)<<3 | int(msg[8])>>5
	if ew == 0 || ns == 0 { // zero means "no velocity information"
		return
	}
	vx := float64(ew-1) * step
	vy := float64(ns-1) * step
	if msg[5]>>2&1 == 1 {
		vx = -vx // westbound
	}
	if msg[7]>>7&1 == 1 {
		vy = -vy // southbound
	}

	a.HasVelocity = true
	a.GroundSpeed = math.Hypot(vx, vy)
	a.Track = math.Mod(math.Atan2(vx, vy)*180/math.Pi+360, 360)

	if vr := int(msg[8]&0x07)<<6 | int(msg[9])>>2; vr != 0 {
		a.VerticalRate = (vr - 1) * 64
		if msg[8]>>3&1 == 1 {
			a.VerticalRate = -a.VerticalRate
		}
	}
}
