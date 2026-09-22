package modes

import "testing"

// statusFrame builds a DF17 aircraft status squitter: type code 28,
// subtype st, emergency state em, and a 13-bit identity code.
func statusFrame(st byte, em Emergency, id13 uint16) []byte {
	msg := make([]byte, longBits/8)
	msg[0] = 17 << 3
	msg[1], msg[2], msg[3] = 0x40, 0x62, 0x1D
	msg[4] = 28<<3 | st
	msg[5] = byte(em)<<5 | byte(id13>>8)&0x1F
	msg[6] = byte(id13)
	return msg
}

// TestSquawkBits checks the interleaved Mode A encoding against codes
// worked out by hand. The bit order — C1 A1 C2 A2 C4 A4 X B1 D1 B2 D2
// B4 D4 — is the one thing here that cannot be checked by round-tripping
// against an encoder written from the same misunderstanding.
func TestSquawkBits(t *testing.T) {
	cases := []struct {
		id13 uint16
		want string
	}{
		{0xAAA, "7700"}, // A=7 B=7: A1 A2 A4 and B1 B2 B4 all set
		{0xA8A, "7600"},
		{0xAA2, "7500"},
		{0x808, "1200"}, // A1 and B2: the US VFR code
		{0x000, "0000"},
	}
	for _, c := range cases {
		if got := squawk(c.id13); got != c.want {
			t.Errorf("squawk(%#x) = %q, want %q", c.id13, got, c.want)
		}
	}
}

// TestSquawkRoundTrip covers the whole code space against an encoder
// built the other way round, which catches a digit swapped between A,
// B, C and D.
func TestSquawkRoundTrip(t *testing.T) {
	set := func(id *uint16, n uint) { *id |= 1 << (12 - n) }
	for a := range 8 {
		for b := range 8 {
			for c := range 8 {
				for d := range 8 {
					var id uint16
					for _, v := range []struct {
						digit int
						bits  [3]uint // the 1, 2 and 4 bit positions
					}{
						{a, [3]uint{1, 3, 5}},
						{b, [3]uint{7, 9, 11}},
						{c, [3]uint{0, 2, 4}},
						{d, [3]uint{8, 10, 12}},
					} {
						for i, pos := range v.bits {
							if v.digit&(1<<i) != 0 {
								set(&id, pos)
							}
						}
					}
					want := string([]byte{byte('0' + a), byte('0' + b), byte('0' + c), byte('0' + d)})
					if got := squawk(id); got != want {
						t.Fatalf("squawk(%#x) = %q, want %q", id, got, want)
					}
				}
			}
		}
	}
}

func TestDecodeStatusSquitter(t *testing.T) {
	a, ok := DecodeADSB(statusFrame(1, GeneralEmergency, 0xAAA))
	if !ok {
		t.Fatal("a status squitter was not decoded")
	}
	if !a.HasStatus {
		t.Fatal("HasStatus is false")
	}
	if a.Squawk != "7700" {
		t.Errorf("squawk = %q, want 7700", a.Squawk)
	}
	if a.Emergency != GeneralEmergency || a.Emergency.String() != "general emergency" {
		t.Errorf("emergency = %v (%s)", byte(a.Emergency), a.Emergency)
	}
	if a.ICAO != 0x40621D {
		t.Errorf("icao = %06x", a.ICAO)
	}

	// Subtype 2 is the TCAS resolution advisory report, which says what
	// a collision-avoidance box is doing rather than what a crew has
	// declared, and is not decoded here.
	if a, _ := DecodeADSB(statusFrame(2, GeneralEmergency, 0xAAA)); a.HasStatus {
		t.Error("a TCAS advisory was read as a crew emergency")
	}

	// No emergency is the common case: the squawk is still worth having.
	a, _ = DecodeADSB(statusFrame(1, NoEmergency, 0x808))
	if a.Emergency != NoEmergency || a.Squawk != "1200" {
		t.Errorf("routine status = %v/%q", a.Emergency, a.Squawk)
	}
}

func TestEmergencyNames(t *testing.T) {
	for _, c := range []struct {
		e    Emergency
		want string
	}{
		{NoEmergency, "none"},
		{GeneralEmergency, "general emergency"},
		{LifeguardMedical, "lifeguard/medical"},
		{MinimumFuel, "minimum fuel"},
		{NoCommunications, "no communications"},
		{UnlawfulInterference, "unlawful interference"},
		{DownedAircraft, "downed aircraft"},
		{7, "reserved"},
	} {
		if got := c.e.String(); got != c.want {
			t.Errorf("Emergency(%d) = %q, want %q", c.e, got, c.want)
		}
	}
}
